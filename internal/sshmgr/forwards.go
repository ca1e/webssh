package sshmgr

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/crypto/ssh"
)

// ForwardStats 一条转发的连接与流量统计(k8s 类型填充,其余类型为零)。
// 方向以本机使用者视角记:Sent=本机→服务(上行),Recv=服务→本机(下行)。
type ForwardStats struct {
	Active int64 `json:"active"`
	Total  int64 `json:"total"`
	Recv   int64 `json:"recv_bytes"`
	Sent   int64 `json:"sent_bytes"`
}

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
	// Mode 转发模式:"std"=标准 SSH 转发通道;"raw"=原始模式(仅 exec
	// session + 远端辅助程序,绕过 AllowTcpForwarding=no,见 raw_forwarder.go)。
	// 请求 mode=auto 时按 sshd 探测结果落为二者之一;运行中被拒绝也会热切换。
	Mode string `json:"mode"`
	// LastError 最近一次桥接失败的远端诊断(列表快照时填充)。
	LastError string       `json:"last_error,omitempty"`
	Stats     ForwardStats `json:"stats"`

	sess     *Session
	ln       net.Listener
	stopOnce sync.Once

	// 原始模式复用通道引用:非 nil 时隧道连接经 rf.dial 发起,
	// 停止时按引用计数归还(raw_forwarder.go)。
	rf *RawForwarder

	// dial 当前拨号实现(std:client.Dial;raw:rf.dial),由 stateMu 保护,
	// 支持运行中热切换(auto 模式被 sshd 拒绝后降级为 raw)。
	stateMu sync.Mutex
	dial    func(network, addr string) (net.Conn, error)
	// rawOnce 保证 std→raw 的自动切换全局只尝试一次。
	rawOnce sync.Once
	rawOK   bool // 切换是否成功
	// stopping 置位于 stop 开始:与运行中的切换竞态时,放弃已取得的
	// 复用通道引用,避免泄漏。
	stopping atomic.Bool

	// 原子计数:当前活跃/累计连接与双向流量;atomic 字段不拷出,
	// ListForwards 快照时汇总进 Stats。
	activeN atomic.Int64
	totalN  atomic.Int64
	recvN   atomic.Int64
	sentN   atomic.Int64

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

// getMode/setMode 读改当前生效模式;stateMu 保护热切换与快照读的并发。
func (f *Forward) getMode() string {
	f.stateMu.Lock()
	defer f.stateMu.Unlock()
	return f.Mode
}

func (f *Forward) setMode(m string) {
	f.stateMu.Lock()
	f.Mode = m
	f.stateMu.Unlock()
}

// dialUpstream 当前生效的拨号实现;auto 模式下标准拨号被 sshd 拒绝时,
// 首次失败即降级为原始模式并原地重试(监听端口不变,客户端无感)。
func (f *Forward) dialUpstream(network, addr string) (net.Conn, error) {
	f.stateMu.Lock()
	dial := f.dial
	f.stateMu.Unlock()
	up, err := dial(network, addr)
	if err == nil {
		return up, nil
	}
	if f.getMode() == "std" && forwardDenied(err) && f.maybeSwitchRaw() {
		f.stateMu.Lock()
		dial = f.dial
		f.stateMu.Unlock()
		f.setLastError("标准转发被 sshd 拒绝,已自动切换原始模式")
		return dial(network, addr)
	}
	return nil, err
}

// maybeSwitchRaw 尝试一次性切换到原始模式复用通道;成功后成为该转发
// 的固定拨号实现。并发连接同时触发时由 rawOnce 收敛为单次。
func (f *Forward) maybeSwitchRaw() bool {
	f.rawOnce.Do(func() {
		rf, err := f.sess.acquireRawForwarder()
		if err != nil {
			f.setLastError("切换原始模式失败: " + err.Error())
			return
		}
		if f.stopping.Load() {
			rf.release() // 停止竞态:立即归还,引用不泄漏
			return
		}
		f.stateMu.Lock()
		f.rf, f.dial, f.rawOK = rf, rf.dial, true
		f.stateMu.Unlock()
		f.setMode("raw")
	})
	f.stateMu.Lock()
	defer f.stateMu.Unlock()
	return f.rawOK
}

// forwardDenied 判断拨号失败是否源于 sshd 转发禁令(仅此类错误值得
// 降级到原始模式;目标不可达等错误两种模式同样无解)。
func forwardDenied(err error) bool {
	var ocErr *ssh.OpenChannelError
	if errors.As(err, &ocErr) {
		return ocErr.Reason == ssh.Prohibited
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "administratively prohibited") ||
		strings.Contains(msg, "forwarding disabled")
}

func validPort(p int) error {
	if p < 1 || p > 65535 {
		return fmt.Errorf("端口必须在 1-65535 之间")
	}
	return nil
}

