package sshmgr

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// ---------- 测试用远端 helper 模拟器 ----------

// fakeHelper 在 net.Pipe 的远端侧实现与 tools/socks5-helper 相同的帧协议:
// OPEN 真实拨号(可指定失败注入)、DATA 双向搬运、CLOSE 关闭。行为与真实
// helper 一致,使 mux 的单元测试覆盖真实数据路径。
type fakeHelper struct {
	t        *testing.T
	side     net.Conn
	mu       sync.Mutex
	conns    map[uint32]net.Conn
	nextID   uint32
	dialFail map[string]error // 目标地址 → 注入的拨号错误
}

func newFakeHelper(t *testing.T, dialFail map[string]error) (*RawForwarder, *fakeHelper) {
	t.Helper()
	local, remote := net.Pipe()
	t.Cleanup(func() { local.Close(); remote.Close() })
	rf := &RawForwarder{sess: &Session{}, conns: map[uint32]*rawConn{}}
	rf.stdin = local
	go rf.readLoop(bufio.NewReader(local))
	h := &fakeHelper{t: t, side: remote, conns: map[uint32]net.Conn{}, dialFail: dialFail}
	go h.serve()
	t.Cleanup(h.closeAll)
	return rf, h
}

func (h *fakeHelper) writeFrame(id uint32, typ byte, payload []byte) {
	h.t.Helper()
	head := make([]byte, rawHeaderLen)
	binary.BigEndian.PutUint32(head[0:4], id)
	head[4] = typ
	binary.BigEndian.PutUint32(head[5:9], uint32(len(payload)))
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, err := h.side.Write(head); err != nil {
		return
	}
	if len(payload) > 0 {
		h.side.Write(payload)
	}
}

func (h *fakeHelper) readFrame() (uint32, byte, []byte, error) {
	head := make([]byte, rawHeaderLen)
	if _, err := io.ReadFull(h.side, head); err != nil {
		return 0, 0, nil, err
	}
	id := binary.BigEndian.Uint32(head[0:4])
	typ := head[4]
	length := binary.BigEndian.Uint32(head[5:9])
	var payload []byte
	if length > 0 {
		payload = make([]byte, length)
		if _, err := io.ReadFull(h.side, payload); err != nil {
			return 0, 0, nil, err
		}
	}
	return id, typ, payload, nil
}

func (h *fakeHelper) serve() {
	for {
		id, typ, payload, err := h.readFrame()
		if err != nil {
			return
		}
		switch typ {
		case frameOPEN:
			addr := string(payload)
			if ferr, injected := h.dialFail[addr]; injected {
				h.writeFrame(id, frameERROR, []byte(ferr.Error()))
				continue
			}
			c, err := net.DialTimeout("tcp", addr, 3*time.Second)
			if err != nil {
				h.writeFrame(id, frameERROR, []byte(err.Error()))
				continue
			}
			h.mu.Lock()
			h.conns[id] = c
			h.mu.Unlock()
			h.writeFrame(id, frameACK, nil)
			go func(id uint32, c net.Conn) {
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						h.writeFrame(id, frameDATA, buf[:n])
					}
					if err != nil {
						break
					}
				}
				h.writeFrame(id, frameCLOSE, nil)
				h.mu.Lock()
				delete(h.conns, id)
				h.mu.Unlock()
			}(id, c)
		case frameDATA:
			h.mu.Lock()
			c := h.conns[id]
			h.mu.Unlock()
			if c != nil {
				c.Write(payload)
			}
		case frameCLOSE:
			h.mu.Lock()
			c := h.conns[id]
			delete(h.conns, id)
			h.mu.Unlock()
			if c != nil {
				c.Close()
			}
		}
	}
}

func (h *fakeHelper) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, c := range h.conns {
		c.Close()
		delete(h.conns, id)
	}
	h.side.Close()
}

// ---------- 单元测试 ----------

