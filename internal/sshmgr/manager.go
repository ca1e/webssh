package sshmgr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"webssh/internal/store"
)

// Manager 管理所有活动 SSH 会话,并维护 known_hosts(TOFU:首次信任,变更拒绝)。
type Manager struct {
	mu        sync.Mutex
	sessions  map[string]*Session
	knownPath string
	known     map[string]string // "host:port" -> SHA256 指纹

	maxSessions int // 活动会话上限,0 表示不限制

	// orphanGrace 最后一个终端断开后到自动回收之间的宽限期。覆盖页面刷新
	// (WS 强断→重连)与瞬时断网;关浏览器后 sessionStorage 随标签页销毁,
	// 孤儿会话无人再接管,宽限期一到即回收,不再永久占用连接与 maxSessions 名额。
	orphanGrace time.Duration

	// resolve 凭据引用解析器(secrets.Reveal 注入);拨号时现场解密。
	resolve secretResolver

	// dialTelnetFn telnet 拨号注入点(测试用);nil 时使用真实拨号。
	dialTelnetFn func(ctx context.Context, h store.Host) (*telnetConn, string, error)
}

// New 构造会话管理器。凭据解析器经 SetSecretResolver 注入(main.go 在
// secrets.Open 之后、服务启动之前调用一次;未注入时密码/口令主机不可拨号)。
func New(configDir string, maxSessions int, orphanGrace time.Duration) *Manager {
	m := &Manager{
		sessions:    map[string]*Session{},
		knownPath:   filepath.Join(configDir, "known_hosts.json"),
		known:       map[string]string{},
		maxSessions: maxSessions,
		orphanGrace: orphanGrace,
	}
	if data, err := os.ReadFile(m.knownPath); err == nil {
		_ = json.Unmarshal(data, &m.known)
	}
	return m
}

// SetSecretResolver 注入凭据引用解析器(secrets.Store.Reveal)。
func (m *Manager) SetSecretResolver(r func(ref string) (string, error)) {
	m.resolve = secretResolver(r)
}

func (m *Manager) saveKnown() {
	data, err := json.MarshalIndent(m.known, "", "  ")
	if err != nil {
		return
	}
	tmp := m.knownPath + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, m.knownPath)
	}
}

func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}

func (m *Manager) List() []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	return out
}

