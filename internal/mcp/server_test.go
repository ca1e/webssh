package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"webssh/internal/auth"
	"webssh/internal/secrets"
	"webssh/internal/sshmgr"
	"webssh/internal/store"
)

// testSecret 凭据明文:快照测试断言任何工具输出/错误都不含它。
const testSecret = "sup3r-secret-pw-明文"

// newFixture 构造装配完整的测试 Server:
// 凭据库 + 一台密码主机(引用)+ 一台 telnet 主机 + 默认策略。
func newFixture(t *testing.T, mcpJSON string) *Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	sec, err := secrets.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := sec.Put("", secrets.KindPassword, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir, sec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(store.Host{
		Name: "box1", Host: "203.0.113.10", Port: 22,
		Username: "root", AuthType: store.AuthPassword, PasswordRef: ref,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(store.Host{
		Name: "con1", Host: "203.0.113.20", ConnType: store.ConnTelnet,
	}); err != nil {
		t.Fatal(err)
	}
	if mcpJSON != "" {
		writeFile(t, dir, "mcp.json", mcpJSON)
	}
	mgr := sshmgr.New(dir, 8, 0)
	mgr.SetSecretResolver(sec.Reveal)
	authSt, err := auth.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(dir, st, mgr, authSt)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// ---------- bearer 中间件 / 路由 ----------

func mcpPost(r *gin.Engine, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	// Streamable HTTP 要求同时接受 JSON 与 SSE
	req.Header.Set("Accept", "application/json, text/event-stream")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`

// 未配置口令:/mcp 一律 404(fail-closed,不返回 401、不暴露服务存在性)。
func TestBearerMissingTokenConfig404(t *testing.T) {
	s := newFixture(t, "")
	r := gin.New()
	s.Register(r)
	for _, env := range []string{"", "false"} {
		t.Setenv("WEBSSH_MCP_TOKEN", "")
		t.Setenv("WEBSSH_MCP_ENABLED", env)
		w := mcpPost(r, initBody, nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("未配置口令应 404,得到 %d(env=%q)", w.Code, env)
		}
	}
}

func TestBearerWrongAndMissingToken401(t *testing.T) {
	s := newFixture(t, `{"token":"right-pass"}`)
	r := gin.New()
	s.Register(r)

	// 缺失
	w := mcpPost(r, initBody, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("无 token 应 401,得到 %d", w.Code)
	}
	// 错误
	w = mcpPost(r, initBody, map[string]string{"Authorization": "Bearer wrong"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("错 token 应 401,得到 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "令牌无效或缺失") || !strings.Contains(w.Body.String(), "Bearer") {
		t.Fatalf("401 文案不符: %q", w.Body.String())
	}
	// 正确 → 放行到 MCP 协议层(JSON-RPC 响应)
	w = mcpPost(r, initBody, map[string]string{"Authorization": "Bearer right-pass"})
	if w.Code != http.StatusOK {
		t.Fatalf("正确 token 应 200,得到 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "serverInfo") {
		t.Fatalf("initialize 响应异常: %s", w.Body.String())
	}
	// Bearer 前缀大小写敏感
	w = mcpPost(r, initBody, map[string]string{"Authorization": "bearer right-pass"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("Bearer 前缀大小写错误应 401,得到 %d", w.Code)
	}
}

// ---------- InMemory 协议层:tools/list 与 deny 路径 ----------

// secretPattern 凭据零暴露断言模式:任何工具输出/错误不得命中。
var secretPattern = regexp.MustCompile(`(?i)password|passphrase|BEGIN[ -][A-Z ]*PRIVATE KEY|` + regexp.QuoteMeta(testSecret))

// callTool 用 InMemory transports 调用工具,返回工具文本与结构化输出。
func callTool(t *testing.T, s *Server, name string, args map[string]any) (string, any, bool) {
	t.Helper()
	serverConn, clientConn := mcp.NewInMemoryTransports()
	sc, err := s.currentServer().Connect(context.Background(), serverConn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Wait()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), clientConn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s) 协议错误: %v", name, err)
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	var structured any
	if res.StructuredContent != nil {
		blob, _ := json.Marshal(res.StructuredContent)
		_ = json.Unmarshal(blob, &structured)
	}
	return text.String(), structured, res.IsError
}

func TestToolsListFullSurface(t *testing.T) {
	s := newFixture(t, "")
	serverConn, clientConn := mcp.NewInMemoryTransports()
	sc, err := s.currentServer().Connect(context.Background(), serverConn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Wait()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), clientConn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tl := range res.Tools {
		got[tl.Name] = true
	}
	if len(got) != len(allToolNames) {
		t.Fatalf("工具数 = %d, want %d: %v", len(got), len(allToolNames), got)
	}
	for _, w := range allToolNames {
		if !got[w] {
			t.Fatalf("缺少工具 %s;实际: %v", w, got)
		}
	}
}

// TestDenyPaths deny 双路径:拒绝文案必须"发生了什么+原因+下一步"且无堆栈、
// 无凭据模式;返回必须是 IsError 文本而非协议错误。
func TestDenyPaths(t *testing.T) {
	s := newFixture(t, "")
	t.Setenv("WEBSSH_MCP_TOKEN", "x") // 不影响 InMemory 直连

	denies := []struct {
		name   string
		tool   string
		args   map[string]any
		wantIn []string
	}{
		{"未知主机", "ssh_host_info", map[string]any{"host": "ghost"}, []string{"不存在", "ssh_list_hosts"}},
		{"telnet 主机", "ssh_exec", map[string]any{"host": "con1", "command": "uptime"}, []string{"仅 SSH 主机支持"}},
		{"空命令", "ssh_exec", map[string]any{"host": "box1", "command": "  "}, []string{"缺少 command"}},
		{"相对路径", "ssh_download_file", map[string]any{"host": "box1", "path": "relative/file"}, []string{"绝对路径"}},
		{"新建缺凭据", "ssh_create_host", map[string]any{"name": "n", "host": "1.2.3.4", "username": "root", "auth_type": "password"}, []string{"口令认证必须提交"}},
		{"新建缺私钥", "ssh_create_host", map[string]any{"name": "n", "host": "1.2.3.4", "username": "root", "auth_type": "key"}, []string{"key_path"}},
		{"新建坏认证", "ssh_create_host", map[string]any{"name": "n", "host": "1.2.3.4", "username": "root", "auth_type": "none"}, []string{"auth_type 非法"}},
		{"删除未确认", "ssh_delete_host", map[string]any{"host": "box1"}, []string{"confirm=true"}},
		{"复制未知主机", "ssh_duplicate_host", map[string]any{"host": "ghost"}, []string{"不存在"}},
	}
	for _, c := range denies {
		text, _, isErr := callTool(t, s, c.tool, c.args)
		if !isErr {
			t.Fatalf("%s: 应返回工具错误", c.name)
		}
		for _, want := range c.wantIn {
			if !strings.Contains(text, want) {
				t.Fatalf("%s: 文案缺 %q: %q", c.name, want, text)
			}
		}
		if strings.Contains(text, "goroutine") || strings.Contains(text, ".go:") {
			t.Fatalf("%s: 文案含堆栈: %q", c.name, text)
		}
		if secretPattern.MatchString(text) {
			t.Fatalf("%s: 文案命中凭据模式: %q", c.name, text)
		}
	}
}

// TestHostsToolsSnapshotZeroSecrets hosts 域 happy 路径:输出不含凭据词形。
func TestHostsToolsSnapshotZeroSecrets(t *testing.T) {
	s := newFixture(t, "")
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"ssh_list_hosts", map[string]any{}},
		{"ssh_host_info", map[string]any{"host": "box1"}},
	} {
		_, structured, isErr := callTool(t, s, c.tool, c.args)
		if isErr {
			t.Fatalf("%s 不应报错", c.tool)
		}
		blob, _ := json.Marshal(structured)
		if secretPattern.MatchString(string(blob)) {
			t.Fatalf("%s 输出命中凭据模式: %s", c.tool, blob)
		}
	}
}

// TestHostWriteLifecycle create → duplicate → delete 全链路(单元,无真连接):
// 验证脱敏投影、凭据只进不出(提交的明文不回显、跳板引用解析、下游引用拒删)。
func TestHostWriteLifecycle(t *testing.T) {
	s := newFixture(t, "")

	// create(口令认证):提交的明文经同一 secretPattern 断言不得回显。
	text, out, isErr := callTool(t, s, "ssh_create_host", map[string]any{
		"name": "newbox", "host": "203.0.113.99", "username": "ops",
		"auth_type": "password", "password": testSecret, "group": "g1",
	})
	if isErr {
		t.Fatalf("create 失败: %s", text)
	}
	blob, _ := json.Marshal(out)
	if secretPattern.MatchString(string(blob)) {
		t.Fatalf("create 输出命中凭据模式: %s", blob)
	}
	var created struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Auth      string `json:"auth"`
		HasSecret bool   `json:"has_secret"`
	}
	if err := json.Unmarshal(blob, &created); err != nil || created.ID == "" ||
		created.Name != "newbox" || created.Auth != "secret" || !created.HasSecret {
		t.Fatalf("create 投影不符: %s err=%v", blob, err)
	}

	// create(key 认证 + jump 名称解析为 ID)。
	text, out, isErr = callTool(t, s, "ssh_create_host", map[string]any{
		"name": "jumpbox", "host": "203.0.113.88", "username": "ops",
		"auth_type": "key", "key_path": "~/.ssh/id_ed25519", "jump": "newbox",
	})
	if isErr {
		t.Fatalf("create(jump) 失败: %s", text)
	}
	blob, _ = json.Marshal(out)
	if !strings.Contains(string(blob), `"jump_id":"`+created.ID+`"`) {
		t.Fatalf("jump 名称未解析为 ID: %s", blob)
	}

	// duplicate:默认新名 + 凭据引用共享(脱敏投影同为 secret 型)。
	text, out, isErr = callTool(t, s, "ssh_duplicate_host", map[string]any{"host": "newbox"})
	if isErr {
		t.Fatalf("duplicate 失败: %s", text)
	}
	blob, _ = json.Marshal(out)
	if !strings.Contains(string(blob), `"name":"newbox - copy"`) {
		t.Fatalf("duplicate 缺省名不符: %s", blob)
	}
	var dup struct {
		ID   string `json:"id"`
		Auth string `json:"auth"`
	}
	_ = json.Unmarshal(blob, &dup)
	if dup.ID == created.ID || dup.Auth != "secret" {
		t.Fatalf("duplicate 应为新 ID 且共享凭据形态: %s", blob)
	}
	// 源主机仍被副本引用? 副本无 jump,不受影响;delete 源 newbox 被 jumpbox 引用 → 拒删。
	text, _, isErr = callTool(t, s, "ssh_delete_host", map[string]any{"host": "newbox", "confirm": true})
	if !isErr || !strings.Contains(text, "跳板机") {
		t.Fatalf("被跳板引用的主机应拒删: %q isErr=%v", text, isErr)
	}

	// delete:先删 jumpbox,再删 newbox;删后 list 不再可见。
	for _, name := range []string{"jumpbox", "newbox", "newbox - copy"} {
		text, _, isErr = callTool(t, s, "ssh_delete_host", map[string]any{"host": name, "confirm": true})
		if isErr {
			t.Fatalf("delete %s 失败: %s", name, text)
		}
	}
	text, out, isErr = callTool(t, s, "ssh_list_hosts", nil)
	if isErr {
		t.Fatalf("list 失败: %s", text)
	}
	blob, _ = json.Marshal(out)
	for _, gone := range []string{"newbox", "jumpbox"} {
		if strings.Contains(string(blob), `"`+gone+`"`) {
			t.Fatalf("删除后仍在列表: %s -> %s", gone, blob)
		}
	}
}

// TestWriteToolsAutoVisible 可见主机开关开启时:create/duplicate 产出的新主机
// 自动加入名单并保持既有成员;开关关闭时为无操作(不影响 mcp.json)。
func TestWriteToolsAutoVisible(t *testing.T) {
	s := newFixture(t, `{"visible_hosts":{"enabled":true,"ids":["keep-me"]}}`)
	// box1 不在名单内:先验证基线不可见,再走 create→duplicate。
	if _, structured, isErr := callTool(t, s, "ssh_list_hosts", nil); isErr {
		t.Fatalf("list 失败: %v", structured)
	} else {
		blob, _ := json.Marshal(structured)
		if strings.Contains(string(blob), "box1") {
			t.Fatalf("开关开启时 box1 不应可见: %s", blob)
		}
	}
	_, out, isErr := callTool(t, s, "ssh_create_host", map[string]any{
		"name": "auto1", "host": "203.0.113.55", "username": "ops",
		"auth_type": "key", "key_path": "/srv/keys/id_auto",
	})
	if isErr {
		t.Fatalf("create 失败: %v", out)
	}
	blob, _ := json.Marshal(out)
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(blob, &created)
	if created.ID == "" {
		t.Fatalf("create 无 ID: %s", blob)
	}
	if _, out, isErr = callTool(t, s, "ssh_host_info", map[string]any{"host": "auto1"}); isErr {
		t.Fatalf("新主机应立即可见: %v", out)
	}
	if _, out, isErr = callTool(t, s, "ssh_duplicate_host", map[string]any{"host": "auto1"}); isErr {
		t.Fatalf("duplicate 失败: %v", out)
	}
	// 名单:keep-me 保留,auto1 与副本均在。
	set := s.Settings()
	if !set.VisibleHosts.Enabled {
		t.Fatal("开关被意外关闭")
	}
	for _, want := range []string{"keep-me", created.ID} {
		found := false
		for _, id := range set.VisibleHosts.IDs {
			if id == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("名单缺 %s: %v", want, set.VisibleHosts.IDs)
		}
	}
	if len(set.VisibleHosts.IDs) != 3 {
		t.Fatalf("名单应恰为 keep-me + auto1 + 副本: %v", set.VisibleHosts.IDs)
	}
}

// TestReloadKeepsOldOnFailure SIGHUP 语义:坏配置重载失败保持旧策略。
func TestReloadKeepsOldOnFailure(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "mcp.json", `{"defaults":{"exec_timeout":"20s"}}`)
	sec, err := secrets.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir, sec)
	if err != nil {
		t.Fatal(err)
	}
	mgr := sshmgr.New(dir, 8, 0)
	mgr.SetSecretResolver(sec.Reveal)
	authSt, err := auth.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(dir, st, mgr, authSt)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.conf.Defaults.ExecTimeout; got != 20*time.Second {
		t.Fatalf("初始 exec_timeout = %v", got)
	}
	writeFile(t, dir, "mcp.json", `{"defaults":{"exec_timeout":"broken"}}`) // 非法时长
	if err := s.Reload(); err == nil {
		t.Fatal("非法配置应重载失败")
	}
	if got := s.conf.Defaults.ExecTimeout; got != 20*time.Second {
		t.Fatalf("重载失败后应保持旧策略,得到 %v", got)
	}
	// 合法新配置可热切换
	writeFile(t, dir, "mcp.json", `{"defaults":{"exec_timeout":"30s"}}`)
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := s.conf.Defaults.ExecTimeout; got != 30*time.Second {
		t.Fatalf("热重载未生效: %v", got)
	}
}

// envToken 优先于 mcp.json 的合成顺序(env 在 New 构造时固定)。
func TestEnvTokenPriority(t *testing.T) {
	t.Setenv("WEBSSH_MCP_TOKEN", "env-token")
	s := newFixture(t, `{"token":"file-token"}`)
	if s.tokenValue() != "env-token" {
		t.Fatalf("env 应优先: %q", s.tokenValue())
	}
	// 无 env 时回落 mcp.json
	t.Setenv("WEBSSH_MCP_TOKEN", "")
	s2 := newFixture(t, `{"token":"file-token"}`)
	if s2.tokenValue() != "file-token" {
		t.Fatalf("应取 mcp.json token: %q", s2.tokenValue())
	}
}

// TestLockscreenPasswordAuth 未显式配置 token 时,MCP Bearer 回落为锁屏密码:
// 正确密码放行、错误密码 401;env token 优先级高于锁屏密码。
func TestLockscreenPasswordAuth(t *testing.T) {
	dir := t.TempDir()
	sec, err := secrets.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := sec.Put("", secrets.KindPassword, testSecret)
	st, err := store.Open(dir, sec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(store.Host{Name: "box1", Host: "203.0.113.10", Username: "root", AuthType: store.AuthPassword, PasswordRef: ref}); err != nil {
		t.Fatal(err)
	}
	authSt, err := auth.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := authSt.SetPassword("lock-pass-42"); err != nil {
		t.Fatal(err)
	}
	mgr := sshmgr.New(dir, 8, 0)
	mgr.SetSecretResolver(sec.Reveal)
	s, err := New(dir, st, mgr, authSt)
	if err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	s.Register(r)

	// 尚未配置任何显式 token:锁屏密码即 MCP 口令
	if s.tokenSourceKind() != tokenLockscreen {
		t.Fatalf("应回落锁屏密码,得到 %v", s.tokenSourceKind())
	}
	w := mcpPost(r, initBody, map[string]string{"Authorization": "Bearer lock-pass-42"})
	if w.Code != http.StatusOK {
		t.Fatalf("锁屏密码应放行,得到 %d: %s", w.Code, w.Body.String())
	}
	w = mcpPost(r, initBody, map[string]string{"Authorization": "Bearer wrong"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("错误密码应 401,得到 %d", w.Code)
	}

	// env token 存在时优先于锁屏密码(env 在构造时固定,单独构造)
	t.Setenv("WEBSSH_MCP_TOKEN", "env-pass")
	s2, err := New(dir, st, mgr, authSt)
	if err != nil {
		t.Fatal(err)
	}
	if s2.tokenSourceKind() != tokenShared {
		t.Fatalf("env token 应优先于锁屏密码")
	}
	t.Setenv("WEBSSH_MCP_TOKEN", "")

	// 改锁屏密码后,MCP 认证立即跟随新密码(逐请求校验 auth store)
	if _, _, err := authSt.ChangePassword("lock-pass-42", "new-pass-99"); err != nil {
		t.Fatal(err)
	}
	w = mcpPost(r, initBody, map[string]string{"Authorization": "Bearer lock-pass-42"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("旧锁屏密码应失效,得到 %d", w.Code)
	}
	w = mcpPost(r, initBody, map[string]string{"Authorization": "Bearer new-pass-99"})
	if w.Code != http.StatusOK {
		t.Fatalf("新锁屏密码应放行,得到 %d", w.Code)
	}
}

// TestUpdateSettingsRoundtrip 设置持久化 + 热生效 + guard 状态一致。
func TestUpdateSettingsRoundtrip(t *testing.T) {
	s := newFixture(t, `{"token":"tok-keep"}`)
	if s.guard.Enabled() {
		t.Fatal("默认白名单应关闭")
	}
	set := Settings{PathWhitelist: PathWhitelistCfg{Enabled: true, Paths: []string{"/var/log/"}}}
	if err := s.UpdateSettings(set); err != nil {
		t.Fatal(err)
	}
	got := s.Settings()
	if !got.PathWhitelist.Enabled || len(got.PathWhitelist.Paths) != 1 {
		t.Fatalf("热生效失败: %+v", got)
	}
	if !s.guard.Enabled() || s.guard.Prefixes()[0] != "/var/log/" {
		t.Fatalf("guard 未随重载更新: %v", s.guard.Prefixes())
	}
	// 持久化落盘且保留未知字段(token)
	data, err := os.ReadFile(filepath.Join(s.cfgDir, "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["token"]; !ok {
		t.Fatal("既有字段被丢弃")
	}
}

// TestVisibleHosts 可见主机开关:未勾选主机对 list/info 完全不可见。
func TestVisibleHosts(t *testing.T) {
	s := newFixture(t, "")
	var ids []string
	for _, h := range s.st.List() {
		if h.Name == "box1" {
			ids = append(ids, h.ID)
		}
	}
	if len(ids) != 1 {
		t.Fatalf("应恰好找到 box1")
	}
	set := s.Settings()
	set.VisibleHosts = VisibleHostsCfg{Enabled: true, IDs: ids}
	if err := s.UpdateSettings(set); err != nil {
		t.Fatal(err)
	}
	// 列表:只剩 box1
	text, structured, isErr := callTool(t, s, "ssh_list_hosts", nil)
	if isErr {
		t.Fatalf("list_hosts 不应报错: %s", text)
	}
	blob, _ := json.Marshal(structured)
	if strings.Contains(string(blob), "con1") || !strings.Contains(string(blob), "box1") {
		t.Fatalf("列表过滤失败: %s", blob)
	}
	// 不可见主机:按不存在拒绝(不泄露存在性),telnet/名称两种路径都拦
	for _, host := range []string{"con1"} {
		text, _, isErr = callTool(t, s, "ssh_host_info", map[string]any{"host": host})
		if !isErr || !strings.Contains(text, "不存在") {
			t.Fatalf("不可见主机 %s 应报不存在: %q isErr=%v", host, text, isErr)
		}
	}
	// 关闭开关后恢复(直接验证可见性判定,绕开 telnet 拒绝)
	set.VisibleHosts.Enabled = false
	if err := s.UpdateSettings(set); err != nil {
		t.Fatal(err)
	}
	for _, h := range s.st.List() {
		if !s.hostVisible(h) {
			t.Fatalf("开关关闭后 %s 应恢复可见", h.Name)
		}
	}
}

// TestToolFilter 工具过滤:开启后仅注册勾选的工具。
func TestToolFilter(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "mcp.json", `{"tool_filter":{"tools":["ssh_list_hosts","ssh_exec"]}}`)
	s := newFixtureFile(t, dir)
	res := listToolNames(t, s)
	if len(res) != 2 || !res["ssh_list_hosts"] || !res["ssh_exec"] {
		t.Fatalf("工具过滤未生效: %v", res)
	}
}

// newFixtureFile 在指定目录装配(与 newFixture 相同主机集,不写额外 mcp.json)。
func newFixtureFile(t *testing.T, dir string) *Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	sec, err := secrets.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := sec.Put("", secrets.KindPassword, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir, sec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(store.Host{Name: "box1", Host: "203.0.113.10", Port: 22,
		Username: "root", AuthType: store.AuthPassword, PasswordRef: ref}); err != nil {
		t.Fatal(err)
	}
	mgr := sshmgr.New(dir, 8, 0)
	mgr.SetSecretResolver(sec.Reveal)
	authSt, err := auth.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(dir, st, mgr, authSt)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func listToolNames(t *testing.T, s *Server) map[string]bool {
	t.Helper()
	serverConn, clientConn := mcp.NewInMemoryTransports()
	sc, err := s.currentServer().Connect(context.Background(), serverConn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Wait()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), clientConn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tl := range res.Tools {
		got[tl.Name] = true
	}
	return got
}