// resolveListenPort 校验监听端口:k8s 类型允许 0(内核自动分配,对齐
// OpenLens 的端口自动分配交互),其余类型必须显式指定合法端口。
func resolveListenPort(typ string, listenPort int) error {
	if listenPort == 0 {
		if typ != "k8s" {
			return fmt.Errorf("监听端口不能为空")
		}
		return nil
	}
	return validPort(listenPort)
}

// AddForward 建立一条转发。mode 仅对 local/socks5 生效:
//   - "std":标准 SSH 转发通道(direct-tcpip),sshd 禁止时报错;
//   - "raw":原始模式(exec 复用通道 + 远端辅助程序),绕过转发禁令;
//   - ""/"auto":先探测 sshd 配置,禁止则直接用 raw,允许则用 std
//     (运行中仍被拒时热切换 raw,见 dialUpstream)。
func (s *Session) AddForward(typ, remoteHost, namespace, k8sTarget, k8sPort string, listenPort, remotePort int, mode string) (*Forward, error) {
	if s.isTelnet() {
		return nil, ErrSSHOnly
	}
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	if err := resolveListenPort(typ, listenPort); err != nil {
		return nil, err
	}
	switch mode {
	case "", "auto", "std", "raw":
	default:
		return nil, fmt.Errorf("未知转发模式 %q(可选 auto/std/raw)", mode)
	}
	f := &Forward{
		sess: s, Type: typ, ListenPort: listenPort,
		RemoteHost: remoteHost, RemotePort: remotePort,
		Namespace: namespace, K8sTarget: k8sTarget, K8sPort: k8sPort,
		dial: func(network, addr string) (net.Conn, error) {
			s.mu.Lock()
			client := s.client
			s.mu.Unlock()
			return client.Dial(network, addr)
		},
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
	// local/socks5 的模式裁决:创建本地监听前先确定拨号通道,避免
	// "监听成功但隧道不通"的假成功;raw 在此即部署辅助程序,失败直接报错。
	// k8s 转发走 exec 通道,不经 direct-tcpip,无需(也恰恰要绕过)该检查。
	if typ == "local" || typ == "socks5" {
		if mode == "raw" {
			rf, err := s.acquireRawForwarder()
			if err != nil {
				return nil, err
			}
			f.rf, f.dial = rf, rf.dial
			f.Mode = "raw"
		} else if mode == "std" {
			if allowed, reason := s.checkForwardAllowed(); !allowed {
				return nil, fmt.Errorf("目标主机 sshd 禁止 TCP 转发: %s(可改用原始模式)", reason)
			}
			f.Mode = "std"
		} else { // auto
			allowed, reason := s.checkForwardAllowed()
			if !allowed {
				rf, err := s.acquireRawForwarder()
				if err != nil {
					return nil, fmt.Errorf("sshd 禁止 TCP 转发(%s),且原始模式不可用: %w", reason, err)
				}
				f.rf, f.dial = rf, rf.dial
				f.Mode = "raw"
			} else {
				f.Mode = "std" // 运行中被拒时由 dialUpstream 热切换 raw
			}
		}
	}
	ln, err := net.Listen("tcp", f.listenAddr())
	if err != nil {
		if f.rf != nil {
			f.rf.release() // 监听失败,归还已登记的复用通道引用
		}
		return nil, fmt.Errorf("监听 %s 失败: %w", f.listenAddr(), err)
	}
	f.ln = ln
	// listen_port=0 自动分配:回填内核实际选中的端口,返回给前端展示。
	if f.ListenPort == 0 {
		if ta, ok := ln.Addr().(*net.TCPAddr); ok {
			f.ListenPort = ta.Port
		}
	}

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
			Mode:      f.getMode(),
			LastError: f.lastError(),
			Stats: ForwardStats{
				Active: f.activeN.Load(), Total: f.totalN.Load(),
				Recv: f.recvN.Load(), Sent: f.sentN.Load(),
			},
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
		f.stopping.Store(true)
		if f.ln != nil {
			f.ln.Close()
		}
		// 归还原始模式复用通道引用(非 nil 即持有一次引用计数)。
		// 直用 rf 引用,不回查 Session(避免 s.mu 再入死锁)。
		f.stateMu.Lock()
		rf := f.rf
		f.stateMu.Unlock()
		if rf != nil {
			rf.release()
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
		upstream, err := f.dialUpstream("tcp", target)
		if err != nil {
			return
		}
		defer upstream.Close()
		proxy(conn, upstream)
		return
	}
	// socks5:协商与拨号解耦,拨号走 dialUpstream(auto 拒绝降级亦在此)。
	socks5Serve(conn, f.dialUpstream)
}

// proxy 双向搬运,任一方向结束后关闭两侧连接。
func proxy(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(b, a); a.Close(); b.Close() }()
	go func() { defer wg.Done(); io.Copy(a, b); a.Close(); b.Close() }()
	wg.Wait()
}
