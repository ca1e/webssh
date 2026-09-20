package sshmgr

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

// 原始模式转发(SaSSHimi 模式):绕过 sshd AllowTcpForwarding=no 的本地转发
// 与 SOCKS5 代理。
//
// 原理:不复用 direct-tcpip / tcpip-forward 通道,只开一条普通 SSH exec
// session,在远端运行内嵌的 socks5-helper(见 tools/socks5-helper);本地把
// 多条隧道连接复用为二进制帧流写入其 stdin,从 stdout 读回帧并按连接 ID
// 分发。全程仅 session 通道,转发禁令无从生效。
//
// 帧协议(与远端 helper 保持一致,整数大端):
//
//	+---------+--------+--------+---------+
//	| connID  |  type  | length | payload |
//	| 4 字节  | 1 字节 | 4 字节 | length  |
//	+---------+--------+--------+---------+
//
//	OPEN(1) 本地→远端,负载 host:port;远端拨号成功回 ACK(5,空负载),
//	失败回 ERROR(4,负载为单行错误)。显式应答是必需的:SOCKS5 必须在向
//	客户端回包前得知拨号成败,而 HTTP 等目标不会先发数据,无法以首包推断。
//	DATA(2) 双向,负载为原始字节;CLOSE(3) 双向,关闭该连接。
//
// 数据面:单条 exec 通道承载全部连接,共享 SSH 窗口——单连接打满带宽会与
// 其他连接排队(与 OpenSSH ControlMaster 复用同特性);这是绕开转发禁令的
// 固有代价,普通多并发场景无感。

// 帧类型(远端 helper tools/socks5-helper 同名常量)。
const (
	frameOPEN  byte = 1
	frameDATA  byte = 2
	frameCLOSE byte = 3
	frameERROR byte = 4
	frameACK   byte = 5
)

const (
	rawHeaderLen   = 9         // connID(4) + type(1) + length(4)
	rawMaxPayload  = 1 << 20   // 单帧负载上限(与 helper 一致,超限即协议错误)
	rawWriteChunk  = 128 << 10 // 本地单帧写入上限:适配 SSH 窗口,多连接细粒度交错
	rawOpenTimeout = 15 * time.Second
)

// RawForwarder 一条 SSH 会话上的原始模式转发管理器:部署并驱动远端
// helper,在单个 exec 通道上多路复用该会话全部原始模式转发的隧道连接。
// 惰性创建、引用计数:首个原始转发建立时部署 helper 并启动复用通道,
// 最后一个转发移除后回收通道。一个会话至多一条复用通道。
type RawForwarder struct {
	sess *Session

	mu       sync.Mutex
	started  bool
	stopped  bool
	execSess *ssh.Session // 承载 helper 的 exec 通道(stop 时关闭)

	writeMu sync.Mutex // 串行化向 stdin 的帧写入(多连接并发)
	stdin   io.WriteCloser

	connsMu sync.Mutex
	conns   map[uint32]*rawConn
	nextID  uint32

	refs atomic.Int64 // 使用本复用通道的转发数
	dead atomic.Bool  // 通道已死亡(EOF/协议错误/会话关闭),不再接受新拨号
	once sync.Once    // 读取循环退出只执行一次

	lastErrMu sync.Mutex
	lastErr   string // 通道级诊断(helper stderr、协议错误),供面板展示
}

// rawAddr 实现 net.Addr 的占位地址(net.Conn 接口要求,链路不使用)。
type rawAddr struct{}

func (rawAddr) Network() string { return "raw" }
func (rawAddr) String() string  { return "raw" }

func (rf *RawForwarder) setLastError(msg string) {
	rf.lastErrMu.Lock()
	rf.lastErr = msg
	rf.lastErrMu.Unlock()
}

func (rf *RawForwarder) lastError() string {
	rf.lastErrMu.Lock()
	defer rf.lastErrMu.Unlock()
	return rf.lastErr
}

// acquireRawForwarder 返回会话的 RawForwarder,必要时部署 helper 并启动
// 复用通道。失败立即报错(上传失败/架构不支持等),调用方不会得到
// "监听成功但隧道不通"的假成功。
func (s *Session) acquireRawForwarder() (*RawForwarder, error) {
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	rf := s.rawFwd
	if rf == nil {
		rf = &RawForwarder{sess: s, conns: map[uint32]*rawConn{}}
		s.rawFwd = rf
	}
	s.mu.Unlock()
	if err := rf.start(); err != nil {
		// 部署失败不残留半初始化实例,下次重试全新部署。
		s.mu.Lock()
		if s.rawFwd == rf {
			s.rawFwd = nil
		}
		s.mu.Unlock()
		return nil, err
	}
	rf.refs.Add(1)
	return rf, nil
}

