package sshmgr

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"strconv"
)

// socks5Serve 实现最小 SOCKS5 服务端:仅支持 CONNECT 命令、无鉴权(等效 ssh -D)。
// dialer 决定目标地址如何拨出(此处经 SSH 远程拨号)。
func socks5Serve(conn net.Conn, dial func(network, addr string) (net.Conn, error)) {
	defer conn.Close()
	br := bufio.NewReader(conn)

	// +----+----------+----------+
	// |VER | NMETHODS | METHODS  |
	head := make([]byte, 2)
	if _, err := io.ReadFull(br, head); err != nil {
		return
	}
	if head[0] != 5 {
		return
	}
	methods := make([]byte, int(head[1]))
	if _, err := io.ReadFull(br, methods); err != nil {
		return
	}
	// 仅提供 NO-AUTH(0x00)
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	// +----+-----+-------+------+----------+----------+
	// |VER | CMD |  RSV  | ATYP | DST.ADDR | DST.PORT |
	req := make([]byte, 4)
	if _, err := io.ReadFull(br, req); err != nil {
		return
	}
	if req[0] != 5 {
		return
	}
	replyErr := func(code byte) {
		conn.Write([]byte{0x05, code, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	}
	if req[1] != 0x01 { // 仅 CONNECT
		replyErr(0x07)
		return
	}

	var host string
	switch req[3] {
	case 0x01: // IPv4
		ip := make([]byte, 4)
		if _, err := io.ReadFull(br, ip); err != nil {
			return
		}
		host = net.IP(ip).String()
	case 0x03: // 域名
		n := make([]byte, 1)
		if _, err := io.ReadFull(br, n); err != nil {
			return
		}
		name := make([]byte, int(n[0]))
		if _, err := io.ReadFull(br, name); err != nil {
			return
		}
		host = string(name)
	case 0x04: // IPv6
		ip := make([]byte, 16)
		if _, err := io.ReadFull(br, ip); err != nil {
			return
		}
		host = net.IP(ip).String()
	default:
		replyErr(0x08)
		return
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(br, portBytes); err != nil {
		return
	}
	port := binary.BigEndian.Uint16(portBytes)

	addr := net.JoinHostPort(host, strconv.Itoa(int(port)))
	upstream, err := dial("tcp", addr)
	if err != nil {
		replyErr(0x05) // general failure
		return
	}
	defer upstream.Close()

	// 成功应答,BND.ADDR/PORT 置 0
	if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	proxy(conn, upstream)
}