func hostPort(h store.Host) (string, int) {
	port := h.Port
	if port <= 0 {
		port = 22
	}
	return net.JoinHostPort(h.Host, strconv.Itoa(port)), port
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// secretResolver 把 secrets 库引用解析为明文凭据(拨号时调用)。
type secretResolver func(ref string) (string, error)

// authMethods 依据主机认证类型构造 SSH 认证方法;password/passphrase 从
// secrets 库经 resolver 现场解密,Host 结构体上不存在明文字段。
func authMethods(h store.Host, resolve secretResolver) ([]ssh.AuthMethod, error) {
	switch h.AuthType {
	case store.AuthPassword:
		if h.PasswordRef == "" {
			return nil, fmt.Errorf("未配置密码")
		}
		if resolve == nil {
			return nil, fmt.Errorf("凭据库未接入,无法解析密码")
		}
		pw, err := resolve(h.PasswordRef)
		if err != nil {
			return nil, fmt.Errorf("解析密码失败: %w", err)
		}
		return []ssh.AuthMethod{ssh.Password(pw)}, nil
	case store.AuthKey:
		if h.KeyPath == "" {
			return nil, fmt.Errorf("未配置私钥路径")
		}
		data, err := os.ReadFile(expandHome(h.KeyPath))
		if err != nil {
			return nil, fmt.Errorf("读取私钥失败: %w", err)
		}
		var signer ssh.Signer
		if h.PassphraseRef != "" {
			if resolve == nil {
				return nil, fmt.Errorf("凭据库未接入,无法解析私钥口令")
			}
			pp, err := resolve(h.PassphraseRef)
			if err != nil {
				return nil, fmt.Errorf("解析私钥口令失败: %w", err)
			}
			signer, err = ssh.ParsePrivateKeyWithPassphrase(data, []byte(pp))
		} else {
			signer, err = ssh.ParsePrivateKey(data)
		}
		if err != nil {
			return nil, fmt.Errorf("解析私钥失败: %w", err)
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
	default:
		return nil, fmt.Errorf("未知认证类型 %q", h.AuthType)
	}
}

func (m *Manager) clientConfig(h store.Host, addr string, trustNewKey bool) (*ssh.ClientConfig, error) {
	methods, err := authMethods(h, m.resolve)
	if err != nil {
		return nil, err
	}
	return &ssh.ClientConfig{
		User: h.Username,
		Auth: methods,
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			fp := ssh.FingerprintSHA256(key)
			m.mu.Lock()
			defer m.mu.Unlock()
			old, ok := m.known[addr]
			if !ok || (old != fp && trustNewKey) {
				m.known[addr] = fp
				m.saveKnown()
				return nil
			}
			if old != fp {
				return fmt.Errorf("主机 %s 的密钥指纹已变化(%s → %s),如确认安全请重新连接并勾选“信任新密钥”", addr, old, fp)
			}
			return nil
		},
		Timeout: 10 * time.Second,
	}, nil
}

func (m *Manager) dialHost(ctx context.Context, h store.Host, trustNewKey bool) (*ssh.Client, error) {
	addr, _ := hostPort(h)
	cfg, err := m.clientConfig(h, addr, trustNewKey)
	if err != nil {
		return nil, err
	}
	return dialSSH(ctx, addr, cfg)
}

// dialSSH 建立 TCP 连接并完成 SSH 握手;TCP 拨号走 DialContext 响应取消,
// 握手阶段以 goroutine + select 桥接:ctx 取消时关闭底层连接,使阻塞中的
// 握手立即失败并回收 goroutine(客户端放弃等待时不留最长 10s 的悬挂拨号)。
func dialSSH(ctx context.Context, addr string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	d := net.Dialer{Timeout: cfg.Timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	return handshakeSSH(ctx, conn, addr, cfg)
}

// handshakeSSH 在已建立的连接上完成 SSH 握手,取消语义同 dialSSH。
func handshakeSSH(ctx context.Context, conn net.Conn, addr string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	type result struct {
		client *ssh.Client
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
		if err != nil {
			ch <- result{err: err}
			return
		}
		ch <- result{client: ssh.NewClient(c, chans, reqs)}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			conn.Close()
			return nil, r.err
		}
		return r.client, nil
	case <-ctx.Done():
		conn.Close()
		if r := <-ch; r.client != nil {
			r.client.Close()
		}
		return nil, ctx.Err()
	}
}

// dialThroughJump 经跳板机拨号到目标。ssh.Client.Dial 不支持取消,以 goroutine +
// select 桥接:ctx 取消时关闭跳板机连接,使阻塞中的拨号立即失败并回收 goroutine。
func dialThroughJump(ctx context.Context, jc *ssh.Client, targetAddr string) (net.Conn, error) {
	type result struct {
		nc  net.Conn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		nc, err := jc.Dial("tcp", targetAddr)
		ch <- result{nc, err}
	}()
	select {
	case r := <-ch:
		return r.nc, r.err
	case <-ctx.Done():
		jc.Close()
		if r := <-ch; r.nc != nil {
			r.nc.Close()
		}
		return nil, ctx.Err()
	}
}

// TestConnection 只验证拨号与认证,不保留连接。
func (m *Manager) TestConnection(ctx context.Context, h store.Host, jump *store.Host) error {
	if store.NormalizeConnType(h.ConnType) == store.ConnTelnet {
		conn, addr, err := m.dialTelnetConn(ctx, h)
		if err != nil {
			return fmt.Errorf("连接 %s 失败: %w", addr, err)
		}
		conn.Close()
		return nil
	}
	if jump != nil {
		jc, err := m.dialHost(ctx, *jump, false)
		if err != nil {
			return fmt.Errorf("连接跳板机 %s 失败: %w", jump.Name, err)
		}
		defer jc.Close()
		targetAddr, _ := hostPort(h)
		cfg, err := m.clientConfig(h, targetAddr, false)
		if err != nil {
			return err
		}
		nc, err := dialThroughJump(ctx, jc, targetAddr)
		if err != nil {
			return fmt.Errorf("经跳板机连接 %s 失败: %w", targetAddr, err)
		}
		client, err := handshakeSSH(ctx, nc, targetAddr, cfg)
		if err != nil {
			return fmt.Errorf("经跳板机认证失败: %w", err)
		}
		client.Close()
		return nil
	}
	c, err := m.dialHost(ctx, h, false)
	if err != nil {
		return err
	}
	c.Close()
	return nil
}

// OpenSession 按主机连接方式建立会话;jump 非空时经跳板机二级拨号(SSH)。
// ctx 来自发起请求,客户端取消等待时中止拨号并回收连接。
func (m *Manager) OpenSession(ctx context.Context, h store.Host, jump *store.Host, trustNewKey bool) (*Session, error) {
	if store.NormalizeConnType(h.ConnType) == store.ConnTelnet {
		return m.openTelnetSession(ctx, h)
	}
	m.mu.Lock()
	if m.maxSessions > 0 && len(m.sessions) >= m.maxSessions {
		m.mu.Unlock()
		return nil, fmt.Errorf("会话数量已达上限(%d),请先关闭不再使用的会话", m.maxSessions)
	}
	m.mu.Unlock()

	var (
		client  *ssh.Client
		jumpCli *ssh.Client
		err     error
	)
	targetStr, _ := hostPort(h)
	if jump != nil {
		jumpCli, err = m.dialHost(ctx, *jump, trustNewKey)
		if err != nil {
			return nil, fmt.Errorf("连接跳板机 %s 失败: %w", jump.Name, err)
		}
		targetAddr := targetStr
		cfg, err := m.clientConfig(h, targetAddr, trustNewKey)
		if err != nil {
			jumpCli.Close()
			return nil, err
		}
		nc, err := dialThroughJump(ctx, jumpCli, targetAddr)
		if err != nil {
			jumpCli.Close()
			return nil, fmt.Errorf("经跳板机连接 %s 失败: %w", targetAddr, err)
		}
		client, err = handshakeSSH(ctx, nc, targetAddr, cfg)
		if err != nil {
			nc.Close()
			jumpCli.Close()
			return nil, fmt.Errorf("经跳板机连接 %s 失败: %w", targetAddr, err)
		}
	} else {
		client, err = m.dialHost(ctx, h, trustNewKey)
		if err != nil {
			return nil, err
		}
	}

	s := &Session{
		ID:         store.NewID(),
		Host:       h,
		JumpHost:   jump,
		jumpClient: jumpCli,
		client:     client,
		connType:   store.ConnSSH,
		targetAddr: targetStr,
		OpenedAt:   time.Now(),
		forwards:   map[string]*Forward{},
		mgr:        m,
		StartupCmd: h.StartupCmd,

		FollowSSHPath: h.FollowSSHPath,
	}
	go s.keepalive()
	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()
	s.armOrphan()
	return s, nil
}

// openTelnetSession 建立 telnet 会话:连接即交互终端,登录在终端内透传完成。
// 首条连接供首个终端复用;分屏等后续终端由 telnetTerminal 独立拨号。
func (m *Manager) openTelnetSession(ctx context.Context, h store.Host) (*Session, error) {
	conn, addr, err := m.dialTelnetConn(ctx, h)
	if err != nil {
		return nil, fmt.Errorf("连接 %s 失败: %w", addr, err)
	}
	s := &Session{
		ID:         store.NewID(),
		Host:       h,
		conn:       conn,
		connType:   store.ConnTelnet,
		targetAddr: addr,
		OpenedAt:   time.Now(),
		forwards:   map[string]*Forward{},
		mgr:        m,
	}
	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()
	s.armOrphan()
	return s, nil
}

// CloseAll 关闭全部活动会话(含端口转发与跳板机连接),用于进程退出前收尾。
func (m *Manager) CloseAll() {
	for _, s := range m.List() {
		s.Close()
	}
}

type Session struct {
	ID string `json:"id"`

	Host     store.Host  `json:"-"`
	JumpHost *store.Host `json:"-"`

	OpenedAt time.Time `json:"-"`

	mu          sync.Mutex
	closed      bool
	client      *ssh.Client
	jumpClient  *ssh.Client              // 非空时由本会话持有并负责关闭
	conn        net.Conn                 // telnet 建立连接时拨号的首条连接(ssh 会话为 nil)
	connClaimed bool                     // telnet:conn 是否已被首个终端认领
	telnetExtra map[*telnetConn]struct{} // telnet:后续终端(分屏)独立拨号的连接
	connType    string
	targetAddr  string

	// 终端附着引用计数与孤儿回收计时器(s.mu 保护)。会话的存活性自此等价于
	// "至少一个终端 WS 附着,或 MCP 调用租约 >0,或断开后仍处于宽限期内";
	// 前端关浏览器/关闭标签页之外的路径(崩溃、刷新间隙)都收敛为:宽限期到
	// → 自动回收。
	termRefs    int
	leases      int // MCP 调用租约计数:与 termRefs 平级防孤儿收割
	orphanTimer *time.Timer

	sftpOnce   sync.Once
	sftpClient *sftp.Client
	sftpErr    error
	homeOnce   sync.Once
	homeDir    string
	homeErr    error

	// loginShell 惰性查询的远端登录 shell 路径($SHELL),供终端无感注入
	// 选择策略;查询失败为空串,由调用方回退 type-ahead 注入。
	loginShellOnce sync.Once
	loginShell     string

	// cwd 跟踪远程 shell 当前目录:由 OSC 7 序列或前端识别的 cd 命令更新,
	// 供 SFTP 面板单向同步(会话 → SFTP)。
	cwd string

	// cwdSubs 是 cwd 变更通知的订阅者集合(终端 WS 经此把变更推给前端,
	// 替代 SFTP 面板的轮询)。通知为尽力语义:channel 满即丢弃,消费者
	// 醒来后应调 Cwd() 取最新值。
	cwdSubMu sync.Mutex
	cwdSubs  map[chan string]struct{}

	// StartupCmd 连接后自动执行的命令(无 PTY,失败不中断)。
	StartupCmd string

	// FollowSSHPath 主机级开关:是否注入 OSC 7 提示符钩子让 SFTP 面板跟随终端
	// cd。默认关闭 = 普通 Shell 连接(无任何注入,MOTD 横幅完整);开启后走
	// exec 无感注入(该路径下 sshd 不打印横幅),钩子被远端覆盖时 watchdog 降级。
	FollowSSHPath bool

	forwards map[string]*Forward
	fidSeq   int

	// rawFwd 原始模式转发管理器(惰性创建,见 raw_forwarder.go):
	// 该会话全部原始模式转发共用一条 exec 复用通道。
	rawFwd *RawForwarder

	mgr *Manager
}

func (s *Session) Name() string     { return s.Host.Name }
func (s *Session) IP() string       { return s.Host.Host }
func (s *Session) Username() string { return s.Host.Username }
func (s *Session) Addr() string     { return s.targetAddr }

// ConnType 返回会话连接协议(store.ConnSSH / store.ConnTelnet)。
func (s *Session) ConnType() string { return s.connType }

func (s *Session) isTelnet() bool { return s.connType == store.ConnTelnet }

// AttachTerminal 登记一个终端通道附着(解除待命的孤儿回收);会话已关闭时报错。
func (s *Session) AttachTerminal() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("会话已断开")
	}
	if s.orphanTimer != nil {
		s.orphanTimer.Stop()
		s.orphanTimer = nil
	}
	s.termRefs++
	return nil
}

