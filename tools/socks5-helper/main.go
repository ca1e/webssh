// socks5-helper 是 webssh「原始模式转发」的远端辅助程序。
//
// 它经单个 SSH exec session 的 stdin/stdout 与 webssh 后端通信:后端把多条
// 隧道连接复用为二进制帧流写入 stdin,helper 解析帧、向目标发起 TCP 连接、
// 把响应封装成帧写回 stdout。全程只占用 session 通道,不使用 direct-tcpip /
// tcpip-forward,因此不受 sshd AllowTcpForwarding=no 限制;helper 自身不监听
// 任何网络端口。
//
// 帧协议(与 internal/sshmgr/raw_forwarder.go 保持一致):
//
//	+---------+--------+--------+---------+
//	| connID  |  type  | length | payload |
//	| 4 字节  | 1 字节 | 4 字节 | length  |   (整数均为大端)
//	+---------+--------+--------+---------+
//
//	type: 1=OPEN(host:port) 2=DATA 3=CLOSE 4=ERROR(message) 5=ACK(拨号成功应答)
//
// ACK 是对 OPEN 的成功应答(payload 为空):SOCKS5 层必须在向客户端回包前
// 得知远端拨号成败,而多数目标(如 HTTP 服务)不会主动先发数据,无法用
// 「首包到达」推断成功,故增设显式应答帧。
//
// 用法:webssh 后端经 SFTP 或 base64 上传本程序到远端后以 `exec <path>` 启动。
// `helper version` 输出版本号并退出(供部署方校验二进制可执行、架构匹配)。
package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
)

const version = "1"

// 帧类型,与本地侧 raw_forwarder.go 一致。
const (
	frameOPEN  = 1
	frameDATA  = 2
	frameCLOSE = 3
	frameERROR = 4
	frameACK   = 5
)

const headerLen = 9 // connID(4) + type(1) + length(4)

// maxPayload 单帧负载上限,与本地侧写入上限一致;超出视为协议错误。
const maxPayload = 1 << 20

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("webssh-rawfwd", version)
		return
	}
	if err := run(); err != nil {
		// 诊断信息走 stderr,绝不污染 stdout(那是数据通道)。
		fmt.Fprintln(os.Stderr, "webssh-rawfwd:", err)
		os.Exit(1)
	}
}

func run() error {
	// outMu 串行化 stdout 帧写入:多条连接的 pump goroutine 并发回包,
	// 单帧必须原子落盘,否则流会错乱。
	var outMu sync.Mutex
	writeFrame := func(connID uint32, typ byte, payload []byte) error {
		outMu.Lock()
		defer outMu.Unlock()
		head := make([]byte, headerLen)
		binary.BigEndian.PutUint32(head[0:4], connID)
		head[4] = typ
		binary.BigEndian.PutUint32(head[5:9], uint32(len(payload)))
		if _, err := os.Stdout.Write(head); err != nil {
			return err
		}
		if len(payload) > 0 {
			if _, err := os.Stdout.Write(payload); err != nil {
				return err
			}
		}
		return nil
	}

	var (
		mu    sync.RWMutex
		conns = map[uint32]net.Conn{}
	)
	closeConn := func(connID uint32) {
		mu.Lock()
		c, ok := conns[connID]
		if ok {
			delete(conns, connID)
		}
		mu.Unlock()
		if ok {
			c.Close() // 使该连接的 pump(若在)退出
		}
	}

	// pump 把一条已建立 TCP 连接的远端数据回传为 DATA 帧,读到 EOF(或连接
	// 被关闭导致的读错误)后回 CLOSE 帧并注销。EOF 与被主动关闭统一走 CLOSE:
	// 本地侧以「收到 CLOSE」作为下行结束信号,语义与双向搬运代理一致。
	pump := func(connID uint32, c net.Conn) {
		buf := make([]byte, 32*1024)
		for {
			n, err := c.Read(buf)
			if n > 0 {
				if writeFrame(connID, frameDATA, buf[:n]) != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		c.Close()
		writeFrame(connID, frameCLOSE, nil)
		mu.Lock()
		delete(conns, connID)
		mu.Unlock()
	}

	// dialTimeout 限远端拨号等待;OPEN 的应答(ACK/ERROR)由调用方读循环处理。
	const dialTimeout = 10e9 // 10s

	// stdin 用 bufio 缓冲读:帧头 9 字节 + 负载经 io.ReadFull 拼齐。
	br := bufio.NewReader(os.Stdin)
	for {
		head := make([]byte, headerLen)
		if _, err := io.ReadFull(br, head); err != nil {
			// 通道关闭(本地侧退出/断线)属正常收尾;其余读错误一并结束,
			// 由 defer 统一回收全部连接,不留残留。
			return shutdown(conns, &mu, nil)
		}
		connID := binary.BigEndian.Uint32(head[0:4])
		typ := head[4]
		length := binary.BigEndian.Uint32(head[5:9])
		if length > maxPayload {
			return shutdown(conns, &mu, fmt.Errorf("帧负载超限 %d", length))
		}
		var payload []byte
		if length > 0 {
			payload = make([]byte, length)
			if _, err := io.ReadFull(br, payload); err != nil {
				return shutdown(conns, &mu, err)
			}
		}

		switch typ {
		case frameOPEN:
			addr := string(payload)
			if !validTarget(addr) {
				writeFrame(connID, frameERROR, []byte("目标地址不合法: "+addr))
				continue
			}
			c, err := net.DialTimeout("tcp", addr, dialTimeout)
			if err != nil {
				writeFrame(connID, frameERROR, []byte(singleLine(err.Error())))
				continue
			}
			mu.Lock()
			conns[connID] = c
			mu.Unlock()
			if writeFrame(connID, frameACK, nil) != nil {
				c.Close()
				continue
			}
			go pump(connID, c)
		case frameDATA:
			mu.RLock()
			c := conns[connID]
			mu.RUnlock()
			if c == nil {
				continue // 对端已关闭的迟到数据,丢弃即可
			}
			// net.Conn.Write 保证非 n==len 时返回错误,无需循环补写。
			if _, err := c.Write(payload); err != nil {
				closeConn(connID)
				writeFrame(connID, frameCLOSE, nil)
			}
		case frameCLOSE:
			closeConn(connID)
		case frameERROR, frameACK:
			// 本地→远端方向不使用这两种帧;忽略以保持前向兼容。
		default:
			return shutdown(conns, &mu, fmt.Errorf("未知帧类型 %d", typ))
		}
	}
}

// shutdown 关闭全部连接并返回 err(stdin 结束时 err 为 nil,静默退出)。
func shutdown(conns map[uint32]net.Conn, mu *sync.RWMutex, err error) error {
	mu.Lock()
	for id, c := range conns {
		c.Close()
		delete(conns, id)
	}
	mu.Unlock()
	return err
}

// validTarget 校验 OPEN 负载为 host:port;控制字符与超长串直接拒绝。
func validTarget(addr string) bool {
	if len(addr) == 0 || len(addr) > 255 {
		return false
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return false
	}
	for i := range addr {
		if addr[i] < 0x20 || addr[i] == 0x7f {
			return false
		}
	}
	return true
}

// singleLine 把错误压成单行,适配帧负载(诊断消息不允许带换行)。
func singleLine(s string) string {
	out := make([]byte, 0, len(s))
	for i := range s {
		if len(out) >= 256 {
			break
		}
		if s[i] >= 0x20 {
			out = append(out, s[i])
		}
	}
	return string(out)
}
