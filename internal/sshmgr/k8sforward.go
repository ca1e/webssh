package sshmgr

import (
	"encoding/base64"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// k8s 服务转发:用于 sshd 禁止 TCP 转发、但部署了 kubernetes 的主机。
// 原理:本地监听不变;每个接入连接经 SSH exec 通道(不受
// AllowTcpForwarding 限制)在远端运行一段 bash 脚本,脚本内起
// `kubectl port-forward <target> 0:<port>`(0 = 自动选端口,避免并发冲突),
// 从输出解析实际端口,再用 bash 内建 /dev/tcp 把该端口桥接到通道的
// stdin/stdout。除 kubectl 外不要求远端安装任何额外工具(nc/socat 均不需要);
// 桥接脚本以 base64 落到临时文件再交 bash 执行,规避远端登录 shell 差异
// 与引号问题。本地侧为纯 Go(net.Listen + 通道双向搬运),天然支持
// Windows/macOS/Linux,无需本机任何工具。

const (
	k8sMarkerNoKubectl = "__WS_NO_KUBECTL__"
	k8sMarkerNoBash    = "__WS_NO_BASH__"
	k8sMarkerErr       = "__WS_ERR__"
)

var (
	// 资源名按 RFC 1123 label 收紧(小写字母数字与 - .),配合 shQuote
	// 双保险,杜绝注入。
	k8sNameRe   = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)
	k8sKindRe   = regexp.MustCompile(`^(svc|service|po|pod|pods|deploy|deployment|deployments|rs|replicaset|replicasets|ds|daemonset|daemonsets|sts|statefulset|statefulsets)$`)
	k8sTargetRe = regexp.MustCompile(`^([a-z]+)/([a-z0-9]([a-z0-9.-]*[a-z0-9])?)$`)
	// 服务端口:数字或 IANA 端口名(k8s 要求小写)。
	k8sPortRe = regexp.MustCompile(`^([0-9]{1,5}|[a-z]([a-z0-9-]*[a-z0-9])?)$`)
)

// ValidK8sName 判断字符串是否为合法的 k8s 资源名(RFC 1123 label),供
// handler 层校验 query 参数复用。
func ValidK8sName(s string) bool { return k8sNameRe.MatchString(s) }

// validateK8sArgs 校验 namespace / target(kind/name)/ port。
func validateK8sArgs(namespace, target, port string) error {
	if !k8sNameRe.MatchString(namespace) {
		return fmt.Errorf("namespace 不合法(仅小写字母、数字、- 和 .): %q", namespace)
	}
	m := k8sTargetRe.FindStringSubmatch(target)
	if m == nil || !k8sKindRe.MatchString(m[1]) {
		return fmt.Errorf("目标不合法,应为 kind/name(如 svc/my-app): %q", target)
	}
	if !k8sPortRe.MatchString(port) {
		return fmt.Errorf("服务端口不合法(1-65535 的数字或小写端口名): %q", port)
	}
	if n, err := strconv.Atoi(port); err == nil && (n < 1 || n > 65535) {
		return fmt.Errorf("服务端口必须在 1-65535 之间: %q", port)
	}
	return nil
}

// kubectlResolveSnippet 定位 kubectl:先查当前 PATH,找不到再加载登录环境
// (profile)重查。sshd 的 exec 通道是非交互非登录 shell,不加载 rc/profile,
// 而很多主机把 kubectl 的 PATH 或环境准备放在 profile 里;profile 的输出
// 统一静默,避免污染通道数据流。
const kubectlResolveSnippet = `KUBECTL=$(command -v kubectl 2>/dev/null)
if [ -z "$KUBECTL" ]; then
  . /etc/profile >/dev/null 2>&1
  for f in "$HOME/.bash_profile" "$HOME/.profile" "$HOME/.bashrc"; do
    [ -r "$f" ] && . "$f" >/dev/null 2>&1
  done
  KUBECTL=$(command -v kubectl 2>/dev/null)
fi
[ -n "$KUBECTL" ] || { echo "目标主机未安装 kubectl(PATH 与登录环境中均未找到)" >&2; exit 1; }`

// KubectlResolveSnippet 供包外(K8s 面板脚本)复用的导出别名:拼接远端脚本时
// 先定位 kubectl(PATH 直查 + 登录环境回退),行为与包内各脚本完全一致。
const KubectlResolveSnippet = kubectlResolveSnippet

// k8sPFScript 生成远端桥接 bash 脚本。参数须已通过 validateK8sArgs,
// 此处仍经 shQuote 注入 shell 变量赋值,双保险。
func k8sPFScript(namespace, target, port string) string {
	return fmt.Sprintf(`ns=%s
target=%s
port=%s
%s
log=$(mktemp "${TMPDIR:-/tmp}/webssh-kpf.XXXXXX") || { echo "mktemp failed" >&2; exit 1; }
kpid=
cleanup(){ [ -n "$kpid" ] && kill "$kpid" 2>/dev/null; rm -f "$log"; }
trap 'cleanup; exit 0' HUP INT TERM
trap cleanup EXIT
"$KUBECTL" port-forward --namespace="$ns" --address=127.0.0.1 "$target" "0:$port" >"$log" 2>&1 &
kpid=$!
lp=""
i=0
while [ $i -lt 600 ]; do
  lp=$(sed -n 's/.*Forwarding from 127\.0\.0\.1:\([0-9][0-9]*\).*/\1/p' "$log" | head -n1)
  [ -n "$lp" ] && break
  kill -0 "$kpid" 2>/dev/null || { echo "kubectl port-forward 启动失败:" >&2; cat "$log" >&2; exit 1; }
  i=$((i+1)); sleep 0.1
done
[ -n "$lp" ] || { echo "等待 kubectl port-forward 就绪超时" >&2; exit 1; }
exec 3<>"/dev/tcp/127.0.0.1/$lp" || { echo "无法连接转发的端口 127.0.0.1:$lp" >&2; exit 1; }
cat <&3 &
rpid=$!
cat >&3
kill "$rpid" 2>/dev/null
`, shQuote(namespace), shQuote(target), shQuote(port), kubectlResolveSnippet)
}

