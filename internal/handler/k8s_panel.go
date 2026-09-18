package handler

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"webssh/internal/k8smgr"
	"webssh/internal/sshmgr"
)

// ---------- K8s 面板:集群状态 / Pod watch 增量流 / Pod 日志流 / Pod 终端 ----------
//
// 借鉴 Freelens(桌面端 Kubernetes IDE)的三块设计:
//   cluster-detectors → k8smgr 的集群状态探测缓存
//   kube-watch-event  → kubectl get -w --output-watch-events 的类型化事件流
//   shell-session     → kubectl exec 交互终端(经 wsBridge 桥接)
// 与 K8s 服务转发(k8sforward.go)同一原则:远端只依赖 kubectl,全部经 SSH
// exec 通道;动态参数一律先过 RFC 1123 白名单(ValidK8sName)再经 shQuote。
//
// WS 帧约定:pods 流为文本帧,每帧一个 JSON 文档——首个文档为 List 快照,
// 后续为 {"type":"ADDED|MODIFIED|DELETED","object":{...}} 事件,终止为
// {"type":"exit","error"?};logs 流与 docker_logs 相同为二进制帧透传;
// exec 终端复用容器终端协议(二进制=输出,文本 JSON=input/resize/exit)。

var k8sUpgrader = newUpgrader(32*1024, 32*1024)

// k8sAllNamespaces 「全部命名空间」哨兵值:与合法资源名字符集不冲突。
const k8sAllNamespaces = "_all"

// k8sNsSelector 把 namespace 参数转成 kubectl 的范围参数:空或 _all 返回
// "-A"(全部命名空间),否则校验后返回 "-n 'ns'"。
func k8sNsSelector(ns string) (string, error) {
	if ns == "" || ns == k8sAllNamespaces {
		return "-A", nil
	}
	if !sshmgr.ValidK8sName(ns) {
		return "", fmt.Errorf("namespace 不合法(仅小写字母、数字、- 和 .)")
	}
	return "-n " + shq(ns), nil
}

// k8sState GET /api/sessions/:id/k8s/state?refresh=1
// 返回集群探测结果(k8smgr 内 TTL 缓存,refresh=1 强制重新探测)。
func (a *API) k8sState(c *gin.Context) {
	s, ok := a.mgr.Get(c.Param("id"))
	if !ok {
		fail(c, 404, "会话不存在或已断开")
		return
	}
	c.JSON(200, a.k8s.State(s, c.Query("refresh") == "1"))
}

// k8sPodsWS GET /api/sessions/:id/k8s/pods/ws?namespace=<ns|_all>
// 推送 Pod watch 事件流;每连接独立一条 kubectl exec,断线由前端重连
// 重新拉快照(与 Freelens 重连行为一致,不做服务端共享 watcher)。
func (a *API) k8sPodsWS(c *gin.Context) {
	s, ok := a.mgr.Get(c.Param("id"))
	if !ok {
		fail(c, 404, "会话不存在或已断开")
		return
	}
	nsArg, err := k8sNsSelector(c.Query("namespace"))
	if err != nil {
		fail(c, 400, err.Error())
		return
	}

	ws, err := k8sUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	b := newWSBridge(ws)
	defer b.close()

	// -w --output-watch-events -o json:首文档 List 快照,后续类型化事件
	stdout, cleanup, err := s.RunStream(k8smgr.StreamScript("get pods " + nsArg + " -w --output-watch-events -o json"))
	if err != nil {
		k8sWriteExit(b, err.Error())
		return
	}
	defer cleanup()

	// 文档泵异步:任一侧结束(客户端断开 / watch 流终止)即收尾
	go k8sDocStream(stdout, func(doc json.RawMessage) bool {
		return b.write(websocket.TextMessage, doc) == nil
	})
	b.readLoop(nil)
}

// k8sLogsWS GET /api/sessions/:id/k8s/logs/ws?namespace&pod&container&tail
// 实时推送 Pod 日志(等价 kubectl logs -f --tail N [-c c] -n ns pod)。
func (a *API) k8sLogsWS(c *gin.Context) {
	s, ok := a.mgr.Get(c.Param("id"))
	if !ok {
		fail(c, 404, "会话不存在或已断开")
		return
	}
	ns, pod, ok := k8sRequireNsPod(c)
	if !ok {
		return
	}
	container := c.Query("container")
	if container != "" && !sshmgr.ValidK8sName(container) {
		fail(c, 400, "container 不合法")
		return
	}
	tail, err := strconv.Atoi(c.DefaultQuery("tail", "200"))
	if err != nil || tail < 0 || tail > 100000 {
		fail(c, 400, "tail 必须是 0~100000 之间的整数")
		return
	}

	ws, err := k8sUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	b := newWSBridge(ws)
	defer b.close()

	cpart := ""
	if container != "" {
		cpart = " -c " + shq(container)
	}
	args := fmt.Sprintf("-f --tail %d -n %s %s%s", tail, shq(ns), shq(pod), cpart)
	stdout, cleanup, err := s.RunStream(k8smgr.LogsScript(args))
	if err != nil {
		_ = b.write(websocket.TextMessage, []byte("错误: "+err.Error()))
		return
	}
	defer cleanup()

	b.pumpStdout(stdout, func() {
		exitMsg, _ := json.Marshal(map[string]string{"type": "exit"})
		_ = b.write(websocket.TextMessage, exitMsg)
	})
	b.readLoop(nil)
}

