package handler

import (
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"

	"webssh/internal/opsview"
	"webssh/internal/sshmgr"
)

// ---------- K8s 资源浏览(服务于转发面板的 OpenLens 式端口列表) ----------

// k8sRunnerPrelude 定义 wsrun:优先在登录 shell(bash -lc)中执行 kubectl
// 命令——sshd exec 通道是非交互非登录 shell,PATH/KUBECONFIG 常只在 profile
// 中准备;若登录环境反而不可用(command not found)则回退普通 sh。
// emit 把各段结果以标记包裹输出,错误信息(stderr 合入)一并带回,
// 不再用 2>/dev/null 吞掉——前端要能看到"为什么是空的"。
const k8sRunnerPrelude = `wsrun() {
  if command -v bash >/dev/null 2>&1; then
    out=$(bash -lc "$1" 2>&1); rc=$?
    if [ $rc -ne 0 ] && printf %s "$out" | grep -qi 'command not found'; then
      out=$(sh -c "$1" 2>&1); rc=$?
    fi
  else
    out=$(sh -c "$1" 2>&1); rc=$?
  fi
  return $rc
}
emit() {
  if [ "$2" -eq 0 ]; then
    printf '__WS_%s_OK__\n%s\n__WS_END__\n' "$1" "$out"
  else
    printf '__WS_%s_ERR__\n%s\n__WS_END__\n' "$1" "$out"
  fi
}`

// k8sSectionRe 与 dockerInfoScript 共用的 __WS_*__ 分段协议正则(定义在
// opsview,避免协议分叉)。
var k8sSectionRe = opsview.SectionRe

type k8sPort struct {
	Port string `json:"port"` // 数字或端口名,原样透传给 port-forward
	Name string `json:"name"`
}

type k8sTarget struct {
	Name   string    `json:"name"`
	Status string    `json:"status,omitempty"` // pods: phase
	Ports  []k8sPort `json:"ports"`
}

// kubectl 输出模板:每行 kind<TAB>name<TAB>[status<TAB>]端口列表,
// 端口为 port:name,分号分隔。`\t`/`\n` 必须写作 {"\t"}/{"\n"}(jsonpath
// 动作内的 Go 字符串转义,裸 \t 会被原样输出);模板只用双引号,
// 经 shq 包裹后仍安全。
func k8sSvcCmd(ns string) string {
	return `kubectl get svc -n ` + ns +
		` -o jsonpath='{range .items[*]}svc{"\t"}{.metadata.name}{"\t"}{range .spec.ports[*]}{.port}:{.name};{end}{"\n"}{end}'`
}

func k8sPodCmd(ns string) string {
	return `kubectl get pods -n ` + ns +
		` -o jsonpath='{range .items[*]}po{"\t"}{.metadata.name}{"\t"}{.status.phase}{"\t"}{range .spec.containers[*]}{range .ports[*]}{.containerPort}:{.name};{end}{end}{"\n"}{end}'`
}

const k8sNsCmd = `kubectl get ns --no-headers`

// shq 单引号安全包裹(仅含 [a-z0-9.-] 的 ns 实际无需转义,统一处理)。
func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// k8sResources GET /api/sessions/:id/k8s/resources?namespace=xx
// namespace 为空时列命名空间;否则列该命名空间下的 svc 与 pods(含端口)。
// available=false 表示远端找不到 kubectl;error 携带失败原因(可展示)。
func (a *API) k8sResources(c *gin.Context) {
	s, ok := a.mgr.Get(c.Param("id"))
	if !ok {
		fail(c, 404, "会话不存在或已断开")
		return
	}
	ns := strings.TrimSpace(c.Query("namespace"))
	if ns != "" && !sshmgr.ValidK8sName(ns) {
		fail(c, 400, "namespace 不合法")
		return
	}

	// 组装单次 exec 的多段查询命令(ns 已过白名单校验,直接内联安全)。
	var sb strings.Builder
	sb.WriteString(k8sRunnerPrelude)
	sb.WriteString("\n")
	if ns == "" {
		sb.WriteString(fmt.Sprintf("wsrun %s; emit NS $?\n", shq(k8sNsCmd)))
	} else {
		sb.WriteString(fmt.Sprintf("wsrun %s; emit SVC $?\n", shq(k8sSvcCmd(ns))))
		sb.WriteString(fmt.Sprintf("wsrun %s; emit POD $?\n", shq(k8sPodCmd(ns))))
	}

	out, err := s.Run(sb.String())
	if err != nil && !strings.Contains(out, "__WS_") {
		fail(c, 400, "K8s 资源查询失败: "+err.Error())
		return
	}

	resp := gin.H{"available": true, "namespaces": []string{}, "services": []k8sTarget{}, "pods": []k8sTarget{}}
	var errs []string
	for _, m := range k8sSectionRe.FindAllStringSubmatch(out, -1) {
		sec, status, body := m[1], m[2], strings.TrimSpace(m[3])
		if status == "ERR" {
			if strings.Contains(body, "command not found") {
				resp["available"] = false
			}
			errs = append(errs, fmt.Sprintf("%s: %s", sec, firstLine(body)))
			continue
		}
		switch sec {
		case "NS":
			resp["namespaces"] = parseK8sNS(body)
		case "SVC":
			resp["services"] = parseK8sTargets("svc", body)
		case "POD":
			resp["pods"] = parseK8sTargets("po", body)
		}
	}
	if len(errs) > 0 {
		resp["error"] = strings.Join(errs, "; ")
	}
	c.JSON(200, resp)
}

// parseK8sNS 解析命名空间列表(`kubectl get ns --no-headers` 表格,取首列)。
func parseK8sNS(body string) []string {
	var list []string
	for _, line := range strings.Split(body, "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && f[0] != "NAME" {
			list = append(list, f[0])
		}
	}
	if list == nil {
		list = []string{}
	}
	return list
}

// parseK8sTargets 解析 jsonpath 的 TSV 行:kind<TAB>name<TAB>[status<TAB>]ports;
// ports 为 port:name 的分号序列(无名端口 name 为空)。
func parseK8sTargets(kind, body string) []k8sTarget {
	var out []k8sTarget
	for _, line := range strings.Split(body, "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 3 || f[0] != kind || f[1] == "" {
			continue
		}
		var portStr string
		t := k8sTarget{Name: f[1]}
		if kind == "po" {
			if len(f) > 3 {
				t.Status = f[2]
				portStr = f[3]
			}
		} else {
			portStr = f[2]
		}
		for _, ps := range strings.Split(portStr, ";") {
			if ps == "" {
				continue
			}
			port, name, _ := strings.Cut(ps, ":")
			if port != "" {
				t.Ports = append(t.Ports, k8sPort{Port: port, Name: name})
			}
		}
		if t.Ports == nil {
			t.Ports = []k8sPort{}
		}
		out = append(out, t)
	}
	if out == nil {
		out = []k8sTarget{}
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
