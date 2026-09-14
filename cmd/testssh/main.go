// testssh 是一个轻量测试用 SSH 服务端,用于在本机没有 sshd 时对 webssh 做端到端自测。
// 支持密码认证、真实 PTY shell 和 SFTP 子系统。
//
// 用法:go run ./cmd/testssh -addr :2222 -user test -pass test123
// 注意:SFTP 子系统直接访问真实文件系统,测试文件操作请约定在测试目录内进行。
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

func mustEd25519() ed25519.PrivateKey {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	return priv
}

func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true, Setctty: true}
}

func main() {
	addr := flag.String("addr", ":2222", "监听地址")
	userName := flag.String("user", "test", "用户名")
	pass := flag.String("pass", "test123", "密码")
	flag.Parse()

	signer, err := ssh.NewSignerFromKey(mustEd25519())
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("测试 SSH 服务端已启动 %s(用户 %s,主机密钥指纹 %s)\n",
		*addr, *userName, ssh.FingerprintSHA256(signer.PublicKey()))

	config := &ssh.ServerConfig{
		PasswordCallback: func(cm ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if cm.User() == *userName && string(password) == *pass {
				return nil, nil
			}
			return nil, fmt.Errorf("密码错误")
		},
		// 测试服务端接受任意公钥(便于验证私钥认证链路)
		PublicKeyCallback: func(cm ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			return &ssh.Permissions{}, nil
		},
	}
	config.AddHostKey(signer)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Print(err)
			continue
		}
		go handleConn(conn, config)
	}
}

func handleConn(conn net.Conn, config *ssh.ServerConfig) {
	sconn, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		log.Print("握手失败: ", err)
		return
	}
	defer sconn.Close()
	go ssh.DiscardRequests(reqs)
	for newChan := range chans {
		switch newChan.ChannelType() {
		case "session":
			ch, chReqs, err := newChan.Accept()
			if err != nil {
				continue
			}
			go handleSession(ch, chReqs)
		case "direct-tcpip":
			// 支持 -L/-D/跳板机依赖的 TCP 转发通道
			go handleDirectTCP(newChan)
		default:
			newChan.Reject(ssh.UnknownChannelType, "不支持的通道类型")
		}
	}
}

// direct-tcpip payload: string 目标地址, uint32 目标端口, string 来源地址, uint32 来源端口
func handleDirectTCP(newChan ssh.NewChannel) {
	ch, reqs, err := newChan.Accept()
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	defer ch.Close()

	payload := newChan.ExtraData()
	if len(payload) < 4 {
		return
	}
	n := uint32At(payload, 0)
	if int(4+n+6) > len(payload) {
		return
	}
	host := string(payload[4 : 4+n])
	port := uint32At(payload, int(4+n))
	up, err := net.Dial("tcp", net.JoinHostPort(host, strconv.FormatUint(uint64(port), 10)))
	if err != nil {
		log.Printf("direct-tcpip 拨号 %s:%d 失败: %v", host, port, err)
		return
	}
	defer up.Close()

	done := make(chan struct{}, 2)
	go func() { io.Copy(ch, up); ch.Close(); done <- struct{}{} }()
	go func() { io.Copy(up, ch); up.Close(); done <- struct{}{} }()
	<-done
}

func handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()

	var (
		mu       sync.Mutex
		shellCmd *exec.Cmd
		ptmx     *os.File
		shellOn  bool
		ptySeen  bool // 本会话是否已申请 PTY(exec 请求据此决定是否在 PTY 内执行)
	)

	// runPTY 在 PTY 中启动 cmd 并泵送输入输出;与 OpenSSH 一致,exec 请求在
	// 已申请 PTY 的会话中同样于 PTY 内执行。结束时向通道发送退出状态并关闭。
	runPTY := func(cmd *exec.Cmd, termEnv string, ws pty.Winsize, ch ssh.Channel) {
		cmd.Env = append(os.Environ(), "TERM="+termEnv)
		f, tty, err := pty.Open()
		if err != nil {
			log.Print("打开 PTY 失败: ", err)
			return
		}
		_ = pty.Setsize(f, &ws)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
		cmd.SysProcAttr = sysProcAttr()
		if err := cmd.Start(); err != nil {
			log.Print("启动 shell 失败: ", err)
			f.Close()
			tty.Close()
			return
		}
		tty.Close()
		mu.Lock()
		shellCmd, ptmx, shellOn = cmd, f, true
		mu.Unlock()

		go func() { io.Copy(ch, f) }()
		go func() {
			io.Copy(f, ch)
			mu.Lock()
			if shellOn {
				if cmd := shellCmd; cmd != nil && cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
			}
			mu.Unlock()
		}()
		err = cmd.Wait()
		mu.Lock()
		shellOn = false
		f.Close()
		mu.Unlock()
		fmt.Fprintf(ch, "\r\n[进程已退出]\r\n")
		code := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				code = exitErr.ExitCode()
			} else {
				code = 1
			}
		}
		_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{uint32(code)}))
		ch.CloseWrite()
	}

	startShell := func(termEnv string, ws pty.Winsize) {
		shell := "/bin/bash"
		if _, err := os.Stat(shell); err != nil {
			shell = "/bin/sh"
		}
		cmd := exec.Command(shell, "-i")
		runPTY(cmd, termEnv, ws, ch)
	}

	for req := range reqs {
		switch req.Type {
		case "pty-req":
			termEnv, ws := parsePtyReq(req.Payload)
			mu.Lock()
			pendingTerm, pendingWs, ptySeen = termEnv, ws, true
			mu.Unlock()
			req.Reply(true, nil)
		case "shell":
			req.Reply(true, nil)
			mu.Lock()
			term, ws := pendingTerm, pendingWs
			pendingTerm, pendingWs = "xterm-256color", pty.Winsize{Rows: 24, Cols: 80}
			mu.Unlock()
			startShell(term, ws)
		case "window-change":
			if len(req.Payload) >= 16 {
				ws := pty.Winsize{
					Cols: uint16(uint32At(req.Payload, 0)),
					Rows: uint16(uint32At(req.Payload, 4)),
				}
				mu.Lock()
				if ptmx != nil {
					_ = pty.Setsize(ptmx, &ws)
				}
				mu.Unlock()
			}
			req.Reply(true, nil)
		case "subsystem":
			name := parseSubsystemName(req.Payload)
			if name == "sftp" {
				req.Reply(true, nil)
				server, err := sftp.NewServer(ch)
				if err == nil {
					if err := server.Serve(); err != nil {
						log.Print("sftp 结束: ", err)
					}
					server.Close()
				}
			} else {
				req.Reply(false, nil)
			}
		case "env":
			req.Reply(false, nil)
		case "exec":
			// 支持启动命令等 exec 通道:经登录 shell 执行,使重定向/管道/变量生效。
			// 会话已申请 PTY 时(如终端的无感注入路径)在 PTY 内执行,与 OpenSSH 一致。
			cmdLine := parseExecPayload(req.Payload)
			req.Reply(true, nil)
			shell := "/bin/bash"
			if _, err := os.Stat(shell); err != nil {
				shell = "/bin/sh"
			}
			c := exec.Command(shell, "-c", cmdLine)
			mu.Lock()
			term, ws := pendingTerm, pendingWs
			pendingTerm, pendingWs = "", pty.Winsize{Rows: 24, Cols: 80}
			mu.Unlock()
			if ptySeen && term != "" {
				runPTY(c, term, ws, ch)
				return
			}
			c.Stdout = ch
			c.Stderr = ch
			c.Stdin = ch
			err := c.Run()
			// 发送退出状态,满足 SSH 协议(真实 OpenSSH 会自动发送)
			code := 0
			if err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					code = exitErr.ExitCode()
				} else {
					code = 1
				}
			}
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{uint32(code)}))
			ch.Close()
		default:
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}
}

var (
	pendingTerm = "xterm-256color"
	pendingWs   = pty.Winsize{Rows: 24, Cols: 80}
)

func uint32At(b []byte, off int) uint32 {
	return uint32(b[off])<<24 | uint32(b[off+1])<<16 | uint32(b[off+2])<<8 | uint32(b[off+3])
}

// pty-req payload: 4 个 0 + TERM 字符串 + cols/rows/width/height + 模式串
func parsePtyReq(payload []byte) (string, pty.Winsize) {
	if len(payload) < 4 {
		return "xterm-256color", pty.Winsize{Rows: 24, Cols: 80}
	}
	n := uint32At(payload, 0)
	if int(4+n+16) > len(payload) {
		return "xterm-256color", pty.Winsize{Rows: 24, Cols: 80}
	}
	term := string(payload[4 : 4+n])
	off := int(4 + n)
	return term, pty.Winsize{
		Cols: uint16(uint32At(payload, off)),
		Rows: uint16(uint32At(payload, off+4)),
	}
}

// subsystem payload: 4 字节长度 + 名称
func parseSubsystemName(payload []byte) string {
	if len(payload) < 4 {
		return ""
	}
	n := uint32At(payload, 0)
	if int(4+n) > len(payload) {
		return ""
	}
	return string(payload[4 : 4+n])
}

// exec payload: 4 字节长度 + 命令行
func parseExecPayload(payload []byte) string {
	return parseSubsystemName(payload)
}
