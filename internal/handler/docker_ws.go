package handler

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// ---------- Docker 容器日志流 + 容器 Shell WebSocket ----------

var dockerUpgrader = newUpgrader(8192, 8192)

// dockerLogsWS 实时推送容器日志(等价 docker logs -f --tail N <cid>)
// GET /api/sessions/:sid/docker/logs/ws?cid=xxx&tail=200
func (a *API) dockerLogsWS(c *gin.Context) {
	s, ok := a.mgr.Get(c.Param("id"))
	if !ok {
		fail(c, 404, "会话不存在或已断开")
		return
	}
	cid := c.Query("cid")
	if cid == "" {
		fail(c, 400, "容器 ID 不能为空")
		return
	}
	tail, err := strconv.Atoi(c.DefaultQuery("tail", "200"))
	if err != nil || tail < 0 || tail > 100000 {
		fail(c, 400, "tail 必须是 0~100000 之间的整数")
		return
	}

	ws, err := dockerUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	b := newWSBridge(ws)
	defer b.close()

	// tail 已强制为整数,cid 经白名单清洗;引号统一走 shellQuote,杜绝拼接注入
	cmd := fmt.Sprintf("docker logs -f --tail %d %s 2>&1", tail, shellQuote(sanitizeDockerID(cid)))
	stdout, cleanup, err := s.RunStream(cmd)
	if err != nil {
		_ = b.write(websocket.TextMessage, []byte("错误: "+err.Error()))
		return
	}
	defer cleanup()

	// stdout → WS;readLoop 返回(客户端断开或流结束已关闭桥)即收尾
	b.pumpStdout(stdout, nil)
	b.readLoop(nil)
}

// dockerExecWS 在容器内启动交互式 shell(等价 docker exec -it <cid> bash,失败回退 sh)
// GET /api/sessions/:sid/docker/shell/ws?cid=xxx
func (a *API) dockerExecWS(c *gin.Context) {
	s, ok := a.mgr.Get(c.Param("id"))
	if !ok {
		fail(c, 404, "会话不存在或已断开")
		return
	}
	cid := c.Query("cid")
	if cid == "" {
		fail(c, 400, "容器 ID 不能为空")
		return
	}
	cols := atoiDefault(c.Query("cols"), 80)
	rows := atoiDefault(c.Query("rows"), 24)
	safeID := sanitizeDockerID(cid)

	ws, err := dockerUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	b := newWSBridge(ws)
	defer b.close()

	// bash 优先,失败回退 sh
	term, err := s.NewCommandTerminal(cols, rows,
		fmt.Sprintf("docker exec -it %s bash 2>/dev/null || docker exec -it %s sh", shellQuote(safeID), shellQuote(safeID)))
	if err != nil {
		_ = b.write(websocket.TextMessage, []byte("错误: "+err.Error()))
		return
	}
	defer term.Close()

	go b.pumpPing(30 * time.Second)
	// 容器内 shell 退出时复用终端协议的 exit 消息,前端按"已结束"展示
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

// sanitizeDockerID 过滤容器 ID/名称,仅保留字母数字与 _ - .(白名单清洗)。
func sanitizeDockerID(id string) string {
	var out []byte
	for _, c := range []byte(id) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' || c == '.' {
			out = append(out, c)
		}
	}
	return string(out)
}

// shellQuote 以 POSIX 单引号规则转义字符串,用于拼接远程 shell 命令参数。
// 拼接用户可控内容时必须先经白名单清洗,再统一走本函数,禁止手写引号。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
