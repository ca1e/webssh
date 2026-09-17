package sshmgr

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"webssh/internal/store"
)

// ---------- telnet 连接:透明字节流 + 最小 IAC 处理 ----------

// telnet 协议命令字节(RFC 854)。0xFF(IAC)为转义前缀,后随动词与选项;
// 0xFF 0xFF 表示字面 0xFF。子协商(IAC SB ... IAC SE)整体跳过。
const (
	telIAC  = 255
	telDONT = 254
	telDO   = 253
	telWONT = 252
	telWILL = 251
	telSB   = 250
	telSE   = 240
)

// dialTelnetConn 经 Manager 拨号 telnet 连接(支持测试注入)。
func (m *Manager) dialTelnetConn(ctx context.Context, h store.Host) (*telnetConn, string, error) {
	if m.dialTelnetFn != nil {
		return m.dialTelnetFn(ctx, h)
	}
	return dialTelnet(ctx, h)
}

// telnetAddr 返回 telnet 主机地址,默认端口 23。
func telnetAddr(h store.Host) (string, int) {
	port := h.Port
	if port <= 0 {
		port = 23
	}
	return net.JoinHostPort(h.Host, strconv.Itoa(port)), port
}

// dialTelnet 建立 telnet 连接:纯 TCP 拨号,链路保活交由内核 keepalive。
func dialTelnet(ctx context.Context, h store.Host) (*telnetConn, string, error) {
	addr, _ := telnetAddr(h)
	d := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, addr, err
	}
	return newTelnetConn(nc), addr, nil
}

// telnetConn 包装 TCP 连接为 IAC 感知的双向字节流:Read 剥离协议序列
// (协商一律婉拒:WILL 回 DONT、DO 回 WONT),Write 转义用户数据中的 0xFF,
// 上层视角即一条普通 TCP 连接。同时实现 net.Conn,便于会话统一持有与关闭。
type telnetConn struct {
	conn net.Conn
	wmu  sync.Mutex // 序列化协商应答与用户数据,避免交错写出

	iacSeen bool // 已收到 IAC,等待动词字节
	verb    byte // 已收到协商动词,等待选项字节
	inSB    bool // 处于子协商中
	sbIAC   bool // 子协商中已收到 IAC(等待 SE)
	out     []byte
	readErr error // 底层读错误,待缓冲数据交付后上报
}

func newTelnetConn(nc net.Conn) *telnetConn {
	return &telnetConn{conn: nc}
}

func (c *telnetConn) Read(p []byte) (int, error) {
	for len(c.out) == 0 {
		if c.readErr != nil {
			return 0, c.readErr
		}
		var buf [32 * 1024]byte
		n, err := c.conn.Read(buf[:])
		if n > 0 {
			c.feed(buf[:n])
		}
		if err != nil {
			c.readErr = err
			if len(c.out) == 0 {
				return 0, err
			}
		}
	}
	m := copy(p, c.out)
	c.out = append(c.out[:0], c.out[m:]...)
	return m, nil
}

// feed 消费一段原始输出:过滤 IAC 序列,明文追加到待交付缓冲。
func (c *telnetConn) feed(b []byte) {
	for _, v := range b {
		switch {
		case c.inSB:
			// 子协商内容整体丢弃,只识别 IAC SE 结束(子协商内的 0xFF 按协议
			// 转义为 IAC IAC,此处仅跳过)
			switch {
			case c.sbIAC && v == telSE:
				c.inSB, c.sbIAC = false, false
			case c.sbIAC && v == telIAC:
			case c.sbIAC:
				c.sbIAC = false
			case v == telIAC:
				c.sbIAC = true
			}
		case c.verb != 0:
			c.respond(c.verb, v)
			c.verb = 0
		case c.iacSeen:
			switch v {
			case telIAC: // IAC IAC → 字面 0xFF
				c.out = append(c.out, telIAC)
				c.iacSeen = false
			case telWILL, telWONT, telDO, telDONT:
				c.iacSeen = false
				c.verb = v
			case telSB:
				c.iacSeen = false
				c.inSB = true
			default:
				// 其余单字节命令(NOP/AYT 等)忽略
				c.iacSeen = false
			}
		case v == telIAC:
			c.iacSeen = true
		default:
			c.out = append(c.out, v)
		}
	}
}

