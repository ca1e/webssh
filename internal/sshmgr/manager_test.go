package sshmgr

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"webssh/internal/store"
)

func TestShellInitPayload(t *testing.T) {
	p := shellInitPayload("cd /tmp\necho hi")
	for _, want := range []string{"__ws_cwd", "PROMPT_COMMAND", "precmd", "file://%s%s"} {
		if !strings.Contains(p, want) {
			t.Fatalf("shellInitPayload 缺少 %q:\n%s", want, p)
		}
	}
	// 启动命令逐行以 \r 提交
	if !strings.Contains(p, "cd /tmp\r") || !strings.Contains(p, "echo hi\r") {
		t.Fatalf("启动命令未逐行注入:\n%q", p)
	}
	// 注入输入不允许包含换行(逐行以 \r 提交,避免 PTY 行缓冲歧义)
	if strings.Contains(p, "\n") {
		t.Fatalf("payload 不应包含 \\n:\n%q", p)
	}

	// 无启动命令时仍注入 OSC 7 集成,且只有一行
	p2 := shellInitPayload("   ")
	if !strings.Contains(p2, "__ws_cwd") {
		t.Fatal("空启动命令也应注入 OSC 7 集成")
	}
	if got := strings.Count(p2, "\r"); got != 1 {
		t.Fatalf("空启动命令 payload 应只有 1 行,得到 %d 行", got)
	}
}

func TestHostPort(t *testing.T) {
	cases := []struct {
		h       store.Host
		wantStr string
		wantInt int
	}{
		{store.Host{Host: "example.com"}, "example.com:22", 22},
		{store.Host{Host: "example.com", Port: 2222}, "example.com:2222", 2222},
		{store.Host{Host: "example.com", Port: -1}, "example.com:22", 22},
	}
	for _, tc := range cases {
		gotStr, gotInt := hostPort(tc.h)
		if gotStr != tc.wantStr || gotInt != tc.wantInt {
			t.Fatalf("hostPort(%+v) = %q,%d want %q,%d", tc.h, gotStr, gotInt, tc.wantStr, tc.wantInt)
		}
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("无 HOME 环境,跳过")
	}
	if got := expandHome("~"); got != home {
		t.Fatalf("expandHome(~) = %q, want %q", got, home)
	}
	if got := expandHome("~/foo"); got != filepath.Join(home, "foo") {
		t.Fatalf("expandHome(~/foo) = %q", got)
	}
	if got := expandHome("/etc/ssh"); got != "/etc/ssh" {
		t.Fatalf("expandHome(/etc/ssh) = %q", got)
	}
}

func TestManagerSessionLimit(t *testing.T) {
	// 上限检查发生在拨号前:无需真实连接即可验证拒绝逻辑
	m := New(t.TempDir(), 0, 0) // grace=0:孤儿回收在测试内不生效
	m.mu.Lock()
	m.sessions["fake-1"] = &Session{ID: "fake-1"}
	m.sessions["fake-2"] = &Session{ID: "fake-2"}
	m.mu.Unlock()
	m.maxSessions = 2
	if _, err := m.OpenSession(context.Background(), store.Host{Host: "127.0.0.1"}, nil, true); err == nil {
		t.Fatal("达到会话上限后应拒绝新建")
	} else if !strings.Contains(err.Error(), "上限") {
		t.Fatalf("错误信息应包含上限说明,得到 %q", err.Error())
	}
}

