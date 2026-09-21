//go:build integration

// 集成测试:原始模式转发端到端(经 cmd/testssh)。
// 覆盖:辅助程序真实部署(SFTP 上传 + 远端执行)、原始模式本地转发、
// 原始模式 SOCKS5 代理、多并发请求、资源清理(转发移除/会话关闭后
// 远端无残留进程)。运行:go test -tags=integration ./internal/sshmgr/
package sshmgr

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"webssh/internal/store"
)

// rawHelperProcCount 统计远端(同机)残留的辅助程序进程数。
// 模式用 [w]ebssh 方言写法:pgrep -f 会匹配到执行本次查询的 bash -c
// 自身命令行(其中含同字面量),[w] 正则不匹配 "[w]" 文本,规避自匹配。
// 直接经本地进程表查询(会话关闭后无法再经 SSH 执行命令)。
func rawHelperProcCount(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("pgrep", "-fc", `[w]ebssh-rawfwd-v`).Output()
	if err != nil {
		// pgrep 无匹配时以状态码 1 退出(可能仍输出 0),按计数值解析
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("查询辅助程序进程失败: %v %s", err, string(out))
		}
	}
	n := 0
	fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &n)
	return n
}

// waitRawProcs 等待辅助程序进程数收敛到期望值(异步退出需要一点时间)。
func waitRawProcs(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if n := rawHelperProcCount(t); n == want {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("辅助程序进程数未收敛到 %d,当前 %d", want, rawHelperProcCount(t))
}

// freePort 取一个空闲回环端口(监听后立即释放,交给转发组件)。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("取空闲端口失败: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// TestIntegrationRawLocalForward 原始模式本地转发端到端:
// 监听端口 → HTTP 请求经 SSH exec 复用通道 → 目标服务 → 回程。
func TestIntegrationRawLocalForward(t *testing.T) {
	s := openPlainSession(t, newITManager(t, 8))

	// 目标服务:远端(同机)HTTP,返回会话专属标记
	marker := fmt.Sprintf("raw-local-%d", time.Now().UnixNano())
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "marker=%s path=%s", marker, r.URL.Path)
	}))
	defer target.Close()
	ta := strings.TrimPrefix(target.URL, "http://")
	th, tp, _ := net.SplitHostPort(ta)
	tpi := atoi(t, tp)

	listen := freePort(t)
	f, err := s.AddForward("local", th, "", "", "", listen, tpi, "raw")
	if err != nil {
		t.Fatalf("建立原始模式转发失败: %v", err)
	}
	if f.Mode != "raw" {
		t.Fatalf("生效模式应为 raw,实际 %q", f.Mode)
	}
	waitRawProcs(t, 1) // 首个原始转发应部署并拉起一个辅助程序

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/hello", listen))
	if err != nil {
		t.Fatalf("经隧道请求失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "marker="+marker) || !strings.Contains(string(body), "path=/hello") {
		t.Fatalf("响应不符: %s", string(body))
	}

	// 停止后:监听关闭、复用通道回收、远端辅助程序退出
	id := f.ID
	if !s.RemoveForward(id) {
		t.Fatal("移除转发失败")
	}
	waitRawProcs(t, 0)
	if _, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", listen)); err == nil {
		t.Fatal("移除后监听应已关闭")
	}
}

// TestIntegrationRawSOCKS5 原始模式 SOCKS5 代理端到端:
// SOCKS5 CONNECT 经复用通道由远端辅助程序拨出。
func TestIntegrationRawSOCKS5(t *testing.T) {
	s := openPlainSession(t, newITManager(t, 8))

	marker := fmt.Sprintf("raw-socks-%d", time.Now().UnixNano())
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "marker="+marker)
	}))
	defer target.Close()
	ta := strings.TrimPrefix(target.URL, "http://")
	th, tp, _ := net.SplitHostPort(ta)

	listen := freePort(t)
	if _, err := s.AddForward("socks5", "", "", "", "", listen, 0, "raw"); err != nil {
		t.Fatalf("建立原始模式 SOCKS5 失败: %v", err)
	}
	waitRawProcs(t, 1)

	// 经 SOCKS5 发起 HTTP 请求
	dialSocks := func(addr string) (net.Conn, error) {
		return socks5Connect(t, fmt.Sprintf("127.0.0.1:%d", listen), addr)
	}
	conn, err := dialSocks(net.JoinHostPort(th, tp))
	if err != nil {
		t.Fatalf("SOCKS5 连接失败: %v", err)
	}
	fmt.Fprintf(conn, "GET / HTTP/1.0\r\nHost: %s\r\n\r\n", th)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	conn.Close()
	if !strings.Contains(string(body), marker) {
		t.Fatalf("响应不符: %s", string(body))
	}

	// 拨号失败应回 SOCKS5 错误码(目标不可达)
	badConn, err := socks5Connect(t, fmt.Sprintf("127.0.0.1:%d", listen), "127.0.0.1:1")
	if err == nil {
		badConn.Close()
		t.Fatal("目标不可达时 SOCKS5 应报错")
	}
}

// TestIntegrationRawConcurrent 并发 16 个请求经同一条复用通道,
// 各自校验响应标记,验证多路复用无串流。
func TestIntegrationRawConcurrent(t *testing.T) {
	s := openPlainSession(t, newITManager(t, 8))

	var mu sync.Mutex
	seen := map[string]bool{}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.URL.Path)
	}))
	defer target.Close()
	ta := strings.TrimPrefix(target.URL, "http://")
	th, tp, _ := net.SplitHostPort(ta)
	tpi := atoi(t, tp)

	listen := freePort(t)
	if _, err := s.AddForward("local", th, "", "", "", listen, tpi, "raw"); err != nil {
		t.Fatalf("建立原始模式转发失败: %v", err)
	}

	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			path := fmt.Sprintf("/c%d", i)
			resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s", listen, path))
			if err != nil {
				errs <- err
				return
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(body) != path {
				errs <- fmt.Errorf("数据错乱: want %q got %q", path, string(body))
			}
			mu.Lock()
			seen[path] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if len(seen) != n {
		t.Fatalf("请求未全部完成: %d/%d", len(seen), n)
	}
}

