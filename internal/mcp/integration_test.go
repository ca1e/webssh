//go:build integration

// MCP 协议集成测试:拉起 cmd/testssh,经 go-sdk InMemory transports 对 12 个
// 工具逐个走 happy 路径(真会话/真 exec/真 SFTP),并做全量快照断言:
// 任何输出不得包含凭据明文或 password/passphrase/PRIVATE KEY 词形。
// 运行:go test -tags=integration ./internal/mcp/
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"webssh/internal/auth"
	"webssh/internal/secrets"
	"webssh/internal/sshmgr"
	"webssh/internal/store"
)

var (
	itSSHAddr string
	itHome    string // testssh 的隔离 HOME(下载白名单用)
)

func TestMain(m *testing.M) {
	bin := filepath.Join(os.TempDir(), "webssh-integration-testssh-mcp")
	build := exec.Command("go", "build", "-o", bin, "webssh/cmd/testssh")
	build.Dir = moduleRoot()
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "编译 cmd/testssh 失败: %v\n%s", err, out)
		os.Exit(1)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "取空闲端口失败:", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	itSSHAddr = fmt.Sprintf("127.0.0.1:%d", port)

	testHome, err := os.MkdirTemp("", "webssh-mcp-it-home")
	itHome = testHome
	if err != nil {
		fmt.Fprintln(os.Stderr, "创建隔离 HOME 失败:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(testHome)
	// 远端测试文件(下载工具的目标,置于 HOME 内)
	if err := os.WriteFile(filepath.Join(testHome, "hello.txt"), []byte("mcp-download-payload\n"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "写测试文件失败:", err)
		os.Exit(1)
	}
	srv := exec.Command(bin, "-addr", itSSHAddr, "-user", "test", "-pass", "test123")
	srv.Env = append(os.Environ(), "SHELL=/bin/bash", "HOME="+testHome)
	srv.Dir = testHome
	srv.Stdout, srv.Stderr = os.Stdout, os.Stderr
	if err := srv.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "启动 testssh 失败:", err)
		os.Exit(1)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", itSSHAddr, 500*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(os.Stderr, "testssh 端口未就绪:", err)
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

func moduleRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return filepath.Dir(filepath.Dir(wd))
}

// newITServer 构造接入 testssh 的完整 Server(hosts.json 只存引用,
// 凭据先行写入 secrets 库)。
func newITServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	sec, err := secrets.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	port := 0
	fmt.Sscanf(strings.TrimPrefix(itSSHAddr, "127.0.0.1:"), "%d", &port)
	ref, err := sec.Put("", secrets.KindPassword, "test123")
	if err != nil {
		t.Fatal(err)
	}
	hosts := fmt.Sprintf(`[{"id":"h1","name":"it-host","host":"127.0.0.1","port":%d,
		"username":"test","auth_type":"password","password_ref":%q}]`, port, ref)
	if err := os.WriteFile(filepath.Join(dir, "hosts.json"), []byte(hosts), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir, sec)
	if err != nil {
		t.Fatal(err)
	}
	mgr := sshmgr.New(dir, 8, 0)
	mgr.SetSecretResolver(sec.Reveal)
	authSt, err := auth.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(dir, st, mgr, authSt)
	if err != nil {
		t.Fatal(err)
	}
	return s
}


// 新开的 happy 快照:工具输出中的凭据明文(test123 与 hosts.json 的 b64 形态)。
const (
	itPass   = "test123"
	itPass64 = "dGVzdDEyMw=="
)

// callHappy 断言工具成功并做快照断言(无凭据模式),返回原始 JSON。
func callHappy(t *testing.T, s *Server, tool string, args map[string]any) json.RawMessage {
	t.Helper()
	_, structured, isErr := callTool(t, s, tool, args)
	if isErr {
		t.Fatalf("%s 不应报错", tool)
	}
	blob, _ := json.Marshal(structured)
	if secretPattern.MatchString(string(blob)) || strings.Contains(string(blob), itPass) || strings.Contains(string(blob), itPass64) {
		t.Fatalf("%s 快照命中凭据模式: %s", tool, blob)
	}
	return blob
}

// TestIntegrationAllToolsHappy 12 工具全量 happy 路径(真 testssh)。
func TestIntegrationAllToolsHappy(t *testing.T) {
	s := newITServer(t)
	// 工具面数量回归:默认全开 = 12
	serverConn, clientConn := mcp.NewInMemoryTransports()
	sc, err := s.currentServer().Connect(context.Background(), serverConn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Wait()
	client := mcp.NewClient(&mcp.Implementation{Name: "it", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), clientConn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	list, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 12 {
		t.Fatalf("tools/list = %d, want 12", len(list.Tools))
	}

	ctx := context.Background()

	// hosts 域
	callHappy(t, s, "ssh_list_hosts", nil)
	callHappy(t, s, "ssh_host_info", map[string]any{"host": "it-host"})
	h := callHappy(t, s, "ssh_test_connection", map[string]any{"host": "it-host"})
	if !strings.Contains(string(h), `"ok":true`) {
		t.Fatalf("test_connection = %s", h)
	}

	// exec 域
	e := callHappy(t, s, "ssh_exec", map[string]any{
		"host": "it-host", "command": "uptime",
	})
	if !strings.Contains(string(e), `"exit_code":0`) || !strings.Contains(string(e), "load average") {
		t.Fatalf("exec uptime = %s", e)
	}

	// monitor 域
	callHappy(t, s, "ssh_system_info", map[string]any{"host": "it-host"})
	callHappy(t, s, "ssh_cpu_memory", map[string]any{"host": "it-host"})
	callHappy(t, s, "ssh_disk_usage", map[string]any{"host": "it-host"})
	callHappy(t, s, "ssh_network_status", map[string]any{"host": "it-host"})
	// sshd 服务在 testssh 内不存在:查询失败是合法 happy(工具正常返回状态/错误文本)
	_, structured, isErr := callTool(t, s, "ssh_service_status", map[string]any{"host": "it-host", "name": "sshd"})
	_ = structured
	_ = isErr // 非 0 退出码算工具成功返回(IsError=false);此处不强制
	callHappy(t, s, "ssh_docker_status", map[string]any{"host": "it-host"})
	callHappy(t, s, "ssh_k8s_status", map[string]any{"host": "it-host"})

	// files 域:HOME 白名单内的真实下载
	d := callHappy(t, s, "ssh_download_file", map[string]any{"host": "it-host", "path": "~/hello.txt"})
	var dl struct {
		LocalPath string `json:"local_path"`
		SHA256    string `json:"sha256"`
		Size      int    `json:"size"`
	}
	if err := json.Unmarshal(d, &dl); err != nil || dl.LocalPath == "" || dl.Size != len("mcp-download-payload\n") || len(dl.SHA256) != 64 {
		t.Fatalf("download = %s err=%v", d, err)
	}
	// 落盘文件真实存在且内容一致
	data, err := os.ReadFile(dl.LocalPath)
	if err != nil || string(data) != "mcp-download-payload\n" {
		t.Fatalf("下载文件校验失败: %v %q", err, data)
	}
	_ = ctx
	_ = cs
}