// respond 应答对端协商:WILL/DO 婉拒(WONT/DONT 已是拒绝,无需再答)。
func (c *telnetConn) respond(verb, opt byte) {
	var reply []byte
	switch verb {
	case telWILL:
		reply = []byte{telIAC, telDONT, opt}
	case telDO:
		reply = []byte{telIAC, telWONT, opt}
	}
	if reply == nil {
		return
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, _ = c.conn.Write(reply)
}

// Write 转义用户数据中的 0xFF 后发送,保证字面传输。
func (c *telnetConn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if bytes.IndexByte(p, telIAC) < 0 {
		return c.conn.Write(p)
	}
	buf := make([]byte, 0, len(p)+16)
	for _, b := range p {
		buf = append(buf, b)
		if b == telIAC {
			buf = append(buf, telIAC)
		}
	}
	if _, err := c.conn.Write(buf); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *telnetConn) Close() error                       { return c.conn.Close() }
func (c *telnetConn) LocalAddr() net.Addr                { return c.conn.LocalAddr() }
func (c *telnetConn) RemoteAddr() net.Addr               { return c.conn.RemoteAddr() }
func (c *telnetConn) SetDeadline(t time.Time) error      { return c.conn.SetDeadline(t) }
func (c *telnetConn) SetReadDeadline(t time.Time) error  { return c.conn.SetReadDeadline(t) }
func (c *telnetConn) SetWriteDeadline(t time.Time) error { return c.conn.SetWriteDeadline(t) }

// telnetStdin 终端上行:实现 NVT 行模式(与真实 telnet 客户端在远端未协商
// 接管回显时的行为一致)——
//   - 本地回显:敲下的字符立即出现在本终端(此前裸透传时,HTTP 等 ASCII
//     服务自身不回显,表现为"输入无反应");
//   - 退格编辑:\b/0x7f 本地删字,回显 \b \b 擦除;
//   - 行结束:回车按 NVT 规范补全为 CR LF 整行发送(此前只透传孤立 CR,
//     按 RFC 854 行结束是 CR LF,HTTP 等服务端无法识别孤 CR,请求永无响应);
//   - CR 后紧跟的 LF 属同一行结束,去重避免多发一次空行。
type telnetStdin struct {
	sess *Session
	conn *telnetConn
	out  *io.PipeWriter // 本地回显出口(与本终端的远端输出共用一条流)

	line   []byte
	lastCR bool
}

func (w *telnetStdin) Write(p []byte) (int, error) {
	for i, b := range p {
		switch {
		case b == '\n' && w.lastCR: // CR LF 中的 LF:已随 CR 发送
		case b == '\r' || b == '\n':
			w.line = append(w.line, '\r', '\n')
			w.echo([]byte("\r\n"))
			if _, err := w.conn.Write(w.line); err != nil {
				return i, err
			}
			w.line = w.line[:0]
		case b == '\b' || b == 0x7f:
			if len(w.line) > 0 {
				w.line = w.line[:len(w.line)-1]
				w.echo([]byte("\b \b"))
			}
		default:
			w.line = append(w.line, b)
			w.echo([]byte{b})
		}
		w.lastCR = b == '\r'
	}
	return len(p), nil
}

func (w *telnetStdin) echo(b []byte) { _, _ = w.out.Write(b) }

func (w *telnetStdin) Close() error {
	w.sess.dropTelnetConn(w.conn)
	return nil
}

// telnetBanner 终端首屏提示:telnet 无登录握手,连上即是黑屏,打印已连接
// 提示并新起一行,用户即可在本地回显下直接输入。
func telnetBanner(h store.Host) string {
	host := h.Host
	if p := h.Port; p != 0 && p != 23 {
		host = fmt.Sprintf("%s:%d", host, p)
	}
	return "Connected to " + host + "\r\n"
}

// newTelnetTerminal 把一个终端附着到它独享的 telnet 连接上:远端输出由泵
// 协程拷入本终端的管道(连接独享,无需多路广播),泵先写欢迎行再拷远端,
// 保证横幅总在远端输出之前;上行经 telnetStdin 的 NVT 行模式处理。连接
// 关闭时泵协程把错误带给读侧,前端即收到 exit。
func newTelnetTerminal(sess *Session, conn *telnetConn) *Terminal {
	pr, pw := io.Pipe()
	banner := telnetBanner(sess.Host)
	go func() {
		_, _ = pw.Write([]byte(banner)) // 管道写入阻塞至 WS 泵开始消费,须在泵协程内
		_, err := io.Copy(pw, conn)
		pw.CloseWithError(err)
	}()
	in := &telnetStdin{sess: sess, conn: conn, out: pw}
	return &Terminal{stdin: in, stdout: pr, closer: in}
}

// ErrSSHOnly 统一应答依赖 SSH 通道的能力调用(telnet 会话前端不提供这些
// 能力入口,此处仅守卫直接 API 调用)。
var ErrSSHOnly = fmt.Errorf("该能力经由 SSH 通道提供")
