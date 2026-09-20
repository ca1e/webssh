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
	Type       string `json:"type"` // local | socks5 | k8s
	ListenPort int    `json:"listen_port"`
	RemoteHost string `json:"remote_host"`
	RemotePort int    `json:"remote_port"`
	// Mode 转发模式(仅 local/socks5):""=auto(探测 sshd,禁止即原始模式)
	// | "std"=标准通道 | "raw"=原始模式(仅 SSH exec 通道,绕过转发禁令)
	Mode string `json:"mode"`
	// type=k8s:命名空间、kind/name 目标与服务端口(数字或端口名)
	Namespace string `json:"namespace"`
	K8sTarget string `json:"k8s_target"`
	K8sPort   string `json:"k8s_port"`
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
	f, err := s.AddForward(req.Type, strings.TrimSpace(req.RemoteHost),
		strings.TrimSpace(req.Namespace), strings.TrimSpace(req.K8sTarget),
		strings.TrimSpace(req.K8sPort), req.ListenPort, req.RemotePort,
		strings.TrimSpace(req.Mode))
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
