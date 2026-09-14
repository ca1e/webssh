package handler

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"webssh/internal/store"
	"webssh/internal/transfer"
)

// ---------- 统一文件传输(本机↔远程、远程↔远程,异步任务) ----------

type endpointReq struct {
	Kind string `json:"kind"` // session | local
	SID  string `json:"sid"`
	Path string `json:"path"`
}

type createTransferReq struct {
	Source    endpointReq `json:"source"`
	Target    endpointReq `json:"target"`
	Recursive bool        `json:"recursive"`
}

func (e *endpointReq) valid() string {
	if e.Kind != "session" && e.Kind != "local" {
		return "端点 kind 必须是 session 或 local"
	}
	if strings.TrimSpace(e.Path) == "" {
		return "路径不能为空"
	}
	if e.Kind == "session" && e.SID == "" {
		return "会话端点缺少 sid"
	}
	return ""
}

// createTransfer 创建传输任务并立即返回快照;进度经 /api/transfers/ws 推送。
func (a *API) createTransfer(c *gin.Context) {
	var req createTransferReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, 400, "参数错误: "+err.Error())
		return
	}
	req.Source.Path = strings.TrimSpace(req.Source.Path)
	req.Target.Path = strings.TrimSpace(req.Target.Path)
	if msg := req.Source.valid(); msg != "" {
		fail(c, 400, "源端点: "+msg)
		return
	}
	if msg := req.Target.valid(); msg != "" {
		fail(c, 400, "目标端点: "+msg)
		return
	}
	if req.Source.Kind == "session" && req.Target.Kind == "session" && req.Source.SID == req.Target.SID {
		fail(c, 400, "源与目标不能是同一会话")
		return
	}
	task, err := a.tr.Create(store.NewID(),
		transfer.Endpoint{Kind: req.Source.Kind, SID: req.Source.SID, Path: req.Source.Path},
		transfer.Endpoint{Kind: req.Target.Kind, SID: req.Target.SID, Path: req.Target.Path},
		req.Recursive)
	if err != nil {
		fail(c, 400, err.Error())
		return
	}
	c.JSON(200, task)
}

func (a *API) listTransfers(c *gin.Context) {
	c.JSON(200, a.tr.List())
}

func (a *API) cancelTransfer(c *gin.Context) {
	if !a.tr.Cancel(c.Param("id")) {
		fail(c, 404, "任务不存在")
		return
	}
	c.JSON(200, gin.H{"ok": true})
}

func (a *API) removeTransfer(c *gin.Context) {
	if !a.tr.Remove(c.Param("id")) {
		fail(c, 404, "任务不存在")
		return
	}
	c.JSON(200, gin.H{"ok": true})
}

// transfersWS 推送任务快照列表:每 200ms 对比一次,有变化才下发。
// 连接断开由 readLoop 感知并关闭桥,主循环随之退出。
func (a *API) transfersWS(c *gin.Context) {
	up := newUpgrader(4096, 4096)
	ws, err := up.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	b := newWSBridge(ws)
	defer b.close()
	go b.readLoop(nil)

	push := func() bool {
		data, _ := json.Marshal(a.tr.List())
		return b.write(websocket.TextMessage, data) == nil
	}
	if !push() {
		return
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	var last string
	for {
		select {
		case <-b.doneChan():
			return
		case <-ticker.C:
			data, _ := json.Marshal(a.tr.List())
			if string(data) == last {
				continue
			}
			if b.write(websocket.TextMessage, data) != nil {
				return
			}
			last = string(data)
		}
	}
}
