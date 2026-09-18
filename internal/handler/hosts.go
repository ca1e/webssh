package handler

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"webssh/internal/store"
)

// ---------- 主机管理 ----------

type hostView struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	ConnType      string `json:"conn_type"`
	Username      string `json:"username"`
	AuthType      string `json:"auth_type"`
	KeyPath       string `json:"key_path"`
	JumpID        string `json:"jump_id"`
	Group         string `json:"group"`
	StartupCmd    string `json:"startup_cmd"`
	FollowSSHPath bool   `json:"follow_ssh_path"`
	HasPassword   bool   `json:"has_password"`
	HasPassphrase bool   `json:"has_passphrase"`
}

// defaultPort 返回连接方式的默认端口。
func defaultPort(connType string) int {
	if connType == store.ConnTelnet {
		return 23
	}
	return 22
}

func toView(h store.Host) hostView {
	return hostView{
		ID: h.ID, Name: h.Name, Host: h.Host, Port: orDefault(h.Port, 22),
		ConnType: h.ConnType,
		Username: h.Username, AuthType: h.AuthType, KeyPath: h.KeyPath,
		JumpID: h.JumpID, Group: h.Group, StartupCmd: h.StartupCmd,
		FollowSSHPath: h.FollowSSHPath,
		HasPassword:   h.Password != "", HasPassphrase: h.Passphrase != "",
	}
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

type hostReq struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	ConnType      string `json:"conn_type"`
	Username      string `json:"username"`
	AuthType      string `json:"auth_type"`
	Password      string `json:"password"`
	KeyPath       string `json:"key_path"`
	Passphrase    string `json:"passphrase"`
	JumpID        string `json:"jump_id"`
	Group         string `json:"group"`
	StartupCmd    string `json:"startup_cmd"`
	FollowSSHPath bool   `json:"follow_ssh_path"`
}

func (a *API) listHosts(c *gin.Context) {
	hosts := a.st.List()
	out := make([]hostView, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, toView(h))
	}
	c.JSON(200, out)
}

// exportHosts 直接下载 hosts.json 配置文件原文(凭据为 base64 混淆,非加密)。
func (a *API) exportHosts(c *gin.Context) {
	data, err := a.st.ReadRaw()
	if err != nil {
		fail(c, 400, "读取配置文件失败: "+err.Error())
		return
	}
	c.Header("Content-Disposition", `attachment; filename="hosts.json"`)
	c.Data(200, "application/json; charset=utf-8", data)
}

// importNormalize 按 createHost 同口径校验并规范化导入条目,返回是否有效。
func importNormalize(h store.Host) (store.Host, bool) {
	h.Name = strings.TrimSpace(h.Name)
	h.Host = strings.TrimSpace(h.Host)
	h.Username = strings.TrimSpace(h.Username)
	h.Group = strings.TrimSpace(h.Group)
	ct := store.NormalizeConnType(h.ConnType)
	h.ConnType = ct
	h.Port = orDefault(h.Port, defaultPort(ct))
	if h.Name == "" || h.Host == "" {
		return h, false
	}
	if ct == store.ConnTelnet {
		// telnet 仅保留 名称/主机/端口/分组,SSH 专属字段一律清空(同 createHost)
		h.Username, h.AuthType = "", ""
		h.Password, h.KeyPath, h.Passphrase = "", "", ""
		h.JumpID, h.StartupCmd, h.FollowSSHPath = "", "", false
		return h, true
	}
	if h.Username == "" {
		return h, false
	}
	if h.AuthType != store.AuthPassword && h.AuthType != store.AuthKey {
		return h, false
	}
	if h.AuthType == store.AuthPassword && h.Password == "" {
		return h, false
	}
	if h.AuthType == store.AuthKey && h.KeyPath == "" {
		return h, false
	}
	return h, true
}

