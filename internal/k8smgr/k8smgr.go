package k8smgr

import (
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"time"

	"webssh/internal/sshmgr"
)

// 集群状态层:借鉴 Freelens 的 cluster-detectors——每个集群连接探测一次
// 元数据(context/版本/节点数/发行版)并缓存,供面板展示与后续功能的
// 连接上下文复用。探测经单次 SSH exec 的标记分段协议(ProbeScript),
// 解析出的节点列表用于发行版判定。
//
// 缓存以会话 ID 为键,成功态 TTL 60s、失败态 TTL 10s(失败常为瞬时的
// 网络/RBAC 问题,允许更快重试);并发探测经 singleflight 合并。

const (
	stateTTL    = 60 * time.Second
	stateErrTTL = 10 * time.Second
)

// State 集群探测结果(字段名即 JSON 契约,供前端 K8sPanel 状态卡片使用)。
type State struct {
	Available     bool      `json:"available"`              // kubectl 可用且集群可达(以取到 server 版本为准)
	Error         string    `json:"error,omitempty"`        // 失败原因(可展示;RBAC 等局部失败也记于此)
	ContextName   string    `json:"context,omitempty"`      // 当前 kubeconfig context
	ClusterName   string    `json:"cluster,omitempty"`      // --minify 后的 cluster 名
	ServerAddress string    `json:"server,omitempty"`       // API Server 地址
	ServerVersion string    `json:"version,omitempty"`      // 服务端版本(如 v1.30.4)
	NodeCount     int       `json:"node_count"`             // 节点总数
	ReadyNodes    int       `json:"ready_nodes"`            // Ready=True 的节点数
	Distribution  string    `json:"distribution,omitempty"` // eks|gke|aks|openshift|k3s|kind|minikube,未知为空
	ProbedAt      time.Time `json:"probed_at"`
}

// NodeInfo 参与发行版判定的节点字段子集。
type NodeInfo struct {
	Name       string            `json:"name"`
	ProviderID string            `json:"providerID"`
	Labels     map[string]string `json:"labels"`
	Ready      bool              `json:"ready"`
}

// sectionRe 切分探测脚本的多段输出(与 handler/k8s.go 的 k8sSectionRe 同构)。
var sectionRe = regexp.MustCompile(`(?s)__WS_(\w+)_(OK|ERR)__\n(.*?)\n?__WS_END__`)

type entry struct {
	st   State
	call *call // 非 nil 表示探测进行中
}

type call struct {
	wg sync.WaitGroup
	st State
}

// Manager 集群状态缓存。now 可注入以便测试 TTL 逻辑。
type Manager struct {
	mu      sync.Mutex
	entries map[string]*entry
	now     func() time.Time
}

func New() *Manager {
	return &Manager{entries: make(map[string]*entry), now: time.Now}
}

// State 返回该会话所在集群的探测结果;force 时忽略缓存重新探测,
// 并发调用自动合并到同一次探测(singleflight)。
func (m *Manager) State(s *sshmgr.Session, force bool) State {
	return m.cached(s.ID, force, s.Run)
}

// cached 是 State 的可测内核:run 抽象为"执行探测脚本并返回合并输出",
// 单元测试注入假实现即可覆盖 TTL 与 singleflight 逻辑。
func (m *Manager) cached(key string, force bool, run func(string) (string, error)) State {
	m.mu.Lock()
	e, ok := m.entries[key]
	if !ok {
		if len(m.entries) > 128 {
			m.gcLocked()
		}
		e = &entry{}
		m.entries[key] = e
	}
	ttl := stateTTL
	if !e.st.Available {
		ttl = stateErrTTL
	}
	if !force && e.call == nil && m.now().Sub(e.st.ProbedAt) < ttl {
		st := e.st
		m.mu.Unlock()
		return st
	}
	if e.call != nil {
		c := e.call
		m.mu.Unlock()
		c.wg.Wait()
		return c.st
	}
	c := &call{}
	c.wg.Add(1)
	e.call = c
	m.mu.Unlock()

	c.st = probe(run, m.now)

	m.mu.Lock()
	e.st = c.st
	e.call = nil
	m.mu.Unlock()
	c.wg.Done()
	return c.st
}

// gcLocked 淘汰长期未刷新的条目,防止会话反复重建导致缓存无限增长。
func (m *Manager) gcLocked() {
	cutoff := m.now().Add(-10 * stateTTL)
	for k, e := range m.entries {
		if e.call == nil && e.st.ProbedAt.Before(cutoff) {
			delete(m.entries, k)
		}
	}
}

