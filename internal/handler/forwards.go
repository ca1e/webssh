package handler

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// ---------- 端口转发 / SOCKS5 ----------

func (a *API) listForwards(c *gin.Context) {
	s, ok := a.mgr.Get(c.Param("id"))
	if !ok {
		fail(c, 404, "会话不存在或已断开")
		return
	}
	c.JSON(200, s.ListForwards())
}

type forwardReq struct {
	Type       string `json:"type"` // local | socks5
	ListenPort int    `json:"listen_port"`
	RemoteHost string `json:"remote_host"`
	RemotePort int    `json:"remote_port"`
}

func (a *API) addForward(c *gin.Context) {
	s, ok := a.mgr.Get(c.Param("id"))
	if !ok {
		fail(c, 404, "会话不存在或已断开")
		return
	}
	var req forwardReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, 400, "参数错误: "+err.Error())
		return
	}
	f, err := s.AddForward(req.Type, strings.TrimSpace(req.RemoteHost), req.ListenPort, req.RemotePort)
	if err != nil {
		fail(c, 400, err.Error())
		return
	}
	c.JSON(200, f)
}

func (a *API) delForward(c *gin.Context) {
	s, ok := a.mgr.Get(c.Param("id"))
	if !ok {
		fail(c, 404, "会话不存在或已断开")
		return
	}
	if !s.RemoveForward(c.Param("fid")) {
		fail(c, 404, "转发不存在")
		return
	}
	c.JSON(200, gin.H{"ok": true})
}
