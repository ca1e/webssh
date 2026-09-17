package sshmgr

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"webssh/internal/store"
)

// fakeTelnetConn 借嵌入 nil net.Conn 免实现 net.Conn 全接口,只覆写实际用到的
// Read/Write;写入进入 buf 供断言,读取来自 r。
type fakeTelnetConn struct {
	net.Conn
	r   io.Reader
	buf bytes.Buffer
}

func (f *fakeTelnetConn) Read(p []byte) (int, error)  { return f.r.Read(p) }
func (f *fakeTelnetConn) Write(p []byte) (int, error) { return f.buf.Write(p) }

func TestTelnetConnFeed(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want []byte
	}{
		{"纯数据", []byte("login:"), []byte("login:")},
		{"IAC IAC 还原字面 0xFF", []byte{'a', telIAC, telIAC, 'b'}, []byte{'a', telIAC, 'b'}},
		{"WILL 协商剥离", []byte{'a', telIAC, telWILL, 1, 'b'}, []byte("ab")},
		{"DO 协商剥离", []byte{'a', telIAC, telDO, 24, 'b'}, []byte("ab")},
		{"WONT/DONT 剥离", []byte{'a', telIAC, telWONT, 0, telIAC, telDONT, 1, 'b'}, []byte("ab")},
		{"子协商整体剥离", []byte{'a', telIAC, telSB, 24, 0, 'x', telIAC, telSE, 'b'}, []byte("ab")},
		{"子协商内转义 0xFF", []byte{'a', telIAC, telSB, 0, telIAC, telIAC, telIAC, telSE, 'b'}, []byte("ab")},
	}
	for _, tc := range cases {
		c := newTelnetConn(&fakeTelnetConn{})
		c.feed(tc.in)
		if !bytes.Equal(c.out, tc.want) {
			t.Errorf("%s: out = %q, want %q", tc.name, c.out, tc.want)
		}
	}
}

func TestTelnetConnReadFilter(t *testing.T) {
	// 跨段被截断的序列(动词与选项字节分离)仍须正确剥离并交付明文
	fc := &fakeTelnetConn{r: bytes.NewReader([]byte{'a', telIAC, telWILL, 1, 'b', telIAC})}
	c := newTelnetConn(fc)
	var got []byte
	buf := make([]byte, 16)
	for {
		n, err := c.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			break
		}
	}
	if want := []byte("ab"); !bytes.Equal(got, want) {
		t.Fatalf("Read 过滤结果 = %q, want %q", got, want)
	}
}

func TestTelnetConnRespond(t *testing.T) {
	fc := &fakeTelnetConn{}
	c := newTelnetConn(fc)

	c.feed([]byte{telIAC, telWILL, 1})
	if want := []byte{telIAC, telDONT, 1}; !bytes.Equal(fc.buf.Bytes(), want) {
		t.Fatalf("对 WILL 的应答 = %x, want %x", fc.buf.Bytes(), want)
	}
	fc.buf.Reset()

	c.feed([]byte{telIAC, telDO, 3})
	if want := []byte{telIAC, telWONT, 3}; !bytes.Equal(fc.buf.Bytes(), want) {
		t.Fatalf("对 DO 的应答 = %x, want %x", fc.buf.Bytes(), want)
	}
	fc.buf.Reset()

	// WONT/DONT 本身即拒绝,不再回复
	c.feed([]byte{telIAC, telWONT, 0, telIAC, telDONT, 1})
	if fc.buf.Len() != 0 {
		t.Fatalf("WONT/DONT 不应回复,得到 %x", fc.buf.Bytes())
	}
}

func TestTelnetConnWriteEscape(t *testing.T) {
	fc := &fakeTelnetConn{}
	c := newTelnetConn(fc)
	n, err := c.Write([]byte{'a', telIAC, 'b'})
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("Write 返回值 = %d, want 3(按用户数据长度计)", n)
	}
	if want := []byte{'a', telIAC, telIAC, 'b'}; !bytes.Equal(fc.buf.Bytes(), want) {
		t.Fatalf("写出字节 = %x, want %x", fc.buf.Bytes(), want)
	}

	// 无 0xFF 的数据直通
	fc.buf.Reset()
	if _, err := c.Write([]byte("ls\r\n")); err != nil {
		t.Fatal(err)
	}
	if got := fc.buf.String(); got != "ls\r\n" {
		t.Fatalf("普通数据被改写: %q", got)
	}
}

func TestTelnetAddrDefaultPort(t *testing.T) {
	if _, port := telnetAddr(store.Host{Host: "h"}); port != 23 {
		t.Fatalf("telnet 默认端口 = %d, want 23", port)
	}
	if _, port := telnetAddr(store.Host{Host: "h", Port: 2323}); port != 2323 {
		t.Fatalf("自定义端口 = %d, want 2323", port)
	}
}

// fakeDialer 可注入的 telnet 拨号:每次拨号返回一条新的 net.Pipe 连接,
// 服务端一侧交给断言方使用。
type fakeDialer struct {
	mu    sync.Mutex
	conns []*telnetConn
	peers []net.Conn
	err   error
}

func (f *fakeDialer) dial(context.Context, store.Host) (*telnetConn, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, "", f.err
	}
	c1, c2 := net.Pipe()
	f.conns = append(f.conns, newTelnetConn(c1))
	f.peers = append(f.peers, c2)
	return f.conns[len(f.conns)-1], "fake:23", nil
}

