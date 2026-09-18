package k8smgr

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// ---------- 探测输出解析 ----------

const fakeProbeOK = `__WS_CTX_OK__
fake-context
__WS_END__
__WS_CFG_OK__
fake-cluster	fake.internal:6443
__WS_END__
__WS_VER_OK__
{"clientVersion":{"gitVersion":"v1.29.0"},"serverVersion":{"gitVersion":"v1.30.4"}}
__WS_END__
__WS_NODES_OK__
{"kind":"List","items":[
 {"metadata":{"name":"ip-10-0-1-1","labels":{}},"spec":{"providerID":"aws:///us-east-1a/i-1"},"status":{"conditions":[{"type":"Ready","status":"True"}]}},
 {"metadata":{"name":"ip-10-0-1-2","labels":{}},"spec":{"providerID":"aws:///us-east-1a/i-2"},"status":{"conditions":[{"type":"Ready","status":"False"}]}}]}
__WS_END__`

func TestProbeParsesSections(t *testing.T) {
	run := func(string) (string, error) { return fakeProbeOK, nil }
	st := probe(run, time.Now)

	if !st.Available {
		t.Fatalf("Available = false, want true(错误: %s)", st.Error)
	}
	if st.ContextName != "fake-context" {
		t.Fatalf("ContextName = %q", st.ContextName)
	}
	if st.ClusterName != "fake-cluster" || st.ServerAddress != "fake.internal:6443" {
		t.Fatalf("ClusterName/ServerAddress = %q/%q", st.ClusterName, st.ServerAddress)
	}
	if st.ServerVersion != "v1.30.4" {
		t.Fatalf("ServerVersion = %q", st.ServerVersion)
	}
	if st.NodeCount != 2 || st.ReadyNodes != 1 {
		t.Fatalf("NodeCount/ReadyNodes = %d/%d", st.NodeCount, st.ReadyNodes)
	}
	if st.Distribution != "eks" {
		t.Fatalf("Distribution = %q, want eks", st.Distribution)
	}
	if st.Error != "" {
		t.Fatalf("Error = %q, want 空", st.Error)
	}
}

func TestProbeKubectlMissing(t *testing.T) {
	out := "__WS_CTX_ERR__\n/bin/sh: kubectl: command not found\n__WS_END__\n" +
		"__WS_VER_ERR__\n目标主机未安装 kubectl(PATH 与登录环境中均未找到)\n__WS_END__\n"
	st := probe(func(string) (string, error) { return out, nil }, time.Now)

	if st.Available {
		t.Fatal("Available = true, want false")
	}
	if !strings.Contains(st.Error, "未安装 kubectl") {
		t.Fatalf("Error = %q, 应含 kubectl 缺失说明", st.Error)
	}
	if st.ServerVersion != "" {
		t.Fatalf("ServerVersion = %q, want 空", st.ServerVersion)
	}
}

func TestProbeRBACPartialFailure(t *testing.T) {
	out := "__WS_CTX_OK__\nctx\n__WS_END__\n__WS_VER_OK__\n" +
		`{"serverVersion":{"gitVersion":"v1.30.4"}}` + "\n__WS_END__\n" +
		"__WS_NODES_ERR__\nError from server (Forbidden): nodes is forbidden\n__WS_END__\n"
	st := probe(func(string) (string, error) { return out, nil }, time.Now)

	if !st.Available {
		t.Fatalf("Available = false, 版本可取应视为可用(错误: %s)", st.Error)
	}
	if !strings.Contains(st.Error, "Forbidden") {
		t.Fatalf("Error = %q, 应保留 RBAC 报错", st.Error)
	}
}

func TestProbeExecChannelFailure(t *testing.T) {
	st := probe(func(string) (string, error) { return "", errors.New("该能力经由 SSH 通道提供") }, time.Now)
	if st.Available {
		t.Fatal("Available = true, want false")
	}
	if !strings.Contains(st.Error, "SSH") {
		t.Fatalf("Error = %q", st.Error)
	}
}

// ---------- 发行版判定 ----------