// DetachTerminal 注销一个终端通道;引用归零时启动宽限期孤儿回收计时。
// 与 AttachTerminal 成对(defer),宽限期内任意新终端接上即取消。
func (s *Session) DetachTerminal() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.termRefs <= 0 {
		return
	}
	s.termRefs--
	if s.termRefs == 0 {
		s.armOrphanLocked()
	}
}

// AcquireLease 登记一次 MCP 调用租约:计数 +1 并解除待命的孤儿回收;
// 会话已关闭时报错(调用方应重新开会话)。与 ReleaseLease 成对(defer)。
func (s *Session) AcquireLease() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("会话已断开")
	}
	if s.orphanTimer != nil {
		s.orphanTimer.Stop()
		s.orphanTimer = nil
	}
	s.leases++
	return nil
}

// ReleaseLease 注销一次 MCP 调用租约;计数归零且无终端附着时布防宽限期
// 孤儿回收(与终端引用同一回收路径)。
func (s *Session) ReleaseLease() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.leases <= 0 {
		return
	}
	s.leases--
	if s.leases == 0 {
		s.armOrphanLocked()
	}
}

// armOrphan 在无终端附着时布防孤儿回收计时,用于新建会话:拨号成功但前端
// 死在首个终端建立前(如浏览器请求发出即被关闭)也能被宽限期回收。
func (s *Session) armOrphan() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armOrphanLocked()
}

func (s *Session) armOrphanLocked() {
	// orphanGrace <= 0 关闭该机制(手工构造的 Manager/测试默认不复活的哨兵值)
	// 租约(MCP 调用)与终端附着同为存活凭据:任一 >0 即不布防。
	if s.mgr == nil || s.closed || s.termRefs > 0 || s.leases > 0 || s.mgr.orphanGrace <= 0 {
		return
	}
	s.orphanTimer = time.AfterFunc(s.mgr.orphanGrace, s.reapIfOrphan)
}

