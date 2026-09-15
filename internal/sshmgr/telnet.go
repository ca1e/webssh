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

// telnetAddr 返回 telnet 主机地址,默认端口 23。
func telnetAddr(h store.Host) (string, int) {
	port := h.Port
	if port <= 0 {
		port = 23
	}
	return net.JoinHostPort(h.Host, strconv.Itoa(port)), port
}

// dialTelnet 建立 telnet 连接:纯 TCP 拨号,链路保活交由内核 keepalive。
func dialTelnet(ctx context.Context, h store.Host) (net.Conn, string, error) {
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

func (c *telnetConn) Close() error                   { return c.conn.Close() }
func (c *telnetConn) LocalAddr() net.Addr            { return c.conn.LocalAddr() }
func (c *telnetConn) RemoteAddr() net.Addr           { return c.conn.RemoteAddr() }
func (c *telnetConn) SetDeadline(t time.Time) error  { return c.conn.SetDeadline(t) }
func (c *telnetConn) SetReadDeadline(t time.Time) error  { return c.conn.SetReadDeadline(t) }
func (c *telnetConn) SetWriteDeadline(t time.Time) error { return c.conn.SetWriteDeadline(t) }

// newTelnetTerminal 把已建立的 telnet 连接包装为交互终端:stdin/stdout 即
// 连接本身(IAC 已由 telnetConn 处理),登录与 shell 交互完全透传。
func newTelnetTerminal(conn io.ReadWriteCloser) *Terminal {
	return &Terminal{stdin: conn, stdout: conn, closer: conn}
}

// ErrSSHOnly 统一应答依赖 SSH 通道的能力调用(telnet 会话前端不提供这些
// 能力入口,此处仅守卫直接 API 调用)。
var ErrSSHOnly = fmt.Errorf("该能力经由 SSH 通道提供")