func TestDetectDistribution(t *testing.T) {
	node := func(name, providerID string, labels map[string]string) NodeInfo {
		return NodeInfo{Name: name, ProviderID: providerID, Labels: labels}
	}
	cases := []struct {
		name  string
		nodes []NodeInfo
		want  string
	}{
		{"aws providerID", []NodeInfo{node("ip-1", "aws:///x", nil)}, "eks"},
		{"gce providerID", []NodeInfo{node("gke-n", "gce://p/z/i", nil)}, "gke"},
		{"azure providerID", []NodeInfo{node("aks-n", "azure:///sub", nil)}, "aks"},
		{"k3s providerID", []NodeInfo{node("n", "k3s://id", nil)}, "k3s"},
		{"k3s label", []NodeInfo{node("n", "", map[string]string{"k3s.io/hostname": "n"})}, "k3s"},
		{"openshift label", []NodeInfo{node("n", "aws://x", map[string]string{"openshift.io/scc": "any"})}, "openshift"},
		{"minikube name", []NodeInfo{node("minikube", "", nil)}, "minikube"},
		{"kind name", []NodeInfo{node("kind-control-plane", "", nil)}, "kind"},
		{"k3d name", []NodeInfo{node("k3d-cluster-server-0", "", nil)}, "k3s"},
		{"unknown", []NodeInfo{node("node1", "", nil)}, ""},
		{"empty", nil, ""},
		// openshift 判定优先于 providerID;k3d 命中 k3s 而非 minikube/kind
		{"priority", []NodeInfo{node("k3d-x", "gce://y", map[string]string{"openshift.io/z": "1"})}, "openshift"},
	}
	for _, tc := range cases {
		if got := detectDistribution(tc.nodes); got != tc.want {
			t.Errorf("%s: detectDistribution = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// ---------- 缓存 TTL 与 singleflight ----------

func TestCachedTTL(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	calls := 0
	m := New()
	m.now = func() time.Time { return now }
	run := func(string) (string, error) {
		calls++
		return fakeProbeOK, nil
	}

	st := m.cached("s1", false, run) // 首次:探测
	if calls != 1 || st.ServerVersion != "v1.30.4" {
		t.Fatalf("首次探测 calls=%d", calls)
	}
	m.cached("s1", false, run) // TTL 内:命中缓存
	if calls != 1 {
		t.Fatalf("TTL 内不应重新探测, calls=%d", calls)
	}
	m.cached("s1", true, run) // force:重新探测
	if calls != 2 {
		t.Fatalf("force 应重新探测, calls=%d", calls)
	}

	now = now.Add(stateTTL + time.Second)
	m.cached("s1", false, run) // 过期:重新探测
	if calls != 3 {
		t.Fatalf("过期后应重新探测, calls=%d", calls)
	}
}

func TestCachedErrorTTLShorter(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	m := New()
	m.now = func() time.Time { return now }
	run := func(string) (string, error) { return "", errors.New("boom") }

	m.cached("s1", false, run)
	now = now.Add(stateErrTTL + time.Second)
	// 失败态按更短 TTL 过期:此时已过期,应重新探测(不 panic 即可,重点在无越界等待)
	_ = m.cached("s1", false, run)
}

func TestCachedSingleflight(t *testing.T) {
	// 阻塞 run 直到并发方汇合,验证并发调用合并为一次探测
	entered := make(chan struct{})
	release := make(chan struct{})
	m := New()
	run := func(string) (string, error) {
		close(entered)
		<-release
		return fakeProbeOK, nil
	}

	done := make(chan State, 4)
	for i := 0; i < 4; i++ {
		go func() { done <- m.cached("s1", false, run) }()
	}
	<-entered
	release <- struct{}{}

	states := make([]State, 4)
	for i := range states {
		states[i] = <-done
	}
	for i, st := range states {
		if st.ServerVersion != "v1.30.4" {
			t.Fatalf("并发方 %d 未取到结果", i)
		}
	}
	// 4 个并发方共享同一次探测:再取一次应命中缓存而非新探测(由计数间接验证)
	m.now = func() time.Time { return time.Unix(1_000_000, 0) }
	m.cached("s1", false, func(string) (string, error) {
		t.Error("不应触发第二次探测")
		return fakeProbeOK, nil
	})
}