func (s *Session) reapIfOrphan() {
	s.mu.Lock()
	if s.closed || s.termRefs > 0 || s.leases > 0 {
		s.mu.Unlock()
		return
	}
	s.orphanTimer = nil
	s.mu.Unlock()
	log.Printf("会话 %s(%s@%s)%v 内无终端连接,自动回收", s.ID, s.Username(), s.Addr(), s.mgr.orphanGrace)
	s.Close()
}

func (s *Session) checkOpen() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("会话已断开")
	}
	return nil
}

func (s *Session) keepalive() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for range t.C {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		client := s.client
		s.mu.Unlock()
		if _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
			s.Close()
			return
		}
	}
}

// Close 断开会话:停止全部转发并关闭 SSH(及跳板机)连接。
func (s *Session) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	if s.orphanTimer != nil {
		s.orphanTimer.Stop()
		s.orphanTimer = nil
	}
	for _, f := range s.forwards {
		f.stop()
	}
	s.forwards = map[string]*Forward{}
	s.mu.Unlock()
	// 原始模式复用通道随会话一并回收(在关闭 SSH client 前,其 exec
	// session 的关闭需要 client 存活)。
	s.stopRawForwarder()
	s.mu.Lock()
	client, jump, conn := s.client, s.jumpClient, s.conn
	extra := make([]*telnetConn, 0, len(s.telnetExtra))
	for c := range s.telnetExtra {
		extra = append(extra, c)
	}
	s.telnetExtra = nil
	s.mu.Unlock()

	if client != nil {
		client.Close()
	}
	if jump != nil {
		jump.Close()
	}
	if conn != nil {
		conn.Close()
	}
	// telnet 分屏窗格各自独享的连接一并回收(终端尚未关闭的也不例外:
	// 会话已死,窗格随即收到 EOF)
	for _, c := range extra {
		c.Close()
	}
	s.mgr.mu.Lock()
	delete(s.mgr.sessions, s.ID)
	s.mgr.mu.Unlock()
}

// Terminal 一次交互式 shell 会话:SSH 为 PTY 通道,telnet 为裸连接透传。
type Terminal struct {
	sess   *ssh.Session // SSH 通道;telnet 为 nil
	closer io.Closer    // sess 为空时的关闭通道(telnet 连接)
	stdin  io.WriteCloser
	stdout io.Reader
	// initLine 注入交互式 shell 的 OSC 7 集成行(不含提交符 \r),供 handler
	// 预挂回显剥离(echoStripReader,精确匹配、不匹配即透传)。
	initLine string
	// seamless 标记当前终端走无感注入(exec 环境通道):钩子随环境在首条提示符
	// 前生效,正常情况下 initLine 永不出现在输出中;watchdog 检测到钩子失效时
	// 才降级补注该行,剥离器届时开始命中。
	seamless  bool
	osc7Seen  atomic.Bool
	watchOnce sync.Once
}

func (t *Terminal) Stdin() io.WriteCloser { return t.stdin }
func (t *Terminal) Stdout() io.Reader     { return t.stdout }

// InitLine 返回启动注入的 shell 集成行,空串表示无需剥离回显。
func (t *Terminal) InitLine() string { return t.initLine }

// MarkOSC7 由输出侧在流中首次发现 OSC 7 序列时调用,解除降级看门狗。
func (t *Terminal) MarkOSC7() { t.osc7Seen.Store(true) }

// osc7Watchdog 等待远端钩子产出首个 OSC 7 序列;超时未出现(常见于远端启动
// 脚本覆盖 PROMPT_COMMAND,如把它挪作窗口标题)则降级为 type-ahead 注入,
// 注入行的回显由已挂载的 echoStripReader 按精确字节剥离,输出不受影响。
func (t *Terminal) osc7Watchdog() {
	time.Sleep(3 * time.Second)
	if t.osc7Seen.Load() {
		return
	}
	_, _ = t.stdin.Write([]byte(t.initLine + "\r"))
}

func (t *Terminal) Resize(cols, rows int) error {
	if t.sess == nil {
		return nil // 连接未提供窗口尺寸通道,保持远端默认
	}
	return t.sess.WindowChange(rows, cols)
}

func (t *Terminal) Close() error {
	if t.sess != nil {
		return t.sess.Close()
	}
	if t.closer != nil {
		return t.closer.Close()
	}
	return nil
}

// NewTerminal 申请 PTY 并启动交互式 shell。TERM 固定 xterm-256color,
// 输出为原始字节流,转义协议由前端 xterm.js 完整解析。
//
// OSC 7 集成(跟踪 cwd)仅当主机勾选 FollowSSHPath 时注入,注入方式按登录
// shell 分级:
//  1. bash/zsh:无感注入——经 exec 请求在外层一次性 shell 里备好钩子环境后
//     exec 登录 shell(见 seamlessCmd),钩子在首条提示符前就绪:无回显、
//     单条提示符;代价是 sshd 对 exec 请求不打印 Last login/MOTD 横幅;
//     bash 路径若远端启动脚本覆盖 PROMPT_COMMAND,由 Terminal.osc7Watchdog
//     降级补注兜底;
//  2. 其他 shell 或 exec 启动失败:回退登录 shell + type-ahead 注入
//     (shellInitPayload:钩子行 + 启动命令,回显由 handler 剥离)。
//
// 未勾选时为普通连接:Shell 请求 + 仅提交 startupCmd(如有),无任何注入。
func (s *Session) NewTerminal(cols, rows int, startupCmd string) (*Terminal, error) {
	if s.isTelnet() {
		if err := s.checkOpen(); err != nil {
			return nil, err
		}
		return s.telnetTerminal()
	}
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	if !s.FollowSSHPath {
		return s.newTerminal(cols, rows, func(sess *ssh.Session) error { return sess.Shell() }, startupLines(startupCmd), "", false)
	}
	if cmd := seamlessCmd(s.LoginShell()); cmd != "" {
		t, err := s.newTerminal(cols, rows, func(sess *ssh.Session) error { return sess.Start(cmd) }, startupLines(startupCmd), osc7HookLine, true)
		if err == nil {
			return t, nil
		}
		// exec 通道启动失败(罕见):回退登录 shell + type-ahead
	}
	return s.newTerminal(cols, rows, func(sess *ssh.Session) error { return sess.Shell() }, shellInitPayload(startupCmd), osc7HookLine, false)
}