// TestRawMuxEchoRoundtrip 打通 OPEN→ACK→DATA→CLOSE 全链路:经模拟 helper
// 对真实回环 echo 服务读写,验证帧封装与连接映射的正确性。
func TestRawMuxEchoRoundtrip(t *testing.T) {
	echo := newEchoServer(t)
	rf, _ := newFakeHelper(t, nil)

	conn, err := rf.dial("tcp", echo.addr)
	if err != nil {
		t.Fatalf("dial 失败: %v", err)
	}
	defer conn.Close()

	msg := "hello mux"
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatalf("write 失败: %v", err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read 失败: %v", err)
	}
	if string(buf) != msg {
		t.Fatalf("回读不一致: %q", string(buf))
	}
}

// TestRawMuxConcurrentNoMixup 并发 32 条连接各自发送独特标记,验证多路
// 复用不会串流(验收:多并发连接正常,无数据错乱)。
func TestRawMuxConcurrentNoMixup(t *testing.T) {
	echo := newEchoServer(t)
	rf, _ := newFakeHelper(t, nil)

	const n = 32
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := rf.dial("tcp", echo.addr)
			if err != nil {
				errs <- err
				return
			}
			defer conn.Close()
			msg := fmt.Sprintf("conn-marker-%d", time.Now().UnixNano())
			if _, err := conn.Write([]byte(msg)); err != nil {
				errs <- err
				return
			}
			buf := make([]byte, len(msg))
			if _, err := io.ReadFull(conn, buf); err != nil {
				errs <- err
				return
			}
			if string(buf) != msg {
				errs <- fmt.Errorf("数据错乱: sent %q got %q", msg, string(buf))
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestRawMuxOpenError 拨号失败(ERROR 帧)应携带远端原因返回给调用方。
func TestRawMuxOpenError(t *testing.T) {
	rf, _ := newFakeHelper(t, map[string]error{
		"10.255.255.1:80": errors.New("connect: connection refused"),
	})
	_, err := rf.dial("tcp", "10.255.255.1:80")
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("应透传远端拨号错误,实际: %v", err)
	}
}

// TestRawMuxRemoteClose 远端关闭连接后本地 Read 应返回 io.EOF。
func TestRawMuxRemoteClose(t *testing.T) {
	once := newOnceServer(t)
	rf, _ := newFakeHelper(t, nil)

	conn, err := rf.dial("tcp", once.addr)
	if err != nil {
		t.Fatalf("dial 失败: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("x")); err != nil {
		t.Fatalf("write 失败: %v", err)
	}
	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	if n == 0 || err != nil {
		t.Fatalf("应读到一次性数据,实际 n=%d err=%v", n, err)
	}
	if _, err := conn.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("远端关闭后应返回 io.EOF,实际: %v", err)
	}
}

// TestRawMuxChannelDeath 复用通道死亡后:存量连接读侧报错、新拨号失败,
// 且错误信息携带通道诊断。
func TestRawMuxChannelDeath(t *testing.T) {
	echo := newEchoServer(t)
	rf, helper := newFakeHelper(t, nil)

	conn, err := rf.dial("tcp", echo.addr)
	if err != nil {
		t.Fatalf("dial 失败: %v", err)
	}
	helper.closeAll() // 模拟远端 helper 退出(stdout EOF)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if rf.dead.Load() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !rf.dead.Load() {
		t.Fatal("通道应被标记死亡")
	}
	buf := make([]byte, 8)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("存量连接读侧应报错")
	}
	if _, err := rf.dial("tcp", echo.addr); err == nil {
		t.Fatal("通道死亡后新拨号应失败")
	}
}

// TestRawMuxLargeTransfer 大块数据(超过单帧上限,触发分帧)完整性。
func TestRawMuxLargeTransfer(t *testing.T) {
	echo := newEchoServer(t)
	rf, _ := newFakeHelper(t, nil)

	conn, err := rf.dial("tcp", echo.addr)
	if err != nil {
		t.Fatalf("dial 失败: %v", err)
	}
	defer conn.Close()
	// rawWriteChunk 分帧路径:写入 3 倍帧上限的数据并完整回读比对。
	payload := make([]byte, rawWriteChunk*3+7)
	for i := range payload {
		payload[i] = byte(i)
	}
	go func() {
		conn.Write(payload)
	}()
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("大块传输失败: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("大块传输内容不一致: got %d bytes", len(got))
	}
}

