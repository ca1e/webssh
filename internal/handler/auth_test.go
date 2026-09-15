package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"webssh/internal/auth"
	"webssh/internal/config"
	"webssh/internal/store"
)

// newAuthTestServer 构造仅装配鉴权链路的测试服务(nil 依赖:受保护接口在中间件
// 即被拦截,不会触达 nil store/manager)。
func newAuthTestServer(t *testing.T) *gin.Engine {
	t.Helper()
	authSt, err := auth.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewAPI(&config.Config{}, st, nil, nil, authSt).Register(r)
	return r
}

func doJSON(r *gin.Engine, method, url, cookie string, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, url, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, url, nil)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestAuthFlow(t *testing.T) {
	r := newAuthTestServer(t)

	// 初始状态:未设密码、锁定
	w := doJSON(r, "GET", "/api/auth/status", "", "")
	if w.Code != 200 {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	var st struct {
		HasPassword bool `json:"has_password"`
		Locked      bool `json:"locked"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.HasPassword || !st.Locked {
		t.Fatalf("初始状态错误: %+v", st)
	}

	// 未解锁时受保护接口 401,健康检查放行
	if w = doJSON(r, "GET", "/api/hosts", "", ""); w.Code != 401 {
		t.Fatalf("未解锁访问 /api/hosts = %d, want 401", w.Code)
	}
	if w = doJSON(r, "GET", "/api/health", "", ""); w.Code != 200 {
		t.Fatalf("/api/health = %d, want 200", w.Code)
	}

	// 首次设置密码:200 并下发 token Cookie
	w = doJSON(r, "POST", "/api/auth/setup", "", `{"password":"secret1"}`)
	if w.Code != 200 {
		t.Fatalf("setup = %d, body: %s", w.Code, w.Body.String())
	}
	cookie := w.Header().Get("Set-Cookie")
	if !strings.Contains(cookie, "webssh_token=") {
		t.Fatalf("setup 未下发 token Cookie: %q", cookie)
	}
	token := strings.Split(strings.Split(cookie, ";")[0], "=")[1]

	// 凭 Cookie 访问受保护接口
	if w = doJSON(r, "GET", "/api/hosts", "webssh_token="+token, ""); w.Code != 200 {
		t.Fatalf("解锁后访问 /api/hosts = %d", w.Code)
	}

	// 已设置后 setup 不可再用;错误密码 unlock 拒绝
	if w = doJSON(r, "POST", "/api/auth/setup", "", `{"password":"another"}`); w.Code != 400 {
		t.Fatalf("重复 setup = %d, want 400", w.Code)
	}
	if w = doJSON(r, "POST", "/api/auth/unlock", "", `{"password":"wrong"}`); w.Code != 401 {
		t.Fatalf("错误密码 unlock = %d, want 401", w.Code)
	}

	// 正确密码 unlock 签发新 token,旧 token 立即失效
	w = doJSON(r, "POST", "/api/auth/unlock", "", `{"password":"secret1"}`)
	if w.Code != 200 {
		t.Fatalf("unlock = %d, body: %s", w.Code, w.Body.String())
	}
	cookie2 := w.Header().Get("Set-Cookie")
	token2 := strings.Split(strings.Split(cookie2, ";")[0], "=")[1]
	if token2 == token {
		t.Fatal("unlock 应回转新 token")
	}
	if w = doJSON(r, "GET", "/api/hosts", "webssh_token="+token, ""); w.Code != 401 {
		t.Fatalf("旧 token = %d, want 401", w.Code)
	}

	// 修改密码:未解锁被拒;当前密码错误 / 新密码过短返回 400
	if w = doJSON(r, "POST", "/api/auth/change", "", `{"current_password":"secret1","new_password":"newpw"}`); w.Code != 401 {
		t.Fatalf("未解锁 change = %d, want 401", w.Code)
	}
	if w = doJSON(r, "POST", "/api/auth/change", "webssh_token="+token2, `{"current_password":"wrong","new_password":"newpw"}`); w.Code != 400 {
		t.Fatalf("错误当前密码 change = %d, want 400", w.Code)
	}
	if w = doJSON(r, "POST", "/api/auth/change", "webssh_token="+token2, `{"current_password":"secret1","new_password":"ab"}`); w.Code != 400 {
		t.Fatalf("过短新密码 change = %d, want 400", w.Code)
	}
	// 成功改密:下发新 Cookie,旧 token 失效,新 token 可访问受保护接口
	w = doJSON(r, "POST", "/api/auth/change", "webssh_token="+token2, `{"current_password":"secret1","new_password":"newpw"}`)
	if w.Code != 200 {
		t.Fatalf("change = %d, body: %s", w.Code, w.Body.String())
	}
	cookie3 := w.Header().Get("Set-Cookie")
	token3 := strings.Split(strings.Split(cookie3, ";")[0], "=")[1]
	if token3 == token2 {
		t.Fatal("change 后应回转新 token")
	}
	if w = doJSON(r, "GET", "/api/hosts", "webssh_token="+token2, ""); w.Code != 401 {
		t.Fatalf("改密后旧 token = %d, want 401", w.Code)
	}
	if w = doJSON(r, "GET", "/api/hosts", "webssh_token="+token3, ""); w.Code != 200 {
		t.Fatalf("改密后新 token = %d, want 200", w.Code)
	}
	// 旧密码失效,新密码可解锁
	if w = doJSON(r, "POST", "/api/auth/unlock", "", `{"password":"secret1"}`); w.Code != 401 {
		t.Fatalf("旧密码 unlock = %d, want 401", w.Code)
	}
	if w = doJSON(r, "POST", "/api/auth/unlock", "", `{"password":"newpw"}`); w.Code != 200 {
		t.Fatalf("新密码 unlock = %d, want 200", w.Code)
	}

	// logout 注销后 token 失效
	if w = doJSON(r, "POST", "/api/auth/logout", "webssh_token="+token3, ""); w.Code != 200 {
		t.Fatalf("logout = %d", w.Code)
	}
	if w = doJSON(r, "GET", "/api/hosts", "webssh_token="+token3, ""); w.Code != 401 {
		t.Fatalf("logout 后访问 = %d, want 401", w.Code)
	}
}