// probe 执行探测脚本并解析各分段;now 注入时间源(测试用假时钟校准 TTL)。
func probe(run func(string) (string, error), now func() time.Time) State {
	out, err := run(ProbeScript())
	st := State{ProbedAt: now()}
	if err != nil && !strings.Contains(out, "__WS_") {
		// exec 通道失败且无分段输出(如 kubectl 定位失败的 stderr、telnet 会话):
		// 优先透出远端文本(exit status N 对用户无意义)
		st.Error = firstNonEmpty(firstLine(out), firstLine(err.Error()))
		return st
	}
	var verOK, nodeOK bool
	var errs []string
	for _, sec := range sectionRe.FindAllStringSubmatch(out, -1) {
		name, status, body := sec[1], sec[2], strings.TrimSpace(sec[3])
		if status == "ERR" {
			errs = append(errs, name+": "+firstLine(body))
			continue
		}
		switch name {
		case "CTX":
			st.ContextName = body
		case "CFG":
			st.ContextName = firstNonEmpty(st.ContextName, parseTab(body, 0))
			st.ClusterName = parseTab(body, 0)
			st.ServerAddress = parseTab(body, 1)
		case "VER":
			if v, ok := parseServerVersion(body); ok {
				st.ServerVersion = v
				verOK = true
			}
		case "NODES":
			nodes, ok := parseNodes(body)
			if !ok {
				errs = append(errs, "NODES: 节点列表解析失败")
				continue
			}
			nodeOK = true
			st.NodeCount = len(nodes)
			for _, n := range nodes {
				if n.Ready {
					st.ReadyNodes++
				}
			}
			st.Distribution = detectDistribution(nodes)
		}
	}
	st.Available = verOK
	if !verOK {
		// kubectl 缺失或集群不可达:取第一段可读的报错
		st.Error = strings.Join(errs, "; ")
	} else if len(errs) > 0 && !nodeOK {
		// 版本可取但节点列表失败(RBAC 受限):可用但注明
		st.Error = strings.Join(errs, "; ")
	}
	return st
}

// parseServerVersion 从 kubectl version -o json 输出提取 server.gitVersion。
func parseServerVersion(body string) (string, bool) {
	var doc struct {
		ServerVersion struct {
			GitVersion string `json:"gitVersion"`
		} `json:"serverVersion"`
	}
	if json.Unmarshal([]byte(body), &doc) != nil || doc.ServerVersion.GitVersion == "" {
		return "", false
	}
	return doc.ServerVersion.GitVersion, true
}

// parseNodes 解析 kubectl get nodes -o json 的 List 输出。
func parseNodes(body string) ([]NodeInfo, bool) {
	var doc struct {
		Kind  string `json:"kind"`
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				ProviderID string `json:"providerID"`
			} `json:"spec"`
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(body), &doc) != nil {
		return nil, false
	}
	nodes := make([]NodeInfo, 0, len(doc.Items))
	for _, it := range doc.Items {
		n := NodeInfo{Name: it.Metadata.Name, ProviderID: it.Spec.ProviderID, Labels: it.Metadata.Labels}
		for _, c := range it.Status.Conditions {
			if c.Type == "Ready" {
				n.Ready = c.Status == "True"
			}
		}
		nodes = append(nodes, n)
	}
	return nodes, true
}

// detectDistribution 依据节点 providerID 前缀、特征 label 与节点名模式判定
// 发行版(对标 Freelens cluster-detectors 的发行版探测器),未知返回空串。
func detectDistribution(nodes []NodeInfo) string {
	for _, n := range nodes {
		for k := range n.Labels {
			if strings.Contains(k, "openshift.io") {
				return "openshift"
			}
		}
	}
	for _, n := range nodes {
		if _, ok := n.Labels["k3s.io/hostname"]; ok {
			return "k3s"
		}
	}
	for _, n := range nodes {
		switch {
		case strings.HasPrefix(n.ProviderID, "aws://"):
			return "eks"
		case strings.HasPrefix(n.ProviderID, "gce://"):
			return "gke"
		case strings.HasPrefix(n.ProviderID, "azure://"):
			return "aks"
		case strings.HasPrefix(n.ProviderID, "k3s://"):
			return "k3s"
		}
	}
	for _, n := range nodes {
		switch {
		case strings.HasPrefix(n.Name, "minikube"):
			return "minikube"
		case strings.HasPrefix(n.Name, "kind-"):
			return "kind"
		case strings.HasPrefix(n.Name, "k3d-"):
			return "k3s"
		}
	}
	return ""
}

// parseTab 取 TSV 行的第 i 列(CFG 段输出 name\tserver)。
func parseTab(body string, i int) string {
	f := strings.Split(firstLine(body), "\t")
	if len(f) > i {
		return strings.TrimSpace(f[i])
	}
	return ""
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
