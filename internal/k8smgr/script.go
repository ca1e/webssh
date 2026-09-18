package k8smgr

import (
	"fmt"
	"strings"

	"webssh/internal/sshmgr"
)

// K8s 面板的远端脚本层:全部能力经 SSH exec 通道执行,远端只依赖 kubectl
// (与 sshmgr 的 K8s 服务转发同一原则)。脚本统一先经 KubectlResolveSnippet
// 定位 kubectl(PATH 直查 + 登录环境回退),sshd exec 通道是非交互非登录
// shell,不加载 rc/profile,而很多主机把 kubectl 的 PATH 放在 profile 里。

// shQuote 以 POSIX 单引号规则转义字符串(与 handler/manager 中的同名实现一致;
// 包间不导出以避免耦合,调用方传入的动态参数必须先过 RFC 1123 白名单校验)。
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ProbeScript 生成集群状态探测脚本:单次 exec 输出四段标记包裹的结果
// (与 handler/k8s.go 的 wsrun/emit 协议同构)。kubectl 经 KubectlResolveSnippet
// 定位(PATH 直查 + 登录环境回退——不能依赖 bash -lc:登录 shell 反而会重置
// 继承的 PATH);全部命令加 --request-timeout 兜底,避免集群不可达时
// exec 通道长时间挂起。
func ProbeScript() string {
	return sshmgr.KubectlResolveSnippet + `
emit() {
  if [ "$2" -eq 0 ]; then
    printf '__WS_%s_OK__\n%s\n__WS_END__\n' "$1" "$out"
  else
    printf '__WS_%s_ERR__\n%s\n__WS_END__\n' "$1" "$out"
  fi
}
out=$("$KUBECTL" config current-context --request-timeout=8s 2>&1); emit CTX $?
out=$("$KUBECTL" config view --minify -o jsonpath=` + shQuote(`{.clusters[0].name}{"\t"}{.clusters[0].cluster.server}`) + ` 2>&1); emit CFG $?
out=$("$KUBECTL" version -o json --request-timeout=8s 2>&1); emit VER $?
out=$("$KUBECTL" get nodes -o json --request-timeout=8s 2>&1); emit NODES $?`
}

// StreamScript 生成持续 JSON 流脚本(kubectl get -w / logs -f 等):
// kubectl 的 stderr 落到临时文件不混入 stdout 数据流;命令失败(含启动即失败,
// 如 RBAC 拒绝/集群不可达)时把 stderr 尾部折进一条 {"type":"exit","error":...}
// JSON 行再退出,调用方的流式解码器将其作为终止事件透出,前端可见失败原因。
// args 中的动态参数由调用方先经白名单校验 + shQuote 处理。
func StreamScript(args string) string {
	return fmt.Sprintf(`%s
errf=$(mktemp "${TMPDIR:-/tmp}/webssh-k8s.XXXXXX") || exit 1
trap 'rm -f "$errf"' EXIT
if ! "$KUBECTL" %s 2>"$errf"; then
  msg=$(tail -c 400 "$errf" | tr '\n' ' ' | tr -d '"\\')
  printf '{"type":"exit","error":"kubectl: %%s"}\n' "$msg"
  exit 1
fi`, sshmgr.KubectlResolveSnippet, args)
}

// LogsScript 生成 Pod 日志流脚本(kubectl logs -f)。与 StreamScript 不同:
// 日志是纯文本流,stderr 直接 2>&1 并入输出,kubectl 的报错(RBAC/不存在/
// 容器未就绪等)即成为日志面板可见的首行内容。
func LogsScript(args string) string {
	return fmt.Sprintf(`%s
exec "$KUBECTL" logs %s 2>&1`, sshmgr.KubectlResolveSnippet, args)
}

// ExecScript 生成 Pod 交互终端脚本(PTY 内运行)。bash 优先、失败回退 sh,
// 与 docker exec 的回退策略一致;不能用 exec 内建做回退(命令不存在时
// shell 直接退出,|| 不会执行)。
func ExecScript(namespace, pod, container string) string {
	cpart := ""
	if container != "" {
		cpart = " -c " + shQuote(container)
	}
	return fmt.Sprintf(`%s
"$KUBECTL" exec -it -n %s %s%s -- bash 2>/dev/null || "$KUBECTL" exec -it -n %s %s%s -- sh`,
		sshmgr.KubectlResolveSnippet, shQuote(namespace), shQuote(pod), cpart,
		shQuote(namespace), shQuote(pod), cpart)
}