// importHosts 按 hosts.json 磁盘格式导入并合并(同名+同主机覆盖,其余保留);
// 文件整体不合法返回 400,单条无效静默跳过。
func (a *API) importHosts(c *gin.Context) {
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	var wires []store.HostWire
	if err := dec.Decode(&wires); err != nil {
		fail(c, 400, "文件格式错误")
		return
	}
	if dec.More() {
		fail(c, 400, "文件格式错误")
		return
	}
	hosts := make([]store.Host, 0, len(wires))
	for _, w := range wires {
		h, err := store.HostFromWire(w)
		if err != nil {
			continue // 凭据非合法 base64,静默跳过该条
		}
		if h, ok := importNormalize(h); ok {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		fail(c, 400, "文件中没有有效主机")
		return
	}
	n, err := a.st.UpsertMany(hosts)
	if err != nil {
		fail(c, 500, err.Error())
		return
	}
	c.JSON(200, gin.H{"ok": true, "count": n})
}

// validateHost 校验公共字段与按连接方式的必填项。
func validateHost(req hostReq) string {
	if strings.TrimSpace(req.Name) == "" {
		return "名称不能为空"
	}
	if strings.TrimSpace(req.Host) == "" {
		return "主机 IP 不能为空"
	}
	if store.NormalizeConnType(req.ConnType) == store.ConnTelnet {
		return "" // telnet 只需 名称/主机/端口
	}
	if strings.TrimSpace(req.Username) == "" {
		return "用户名不能为空"
	}
	if req.AuthType != store.AuthPassword && req.AuthType != store.AuthKey {
		return "认证类型必须是 password 或 key"
	}
	return ""
}

func (a *API) createHost(c *gin.Context) {
	var req hostReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, 400, "参数错误: "+err.Error())
		return
	}
	ct := store.NormalizeConnType(req.ConnType)
	if msg := validateHost(req); msg != "" {
		fail(c, 400, msg)
		return
	}
	// telnet 仅保留 名称/主机/端口/分组,SSH 专属字段一律清空(表单在切到
	// telnet 前预填的默认值如 username=root 不得入库,否则列表会显示 root)
	username, authType := strings.TrimSpace(req.Username), req.AuthType
	password, keyPath, passphrase := req.Password, req.KeyPath, req.Passphrase
	jumpID, startupCmd, follow := req.JumpID, req.StartupCmd, req.FollowSSHPath
	if ct == store.ConnTelnet {
		username, authType = "", ""
		password, keyPath, passphrase = "", "", ""
		jumpID, startupCmd, follow = "", "", false
	}
	if ct == store.ConnSSH {
		if req.AuthType == store.AuthPassword && req.Password == "" {
			fail(c, 400, "密码认证必须填写密码")
			return
		}
		if req.AuthType == store.AuthKey && req.KeyPath == "" {
			fail(c, 400, "私钥认证必须填写私钥路径")
			return
		}
	}
	h, err := a.st.Put(store.Host{
		Name: strings.TrimSpace(req.Name), Host: strings.TrimSpace(req.Host),
		Port: orDefault(req.Port, defaultPort(ct)), ConnType: ct,
		Username: username,
		AuthType: authType, Password: password, KeyPath: keyPath,
		Passphrase: passphrase, JumpID: jumpID,
		Group: strings.TrimSpace(req.Group), StartupCmd: startupCmd,
		FollowSSHPath: follow,
	})
	if err != nil {
		fail(c, 500, err.Error())
		return
	}
	c.JSON(200, toView(h))
}

// update:密码/口令/私钥路径留空表示保留原值。
func (a *API) updateHost(c *gin.Context) {
	id := c.Param("id")
	old, ok := a.st.Get(id)
	if !ok {
		fail(c, 404, "主机不存在")
		return
	}
	var req hostReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, 400, "参数错误: "+err.Error())
		return
	}
	ct := store.NormalizeConnType(req.ConnType)
	if msg := validateHost(req); msg != "" {
		fail(c, 400, msg)
		return
	}
	// telnet 仅保留 名称/主机/端口/分组,SSH 专属字段一律清空
	username, authType := strings.TrimSpace(req.Username), req.AuthType
	password, keyPath, passphrase := pickSecret(req.Password, old.Password),
		pickSecret(req.KeyPath, old.KeyPath), pickSecret(req.Passphrase, old.Passphrase)
	jumpID, startupCmd, follow := req.JumpID, req.StartupCmd, req.FollowSSHPath
	if ct == store.ConnTelnet {
		username, authType = "", ""
		password, keyPath, passphrase = "", "", ""
		jumpID, startupCmd, follow = "", "", false
	}
	if ct == store.ConnSSH {
		if req.AuthType == store.AuthPassword && req.Password == "" && old.Password == "" {
			fail(c, 400, "密码认证必须填写密码")
			return
		}
		if req.AuthType == store.AuthKey && req.KeyPath == "" && old.KeyPath == "" {
			fail(c, 400, "私钥认证必须填写私钥路径")
			return
		}
	}
	h, err := a.st.Put(store.Host{
		ID:   id,
		Name: strings.TrimSpace(req.Name), Host: strings.TrimSpace(req.Host),
		Port: orDefault(req.Port, defaultPort(ct)), ConnType: ct,
		Username:      username,
		AuthType:      authType,
		Password:      password,
		KeyPath:       keyPath,
		Passphrase:    passphrase,
		JumpID:        jumpID,
		Group:         strings.TrimSpace(req.Group),
		StartupCmd:    startupCmd,
		FollowSSHPath: follow,
	})
	if err != nil {
		fail(c, 500, err.Error())
		return
	}
	c.JSON(200, toView(h))
}