// release 归还一个使用方引用;归零后回收复用通道(会话仍存活,后续再建
// 原始转发时按需重新部署)。注意:不得回调 Session 加锁——Session.Close
// 持有 s.mu 期间会经 Forward.stop 走到这里,再入锁即死锁。
func (rf *RawForwarder) release() {
	if rf.refs.Add(-1) <= 0 {
		rf.stop()
	}
}

// stopRawForwarder 会话关闭时无条件回收。
func (s *Session) stopRawForwarder() {
	s.mu.Lock()
	rf := s.rawFwd
	s.mu.Unlock()
	if rf != nil {
		rf.stop()
	}
}

// start 部署 helper(远端已缓存同名二进制则复用)并打开复用通道;幂等。
func (rf *RawForwarder) start() error {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	if rf.stopped {
		return fmt.Errorf("原始模式通道已关闭")
	}
	if rf.started {
		return nil
	}
	path, err := rf.sess.deployRawHelper()
	if err != nil {
		return err
	}
	if err := rf.sess.checkOpen(); err != nil {
		return err
	}
	rf.sess.mu.Lock()
	client := rf.sess.client
	rf.sess.mu.Unlock()
	sess, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("创建执行会话失败: %w", err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		return err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		return err
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		sess.Close()
		return err
	}
	// exec 让登录 shell 以 helper 替换自身:进程树干净、SSH 通道直连
	// helper 的 stdin/stdout,无中间 shell 的缓冲或信号转发问题。
	if err := sess.Start("exec " + shQuote(path)); err != nil {
		sess.Close()
		return fmt.Errorf("启动远端辅助程序失败: %w", err)
	}
	rf.execSess = sess
	rf.stdin = stdin
	go drainReader(stderr, func(msg string) {
		rf.setLastError("原始模式辅助程序: " + msg)
	})
	go rf.readLoop(bufio.NewReaderSize(stdout, 64<<10))
	// Wait 至通道结束;读取循环通常先以 EOF 收尾,这里兜底标记死亡。
	go func() {
		sess.Wait()
		rf.markDead("辅助程序已退出")
	}()
	rf.started = true
	return nil
}

// drainReader 读取诊断流(限长,保留最先写入内容),结束后回调。
func drainReader(r io.Reader, onDone func(msg string)) {
	var buf syncBuffer
	b := make([]byte, 2048)
	for {
		n, err := r.Read(b)
		if n > 0 {
			buf.Write(b[:n])
		}
		if err != nil {
			break
		}
	}
	if msg := strings.TrimSpace(buf.String()); msg != "" {
		onDone(msg)
	}
}

// dial 经复用通道向远端 target(host:port)发起一条隧道连接:
// 分配连接 ID、注册、发 OPEN,等待 ACK/ERROR。
func (rf *RawForwarder) dial(network, addr string) (net.Conn, error) {
	if network != "tcp" {
		return nil, fmt.Errorf("原始模式仅支持 tcp,不支持 %s", network)
	}
	if err := validateRawTarget(addr); err != nil {
		return nil, err
	}
	if rf.dead.Load() {
		return nil, fmt.Errorf("原始模式通道已断开: %s", rf.lastError())
	}

	rc := &rawConn{rf: rf, ack: make(chan error, 1), notify: make(chan struct{}, 1)}
	rc.localAddr, rc.remoteAddr = rawAddr{}, rawAddr{}

	// 先注册再发 OPEN:ACK 可能先于 writeFrame 返回到达。
	rf.connsMu.Lock()
	rf.nextID++ // 连接 ID 从 1 起,0 保留
	rc.id = rf.nextID
	rf.conns[rc.id] = rc
	rf.connsMu.Unlock()

	if err := rf.writeFrame(frameOPEN, rc.id, []byte(addr)); err != nil {
		rf.unregister(rc)
		return nil, fmt.Errorf("发送 OPEN 失败: %w", err)
	}
	select {
	case err := <-rc.ack:
		if err != nil {
			rf.unregister(rc)
			return nil, fmt.Errorf("远端连接 %s 失败: %s", addr, err.Error())
		}
		return rc, nil
	case <-time.After(rawOpenTimeout):
		rc.Close() // CLOSE 一并通知远端放弃,防止半开连接悬挂
		return nil, fmt.Errorf("等待远端连接 %s 超时(%s)", addr, rawOpenTimeout)
	}
}

