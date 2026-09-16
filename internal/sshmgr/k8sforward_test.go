package sshmgr

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateK8sArgs(t *testing.T) {
	ok := []struct{ ns, target, port string }{
		{"default", "svc/my-app", "80"},
		{"kube-system", "pod/core-dns-abc123", "53"},
		{"a", "service/a", "https"},
		{"ns-1.x", "deploy/web", "8080"},
		{"default", "pods/x1", "mysql"},
	}
	for _, c := range ok {
		if err := validateK8sArgs(c.ns, c.target, c.port); err != nil {
			t.Errorf("合法参数被拒 %v: %v", c, err)
		}
	}
	bad := []struct{ ns, target, port string }{
		{"Default", "svc/x", "80"},        // 大写
		{"", "svc/x", "80"},               // 空 ns
		{"a; rm -rf /", "svc/x", "80"},    // 注入
		{"a'$(id)'", "svc/x", "80"},       // 注入
		{"default", "svc", "80"},          // 缺 kind
		{"default", "svc/x", ""},          // 空端口
		{"default", "svc/", "80"},         // 空名
		{"default", "evil/x", "80"},       // 未知 kind
		{"default", "svc/x;echo", "80"},   // target 注入
		{"default", "svc/x", "0"},         // 端口 0
		{"default", "svc/x", "70000"},     // 越界
		{"default", "svc/x", "$(id)"},     // 端口注入
		{"default", "svc/x", "HTTP-NAME"}, // 端口名大写
		{"default", "svc/-x", "80"},       // 名以 - 开头
	}
	for _, c := range bad {
		if err := validateK8sArgs(c.ns, c.target, c.port); err == nil {
			t.Errorf("非法参数被接受 %v", c)
		}
	}
}

func TestK8sPFScriptContent(t *testing.T) {
	script := k8sPFScript("prod", "svc/api", "http")
	for _, want := range []string{
		`ns='prod'`,
		`target='svc/api'`,
		`port='http'`,
		`KUBECTL=$(command -v kubectl 2>/dev/null)`,
		`"$KUBECTL" port-forward --namespace="$ns" --address=127.0.0.1 "$target" "0:$port"`,
		`Forwarding from 127\.0\.0\.1:\([0-9][0-9]*\)`,
		`exec 3<>"/dev/tcp/127.0.0.1/$lp"`,
		`trap 'cleanup; exit 0' HUP INT TERM`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("脚本缺少关键片段 %q", want)
		}
	}
}

func TestK8sPFExecCmdEmbedsScript(t *testing.T) {
	cmd := k8sPFExecCmd("default", "svc/x", "80")
	// 引导层:mktemp + base64 解码 + bash 执行 + 自清理
	for _, want := range []string{`mktemp`, `base64 -d`, `bash "$tf"`, `rm -f "$tf"`} {
		if !strings.Contains(cmd, want) {
			t.Errorf("引导命令缺少 %q", want)
		}
	}
	// 提取 base64 载荷并还原,应与 k8sPFScript 输出一致
	i := strings.Index(cmd, "echo ")
	j := strings.Index(cmd[i:], " | base64 -d")
	if i < 0 || j < 0 {
		t.Fatalf("无法定位 base64 载荷: %s", cmd)
	}
	payload := strings.Trim(cmd[i+len("echo ") : i+j], "'")
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("base64 载荷无法解码: %v", err)
	}
	if string(decoded) != k8sPFScript("default", "svc/x", "80") {
		t.Error("base64 载荷与 k8sPFScript 不一致")
	}
}

func TestValidK8sName(t *testing.T) {
	if !ValidK8sName("kube-system") || ValidK8sName("a b;ls") {
		t.Error("ValidK8sName 判定异常")
	}
}

// TestK8sScriptBashSyntax 把生成的脚本交 `bash -n` 做语法校验,
// 防止脚本模板改动引入语法错误(运行期才会暴露)。bash 不存在的机器跳过。
func TestK8sScriptBashSyntax(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash on PATH")
	}
	for _, s := range []string{k8sPFScript("default", "svc/x", "80"), k8sPFExecCmd("default", "svc/x", "80")} {
		f := filepath.Join(t.TempDir(), "s.sh")
		if err := os.WriteFile(f, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(bash, "-n", f).CombinedOutput(); err != nil {
			t.Errorf("bash -n 失败: %v\n%s", err, out)
		}
	}
}