func pickSecret(newVal, oldVal string) string {
	if newVal != "" {
		return newVal
	}
	return oldVal
}

// duplicateHost 复制主机为一份新配置(含已保存的密码/私钥口令),名称追加 " - copy"。
func (a *API) duplicateHost(c *gin.Context) {
	old, ok := a.st.Get(c.Param("id"))
	if !ok {
		fail(c, 404, "主机不存在")
		return
	}
	h, err := a.st.Put(store.Host{
		Name:          old.Name + " - copy",
		Host:          old.Host,
		Port:          old.Port,
		ConnType:      old.ConnType,
		Username:      old.Username,
		AuthType:      old.AuthType,
		Password:      old.Password,
		KeyPath:       old.KeyPath,
		Passphrase:    old.Passphrase,
		JumpID:        old.JumpID,
		Group:         old.Group,
		StartupCmd:    old.StartupCmd,
		FollowSSHPath: old.FollowSSHPath,
	})
	if err != nil {
		fail(c, 500, err.Error())
		return
	}
	c.JSON(200, toView(h))
}

func (a *API) deleteHost(c *gin.Context) {
	if !a.st.Delete(c.Param("id")) {
		fail(c, 404, "主机不存在")
		return
	}
	c.JSON(200, gin.H{"ok": true})
}

func (a *API) resolveJump(id string) (*store.Host, error) {
	if id == "" {
		return nil, nil
	}
	j, ok := a.st.Get(id)
	if !ok {
		return nil, fmt.Errorf("跳板机不存在(可能已被删除)")
	}
	return &j, nil
}

// testHost 支持在表单未保存/密码留空(沿用已存密码)的情况下测试。
func (a *API) testHost(c *gin.Context) {
	var req hostReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, 400, "参数错误: "+err.Error())
		return
	}
	ct := store.NormalizeConnType(req.ConnType)
	h := store.Host{
		Name: req.Name, Host: strings.TrimSpace(req.Host), Port: orDefault(req.Port, defaultPort(ct)),
		ConnType: ct,
		Username: strings.TrimSpace(req.Username), AuthType: req.AuthType,
		Password: req.Password, KeyPath: req.KeyPath, Passphrase: req.Passphrase,
		JumpID: req.JumpID, Group: strings.TrimSpace(req.Group), StartupCmd: req.StartupCmd,
	}
	if req.ID != "" && ct == store.ConnSSH {
		if old, ok := a.st.Get(req.ID); ok {
			if h.Password == "" {
				h.Password = old.Password
			}
			if h.KeyPath == "" {
				h.KeyPath = old.KeyPath
			}
			if h.Passphrase == "" {
				h.Passphrase = old.Passphrase
			}
		}
	}
	jump, err := a.resolveJump(h.JumpID)
	if err != nil {
		fail(c, 400, err.Error())
		return
	}
	start := time.Now()
	if err := a.mgr.TestConnection(c.Request.Context(), h, jump); err != nil {
		fail(c, 400, err.Error())
		return
	}
	c.JSON(200, gin.H{"ok": true, "elapsed_ms": time.Since(start).Milliseconds()})
}

func (a *API) listKeys(c *gin.Context) {
	entries, err := os.ReadDir(a.cfg.KeysDir)
	if err != nil {
		fail(c, 400, "读取密钥目录失败: "+err.Error())
		return
	}
	type keyInfo struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	out := []keyInfo{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(a.cfg.KeysDir, e.Name())
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		buf := make([]byte, 64)
		n, _ := f.Read(buf)
		f.Close()
		head := string(buf[:n])
		if strings.Contains(head, "-----BEGIN") && strings.Contains(head, "PRIVATE KEY-----") {
			out = append(out, keyInfo{Name: e.Name(), Path: p})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	c.JSON(200, out)
}