// k8sExecWS GET /api/sessions/:id/k8s/exec/ws?namespace&pod&container&cols&rows
// 在 Pod 内启动交互式终端(等价 kubectl exec -it [-c c] -n ns pod -- bash|sh)。
func (a *API) k8sExecWS(c *gin.Context) {
	s, ok := a.mgr.Get(c.Param("id"))
	if !ok {
		fail(c, 404, "会话不存在或已断开")
		return
	}
	ns, pod, ok := k8sRequireNsPod(c)
	if !ok {
		return
	}
	container := c.Query("container")
	if container != "" && !sshmgr.ValidK8sName(container) {
		fail(c, 400, "container 不合法")
		return
	}
	cols := atoiDefault(c.Query("cols"), 80)
	rows := atoiDefault(c.Query("rows"), 24)

	ws, err := k8sUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	b := newWSBridge(ws)
	defer b.close()

	term, err := s.NewCommandTerminal(cols, rows, k8smgr.ExecScript(ns, pod, container))
	if err != nil {
		_ = b.write(websocket.TextMessage, []byte("错误: "+err.Error()))
		return
	}
	defer term.Close()

	go b.pumpPing(30 * time.Second)
	// Pod 内 shell 退出时复用终端协议的 exit 消息,前端按"已结束"展示
	exitMsg, _ := json.Marshal(map[string]string{"type": "exit"})
	b.pumpStdout(term.Stdout(), func() {
		_ = b.write(websocket.TextMessage, exitMsg)
	})

	// WS → stdin + resize;非 JSON 控制消息的文本/二进制帧直接写入 stdin
	b.readLoop(func(mt int, data []byte) bool {
		if mt == websocket.TextMessage && len(data) > 0 && data[0] == '{' {
			var msg struct {
				Type string `json:"type"`
				Data string `json:"data"`
				Cols int    `json:"cols"`
				Rows int    `json:"rows"`
			}
			if json.Unmarshal(data, &msg) == nil {
				switch msg.Type {
				case "resize":
					if msg.Cols > 0 && msg.Rows > 0 {
						term.Resize(msg.Cols, msg.Rows)
					}
					return true
				case "input":
					if msg.Data != "" {
						term.Stdin().Write([]byte(msg.Data))
					}
					return true
				}
			}
		}
		term.Stdin().Write(data)
		return true
	})
}

// k8sRequireNsPod 校验并取出 logs/exec 端点共用的 namespace+pod 参数。
func k8sRequireNsPod(c *gin.Context) (ns, pod string, ok bool) {
	ns, pod = c.Query("namespace"), c.Query("pod")
	if !sshmgr.ValidK8sName(ns) || !sshmgr.ValidK8sName(pod) {
		fail(c, 400, "namespace/pod 不合法")
		return "", "", false
	}
	return ns, pod, true
}

// k8sDocStream 从 r 连续解码 JSON 文档并逐个交给 emit;emit 返回 false
// (下游写失败)或流结束时结束,结束时补发一条 {"type":"exit"[,"error":...]}
// 终止帧,让前端能确定性地感知 watch 流已终止。脚本层把 kubectl 失败折成
// exit 文档,此处识别后透传并停止读取。独立成纯函数以便单元测试覆盖
// List/事件/坏数据/exit 各分支。
func k8sDocStream(r io.Reader, emit func(json.RawMessage) bool) {
	dec := json.NewDecoder(bufio.NewReaderSize(r, 64*1024))
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			var exitMsg json.RawMessage
			if err == io.EOF {
				exitMsg, _ = json.Marshal(map[string]string{"type": "exit"})
			} else {
				exitMsg, _ = json.Marshal(map[string]string{
					"type":  "exit",
					"error": "K8s 输出流中断: " + firstLine(err.Error()),
				})
			}
			emit(exitMsg)
			return
		}
		var probeDoc struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(raw, &probeDoc)
		if !emit(raw) || probeDoc.Type == "exit" {
			return
		}
	}
}

// k8sWriteExit 向客户端写一条 exit 文本帧(建流失败时使用)。
func k8sWriteExit(b *wsBridge, errMsg string) {
	exitMsg, _ := json.Marshal(map[string]string{"type": "exit", "error": errMsg})
	_ = b.write(websocket.TextMessage, exitMsg)
}