// k8sPFExecCmd 生成经 $SHELL -c 提交的引导命令:把脚本 base64 解码落到
// 临时文件再交 bash 执行。bash 的 stdin/stdout 即 SSH 通道,供脚本内
// /dev/tcp 桥接数据。
func k8sPFExecCmd(namespace, target, port string) string {
	b64 := base64.StdEncoding.EncodeToString([]byte(k8sPFScript(namespace, target, port)))
	return fmt.Sprintf(`tf=$(mktemp "${TMPDIR:-/tmp}/webssh-kpf-cmd.XXXXXX") || exit 1
echo %s | base64 -d >"$tf" || { rm -f "$tf"; exit 1; }
bash "$tf"; rc=$?
rm -f "$tf"
exit $rc`, shQuote(b64))
}

// checkK8sReady 在添加转发时预检:远端有 kubectl 与 bash、目标资源可访问。
// kubectl 查找与桥接脚本一致(含登录环境回退),报错原样透出
// (RBAC/集群不可达/资源不存在等一目了然)。
func (s *Session) checkK8sReady(namespace, target string) error {
	probe := fmt.Sprintf(`if ! command -v bash >/dev/null 2>&1; then echo %s; exit 0; fi
%s
out=$("$KUBECTL" get -n %s %s -o name 2>&1) || printf '%s%%s\n' "$out"`,
		shQuote(k8sMarkerNoBash), kubectlResolveSnippet,
		shQuote(namespace), shQuote(target), k8sMarkerErr)
	out, err := s.Run(probe)
	if err != nil {
		return fmt.Errorf("预检失败: %s", strings.TrimSpace(out+" "+err.Error()))
	}
	switch {
	case strings.Contains(out, k8sMarkerNoBash):
		return fmt.Errorf("目标主机未安装 bash,无法桥接转发通道")
	case strings.Contains(out, "未安装 kubectl"):
		return fmt.Errorf("目标主机未安装 kubectl(PATH 与登录环境中均未找到),无法使用 K8s 服务转发")
	case strings.Contains(out, k8sMarkerErr):
		// 取标记之后的部分作为 kubectl 报错详情
		detail := out[strings.Index(out, k8sMarkerErr)+len(k8sMarkerErr):]
		detail = strings.TrimSpace(strings.SplitN(detail, "\n", 2)[0])
		return fmt.Errorf("kubectl 无法访问 %s(命名空间 %s): %s", target, namespace, detail)
	}
	return nil
}

// k8sTunnel 为一条本地连接打开远端 port-forward 桥并双向搬运,
// 同时累计连接与流量统计(OpenLens 式面板数据)。
func (f *Forward) k8sTunnel(conn io.ReadWriteCloser) {
	f.totalN.Add(1)
	f.activeN.Add(1)
	defer f.activeN.Add(-1)
	sess, err := f.sess.client.NewSession()
	if err != nil {
		f.setLastError("创建 SSH 通道失败: " + err.Error())
		return
	}
	defer sess.Close()
	stdin, err := sess.StdinPipe()
	if err != nil {
		return
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		return
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		return
	}
	if err := sess.Start(k8sPFExecCmd(f.Namespace, f.K8sTarget, f.K8sPort)); err != nil {
		f.setLastError("启动远端转发失败: " + err.Error())
		return
	}
	// 脚本 stderr 仅用于诊断:桥接失败原因(kubectl 报错等)收进 lastError
	// 供面板展示,限长防止异常刷屏。
	var errBuf syncBuffer
	go func() {
		b := make([]byte, 2048)
		for {
			n, rerr := stderr.Read(b)
			if n > 0 {
				errBuf.Write(b[:n])
			}
			if rerr != nil {
				break
			}
		}
	}()
	pumpCounted(conn, stdin, stdout, f)
	if msg := errBuf.String(); msg != "" {
		f.setLastError(msg)
	}
}

// pumpCounted 在本地连接与远端通道间双向搬运并计数:
// conn→stdin 记 Sent(上行),stdout→conn 记 Recv(下行)。
// 任一方向结束即关闭两侧,语义与 proxy 一致。
func pumpCounted(conn io.ReadWriteCloser, stdin io.WriteCloser, stdout io.Reader, f *Forward) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		b := make([]byte, 32*1024)
		for {
			n, err := conn.Read(b)
			if n > 0 {
				f.sentN.Add(int64(n))
				if _, werr := stdin.Write(b[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		stdin.Close()
		conn.Close()
	}()
	go func() {
		defer wg.Done()
		b := make([]byte, 32*1024)
		for {
			n, err := stdout.Read(b)
			if n > 0 {
				f.recvN.Add(int64(n))
				if _, werr := conn.Write(b[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		conn.Close()
	}()
	wg.Wait()
}

// syncBuffer 限长线程安全缓冲(保留最先写入的内容)。
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if rest := 4096 - b.buf.Len(); rest > 0 {
		if len(p) > rest {
			p = p[:rest]
		}
		b.buf.Write(p)
	}
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