// telnetTerminal 为当前终端取一条独享连接:首个终端复用建立会话时拨号的
// conn,后续终端(分屏新窗格、断线后的重连)各自独立拨号——telnet 无通道
// 复用能力,如此每个窗格即一条独立会话,与 SSH 分屏各自申请 PTY 通道同语义。
// 终端关闭时经 dropTelnetConn 只回收自己的连接,不影响其他窗格。
func (s *Session) telnetTerminal() (*Terminal, error) {
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if c, ok := s.conn.(*telnetConn); ok && !s.connClaimed {
		s.connClaimed = true
		s.mu.Unlock()
		return newTelnetTerminal(s, c), nil
	}
	s.mu.Unlock()

	conn, _, err := s.mgr.dialTelnetConn(context.Background(), s.Host)
	if err != nil {
		return nil, fmt.Errorf("建立终端连接失败: %w", err)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		conn.Close()
		return nil, fmt.Errorf("会话已断开")
	}
	if s.telnetExtra == nil {
		s.telnetExtra = make(map[*telnetConn]struct{})
	}
	s.telnetExtra[conn] = struct{}{}
	s.mu.Unlock()
	return newTelnetTerminal(s, conn), nil
}

// dropTelnetConn 注销并关闭一个终端独享的连接(终端关闭时回调)。
func (s *Session) dropTelnetConn(conn *telnetConn) {
	s.mu.Lock()
	delete(s.telnetExtra, conn)
	s.mu.Unlock()
	_ = conn.Close()
}

// newTerminal 建立 PTY、以 start 指定的方式启动 shell 并提交预输入。
// initLine 为注入 shell 的钩子行,供 handler 剥离回显(空串表示无注入);
// 预输入立即写入即可,无需等待提示符:输入会在 PTY 行缓冲中暂存,shell 就绪后
// 按序读取(与终端 type-ahead 同机制)。type-ahead 行会被回显两次(shell 就绪
// 前的内核 ECHO、行编辑器读取预输入时的行编辑回显),由 handler 输出侧按精确
// 字节剥离(echoStripReader);此处不做 ECHO 抑制(内核级关 ECHO 会连累
// read/密码提示等场景的输入不可见)。
func (s *Session) newTerminal(cols, rows int, start func(*ssh.Session) error, stdinPayload, initLine string, seamless bool) (*Terminal, error) {
	sess, err := s.client.NewSession()
	if err != nil {
		return nil, err
	}
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 115200,
		ssh.TTY_OP_OSPEED: 115200,
	}
	if err := sess.RequestPty("xterm-256color", rows, cols, modes); err != nil {
		sess.Close()
		return nil, fmt.Errorf("申请 PTY 失败: %w", err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	if err := start(sess); err != nil {
		sess.Close()
		return nil, fmt.Errorf("启动 shell 失败: %w", err)
	}
	if stdinPayload != "" {
		if _, err := stdin.Write([]byte(stdinPayload)); err != nil {
			sess.Close()
			return nil, fmt.Errorf("写入 shell 初始化失败: %w", err)
		}
	}
	t := &Terminal{sess: sess, stdin: stdin, stdout: stdout, initLine: initLine, seamless: seamless}
	if seamless {
		// 终端随 WS 断开而关闭,看门狗最长存活 3s,直接后台等待即可
		go t.osc7Watchdog()
	}
	return t, nil
}

// osc7HookLine 注入交互式 shell 的 OSC 7 集成行(不含提交符 \r):
// 定义 __ws_cwd 并按 shell 类型注册提示符钩子(bash 用 PROMPT_COMMAND,
// zsh 用 precmd),每次显示提示符前输出 OSC 7 序列,服务端 ScanOSC7 据此跟踪
// cwd。仅 bash/zsh 生效;其他 shell(fish 等)静默跳过,此时 SFTP 面板不自动
// 跟随 cwd。
const osc7HookLine = `__ws_cwd(){ printf '\033]7;file://%s%s\033\\' "${HOSTNAME:-$HOST}" "$PWD"; }; if [ -n "${BASH_VERSION:-}" ]; then PROMPT_COMMAND="__ws_cwd${PROMPT_COMMAND:+;$PROMPT_COMMAND}"; export PROMPT_COMMAND; elif [ -n "${ZSH_VERSION:-}" ]; then precmd() { __ws_cwd; }; fi`

// startupLines 把用户启动命令转成 type-ahead 输入(每行以 \r 提交)。
// 无感注入路径下钩子已随 exec 环境就位,这里只提交用户命令;启动命令的回显
// 保持可见,与用户手动输入一致。
func startupLines(startupCmd string) string {
	var b strings.Builder
	if cmd := strings.TrimSpace(startupCmd); cmd != "" {
		for _, line := range strings.Split(strings.ReplaceAll(cmd, "\r\n", "\n"), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				b.WriteString(line + "\r")
			}
		}
	}
	return b.String()
}

// shellInitPayload 生成回退路径写入交互式 shell 的初始化输入:
// osc7HookLine(OSC 7 shell 集成)+ startupCmd 逐行原样执行。
func shellInitPayload(startupCmd string) string {
	return osc7HookLine + "\r" + startupLines(startupCmd)
}

