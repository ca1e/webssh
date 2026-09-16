package sshmgr

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
)

// Forward 一条活动转发规则,仅监听 127.0.0.1。
type Forward struct {
	ID         string `json:"id"`
	Type       string `json:"type"` // "local" | "socks5" | "k8s"
	ListenPort int    `json:"listen_port"`
	RemoteHost string `json:"remote_host,omitempty"` // type=local
	RemotePort int    `json:"remote_port,omitempty"` // type=local
	// type=k8s:经远端 kubectl port-forward 桥接(见 k8sforward.go)
	Namespace string `json:"namespace,omitempty"`
	K8sTarget string `json:"k8s_target,omitempty"`
	K8sPort   string `json:"k8s_port,omitempty"`
	// LastError 最近一次桥接失败的远端诊断(列表快照时填充)。
	LastError string `json:"last_error,omitempty"`

	sess     *Session
	ln       net.Listener
	stopOnce sync.Once

	// lastErr 最近一次桥接失败的远端诊断(stderr),供面板展示。
	lastErrMu sync.Mutex
	lastErr   string
}

func (f *Forward) setLastError(msg string) {
	f.lastErrMu.Lock()
	defer f.lastErrMu.Unlock()
	f.lastErr = msg
}

func (f *Forward) lastError() string {
	f.lastErrMu.Lock()
	defer f.lastErrMu.Unlock()
	return f.lastErr
}

func (f *Forward) listenAddr() string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(f.ListenPort))
}

func validPort(p int) error {
	if p < 1 || p > 65535 {
		return fmt.Errorf("端口必须在 1-65535 之间")
	}
	return nil
}

func (s *Session) AddForward(typ, remoteHost, namespace, k8sTarget, k8sPort string, listenPort, remotePort int) (*Forward, error) {
	if s.isTelnet() {
		return nil, ErrSSHOnly
	}
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	if err := validPort(listenPort); err != nil {
		return nil, err
	}
	f := &Forward{
		sess: s, Type: typ, ListenPort: listenPort,
		RemoteHost: remoteHost, RemotePort: remotePort,
		Namespace: namespace, K8sTarget: k8sTarget, K8sPort: k8sPort,
	}
	switch typ {
	case "local":
		if err := validPort(remotePort); err != nil {
			return nil, err
		}
		if remoteHost == "" {
			return nil, fmt.Errorf("目标主机不能为空")
		}
	case "socks5":
	case "k8s":
		if err := validateK8sArgs(namespace, k8sTarget, k8sPort); err != nil {
			return nil, err
		}
		if err := s.checkK8sReady(namespace, k8sTarget); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("未知转发类型 %q", typ)
	}
	// 创建本地监听前,先探测远端 sshd 是否允许 TCP 转发。
	// 避免"监听成功但隧道被 sshd 拒绝"的假成功问题。
	// k8s 转发走 exec 通道,不经 direct-tcpip,无需(也恰恰要绕过)该检查。
	if typ != "k8s" {
		if allowed, reason := s.checkForwardAllowed(); !allowed {
			return nil, fmt.Errorf("目标主机 sshd 禁止 TCP 转发: %s", reason)
		}
	}
	ln, err := net.Listen("tcp", f.listenAddr())
	if err != nil {
		return nil, fmt.Errorf("监听 %s 失败: %w", f.listenAddr(), err)
	}
	f.ln = ln

	s.mu.Lock()
	s.fidSeq++
	f.ID = fmt.Sprintf("f%d", s.fidSeq)
	s.forwards[f.ID] = f
	s.mu.Unlock()

	go f.acceptLoop()
	return f, nil
}

func (s *Session) RemoveForward(id string) bool {
	s.mu.Lock()
	f, ok := s.forwards[id]
	if ok {
		delete(s.forwards, id)
	}
	s.mu.Unlock()
	if ok {
		f.stop()
	}
	return ok
}

func (s *Session) ListForwards() []Forward {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Forward, 0, len(s.forwards))
	for _, f := range s.forwards {
		out = append(out, Forward{
			ID: f.ID, Type: f.Type, ListenPort: f.ListenPort,
			RemoteHost: f.RemoteHost, RemotePort: f.RemotePort,
			Namespace: f.Namespace, K8sTarget: f.K8sTarget, K8sPort: f.K8sPort,
			LastError: f.lastError(),
		})
	}
	return out
}

func (f *Forward) acceptLoop() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			f.stop()
			return
		}
		go f.handle(conn)
	}
}

func (f *Forward) stop() {
	f.stopOnce.Do(func() {
		if f.ln != nil {
			f.ln.Close()
		}
	})
}

func (f *Forward) handle(conn net.Conn) {
	defer conn.Close()
	if f.Type == "k8s" {
		f.k8sTunnel(conn)
		return
	}
	if f.Type == "local" {
		target := net.JoinHostPort(f.RemoteHost, strconv.Itoa(f.RemotePort))
		upstream, err := f.sess.client.Dial("tcp", target)
		if err != nil {
			return
		}
		defer upstream.Close()
		proxy(conn, upstream)
		return
	}
	socks5Serve(conn, func(network, addr string) (net.Conn, error) {
		return f.sess.client.Dial(network, addr)
	})
}

// proxy 双向搬运,任一方向结束后关闭两侧连接。
func proxy(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(b, a); a.Close(); b.Close() }()
	go func() { defer wg.Done(); io.Copy(a, b); a.Close(); b.Close() }()
	wg.Wait()
}
