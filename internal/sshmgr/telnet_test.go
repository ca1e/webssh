package sshmgr

import (
	"bytes"
	"io"
	"net"
	"testing"

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