// LoginShell 返回远端登录 shell 路径(sshd 对 exec 请求以它包装命令,其环境
// 中的 $SHELL 即登录 shell),查询失败或未设置返回空串。每会话仅查询一次。
func (s *Session) LoginShell() string {
	s.loginShellOnce.Do(func() {
		if out, err := s.Run(`echo -n "$SHELL"`); err == nil {
			s.loginShell = strings.TrimSpace(out)
		}
	})
	return s.loginShell
}

// shQuote 以单引号包裹路径,保证登录 shell 路径含空格/特殊字符时保持字面。
func shQuote(p string) string {
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}

// seamlessCmd 依据登录 shell 生成无感注入的 exec 命令;非 bash/zsh 返回空串
// (调用方回退 type-ahead)。bash 直接经环境变量传 PROMPT_COMMAND——赋值处于
// 双引号上下文,钩子文本中的引号/美元/反斜杠均需按 bash 双引号规则转义
// (\\ → \、\" → "、\$ → $);zsh 无环境钩子通道,改用 ZDOTDIR 临时 rc 目录:
// 逐个转发用户真实的 .zshenv/.zprofile/.zshrc/.zlogin 后追加 precmd 钩子,
// 保证用户配置完整加载;登录序列末尾(.zlogin)自清理临时目录并还原 ZDOTDIR,
// 避免残留与子 shell 继承。
func seamlessCmd(shellPath string) string {
	if shellPath == "" {
		return ""
	}
	q := shQuote(shellPath)
	switch path.Base(shellPath) {
	case "bash":
		// 注意 export 必须显式:bash 默认模式下独立赋值行只创建 shell 变量,
		// exec 替换进程时不会传入环境(前缀赋值才随 exec 传递)。
		return `PROMPT_COMMAND="__ws_cwd(){ printf '\\033]7;file://%s%s\\033\\\\' \"\${HOSTNAME:-\$HOST}\" \"\$PWD\"; }; __ws_cwd"
export PROMPT_COMMAND
exec ` + q + ` -l -i`
	case "zsh":
		return `d=$(mktemp -d "${TMPDIR:-/tmp}/.webssh-zsh-XXXXXX") || { exec ` + q + ` -l -i; }
cat > "$d/.zshenv" <<'WS_EOF'
_ws_zd="$ZDOTDIR"; ZDOTDIR="$HOME"
[ -r "$HOME/.zshenv" ] && . "$HOME/.zshenv"
ZDOTDIR="$_ws_zd"; unset _ws_zd
WS_EOF
cat > "$d/.zprofile" <<'WS_EOF'
_ws_zd="$ZDOTDIR"; ZDOTDIR="$HOME"
[ -r "$HOME/.zprofile" ] && . "$HOME/.zprofile"
ZDOTDIR="$_ws_zd"; unset _ws_zd
WS_EOF
cat > "$d/.zshrc" <<'WS_EOF'
_ws_zd="$ZDOTDIR"; ZDOTDIR="$HOME"
[ -r "$HOME/.zshrc" ] && . "$HOME/.zshrc"
ZDOTDIR="$_ws_zd"; unset _ws_zd
__ws_cwd(){ printf '\033]7;file://%s%s\033\\' "${HOSTNAME:-$HOST}" "$PWD" }
precmd_functions+=(__ws_cwd)
WS_EOF
cat > "$d/.zlogin" <<'WS_EOF'
_ws_zd="$ZDOTDIR"; ZDOTDIR="$HOME"
[ -r "$HOME/.zlogin" ] && . "$HOME/.zlogin"
ZDOTDIR="$_ws_zd"; unset _ws_zd
[ -n "${_WS_DIR:-}" ] && command rm -rf -- "$_WS_DIR"
unset ZDOTDIR _WS_DIR
WS_EOF
chmod 700 "$d"
[ -r "$d/.zshrc" ] || { command rm -rf -- "$d"; exec ` + q + ` -l -i; }
_WS_DIR="$d" ZDOTDIR="$d" exec ` + q + ` -l -i`
	}
	return ""
}