// TestIntegrationRawHelperCleanup 会话关闭后远端辅助程序无残留进程。
func TestIntegrationRawHelperCleanup(t *testing.T) {
	s := openPlainSession(t, newITManager(t, 8))

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer target.Close()
	ta := strings.TrimPrefix(target.URL, "http://")
	th, tp, _ := net.SplitHostPort(ta)

	listen := freePort(t)
	if _, err := s.AddForward("local", th, "", "", "", listen, atoi(t, tp), "raw"); err != nil {
		t.Fatalf("建立原始模式转发失败: %v", err)
	}
	waitRawProcs(t, 1)

	s.Close()
	waitRawProcs(t, 0)
}

// TestIntegrationAutoFallbackToRaw 验收核心:sshd 拒绝 direct-tcpip
// (AllowTcpForwarding=no 语义)时,auto 模式的标准转发在首次连接被拒后
// 自动降级为原始模式,客户端无感,流量照常到达目标。
func TestIntegrationAutoFallbackToRaw(t *testing.T) {
	// 额外拉起一个拒绝转发通道的 testssh 实例
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("取空闲端口失败: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	bin := filepath.Join(os.TempDir(), "webssh-integration-testssh")
	srv := exec.Command(bin, "-addr", fmt.Sprintf("127.0.0.1:%d", port),
		"-user", "test", "-pass", "test123", "-deny-forward")
	srv.Env = append(os.Environ(), "SHELL=/bin/bash")
	// 同 TestMain:工作目录隔离,辅助程序部署落临时目录,不污染仓库
	srv.Dir = t.TempDir()
	srv.Stdout, srv.Stderr = os.Stdout, os.Stderr
	if err := srv.Start(); err != nil {
		t.Fatalf("启动 deny-forward testssh 失败: %v", err)
	}
	t.Cleanup(func() { _ = srv.Process.Kill(); _ = srv.Wait() })

	// 等端口就绪
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("deny-forward testssh 未就绪: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	mgr := newITManager(t, 8)
	host := store.Host{
		Name: "it-deny-host", Host: "127.0.0.1", Port: port,
		Username: "test", AuthType: store.AuthPassword, PasswordRef: itPwRef,
	}
	s, err := mgr.OpenSession(context.Background(), host, nil, true)
	if err != nil {
		t.Fatalf("OpenSession 失败: %v", err)
	}
	t.Cleanup(s.Close)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "fallback-ok")
	}))
	defer target.Close()
	ta := strings.TrimPrefix(target.URL, "http://")
	th, tp, _ := net.SplitHostPort(ta)

	listen := freePort(t)
	// mode=auto:探测结果若识别为禁止则直接 raw;识别为允许则先 std,
	// 首条连接被拒后热切换 raw —— 两种路径最终都必须可用。
	f, err := s.AddForward("local", th, "", "", "", listen, atoi(t, tp), "auto")
	if err != nil {
		t.Fatalf("建立 auto 转发失败: %v", err)
	}

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", listen))
	if err != nil {
		t.Fatalf("经隧道请求失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "fallback-ok") {
		t.Fatalf("响应不符: %s", string(body))
	}
	if f.getMode() != "raw" {
		t.Fatalf("拒绝环境下 auto 应落在原始模式,实际 %q(last=%s)", f.getMode(), f.lastError())
	}
}

// socks5Connect 最小 SOCKS5 客户端:仅 NO-AUTH + CONNECT,返回服务端
// 应答码(0x00=成功)与连接。
func socks5Connect(t *testing.T, proxyAddr, target string) (net.Conn, error) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		conn.Close()
		return nil, err
	}
	r := bufio.NewReader(conn)
	head := make([]byte, 2)
	if _, err := io.ReadFull(r, head); err != nil || head[0] != 5 || head[1] != 0 {
		conn.Close()
		return nil, fmt.Errorf("方法协商失败: % x %v", head, err)
	}
	host, portStr, _ := net.SplitHostPort(target)
	p, err := parsePort(portStr)
	if err != nil {
		conn.Close()
		return nil, err
	}
	req := append([]byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}, []byte(host)...)
	req = binary.BigEndian.AppendUint16(req, p)
	if _, err := conn.Write(req); err != nil {
		conn.Close()
		return nil, err
	}
	resp := make([]byte, 4)
	if _, err := io.ReadFull(r, resp); err != nil {
		conn.Close()
		return nil, err
	}
	if resp[1] != 0x00 {
		conn.Close()
		return nil, fmt.Errorf("SOCKS5 应答码 0x%02x(目标不可达或被拒绝)", resp[1])
	}
	// 跳过 BND.ADDR/PORT
	switch resp[3] {
	case 0x01:
		io.ReadFull(r, make([]byte, 6))
	case 0x04:
		io.ReadFull(r, make([]byte, 18))
	default:
		conn.Close()
		return nil, fmt.Errorf("异常应答地址类型 0x%02x", resp[3])
	}
	return conn, nil
}

func parsePort(s string) (uint16, error) {
	p, err := strconv.Atoi(s)
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("端口不合法: %q", s)
	}
	return uint16(p), nil
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("端口解析失败: %q", s)
	}
	return n
}
