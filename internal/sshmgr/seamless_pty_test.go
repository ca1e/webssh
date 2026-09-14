//go:build integration

package sshmgr

import (
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// TestIntegrationSeamlessPTY 在真实 PTY 中执行 seamlessCmd 产物,验证无感注入的
// 端到端形态:单条提示符、首条提示符前出现 OSC 7、无注入回显、cd 后钩子跟随。
// 用 --noprofile 替换 -l 模拟无覆盖脚本的干净主机;覆盖型主机由 watchdog 降级,
// 该路径已由 TestIntegrationTerminalCwdTracking 覆盖。
func TestIntegrationSeamlessPTY(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("无 bash,跳过")
	}
	cmd := seamlessCmd("/bin/bash")
	cmd = strings.Replace(cmd, "exec '/bin/bash' -l -i", "exec '/bin/bash' --noprofile -l -i", 1)

	home := t.TempDir()
	f, tty, err := pty.Open()
	if err != nil {
		t.Fatalf("打开 PTY 失败: %v", err)
	}
	defer f.Close()
	defer tty.Close()

	proc := exec.Command("bash", "-c", cmd)
	proc.Env = []string{"PATH=" + os.Getenv("PATH"), "TERM=xterm-256color", "HOME=" + home, "SHELL=/bin/bash"}
	proc.Stdin, proc.Stdout, proc.Stderr = tty, tty, tty
	if err := proc.Start(); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	defer func() {
		_ = proc.Process.Kill()
		_, _ = proc.Process.Wait()
	}()

	// 后台 goroutine 持续读取(PTY 无数据时 Read 阻塞,无法用 deadline 轮询),
	// 主流程按时间点取快照断言。
	var (
		mu  sync.Mutex
		out []byte
	)
	stop := make(chan struct{})
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := f.Read(buf)
			mu.Lock()
			if n > 0 {
				out = append(out, buf[:n]...)
			}
			mu.Unlock()
			if err != nil {
				return
			}
			select {
			case <-stop:
				return
			default:
			}
		}
	}()
	defer close(stop)
	snapshot := func() []byte {
		mu.Lock()
		defer mu.Unlock()
		return append([]byte(nil), out...)
	}

	time.Sleep(1500 * time.Millisecond)
	out1 := snapshot()
	if i := strings.Index(string(out1), "\x1b]7;"); i < 0 {
		t.Fatalf("首条提示符前未出现 OSC 7,输出: %q", string(out1))
	} else if p := strings.Index(string(out1), "bash-"); p >= 0 && i > p {
		t.Fatalf("OSC 7 晚于首条提示符,输出: %q", string(out1))
	}
	if strings.Contains(string(out1), "PROMPT_COMMAND") {
		t.Fatalf("注入命令被回显,输出: %q", string(out1))
	}

	_, _ = f.Write([]byte("cd /tmp\r"))
	time.Sleep(1000 * time.Millisecond)
	out2 := snapshot()
	if !strings.Contains(string(out2), "file://") || !strings.Contains(string(out2), "/tmp") {
		t.Fatalf("cd 后 OSC 7 未跟随,输出: %q", string(out2))
	}
}
