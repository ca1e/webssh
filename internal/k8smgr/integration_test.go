//go:build integration

// K8s 面板集成测试:拉起 cmd/testssh 并在其 PATH 前部放置假 kubectl 脚本,
// 端到端验证集群状态探测(含缓存)、Pod watch 事件流、错误折叠、日志流与
// exec 终端的完整链路。运行:go test -tags=integration ./internal/k8smgr/
package k8smgr

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"webssh/internal/sshmgr"
	"webssh/internal/store"
)

// fakeKubectl 模拟远端 kubectl:按子命令返回固定数据;调用次数落盘
// ($HOME/kubectl-calls)供缓存断言;get pods 命中 forbidden-ns 时以
// RBAC 报错退出,验证 StreamScript 的错误折叠。
const fakeKubectl = `#!/bin/bash
echo x >> "$HOME/kubectl-calls"
cmd="$1"; sub="$2"
case "$cmd/$sub" in
  config/current-context) echo "fake-context"; exit 0;;
  config/view) printf 'fake-cluster\tfake.internal:6443\n'; exit 0;;
  version/*) echo '{"clientVersion":{"gitVersion":"v0.0.0"},"serverVersion":{"gitVersion":"v1.30.4-fake"}}'; exit 0;;
  get/nodes) cat <<'NODES'
{"kind":"List","apiVersion":"v1","items":[
 {"metadata":{"name":"node-1","labels":{}},"spec":{"providerID":"aws:///x/i-1"},"status":{"conditions":[{"type":"Ready","status":"True"}]}},
 {"metadata":{"name":"node-2","labels":{}},"spec":{"providerID":"aws:///x/i-2"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}
NODES
    exit 0;;
  get/pods)
    if [[ "$*" == *"forbidden-ns"* ]]; then
      echo "Error from server (Forbidden): pods is forbidden in namespace forbidden-ns" >&2
      exit 1
    fi
    echo '{"kind":"List","apiVersion":"v1","metadata":{"resourceVersion":"1"},"items":[{"metadata":{"name":"pod-a","namespace":"default","creationTimestamp":"2026-01-01T00:00:00Z"},"spec":{"nodeName":"node-1","containers":[{"name":"app"}]},"status":{"phase":"Running","podIP":"10.0.0.5","containerStatuses":[{"ready":true,"restartCount":2}]}}]}'
    if [[ "$*" == *" -w"* ]]; then
      echo '{"type":"ADDED","object":{"metadata":{"name":"pod-b","namespace":"default"},"spec":{"nodeName":"node-1","containers":[{"name":"app"}]},"status":{"phase":"Running"}}}'
      sleep 0.2
      echo '{"type":"MODIFIED","object":{"metadata":{"name":"pod-a","namespace":"default"},"spec":{"containers":[{"name":"app"}]},"status":{"phase":"Running"}}}'
      sleep 0.2
      echo '{"type":"DELETED","object":{"metadata":{"name":"pod-b","namespace":"default"}}}'
    fi
    exit 0;;
  logs/*) echo "log line 1"; sleep 0.1; echo "log line 2"; sleep 0.1; echo "log line 3"; exit 0;;
  exec/*) echo "FAKE_EXEC_READY"; cat; exit 0;;
  *) echo "fake-kubectl: unknown command: $*" >&2; exit 1;;
esac`

var testSSHAddr string

func TestMain(m *testing.M) {
	// 编译 cmd/testssh(与 sshmgr 的集成 harness 同构,独立成包避免测试包导入环)
	bin := filepath.Join(os.TempDir(), "webssh-integration-testssh-k8s")
	build := exec.Command("go", "build", "-o", bin, "webssh/cmd/testssh")
	build.Dir = moduleRoot()
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "编译 cmd/testssh 失败: %v\n%s", err, out)
		os.Exit(1)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "取空闲端口失败: %v\n", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	testSSHAddr = fmt.Sprintf("127.0.0.1:%d", port)

	// 隔离 HOME 并在 <HOME>/bin 放置假 kubectl(置于 PATH 最前)
	testHome, err := os.MkdirTemp("", "webssh-k8s-it-home")
	if err != nil {
		fmt.Fprintf(os.Stderr, "创建隔离 HOME 失败: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(testHome)
	binDir := filepath.Join(testHome, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "创建 bin 目录失败: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(filepath.Join(binDir, "kubectl"), []byte(fakeKubectl), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "写入假 kubectl 失败: %v\n", err)
		os.Exit(1)
	}

	srv := exec.Command(bin, "-addr", testSSHAddr, "-user", "test", "-pass", "test123")
	srv.Env = append(os.Environ(),
		"SHELL=/bin/bash", "HOME="+testHome,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	srv.Stdout, srv.Stderr = os.Stdout, os.Stderr
	if err := srv.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "启动 testssh 失败: %v\n", err)
		os.Exit(1)
	}

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

// moduleRoot 返回 webssh 模块根目录(测试文件位于 internal/k8smgr)。
func moduleRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return filepath.Dir(filepath.Dir(wd))
}