func TestScanOSC7(t *testing.T) {
	cases := []struct {
		name  string
		chunk string
		want  string
	}{
		{"BEL 结尾", "\x1b]7;file://h1/root\x07tail", "/root"},
		{"ST 结尾", "\x1b]7;file://h1/root\x1b\\tail", "/root"},
		// 线上实测形态:OSC 7 以 ST 结尾,提示符紧随 BEL 结尾的 OSC 0 标题,
		// 必须取更早的 ST,否则标题序列被吞进路径(SFTP 打开 /root\]0;root@…)
		{"ST 结尾后随 OSC0 标题", "\x1b]7;file://server113/root\x1b\\\x1b]0;root@server113:~\x07", "/root"},
		{"URL 编码", "\x1b]7;file://h1/my%20dir\x07", "/my dir"},
		{"深路径", "\x1b]7;file://h1/a/b/c/d/e/f\x07", "/a/b/c/d/e/f"},
		{"非绝对路径忽略", "\x1b]7;file://h1\x07", ""},
		{"序列被截断则等待", "\x1b]7;file://h1/ro", ""},
	}
	for _, tc := range cases {
		s := &Session{}
		ScanOSC7(s, []byte(tc.chunk))
		if got := s.Cwd(); got != tc.want {
			t.Errorf("%s: Cwd() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestSetCwdRejectsControlChars(t *testing.T) {
	s := &Session{}
	for _, bad := range []string{"/root\x1b\\]0;root@h", "/tmp\x07", "/a\r\nb", ""} {
		s.SetCwd(bad)
		if got := s.Cwd(); got != "" {
			t.Errorf("SetCwd(%q) 被接受, Cwd() = %q", bad, got)
		}
	}
	s.SetCwd("/tmp")
	s.SetCwd("/bad\x1b")
	if got := s.Cwd(); got != "/tmp" {
		t.Errorf("非法更新不应覆盖原值, Cwd() = %q", got)
	}
}

// TestSeamlessCmdBashHookValue 功能验证 bash 无感注入的引号转义:经 bash 求值
// 后 PROMPT_COMMAND 的值必须与 type-ahead 钩子的 __ws_cwd 定义等价。
func TestSeamlessCmdBashHookValue(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("无 bash,跳过")
	}
	cmd := seamlessCmd("/usr/local/bin/bash")
	if cmd == "" || !strings.Contains(cmd, "exec '/usr/local/bin/bash' -l -i") {
		t.Fatalf("bash 无感命令生成错误:\n%s", cmd)
	}
	// bash 默认模式下独立赋值行不进环境,缺 export 会导致内层 bash 收不到钩子
	if !strings.Contains(cmd, "\nexport PROMPT_COMMAND\n") {
		t.Fatalf("bash 无感命令缺少 export:\n%s", cmd)
	}
	// 将 exec 行替换为打印,验证赋值求值结果
	printer := strings.Replace(cmd, "exec '/usr/local/bin/bash' -l -i", `printf '%s' "$PROMPT_COMMAND"`, 1)
	out, err := exec.Command("bash", "-c", printer).Output()
	if err != nil {
		t.Fatalf("bash 求值失败: %v", err)
	}
	want := `__ws_cwd(){ printf '\033]7;file://%s%s\033\\' "${HOSTNAME:-$HOST}" "$PWD"; }; __ws_cwd`
	if string(out) != want {
		t.Fatalf("PROMPT_COMMAND 值错误:\n got %q\nwant %q", string(out), want)
	}
}

// TestSeamlessCmdZsh 功能验证 zsh 无感注入:生成的脚本在 zsh 下执行后,临时
// ZDOTDIR 中应生成含钩子的 .zshrc 且用户配置转发逻辑就位。
func TestSeamlessCmdZsh(t *testing.T) {
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("无 zsh,跳过")
	}
	cmd := seamlessCmd("/bin/zsh")
	if cmd == "" || !strings.Contains(cmd, "exec '/bin/zsh' -l -i") {
		t.Fatalf("zsh 无感命令生成错误:\n%s", cmd)
	}
	// 干净 HOME 模拟远端;把最终 exec 行替换为打印诊断信息
	printer := strings.Replace(cmd, `_WS_DIR="$d" ZDOTDIR="$d" exec '/bin/zsh' -l -i`,
		`printf 'ZDOTDIR=%s\n' "$d"; ls -A "$d"; printf -- '--- .zshrc ---\n'; cat "$d/.zshrc"`, 1)
	home := t.TempDir()
	c := exec.Command("zsh", "-c", printer)
	c.Env = append(os.Environ(), "HOME="+home, "SHELL=/bin/zsh")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("zsh 求值失败: %v\n%s", err, out)
	}
	text := string(out)
	for _, want := range []string{
		"ZDOTDIR=/tmp/.webssh-zsh-",
		".zshenv", ".zprofile", ".zshrc", ".zlogin",
		`printf '\033]7;file://%s%s\033\\' "${HOSTNAME:-$HOST}" "$PWD"`,
		"precmd_functions+=(__ws_cwd)",
		`. "$HOME/.zshrc"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("zsh 脚本产物缺少 %q:\n%s", want, text)
		}
	}
}

func TestSeamlessCmdUnknownShell(t *testing.T) {
	for _, sh := range []string{"", "/usr/bin/fish", "/bin/sh", "/bin/csh"} {
		if got := seamlessCmd(sh); got != "" {
			t.Errorf("seamlessCmd(%q) 应回退空串,得到:\n%s", sh, got)
		}
	}
}

func TestCwdSubscribeNotify(t *testing.T) {
	s := &Session{}
	ch := s.SubscribeCwd()
	defer s.UnsubscribeCwd(ch)

	recv := func() (string, bool) {
		select {
		case p := <-ch:
			return p, true
		default:
			return "", false
		}
	}

	s.SetCwd("/a")
	if p, ok := recv(); !ok || p != "/a" {
		t.Fatalf("首次变更应收到通知 /a,得到 %q ok=%v", p, ok)
	}
	// 同值不广播
	s.SetCwd("/a")
	if p, ok := recv(); ok {
		t.Fatalf("同值不应广播,得到 %q", p)
	}
	// 非法值不广播
	s.SetCwd("/bad\x1b")
	if _, ok := recv(); ok {
		t.Fatal("非法值不应广播")
	}
	// 缓冲满即丢:连续变更不阻塞发送方,订阅方最终读到最新值
	s.SetCwd("/b")
	s.SetCwd("/c")
	if p, ok := recv(); !ok || p != "/b" {
		t.Fatalf("应收到首条 /b,得到 %q ok=%v", p, ok)
	}
	if got := s.Cwd(); got != "/c" {
		t.Fatalf("Cwd() 应为最新值 /c,得到 %q", got)
	}
	// 退订后不再收到
	s.UnsubscribeCwd(ch)
	s.SetCwd("/d")
	if p, ok := recv(); ok {
		t.Fatalf("退订后不应收到通知,得到 %q", p)
	}
}