// TestValidateRawTarget 目标校验与远端 helper 对齐。
func TestValidateRawTarget(t *testing.T) {
	cases := []struct {
		addr string
		ok   bool
	}{
		{"127.0.0.1:80", true},
		{"example.com:443", true},
		{"[::1]:8080", true},
		{"127.0.0.1", false},                      // 缺端口
		{"127.0.0.1:0", false},                    // 端口越界
		{"127.0.0.1:70000", false},                // 端口越界
		{"127.0.0.1:abc", false},                  // 端口非数字
		{":80", false},                            // 缺主机
		{strings.Repeat("a", 300) + ":80", false}, // 超长
	}
	for _, tc := range cases {
		err := validateRawTarget(tc.addr)
		if tc.ok != (err == nil) {
			t.Errorf("validateRawTarget(%q) = %v, 期望 ok=%v", tc.addr, err, tc.ok)
		}
	}
}

// TestForwardDenied 仅 sshd 转发禁令类错误触发自动降级。
func TestForwardDenied(t *testing.T) {
	denied := &ssh.OpenChannelError{Reason: ssh.Prohibited, Message: "open failed"}
	other := &ssh.OpenChannelError{Reason: ssh.ConnectionFailed, Message: "connect failed"}
	unreachable := errors.New("dial tcp 1.2.3.4:80: connect: connection timed out")
	textDenied := errors.New("ssh: rejected: administratively prohibited (open failed)")
	if !forwardDenied(denied) || !forwardDenied(textDenied) {
		t.Fatal("转发禁令错误应被识别")
	}
	if forwardDenied(other) || forwardDenied(unreachable) {
		t.Fatal("非禁令错误不应触发降级")
	}
}

// TestRawHelperEmbedded 内嵌产物完整:合法 gzip、ELF 头、架构匹配。
func TestRawHelperEmbedded(t *testing.T) {
	for arch, name := range rawHelperFiles {
		f, err := rawHelperFS.Open(name)
		if err != nil {
			t.Fatalf("%s 打开失败: %v", name, err)
		}
		zr, err := gzip.NewReader(f)
		if err != nil {
			t.Fatalf("%s 不是合法 gzip: %v", name, err)
		}
		head := make([]byte, 20)
		if _, err := io.ReadFull(zr, head); err != nil {
			t.Fatalf("%s 解压失败: %v", name, err)
		}
		if !bytes.HasPrefix(head, []byte("\x7fELF")) {
			t.Fatalf("%s 不是 ELF: % x", name, head[:4])
		}
		// e_machine(小端,偏移 18):amd64=0x3e,arm64=0xb7
		machine := binary.LittleEndian.Uint16(head[18:20])
		want := map[string]uint16{"amd64": 0x3e, "arm64": 0xb7}[arch]
		if machine != want {
			t.Fatalf("%s 架构不符: e_machine=%#x 期望 %#x", name, machine, want)
		}
		zr.Close()
		f.Close()
	}
}

// ---------- 测试用回环服务 ----------

// echoServer 每条连接回显收到的数据。
type echoServer struct {
	addr string
	ln   net.Listener
}

func newEchoServer(t *testing.T) *echoServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("回环监听失败: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(conn)
		}
	}()
	return &echoServer{addr: ln.Addr().String(), ln: ln}
}

// onceServer 只回写一段数据后立即关闭连接(模拟远端主动关闭)。
type onceServer struct {
	addr string
	ln   net.Listener
}

func newOnceServer(t *testing.T) *onceServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("回环监听失败: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				c.Write([]byte("one-shot"))
				time.Sleep(100 * time.Millisecond)
			}(conn)
		}
	}()
	return &onceServer{addr: ln.Addr().String(), ln: ln}
}