// newTestTelnetSession 构造走 fake 拨号的 telnet 会话(orphanGrace<=0 为
// 测试哨兵,不启动孤儿回收)。
func newTestTelnetSession(t *testing.T) (*Manager, *Session, *fakeDialer) {
	t.Helper()
	m := New(t.TempDir(), 0, 0)
	fd := &fakeDialer{}
	m.dialTelnetFn = fd.dial
	s, err := m.openTelnetSession(context.Background(), store.Host{Name: "t", Host: "fake"})
	if err != nil {
		t.Fatalf("openTelnetSession: %v", err)
	}
	return m, s, fd
}

// TestTelnetTerminalIndependentConns 分屏语义:每个终端独享一条连接,互不
// 干扰;关闭一个终端只回收它自己的连接;会话关闭回收全部连接。
func TestTelnetTerminalIndependentConns(t *testing.T) {
	m, s, fd := newTestTelnetSession(t)

	t1, err := s.NewTerminal(80, 24, "")
	if err != nil {
		t.Fatal(err)
	}
	t2, err := s.NewTerminal(80, 24, "")
	if err != nil {
		t.Fatal(err)
	}
	// 模拟前端 WS 泵持续消费终端输出(本地回显写入管道需要读者)
	for _, tt := range []*Terminal{t1, t2} {
		go func(r io.Reader) {
			buf := make([]byte, 128)
			for {
				if _, err := r.Read(buf); err != nil {
					return
				}
			}
		}(tt.Stdout())
	}
	if len(fd.conns) != 2 {
		t.Fatalf("终端数量 = %d, 拨号连接数 = %d(每终端应独享一条)", 2, len(fd.conns))
	}

	// 终端 1 的输入只到达连接 1 的对端
	go func() { _, _ = t1.Stdin().Write([]byte("one\r")) }()
	got := make([]byte, 5)
	if err := readAllTimeout(t, fd.peers[0], got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "one\r\n" {
		t.Fatalf("连接 1 收到 %q, want one\\r\\n(回车应补全为 CR LF)", got)
	}

	// 关闭终端 1:仅其连接被回收,终端 2 的连接不受影响
	if err := t1.Close(); err != nil {
		t.Fatal(err)
	}
	if err := expectClosed(t, fd.conns[0]); err != nil {
		t.Fatalf("终端 1 关闭后其连接应关闭: %v", err)
	}
	go func() { _, _ = t2.Stdin().Write([]byte("two\r")) }()
	if err := readAllTimeout(t, fd.peers[1], got); err != nil {
		t.Fatalf("终端 2 连接收到的数据 %q: %v", got, err)
	}
	if string(got) != "two\r\n" {
		t.Fatalf("连接 2 收到 %q, want two\\r\\n", got)
	}

	// 会话关闭:剩余终端的连接全部回收
	m.CloseAll()
	if err := expectClosed(t, fd.conns[1]); err != nil {
		t.Fatalf("会话关闭后终端 2 连接应关闭: %v", err)
	}
}

func readAllTimeout(t *testing.T, r io.Reader, b []byte) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		got := 0
		for got < len(b) {
			n, err := r.Read(b[got:])
			got += n
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		return errors.New("等待数据超时")
	}
}

func expectClosed(t *testing.T, c net.Conn) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 8)
		for {
			if _, err := c.Read(buf); err != nil {
				done <- nil
				return
			}
		}
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		return errors.New("等待连接关闭超时")
	}
}

// TestTelnetStdinLineDiscipline NVT 行模式:本地回显、退格擦除、回车补全
// CR LF、CR 后的 LF 去重。
func TestTelnetStdinLineDiscipline(t *testing.T) {
	fc := &fakeTelnetConn{}
	pr, pw := io.Pipe()
	defer pr.Close()
	w := &telnetStdin{sess: &Session{}, conn: newTelnetConn(fc), out: pw}

	// 回显收集(管道需并发读取)
	var echo bytes.Buffer
	echoDone := make(chan struct{})
	go func() {
		buf := make([]byte, 64)
		for {
			n, err := pr.Read(buf)
			echo.Write(buf[:n])
			if err != nil {
				close(echoDone)
				return
			}
		}
	}()

	// "ab" → 退格擦掉 b → 回车成行;再写 "\r\n" 验证 LF 去重
	if n, err := w.Write([]byte("ab\bX\r")); err != nil || n != 5 {
		t.Fatalf("Write = %d, %v; want 5, nil", n, err)
	}
	if got := fc.buf.String(); got != "aX\r\n" {
		t.Fatalf("远端收到 %q, want aX\\r\\n(退格本地删除、回车补 CR LF)", got)
	}
	fc.buf.Reset()
	if _, err := w.Write([]byte("\r\n")); err != nil {
		t.Fatal(err)
	}
	if got := fc.buf.String(); got != "\r\n" {
		t.Fatalf("远端收到 %q, want 恰好一次 CR LF(LF 随 CR 去重)", got)
	}
	// 关闭回显管道,让收集协程退出
	pw.Close()
	<-echoDone
	if got := echo.String(); got != "ab\b \bX\r\n\r\n" {
		t.Fatalf("本地回显 = %q, want ab\\b \\bX\\r\\n\\r\\n", got)
	}
}
