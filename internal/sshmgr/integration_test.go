//go:build integration

// 集成测试:拉起 cmd/testssh(进程内迷你 SSH 服务端),验证
// OpenSession → Run / 终端 + OSC 7 cwd 跟踪 / SFTP 读写 / 会话上限的端到端链路。
// 运行:go test -tags=integration ./internal/sshmgr/
package sshmgr

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"webssh/internal/secrets"
	"webssh/internal/store"
)

var testSSHAddr string

// itSec 集成测试包级凭据库:所有 Manager 经 newITManager 注入其 Reveal,
// 测试主机引用 itPwRef(test123)。与生产装配同路径:store.Host 只持引用。
var (
	itSec   *secrets.Store
	itPwRef string
)

func TestMain(m *testing.M) {
	// 编译 cmd/testssh 为临时二进制(测试无需 shell 命令行协调)
	bin := filepath.Join(os.TempDir(), "webssh-integration-testssh")
	build := exec.Command("go", "build", "-o", bin, "webssh/cmd/testssh")
	build.Dir = moduleRoot()
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "编译 cmd/testssh 失败: %v\n%s", err, out)
		os.Exit(1)
	}

	// 取空闲端口后启动测试服务端
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "取空闲端口失败: %v\n", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	testSSHAddr = fmt.Sprintf("127.0.0.1:%d", port)

	// 环境隔离:固定 SHELL 使会话的登录 shell 探测走 bash(无感注入路径),
	// 隔离 HOME 避免读取测试机真实启动脚本(bashrc 若覆盖 PROMPT_COMMAND 会
	// 触发 watchdog 降级,虽不影响正确性但让测试路径不确定)。
	testHome, err := os.MkdirTemp("", "webssh-it-home")
	if err != nil {
		fmt.Fprintf(os.Stderr, "创建隔离 HOME 失败: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(testHome)

	// 包级凭据库 + 测试密码引用(目录同样隔离在临时区)
	secDir, err := os.MkdirTemp("", "webssh-it-secrets")
	if err != nil {
		fmt.Fprintf(os.Stderr, "创建凭据目录失败: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(secDir)
	itSec, err = secrets.Open(secDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开测试凭据库失败: %v\n", err)
		os.Exit(1)
	}
	itPwRef, err = itSec.Put("", secrets.KindPassword, "test123")
	if err != nil {
		fmt.Fprintf(os.Stderr, "写入测试密码失败: %v\n", err)
		os.Exit(1)
	}
	srv := exec.Command(bin, "-addr", testSSHAddr, "-user", "test", "-pass", "test123")
	srv.Env = append(os.Environ(), "SHELL=/bin/bash", "HOME="+testHome)
	// 工作目录一并隔离:SFTP getcwd 回到该目录(与真实 sshd 把 SFTP 服务
	// 初始化到 $HOME 的行为一致),原始模式辅助程序的部署目录落在临时
	// HOME 下,不污染仓库。
	srv.Dir = testHome
	srv.Stdout, srv.Stderr = os.Stdout, os.Stderr
	if err := srv.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "启动 testssh 失败: %v\n", err)
		os.Exit(1)
	}

	// 等待端口就绪
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", testSSHAddr, 500*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "testssh 端口未就绪: %v\n", err)
			_ = srv.Process.Kill()
			_ = srv.Wait()
			os.Exit(1)
		}
		time.Sleep(100 * time.Millisecond)
	}

	code := m.Run()
	_ = srv.Process.Kill()
	_ = srv.Wait()
	_ = os.Remove(bin)
	os.Exit(code)
}

// moduleRoot 返回 webssh 模块根目录(测试文件位于 internal/sshmgr)。
func moduleRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return filepath.Dir(filepath.Dir(wd))
}

// newITManager 创建接入凭据库的测试 Manager(替代 New(t.TempDir(), n, g))。
func newITManager(t *testing.T, maxSessions int) *Manager {
	t.Helper()
	m := New(t.TempDir(), maxSessions, 0) // grace=0:测试内关闭孤儿回收
	m.SetSecretResolver(itSec.Reveal)
	return m
}