// writeFrame 单帧原子写入(跨连接串行)。
func (rf *RawForwarder) writeFrame(typ byte, id uint32, payload []byte) error {
	rf.writeMu.Lock()
	defer rf.writeMu.Unlock()
	head := make([]byte, rawHeaderLen)
	binary.BigEndian.PutUint32(head[0:4], id)
	head[4] = typ
	binary.BigEndian.PutUint32(head[5:9], uint32(len(payload)))
	if _, err := rf.stdin.Write(head); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := rf.stdin.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// unregister 从连接映射移除(幂等)。
func (rf *RawForwarder) unregister(rc *rawConn) {
	rf.connsMu.Lock()
	delete(rf.conns, rc.id)
	rf.connsMu.Unlock()
}

// readLoop 从 helper stdout 读取帧并分发;通道结束/协议错误时整体回收。
func (rf *RawForwarder) readLoop(br *bufio.Reader) {
	rf.once.Do(func() {
		defer rf.teardown()
		for {
			head := make([]byte, rawHeaderLen)
			if _, err := io.ReadFull(br, head); err != nil {
				rf.markDead("辅助程序通道已断开")
				return
			}
			id := binary.BigEndian.Uint32(head[0:4])
			typ := head[4]
			length := binary.BigEndian.Uint32(head[5:9])
			if length > rawMaxPayload {
				rf.markDead(fmt.Sprintf("协议错误:帧负载超限 %d", length))
				return
			}
			var payload []byte
			if length > 0 {
				payload = make([]byte, length)
				if _, err := io.ReadFull(br, payload); err != nil {
					rf.markDead("辅助程序通道已断开")
					return
				}
			}
			rf.connsMu.Lock()
			rc := rf.conns[id]
			rf.connsMu.Unlock()
			if rc == nil {
				continue // 本地已关闭连接的迟到数据,丢弃
			}
			switch typ {
			case frameACK:
				// 非阻塞投递:握手应答每连接至多一帧,迟到者丢弃,
				// 防御异常对端重复发帧卡死分发循环。
				select {
				case rc.ack <- nil:
				default:
				}
			case frameERROR:
				select {
				case rc.ack <- errors.New(string(payload)):
				default:
				}
			case frameDATA:
				rc.push(payload)
			case frameCLOSE:
				rc.remoteClosed()
			default:
				rf.markDead(fmt.Sprintf("协议错误:未知帧类型 %d", typ))
				return
			}
		}
	})
}

// markDead 标记通道死亡并回收全部连接;读取循环与 Wait 兜底均会调用,
// atomic.Swap 保证幂等,首个到达的诊断优先保留。
func (rf *RawForwarder) markDead(reason string) {
	if rf.dead.Swap(true) {
		return
	}
	if reason != "" && reason != "辅助程序已退出" {
		rf.setLastError("原始模式: " + reason)
	}
	rf.teardown()
}

// teardown 关闭 stdin(远端 helper 收到 EOF 退出)并解除全部连接。
// 幂等:由 stopped 标志防重入。
func (rf *RawForwarder) teardown() {
	rf.mu.Lock()
	if rf.stopped {
		rf.mu.Unlock()
		return
	}
	rf.stopped = true
	rf.mu.Unlock()

	rf.writeMu.Lock()
	if rf.stdin != nil {
		rf.stdin.Close()
	}
	rf.writeMu.Unlock()

	rf.connsMu.Lock()
	conns := rf.conns
	rf.conns = map[uint32]*rawConn{}
	rf.connsMu.Unlock()
	for _, rc := range conns {
		rc.fail(errors.New("原始模式通道已断开"))
		// 非阻塞唤醒尚在握手的 dial(ack 缓冲可能已有迟到应答)。
		select {
		case rc.ack <- errors.New("原始模式通道已断开"):
		default:
		}
	}
}

// stop 主动回收(最后一个转发移除或会话关闭):关闭 stdin 与 exec 通道。
// 正常清理不产生诊断信息,reason 传空。
func (rf *RawForwarder) stop() {
	rf.markDead("")
	rf.mu.Lock()
	sess := rf.execSess
	rf.mu.Unlock()
	if sess != nil {
		sess.Close() // 幂等;stdin.Close 已使 helper 退出,这里兜底回收通道
	}
}

// ---------- rawConn:net.Conn 适配 ----------

// rawReadBufTotal 单连接下行缓冲上限:积压有界,单连接消费停滞不会拖垮
// 整体内存;超过后复用通道读取循环短暂等待消费方腾出空间。
const rawReadBufTotal = 512 << 10

// rawConn 把一条复用连接适配为标准 net.Conn:
// 写路径 → DATA 帧;读路径 → readLoop 分发的 DATA 暂存待取。
type rawConn struct {
	rf   *RawForwarder
	id   uint32
	ack  chan error // OPEN 应答:ACK 为 nil,ERROR 为错误;缓冲 1 不阻塞分发
	once sync.Once  // CLOSE 帧只发一次

	mu      sync.Mutex
	pending [][]byte
	size    int
	readErr error // io.EOF(远端关闭)、net.ErrClosed(本地关闭)或通道错误
	notify  chan struct{}
	closed  bool

	localAddr  net.Addr
	remoteAddr net.Addr
}

// push 复用通道读取循环调用:暂存下行数据并唤醒 Read。
// 缓冲满时短暂等待消费方腾出空间(有背压);连接已放弃时数据直接丢弃,
// 读取循环不被单连接卡死。
func (rc *rawConn) push(data []byte) {
	for {
		rc.mu.Lock()
		if rc.closed || rc.readErr != nil {
			rc.mu.Unlock()
			return
		}
		if rc.size+len(data) <= rawReadBufTotal {
			rc.pending = append(rc.pending, data)
			rc.size += len(data)
			rc.mu.Unlock()
			select {
			case rc.notify <- struct{}{}:
			default:
			}
			return
		}
		rc.mu.Unlock()
		time.Sleep(time.Millisecond) // 等待 Read 消费;有界积压下的轻量自旋
	}
}

// remoteClosed 远端关闭连接:读侧以 io.EOF 结束。
func (rc *rawConn) remoteClosed() {
	rc.mu.Lock()
	if rc.readErr == nil {
		rc.readErr = io.EOF
	}
	rc.mu.Unlock()
	select {
	case rc.notify <- struct{}{}:
	default:
	}
}

// fail 通道级错误:读侧报错并唤醒等待者。
func (rc *rawConn) fail(err error) {
	rc.mu.Lock()
	if rc.readErr == nil {
		rc.readErr = err
	}
	rc.mu.Unlock()
	select {
	case rc.notify <- struct{}{}:
	default:
	}
}

func (rc *rawConn) Read(b []byte) (int, error) {
	for {
		rc.mu.Lock()
		if len(rc.pending) > 0 {
			n := copy(b, rc.pending[0])
			rc.pending[0] = rc.pending[0][n:]
			if len(rc.pending[0]) == 0 {
				rc.pending = rc.pending[1:]
			}
			rc.size -= n
			rc.mu.Unlock()
			return n, nil
		}
		err := rc.readErr
		closed := rc.closed
		rc.mu.Unlock()
		if err != nil {
			return 0, err
		}
		if closed {
			return 0, net.ErrClosed
		}
		<-rc.notify // 等待 push/remoteClosed/fail/Close 唤醒
	}
}

func (rc *rawConn) Write(b []byte) (int, error) {
	rc.mu.Lock()
	closed := rc.closed
	rc.mu.Unlock()
	if closed {
		return 0, net.ErrClosed
	}
	// 大负载分帧:适配 SSH 窗口,也让多连接在通道上细粒度交错。
	sent := 0
	for sent < len(b) {
		end := min(sent+rawWriteChunk, len(b))
		if err := rc.rf.writeFrame(frameDATA, rc.id, b[sent:end]); err != nil {
			return sent, err
		}
		sent = end
	}
	return sent, nil
}

// Close 发送 CLOSE 帧、注销映射并唤醒全部等待者(幂等)。
func (rc *rawConn) Close() error {
	rc.mu.Lock()
	if rc.closed {
		rc.mu.Unlock()
		return net.ErrClosed
	}
	rc.closed = true
	if rc.readErr == nil {
		rc.readErr = net.ErrClosed
	}
	rc.mu.Unlock()
	rc.rf.unregister(rc)
	rc.once.Do(func() {
		// 通道死亡时写入失败无妨:对端随通道一并回收。
		_ = rc.rf.writeFrame(frameCLOSE, rc.id, nil)
	})
	select {
	case rc.notify <- struct{}{}:
	default:
	}
	return nil
}

func (rc *rawConn) LocalAddr() net.Addr  { return rc.localAddr }
func (rc *rawConn) RemoteAddr() net.Addr { return rc.remoteAddr }

// 截止时间:SSH exec 复用通道不提供逐连接超时语义,返回不支持错误
// (与 io.Pipe 等内存实现一致,调用方普遍容忍)。
func (rc *rawConn) SetDeadline(t time.Time) error      { return errNoDeadline }
func (rc *rawConn) SetReadDeadline(t time.Time) error  { return errNoDeadline }
func (rc *rawConn) SetWriteDeadline(t time.Time) error { return errNoDeadline }

var errNoDeadline = errors.New("raw 转发连接不支持截止时间")

// validateRawTarget 与远端 helper 的 validTarget 对齐,提前拦截非法目标。
func validateRawTarget(addr string) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return fmt.Errorf("目标地址不合法,应为 host:port: %q", addr)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("目标端口不合法: %q", portStr)
	}
	if len(addr) > 255 {
		return fmt.Errorf("目标地址过长: %d 字节", len(addr))
	}
	return nil
}

// 编译期接口断言。
var _ net.Conn = (*rawConn)(nil)