// openSession 建立到 testssh 的会话(假 kubectl 经 PATH 前缀被 command -v 命中)。
func openSession(t *testing.T) *sshmgr.Session {
	t.Helper()
	mgr := sshmgr.New(t.TempDir(), 8, 0)
	var port int
	if _, portStr, err := net.SplitHostPort(testSSHAddr); err == nil {
		fmt.Sscanf(portStr, "%d", &port)
	}
	host := store.Host{
		Name: "k8s-it-host", Host: "127.0.0.1", Port: port,
		Username: "test", AuthType: store.AuthPassword, Password: "test123",
	}
	s, err := mgr.OpenSession(context.Background(), host, nil, true)
	if err != nil {
		t.Fatalf("OpenSession 失败: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// kubectlCalls 统计假 kubectl 被调用的次数(探测脚本每次 4 次调用)。
func kubectlCalls(t *testing.T, s *sshmgr.Session) int {
	t.Helper()
	out, err := s.Run(`wc -l < "$HOME/kubectl-calls"`)
	if err != nil {
		t.Fatalf("读取调用计数失败: %v", err)
	}
	var n int
	fmt.Sscanf(strings.TrimSpace(out), "%d", &n)
	return n
}

// readDocs 从流中带超时地读取 n 个 JSON 文档(流式解码,逐个返回)。
func readDocs(t *testing.T, r io.Reader, n int) []map[string]any {
	t.Helper()
	type result struct {
		doc map[string]any
		err error
	}
	ch := make(chan result, n)
	go func() {
		dec := json.NewDecoder(r)
		for i := 0; i < n; i++ {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				ch <- result{err: err}
				return
			}
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			ch <- result{doc: m}
		}
	}()
	docs := make([]map[string]any, 0, n)
	deadline := time.After(10 * time.Second)
	for len(docs) < n {
		select {
		case res := <-ch:
			if res.err != nil {
				t.Fatalf("读取文档 %d/%d 失败: %v", len(docs)+1, n, res.err)
			}
			docs = append(docs, res.doc)
		case <-deadline:
			t.Fatalf("读取文档超时,已读 %d/%d", len(docs), n)
		}
	}
	return docs
}

// readUntil 持续读取直至累积输出包含 want(PTY 场景按字节流断言)。
func readUntil(t *testing.T, r io.Reader, want string, timeout time.Duration) string {
	t.Helper()
	ch := make(chan []byte, 16)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				b := make([]byte, n)
				copy(b, buf[:n])
				ch <- b
			}
			if err != nil {
				close(ch)
				return
			}
		}
	}()
	var acc []byte
	deadline := time.After(timeout)
	for {
		if strings.Contains(string(acc), want) {
			return string(acc)
		}
		select {
		case b, ok := <-ch:
			if !ok {
				t.Fatalf("流提前结束,等待 %q,已读 %q", want, acc)
			}
			acc = append(acc, b...)
		case <-deadline:
			t.Fatalf("超时等待 %q,已读 %q", want, acc)
		}
	}
}

// readAllWithTimeout 读完整个流(流会自行结束的场景)。
func readAllWithTimeout(t *testing.T, r io.Reader) string {
	t.Helper()
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	select {
	case out := <-done:
		return out
	case <-time.After(10 * time.Second):
		t.Fatal("读取流超时")
		return ""
	}
}

// ---------- V1:集群状态探测(端到端 + 缓存命中) ----------