// NewCommandTerminal 与 NewTerminal 类似,但在 PTY 中执行自定义命令而非登录 shell。
// 用于 docker exec 等场景。cmd 为空时等同 NewTerminal。
func (s *Session) NewCommandTerminal(cols, rows int, cmd string) (*Terminal, error) {
	if s.isTelnet() {
		return nil, ErrSSHOnly
	}
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	sess, err := s.client.NewSession()
	if err != nil {
		return nil, err
	}
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 115200,
		ssh.TTY_OP_OSPEED: 115200,
	}
	if err := sess.RequestPty("xterm-256color", rows, cols, modes); err != nil {
		sess.Close()
		return nil, fmt.Errorf("申请 PTY 失败: %w", err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	if cmd != "" {
		err = sess.Start(cmd)
	} else {
		err = sess.Shell()
	}
	if err != nil {
		sess.Close()
		return nil, fmt.Errorf("启动命令失败: %w", err)
	}
	return &Terminal{sess: sess, stdin: stdin, stdout: stdout}, nil
}

// RunStream 执行命令并返回 stdout 流(不缓冲,调用方自行读取)。
// 用于 docker logs -f 等持续流式输出场景。
// 注意:返回的 io.Reader 在 cleanup 调用后失效;此处不实现 Close(sess.Close 统一管理)。
func (s *Session) RunStream(cmd string) (io.Reader, func(), error) {
	if err := s.checkOpen(); err != nil {
		return nil, nil, err
	}
	if s.isTelnet() {
		return nil, nil, ErrSSHOnly
	}
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	sess, err := client.NewSession()
	if err != nil {
		return nil, nil, fmt.Errorf("创建执行会话失败: %w", err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		return nil, nil, err
	}
	if err := sess.Start(cmd); err != nil {
		sess.Close()
		return nil, nil, fmt.Errorf("启动命令失败: %w", err)
	}
	// Wait 保持 SSH 通道活跃以泵送 stdout 流;cleanup 时 sess.Close 会终止它
	go sess.Wait()
	cleanup := func() { sess.Close() }
	return stdout, cleanup, nil
}

// runStartup 已废弃:启动命令改为发送到交互式 shell stdin,在 NewTerminal 中执行。

// SFTP 惰性创建(复用)该会话的 SFTP 客户端。
func (s *Session) SFTP() (*sftp.Client, error) {
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	if s.isTelnet() {
		return nil, ErrSSHOnly
	}
	s.sftpOnce.Do(func() {
		s.sftpClient, s.sftpErr = sftp.NewClient(s.client)
	})
	if s.sftpErr != nil {
		return nil, s.sftpErr
	}
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	return s.sftpClient, nil
}

// Home 返回远程用户主目录(用于 SFTP 初始目录与 "~" 解析)。
func (s *Session) Home() (string, error) {
	sc, err := s.SFTP()
	if err != nil {
		return "", err
	}
	s.homeOnce.Do(func() {
		dir, err := sc.Getwd()
		s.mu.Lock()
		s.homeDir, s.homeErr = dir, err
		if err == nil && s.cwd == "" {
			s.cwd = dir
		}
		s.mu.Unlock()
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.homeDir, s.homeErr
}

// Cwd 返回跟踪到的远程 shell 当前目录,未跟踪过时回退为主目录。
func (s *Session) Cwd() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cwd != "" {
		return s.cwd
	}
	return s.homeDir
}

// Run 在远程主机上执行一次性命令并返回合并输出。
// 用于主机信息采集、Docker 管理等非交互场景。
func (s *Session) Run(cmd string) (string, error) {
	if err := s.checkOpen(); err != nil {
		return "", err
	}
	if s.isTelnet() {
		return "", ErrSSHOnly
	}
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	sess, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("创建执行会话失败: %w", err)
	}
	defer sess.Close()
	out, err := sess.CombinedOutput(cmd)
	return string(out), err
}

// ExecResult 一次分离式 exec 的结果:stdout/stderr 分流、真实退出码与
// 超限截断标记(MCP ssh_exec 工具的返回契约)。
type ExecResult struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	Truncated bool
}

// limitBuffer 是带上限的写目标:超过 cap 后丢弃后续字节并置 Truncated,
// 保证远端进程可继续写完(管道持续排空)而不阻塞、服务端内存不失控。
type limitBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	cap       int64
	n         int64
	truncated bool
}

func (b *limitBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.n < b.cap {
		room := b.cap - b.n
		if int64(len(p)) <= room {
			b.buf.Write(p)
			b.n += int64(len(p))
			return len(p), nil
		}
		b.buf.Write(p[:room])
		b.n = b.cap
	}
	b.truncated = true
	return len(p), nil // 丢弃但不报错:让远端写完、拿到完整退出码
}

func (b *limitBuffer) String() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String(), b.truncated
}

// RunExec 在 SSH exec 通道(无 PTY)执行命令:stdout/stderr 分离、提取真实
// 退出码,输出按 limit 字节/流截断(超限丢弃续流并置 Truncated);ctx 取消
// 时向远端发 SIGKILL 并关闭通道后返回超时错误。telnet 会话返回 ErrSSHOnly
// (与 Run 同语义)。
func (s *Session) RunExec(ctx context.Context, cmd string, limit int64) (ExecResult, error) {
	if err := s.checkOpen(); err != nil {
		return ExecResult{}, err
	}
	if s.isTelnet() {
		return ExecResult{}, ErrSSHOnly
	}
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	sess, err := client.NewSession()
	if err != nil {
		return ExecResult{}, fmt.Errorf("创建执行会话失败: %w", err)
	}
	defer sess.Close()

	stdout, err := sess.StdoutPipe()
	if err != nil {
		return ExecResult{}, fmt.Errorf("创建执行会话失败: %w", err)
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		return ExecResult{}, fmt.Errorf("创建执行会话失败: %w", err)
	}
	outBuf, errBuf := &limitBuffer{cap: limit}, &limitBuffer{cap: limit}
	var pump sync.WaitGroup
	pump.Add(2)
	go func() { defer pump.Done(); _, _ = io.Copy(outBuf, stdout) }()
	go func() { defer pump.Done(); _, _ = io.Copy(errBuf, stderr) }()

	// 启动远端命令:必须在泵就绪后、Wait 前(pump 未就绪会丢输出头部,
	// 不 Start 则 Wait 永久阻塞)
	if err := sess.Start(cmd); err != nil {
		pump.Wait()
		return ExecResult{}, fmt.Errorf("执行命令失败: %w", err)
	}

	// ctx 取消:SIGKILL + Close 让 Wait 立即返回;管道排空后 pump 退出。
	waitErr := make(chan error, 1)
	go func() { waitErr <- sess.Wait() }()
	killCtx := func() {
		_ = sess.Signal(ssh.SIGKILL)
		_ = sess.Close()
	}
	timedOut := false
	select {
	case err = <-waitErr:
	case <-ctx.Done():
		timedOut = true
		killCtx()
		err = <-waitErr
	}
	pump.Wait() // 管道 EOF 后两个泵必然退出,再读缓冲避免竞态

	var res ExecResult
	res.Stdout, res.Truncated = outBuf.String()
	_, truncated := errBuf.String()
	res.Truncated = res.Truncated || truncated
	res.Stderr, _ = errBuf.String()
	if timedOut {
		return res, fmt.Errorf("命令执行超时,已被中止(未超过输出上限的部分仍随错误丢弃)")
	}
	var exitErr *ssh.ExitError
	if errors.As(err, &exitErr) {
		res.ExitCode = exitErr.Waitmsg.ExitStatus()
		return res, nil // 非零退出码是结果而非传输错误
	}
	if err != nil {
		return res, fmt.Errorf("执行命令失败: %w", err)
	}
	return res, nil
}