// openTestSession 建立到 testssh 的会话(test/test123,TOFU 信任新密钥)。
func openTestSession(t *testing.T, mgr *Manager) *Session {
	t.Helper()
	port := 0
	if _, portStr, err := net.SplitHostPort(testSSHAddr); err == nil {
		fmt.Sscanf(portStr, "%d", &port)
	}
	// FollowSSHPath:终端用例依赖注入的 OSC 7 钩子(cwd 跟踪/启动命令)
	host := store.Host{
		Name: "it-host", Host: "127.0.0.1", Port: port,
		Username: "test", AuthType: store.AuthPassword, PasswordRef: itPwRef,
		FollowSSHPath: true,
	}
	s, err := mgr.OpenSession(context.Background(), host, nil, true)
	if err != nil {
		t.Fatalf("OpenSession 失败: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// openPlainSession 建立未勾选 FollowSSHPath 的会话(默认普通连接形态)。
func openPlainSession(t *testing.T, mgr *Manager) *Session {
	s := openTestSession(t, mgr)
	s.FollowSSHPath = false // 开终端前关闭:NewTerminal 据此选择连接方式
	return s
}

func TestIntegrationTerminalPlainDefault(t *testing.T) {
	// 默认(未勾选 FollowSSHPath)= 普通连接:终端无任何注入,
	// 即使执行 cd 也不产生 OSC 7,更无钩子行回显
	s := openPlainSession(t, newITManager(t, 8))
	term, err := s.NewTerminal(80, 24, "")
	if err != nil {
		t.Fatalf("NewTerminal 失败: %v", err)
	}
	defer term.Close()

	if _, err := term.Stdin().Write([]byte("cd /tmp\r")); err != nil {
		t.Fatalf("写入 stdin 失败: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	buf := make([]byte, 32*1024)
	var got []byte
	for time.Now().Before(deadline) {
		n, err := term.Stdout().Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			break
		}
		// 提示符已出现且执行过 cd,足够判定
		if strings.Count(string(got), "$") >= 2 || strings.Contains(string(got), "/tmp") {
			break
		}
	}
	if ScanOSC7(s, got) {
		t.Fatalf("未勾选 FollowSSHPath 时不应出现 OSC 7,输出: %q", string(got))
	}
	if strings.Contains(string(got), "__ws_cwd") {
		t.Fatalf("未勾选 FollowSSHPath 时不应有钩子行回显,输出: %q", string(got))
	}
}

func TestIntegrationRunExec(t *testing.T) {
	s := openTestSession(t, newITManager(t, 8))
	out, err := s.Run("echo hello-webssh")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if strings.TrimSpace(out) != "hello-webssh" {
		t.Fatalf("Run 输出 = %q", out)
	}
}

func TestIntegrationTerminalCwdTracking(t *testing.T) {
	s := openTestSession(t, newITManager(t, 8))
	term, err := s.NewTerminal(80, 24, "")
	if err != nil {
		t.Fatalf("NewTerminal 失败: %v", err)
	}
	defer term.Close()

	// 向 PTY 发送 cd,读输出并扫描 OSC 7,验证注入的提示符钩子驱动 cwd 跟踪
	if _, err := term.Stdin().Write([]byte("cd /tmp\r")); err != nil {
		t.Fatalf("写入 stdin 失败: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	buf := make([]byte, 32*1024)
	for time.Now().Before(deadline) {
		n, err := term.Stdout().Read(buf)
		if n > 0 {
			ScanOSC7(s, buf[:n])
			if s.Cwd() == "/tmp" {
				return // 跟踪成功
			}
		}
		if err != nil {
			t.Fatalf("终端输出流结束而 cwd 未跟踪到: %v", err)
		}
	}
	t.Fatalf("超时:cwd 未跟踪到 /tmp(当前 %q)", s.Cwd())
}

func TestIntegrationTerminalStartupCmd(t *testing.T) {
	s := openTestSession(t, newITManager(t, 8))
	term, err := s.NewTerminal(80, 24, "echo STARTUP_MARKER_$((40+2))")
	if err != nil {
		t.Fatalf("NewTerminal 失败: %v", err)
	}
	defer term.Close()

	// 等待启动命令执行输出(注入即时写入,由 PTY 行缓冲保证时序)
	deadline := time.Now().Add(15 * time.Second)
	buf := make([]byte, 32*1024)
	var collected bytes.Buffer
	for time.Now().Before(deadline) {
		n, err := term.Stdout().Read(buf)
		if n > 0 {
			collected.Write(buf[:n])
			if strings.Contains(collected.String(), "STARTUP_MARKER_42") {
				return
			}
		}
		if err != nil {
			break
		}
	}
	t.Fatalf("超时:未捕获启动命令输出,已收集:\n%s", collected.String())
}

func TestIntegrationSftpRoundtrip(t *testing.T) {
	s := openTestSession(t, newITManager(t, 8))
	sc, err := s.SFTP()
	if err != nil {
		t.Fatalf("SFTP 不可用: %v", err)
	}

	remoteDir := fmt.Sprintf("/tmp/webssh-it-%d", time.Now().UnixNano())
	if err := sc.Mkdir(remoteDir); err != nil {
		t.Fatalf("建远程目录失败: %v", err)
	}
	// testssh 与测试同机运行,直接按本地路径兜底清理
	// (仅靠 SFTP RemoveDirectory 无法删除残留文件的目录)
	t.Cleanup(func() { _ = os.RemoveAll(remoteDir) })

	want := []byte("hello sftp roundtrip")
	w, err := sc.Create(remoteDir + "/a.txt")
	if err != nil {
		t.Fatalf("创建远程文件失败: %v", err)
	}
	if _, err := w.Write(want); err != nil {
		t.Fatalf("写远程文件失败: %v", err)
	}
	w.Close()

	r, err := sc.Open(remoteDir + "/a.txt")
	if err != nil {
		t.Fatalf("打开远程文件失败: %v", err)
	}
	defer r.Close()
	var got bytes.Buffer
	if _, err := got.ReadFrom(r); err != nil {
		t.Fatalf("读远程文件失败: %v", err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("内容不一致: got %q want %q", got.String(), want)
	}
}

func TestIntegrationSessionLimit(t *testing.T) {
	mgr := newITManager(t, 1)
	s := openTestSession(t, mgr) // 占满唯一额度
	_ = s
	_, err := mgr.OpenSession(context.Background(), store.Host{
		Host: "127.0.0.1", Port: 22, Username: "test",
		AuthType: store.AuthPassword, PasswordRef: itPwRef,
	}, nil, true)
	if err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("超限应报上限错误,得到: %v", err)
	}
}

// ---------- RunExec 分离式 exec 通道 ----------

// 退出码提取 + stdout/stderr 分流(testssh exec 已把 stderr 走扩展数据)。
func TestIntegrationRunExecSeparation(t *testing.T) {
	s := openTestSession(t, newITManager(t, 8))
	res, err := s.RunExec(context.Background(), "echo out-line; echo err-line >&2; exit 7", 1<<20)
	if err != nil {
		t.Fatalf("RunExec 失败: %v", err)
	}
	if res.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want 7", res.ExitCode)
	}
	if strings.TrimSpace(res.Stdout) != "out-line" {
		t.Fatalf("Stdout = %q, want out-line", res.Stdout)
	}
	if strings.TrimSpace(res.Stderr) != "err-line" {
		t.Fatalf("Stderr = %q, want err-line(未分离?)", res.Stderr)
	}
	if res.Truncated {
		t.Fatal("未超限不应置 Truncated")
	}
}

// ctx 超时:远端进程被截杀,RunExec 及时返回错误(不挂满 sleep 时长)。
func TestIntegrationRunExecTimeout(t *testing.T) {
	s := openTestSession(t, newITManager(t, 8))
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := s.RunExec(ctx, "sleep 30", 1<<20)
	if err == nil {
		t.Fatal("超时应返回错误")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("截杀不及时: %v", elapsed)
	}
}

// 输出超限:截断到 limit 并置 Truncated。
func TestIntegrationRunExecTruncated(t *testing.T) {
	s := openTestSession(t, newITManager(t, 8))
	res, err := s.RunExec(context.Background(), "head -c 4096 /dev/zero | tr '\\0' 'x'", 64)
	if err != nil {
		t.Fatalf("RunExec 失败: %v", err)
	}
	if !res.Truncated {
		t.Fatal("超限应置 Truncated")
	}
	if int64(len(res.Stdout)) != 64 {
		t.Fatalf("Stdout 长度 = %d, want 64", len(res.Stdout))
	}
}

// telnet 会话不支持 exec:与 Run 同语义返回 ErrSSHOnly。
func TestIntegrationRunExecTelnetRejected(t *testing.T) {
	m := New(t.TempDir(), 8, 0)
	s := &Session{ID: "t", connType: store.ConnTelnet, mgr: m}
	if _, err := s.RunExec(context.Background(), "x", 1024); err != ErrSSHOnly {
		t.Fatalf("telnet 会话应返回 ErrSSHOnly,得到 %v", err)
	}
}

// 租约防收割(真会话 + 短宽限期):AcquireLease 期内不被回收,
// ReleaseLease 归零后宽限期到即回收。
func TestIntegrationLeaseAgainstReap(t *testing.T) {
	m := New(t.TempDir(), 8, 300*time.Millisecond)
	m.SetSecretResolver(itSec.Reveal)
	s := openTestSession(t, m)
	if err := s.AcquireLease(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond)
	if _, ok := m.Get(s.ID); !ok {
		t.Fatal("租约期内会话被回收")
	}
	s.ReleaseLease()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := m.Get(s.ID); !ok {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("ReleaseLease 后宽限期到仍未回收")
}
