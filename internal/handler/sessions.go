package handler

import (
	"github.com/gin-gonic/gin"

	"webssh/internal/store"
)

// ---------- 会话 ----------

type openSessionReq struct {
	HostID      string `json:"host_id"`
	TrustNewKey bool   `json:"trust_new_key"`
}

func (a *API) openSession(c *gin.Context) {
	var req openSessionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, 400, "参数错误: "+err.Error())
		return
	}
	h, ok := a.st.Get(req.HostID)
	if !ok {
		fail(c, 404, "主机不存在")
		return
	}
	var jump *store.Host
	if h.JumpID != "" {
		if h.JumpID == h.ID {
			fail(c, 400, "跳板机不能是主机自身")
			return
		}
		var err error
		jump, err = a.resolveJump(h.JumpID)
		if err != nil {
			fail(c, 400, err.Error())
			return
		}
	}
	// 传入请求 ctx:前端取消等待(关闭连接中的 tab)时中止拨号,不留悬挂连接
	s, err := a.mgr.OpenSession(c.Request.Context(), h, jump, req.TrustNewKey)
	if err != nil {
		fail(c, 400, err.Error())
		return
	}
	resp := gin.H{"session_id": s.ID, "name": h.Name, "host": h.Host, "username": h.Username}
	if jump != nil {
		resp["jump_name"] = jump.Name
	}
	c.JSON(200, resp)
}

type sessionView struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Host     string `json:"host"`
	Username string `json:"username"`
	JumpName string `json:"jump_name,omitempty"`
}

func (a *API) listSessions(c *gin.Context) {
	out := []sessionView{}
	for _, s := range a.mgr.List() {
		v := sessionView{ID: s.ID, Name: s.Name(), Host: s.IP(), Username: s.Username()}
		if s.JumpHost != nil {
			v.JumpName = s.JumpHost.Name
		}
		out = append(out, v)
	}
	c.JSON(200, out)
}

func (a *API) closeSession(c *gin.Context) {
	s, ok := a.mgr.Get(c.Param("id"))
	if !ok {
		fail(c, 404, "会话不存在")
		return
	}
	s.Close()
	c.JSON(200, gin.H{"ok": true})
}