// checkForwardAllowed 探测远端 sshd 是否允许 TCP 转发。
// 优先 sshd -T(生效配置);失败回退读配置文件。无法获取时不阻断(返回 true)。
// 返回 (allowed, reason):allowed=false 时 reason 描述禁止原因。
func (s *Session) checkForwardAllowed() (bool, string) {
	out, err := s.Run(`sshd -T 2>/dev/null | grep -iE '^(allowtcpforwarding|disableforwarding)\b' || grep -iE '^\s*(allowtcpforwarding|disableforwarding)\b' /etc/ssh/sshd_config /etc/ssh/sshd_config.d/* 2>/dev/null | sed 's/#.*//'`)
	if err != nil && out == "" {
		return true, "" // 无法获取,不阻断
	}
	allowTCP, disableFwd := "", ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		// 配置文件回退时 grep 输出带 "path:Keyword value" 前缀;取第一个满足
		// "冒号前含路径分隔符且无空格"的冒号作边界,兼容值含冒号的项
		for i := 0; i < len(line); i++ {
			if line[i] != ':' {
				continue
			}
			prefix := line[:i]
			if strings.Contains(prefix, "/") && !strings.Contains(prefix, " ") {
				line = strings.TrimSpace(line[i+1:])
			}
			break
		}
		lower := strings.ToLower(line)
		parts := strings.Fields(lower)
		if len(parts) < 2 {
			continue
		}
		switch parts[0] {
		case "allowtcpforwarding":
			allowTCP = parts[1]
		case "disableforwarding":
			disableFwd = parts[1]
		}
	}
	if disableFwd == "yes" {
		return false, "DisableForwarding=yes"
	}
	if allowTCP == "no" {
		return false, "AllowTcpForwarding=no"
	}
	return true, ""
}

// SetCwd 更新跟踪的远程 shell 当前目录,仅接受不含控制字符的绝对路径
// (输出流解析异常时可能混入转义序列,直接丢弃本次更新,保持上次的有效值)。
// 值实际变化时向全部订阅者广播新路径。
func (s *Session) SetCwd(p string) {
	if p == "" || !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\x1b\x07\r\n") {
		return
	}
	s.mu.Lock()
	changed := s.cwd != p
	s.cwd = p
	s.mu.Unlock()
	if changed {
		s.notifyCwd(p)
	}
}

// SubscribeCwd 订阅 cwd 变更通知。返回的 channel 缓冲 1,只承诺"有变更"
// 事件而非每一个中间值;调用方退出前必须 UnsubscribeCwd。
func (s *Session) SubscribeCwd() chan string {
	ch := make(chan string, 1)
	s.cwdSubMu.Lock()
	if s.cwdSubs == nil {
		s.cwdSubs = make(map[chan string]struct{})
	}
	s.cwdSubs[ch] = struct{}{}
	s.cwdSubMu.Unlock()
	return ch
}

// UnsubscribeCwd 注销订阅。
func (s *Session) UnsubscribeCwd(ch chan string) {
	s.cwdSubMu.Lock()
	delete(s.cwdSubs, ch)
	s.cwdSubMu.Unlock()
}

func (s *Session) notifyCwd(p string) {
	s.cwdSubMu.Lock()
	defer s.cwdSubMu.Unlock()
	for ch := range s.cwdSubs {
		select {
		case ch <- p:
		default: // 缓冲已满:消费者将以 Cwd()/帧内路径收敛到最新值
		}
	}
}

// ScanOSC7 从 PTY 输出流中提取 OSC 7 序列报告的目录并更新 cwd。
// 序列格式:ESC ] 7 ; file://host/path (BEL 或 ESC \) 结尾,由 shell 集成发出。
// 返回本段数据中是否出现了 OSC 7 序列(供调用方确认钩子已生效)。
func ScanOSC7(s *Session, chunk []byte) bool {
	found := false
	data := string(chunk)
	for {
		start := strings.Index(data, "\x1b]7;")
		if start < 0 {
			return found
		}
		found = true
		rest := data[start+4:]
		// 终止符取 BEL 与 ST 中更早出现者:OSC 7 以 ST 结尾时,远端提示符常紧随
		// 一个 BEL 结尾的 OSC 0 标题序列(如 ESC]0;root@host:~ BEL),先找 BEL 会
		// 越过真正的结尾,把标题序列整个吞进 URI(表现为 SFTP 打开
		// "/root\x1b\]0;root@…" 之类被污染的路径)。
		end := strings.IndexByte(rest, '\x07')
		term := 1
		if i := strings.Index(rest, "\x1b\\"); i >= 0 && (end < 0 || i < end) {
			end, term = i, 2
		}
		if end < 0 {
			return found
		}
		uri := rest[:end]
		data = rest[end+term:]
		p := uri
		if i := strings.Index(p, "://"); i >= 0 {
			p = p[i+3:]
			if j := strings.IndexByte(p, '/'); j >= 0 {
				p = p[j:]
			} else {
				continue
			}
		}
		if p2, err := url.PathUnescape(p); err == nil {
			p = p2
		}
		if strings.HasPrefix(p, "/") {
			s.SetCwd(p)
		}
	}
}

var uidNameCache sync.Map // uid -> name
var gidNameCache sync.Map // gid -> name

// LookupUser 将数字 uid/gid 解析为名称,失败时回退为数字字符串。
func LookupUser(uid uint32) string {
	if v, ok := uidNameCache.Load(uid); ok {
		return v.(string)
	}
	name := strconv.FormatUint(uint64(uid), 10)
	if u, err := user.LookupId(name); err == nil && u.Username != "" {
		name = u.Username
	}
	uidNameCache.Store(uid, name)
	return name
}

func LookupGroup(gid uint32) string {
	if v, ok := gidNameCache.Load(gid); ok {
		return v.(string)
	}
	name := strconv.FormatUint(uint64(gid), 10)
	if g, err := user.LookupGroupId(name); err == nil && g.Name != "" {
		name = g.Name
	}
	gidNameCache.Store(gid, name)
	return name
}
