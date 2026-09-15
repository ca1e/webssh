package handler

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// ---------- 启动密码 ----------

const tokenCookie = "webssh_token"

func setTokenCookie(c *gin.Context, token string, exp time.Time) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name: tokenCookie, Value: token, Path: "/",
		Expires:  exp,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

func clearTokenCookie(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name: tokenCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

// validUnlock 依据请求 Cookie 判断当前客户端是否已解锁。
func (a *API) validUnlock(c *gin.Context) bool {
	ck, err := c.Cookie(tokenCookie)
	return err == nil && a.auth.ValidToken(ck)
}

// requireUnlocked API 鉴权中间件:放行健康检查、认证接口与前端静态资源;
// 其余一律要求有效解锁 token。WS 升级请求走同一路径,自动纳入。
func (a *API) requireUnlocked(c *gin.Context) {
	p := c.Request.URL.Path
	if !strings.HasPrefix(p, "/api/") ||
		p == "/api/health" ||
		strings.HasPrefix(p, "/api/auth/") {
		c.Next()
		return
	}
	if a.validUnlock(c) {
		c.Next()
		return
	}
	c.AbortWithStatusJSON(401, gin.H{"error": "未解锁"})
}

type authPasswordReq struct {
	Password string `json:"password"`
}

// authStatus 供前端启动时决定渲染锁屏(解锁页/设置密码页)还是功能界面。
func (a *API) authStatus(c *gin.Context) {
	c.JSON(200, gin.H{
		"has_password": a.auth.HasPassword(),
		"locked":       !a.validUnlock(c),
	})
}

// authSetup 首次设置启动密码;已设置后此接口不可再用。
func (a *API) authSetup(c *gin.Context) {
	var req authPasswordReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, 400, "参数错误: "+err.Error())
		return
	}
	if len(req.Password) < 4 {
		fail(c, 400, "密码至少 4 位")
		return
	}
	token, exp, err := a.auth.SetPassword(req.Password)
	if err != nil {
		fail(c, 400, err.Error())
		return
	}
	setTokenCookie(c, token, exp)
	c.JSON(200, gin.H{"ok": true})
}

// authUnlock 校验启动密码并签发一天有效期的解锁 token。
func (a *API) authUnlock(c *gin.Context) {
	var req authPasswordReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, 400, "参数错误: "+err.Error())
		return
	}
	token, exp, err := a.auth.Verify(req.Password)
	if err != nil {
		fail(c, 401, err.Error())
		return
	}
	setTokenCookie(c, token, exp)
	c.JSON(200, gin.H{"ok": true})
}

type changePasswordReq struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// authChangePassword 修改启动密码。/api/auth/* 不经鉴权中间件,此处显式要求已解锁;
// 成功后重新签发 token(旧 token 失效,当前设备凭新 Cookie 保持解锁)。
// 当前密码错误返回 400 而非 401,避免前端误判为锁屏。
func (a *API) authChangePassword(c *gin.Context) {
	if !a.validUnlock(c) {
		c.AbortWithStatusJSON(401, gin.H{"error": "未解锁"})
		return
	}
	var req changePasswordReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, 400, "参数错误: "+err.Error())
		return
	}
	if len(req.NewPassword) < 4 {
		fail(c, 400, "新密码至少 4 位")
		return
	}
	token, exp, err := a.auth.ChangePassword(req.CurrentPassword, req.NewPassword)
	if err != nil {
		fail(c, 400, err.Error())
		return
	}
	setTokenCookie(c, token, exp)
	c.JSON(200, gin.H{"ok": true})
}

func (a *API) authLogout(c *gin.Context) {
	a.auth.Logout()
	clearTokenCookie(c)
	c.JSON(200, gin.H{"ok": true})
}