func TestK8sStateIntegration(t *testing.T) {
	s := openSession(t)
	m := New()

	st := m.State(s, true)
	if !st.Available {
		t.Fatalf("Available = false, error = %q", st.Error)
	}
	if st.ContextName != "fake-context" {
		t.Errorf("ContextName = %q", st.ContextName)
	}
	if st.ClusterName != "fake-cluster" || st.ServerAddress != "fake.internal:6443" {
		t.Errorf("ClusterName/ServerAddress = %q/%q", st.ClusterName, st.ServerAddress)
	}
	if st.ServerVersion != "v1.30.4-fake" {
		t.Errorf("ServerVersion = %q", st.ServerVersion)
	}
	if st.NodeCount != 2 || st.ReadyNodes != 2 {
		t.Errorf("NodeCount/ReadyNodes = %d/%d", st.NodeCount, st.ReadyNodes)
	}
	if st.Distribution != "eks" {
		t.Errorf("Distribution = %q, want eks", st.Distribution)
	}

	// 缓存命中:force=false 不产生新调用(一次探测 = 4 次 kubectl)
	if calls := kubectlCalls(t, s); calls != 4 {
		t.Fatalf("首次探测后调用数 = %d, want 4", calls)
	}
	_ = m.State(s, false)
	if calls := kubectlCalls(t, s); calls != 4 {
		t.Fatalf("TTL 内第二次 State 触发了重新探测,调用数 = %d", calls)
	}
	_ = m.State(s, true)
	if calls := kubectlCalls(t, s); calls != 8 {
		t.Fatalf("force 应重新探测,调用数 = %d, want 8", calls)
	}
}

// ---------- V3:Pod watch 事件流(快照 + 类型化事件) ----------

func TestK8sPodsWatchIntegration(t *testing.T) {
	s := openSession(t)
	stdout, cleanup, err := s.RunStream(StreamScript("get pods -A -w --output-watch-events -o json"))
	if err != nil {
		t.Fatalf("RunStream 失败: %v", err)
	}
	defer cleanup()

	docs := readDocs(t, stdout, 4)
	if docs[0]["kind"] != "List" {
		t.Errorf("首文档 kind = %v, want List", docs[0]["kind"])
	}
	items, _ := docs[0]["items"].([]any)
	if len(items) != 1 {
		t.Errorf("快照 items = %d, want 1", len(items))
	}
	wantTypes := []any{"ADDED", "MODIFIED", "DELETED"}
	for i, want := range wantTypes {
		if docs[i+1]["type"] != want {
			t.Errorf("事件 %d type = %v, want %v", i+1, docs[i+1]["type"], want)
		}
	}
}

// ---------- V3:错误折叠(kubectl 失败 → exit 事件) ----------

func TestK8sStreamErrorFoldingIntegration(t *testing.T) {
	s := openSession(t)
	stdout, cleanup, err := s.RunStream(StreamScript("get pods -n 'forbidden-ns' -w --output-watch-events -o json"))
	if err != nil {
		t.Fatalf("RunStream 失败: %v", err)
	}
	defer cleanup()

	docs := readDocs(t, stdout, 1)
	if docs[0]["type"] != "exit" {
		t.Fatalf("应收到 exit 文档,实际: %v", docs[0])
	}
	if errMsg, _ := docs[0]["error"].(string); !strings.Contains(errMsg, "Forbidden") {
		t.Errorf("error 应含 stderr 折叠详情: %v", docs[0]["error"])
	}
}

// ---------- V5:Pod 日志流透传 ----------

func TestK8sLogsIntegration(t *testing.T) {
	s := openSession(t)
	stdout, cleanup, err := s.RunStream(LogsScript("-f --tail 200 -n 'default' 'pod-a'"))
	if err != nil {
		t.Fatalf("RunStream 失败: %v", err)
	}
	defer cleanup()

	out := readAllWithTimeout(t, stdout)
	for _, want := range []string{"log line 1", "log line 2", "log line 3"} {
		if !strings.Contains(out, want) {
			t.Errorf("日志流缺少 %q,实际: %q", want, out)
		}
	}
}

// ---------- V6:exec 终端(PTY + stdin 回显) ----------

func TestK8sExecTerminalIntegration(t *testing.T) {
	s := openSession(t)
	term, err := s.NewCommandTerminal(80, 24, ExecScript("default", "pod-a", ""))
	if err != nil {
		t.Fatalf("NewCommandTerminal 失败: %v", err)
	}
	defer term.Close()

	// 假 kubectl exec 先输出就绪标记,再 cat 回显 stdin
	readUntil(t, term.Stdout(), "FAKE_EXEC_READY", 10*time.Second)
	if _, err := term.Stdin().Write([]byte("k8s-echo-check\n")); err != nil {
		t.Fatalf("写 stdin 失败: %v", err)
	}
	out := readUntil(t, term.Stdout(), "k8s-echo-check", 10*time.Second)
	_ = out
}
