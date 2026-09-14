package handler

import (
	"encoding/json"
	"io"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// ---------- 终端 WebSocket ----------

var upgrader = newUpgrader(32*1024, 32*1024)

// 协议:后端→前端为二进制帧(PTY 原始输出,由 xterm.js 解析完整转义协议),
// 或文本帧 {"type":"exit"};前端→后端为文本帧 {"type":"input","data":...}
// 或 {"type":"resize","cols":...,"rows":...}。
func (a *API) terminalWS(c *gin.Context) {
	s, ok := a.mgr.Get(c.Param("id"))
	if !ok {
		fail(c, 404, "会话不存在")
		return
	}
	cols := atoiDefault(c.Query("cols"), 80)
	rows := atoiDefault(c.Query("rows"), 24)
	term, err := s.NewTerminal(cols, rows, s.StartupCmd)
	if err != nil {
		fail(c, 400, err.Error())
		return
	}
	defer term.Close()
	ws, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	b := newWSBridge(ws)
	defer b.close()

	// PTY 输出 → WS:注入了钩子行(FollowSSHPath 启用)时先经 echoStripReader
	// 剥离注入行回显(按精确字节匹配、不命中即透传;watchdog 降级补注后开始
	// 命中),再经 osc7Reader 扫描 OSC 7 序列跟踪远程 shell 目录并回报终端
	// (解除降级看门狗);普通连接无注入,仅保留 OSC 7 扫描。远端 shell 退出
	// 时通知前端
	exitMsg, _ := json.Marshal(map[string]string{"type": "exit"})
	go b.pumpPing(30 * time.Second)
	var out io.Reader = term.Stdout()
	if line := term.InitLine(); line != "" {
		out = newEchoStripReader(out, line)
	}
	b.pumpStdout(&osc7Reader{r: out, s: s, t: term}, func() {
		_ = b.write(websocket.TextMessage, exitMsg)
	})

	// WS → stdin + resize;readLoop 返回即客户端断开或远端退出已关闭桥
	b.readLoop(func(mt int, data []byte) bool {
		if mt != websocket.TextMessage {
			return true
		}
		var msg struct {
			Type string `json:"type"`
			Data string `json:"data"`
			Cols int    `json:"cols"`
			Rows int    `json:"rows"`
		}
		if json.Unmarshal(data, &msg) != nil {
			return true
		}
		switch msg.Type {
		case "input":
			if msg.Data != "" {
				term.Stdin().Write([]byte(msg.Data))
			}
		case "resize":
			if msg.Cols > 0 && msg.Rows > 0 {
				term.Resize(msg.Cols, msg.Rows)
			}
		}
		return true
	})
}

// sessionCwd 返回当前跟踪到的远程 shell 工作目录(供 SFTP 面板单向同步)。
// 目录来源:NewTerminal 注入的提示符钩子输出 OSC 7 序列(仅 bash/zsh),
// 经 terminalWS 的 osc7Reader 扫描更新;无法注入时保持主目录不再跟随。
func (a *API) sessionCwd(c *gin.Context) {
	s, ok := a.mgr.Get(c.Param("id"))
	if !ok {
		fail(c, 404, "会话不存在或已断开")
		return
	}
	if _, err := s.Home(); err != nil {
		fail(c, 400, "获取主目录失败: "+err.Error())
		return
	}
	c.JSON(200, gin.H{"path": s.Cwd()})
}
