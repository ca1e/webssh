package handler

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"webssh/internal/sshmgr"
)

// ---------- K8s 资源浏览(仅服务于转发面板的输入建议) ----------

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

var k8sSectionRe = regexp.MustCompile(`(?s)__WS_(\w+)_(OK|ERR)__\n(.*?)\n?__WS_END__`)

type k8sItem struct {
	Name string `json:"name"`
	Desc string `json:"desc"` // service: 端口列表;pods: 运行状态;ns: 空
}

// k8sResources GET /api/sessions/:id/k8s/resources?namespace=xx
// namespace 为空时列命名空间;否则列该命名空间下的 svc 与 pods。
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

	var sections []string
	if ns == "" {
		sections = []string{"NS"}
	} else {
		sections = []string{"SVC", "POD"}
	}
	var sb strings.Builder
	sb.WriteString(k8sRunnerPrelude)
	sb.WriteString("\n")
	for _, sec := range sections {
		var cmd string
		switch sec {
		case "NS":
			cmd = "kubectl get ns --no-headers"
		case "SVC":
			// ns 已过 ValidK8sName 白名单,无需再引号包裹
			cmd = fmt.Sprintf("kubectl get svc -n %s --no-headers", ns)
		case "POD":
			cmd = fmt.Sprintf("kubectl get pods -n %s --no-headers", ns)
		}
		sb.WriteString(fmt.Sprintf("wsrun '%s'; emit %s $?\n", cmd, sec))
	}

	out, err := s.Run(sb.String())
	if err != nil && !strings.Contains(out, "__WS_") {
		fail(c, 400, "K8s 资源查询失败: "+err.Error())
		return
	}

	resp := gin.H{"available": true, "namespaces": []k8sItem{}, "services": []k8sItem{}, "pods": []k8sItem{}}
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
		items := parseK8sTable(sec, body)
		key := map[string]string{"NS": "namespaces", "SVC": "services", "POD": "pods"}[sec]
		resp[key] = items
	}
	if len(errs) > 0 {
		resp["error"] = strings.Join(errs, "; ")
	}
	c.JSON(200, resp)
}

// parseK8sTable 解析 `kubectl get ... --no-headers` 表格输出:
// 首列为名称;service 第 5 列是端口(PORTS),pods 第 3 列是状态(STATUS)。
func parseK8sTable(sec, body string) []k8sItem {
	var items []k8sItem
	for _, line := range strings.Split(body, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		it := k8sItem{Name: f[0]}
		switch sec {
		case "SVC":
			if len(f) >= 5 {
				it.Desc = f[4]
			}
		case "POD":
			if len(f) >= 3 {
				it.Desc = f[2]
			}
		}
		items = append(items, it)
	}
	if items == nil {
		items = []k8sItem{}
	}
	return items
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
