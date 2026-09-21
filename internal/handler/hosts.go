package handler

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"webssh/internal/store"
)

// ---------- 主机管理 ----------

// hostView 即 store.HostPublic:统一脱敏投影,凭据只回
// Has* 标识。
type hostView = store.HostPublic

// defaultPort 返回连接方式的默认端口。
func defaultPort(connType string) int {
	if connType == store.ConnTelnet {
		return 23
	}
	return 22
}

func toView(h store.Host) hostView {
	return store.PublicHost(h)
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

// exportReq 导出请求;模式只由前端按钮决定(encrypted/plaintext),
// 不以口令框留空推断——留空/过短在加密模式直接校验报错。
type exportReq struct {
	Mode     string `json:"mode"`
	Password string `json:"password"`
}

// exportHosts 生成 webssh-bundle-v1 导出文件。加密模式(默认推荐)用导出
// 口令经 scrypt 派生密钥逐条加密;无密码模式仅由独立按钮显式触发,整体
// 明文(凭据 base64),文件名 hosts-plaintext.json 并留痕日志。
func (a *API) exportHosts(c *gin.Context) {
	var req exportReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, 400, "参数错误: "+err.Error())
		return
	}
	wires, err := a.st.ExportWires()
	if err != nil {
		fail(c, 500, "导出失败: "+err.Error())
		return
	}
	var data []byte
	filename := "hosts-bundle.json"
	switch req.Mode {
	case "", "encrypted":
		if len(req.Password) < 4 {
			fail(c, 400, "导出口令至少 4 位")
			return
		}
		if data, err = store.EncodeBundleEncrypted(wires, req.Password); err != nil {
			fail(c, 500, "导出失败: "+err.Error())
			return
		}
	case "plaintext":
		if data, err = store.EncodeBundlePlaintext(wires); err != nil {
			fail(c, 500, "导出失败: "+err.Error())
			return
		}
		filename = "hosts-plaintext.json"
	default:
		fail(c, 400, "未知导出模式")
		return
	}
	log.Printf("主机导出: 模式=%s 条数=%d", req.Mode, len(wires))
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
	c.Data(200, "application/json; charset=utf-8", data)
}

// importNormalize 按 createHost 同口径校验并规范化导入条目,返回是否有效。
// "password 认证且最终无任何凭据来源 → 无效":调用前须先完成引用保留合并。
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
		h.PasswordRef, h.KeyPath, h.PassphraseRef = "", "", ""
		h.JumpID, h.StartupCmd, h.FollowSSHPath = "", "", false
		return h, true
	}
	if h.Username == "" {
		return h, false
	}
	if h.AuthType != store.AuthPassword && h.AuthType != store.AuthKey {
		return h, false
	}
	if h.AuthType == store.AuthPassword && h.PasswordRef == "" {
		return h, false
	}
	if h.AuthType == store.AuthKey && h.KeyPath == "" {
		return h, false
	}
	return h, true
}

// importHosts 接收 webssh-bundle-v1 bundle(multipart: file + 可选 password)
// 并合并(同名+同主机覆盖、保留原 ID 与原凭据引用);非 bundle 格式/未知
// 格式 400;单条无效静默跳过,整体口令错误/损坏 400。
func (a *API) importHosts(c *gin.Context) {
	fh, err := c.FormFile("file")
	if err != nil {
		fail(c, 400, "缺少上传文件(表单字段 file)")
		return
	}
	f, err := fh.Open()
	if err != nil {
		fail(c, 400, "读取上传文件失败")
		return
	}
	defer f.Close()
	// bundle 为纯配置数据,64MiB 上限防误传巨型文件拖垮内存
	data, err := io.ReadAll(io.LimitReader(f, 64<<20))
	if err != nil {
		fail(c, 400, "读取上传文件失败")
		return
	}
	wires, err := store.DecodeBundle(data, c.PostForm("password"))
	if err != nil {
		fail(c, 400, store.FriendlyBundleError(err))
		return
	}
	hosts := make([]store.Host, 0, len(wires))
	for _, w := range wires {
		h, err := a.st.ImportWire(w)
		if err != nil {
			continue // 凭据非合法 base64,静默跳过该条
		}
		// 命中现有主机且 bundle 条目无凭据 → 保留原引用,使"最终无任何凭据
		// 来源"的判定基于合并后的真实状态(对齐 pickSecret 留空=保留原值)
		if h.PasswordRef == "" || h.PassphraseRef == "" {
			if old, ok := a.st.FindByNameHost(h.Name, h.Host); ok {
				if h.PasswordRef == "" {
					h.PasswordRef = old.PasswordRef
				}
				if h.PassphraseRef == "" {
					h.PassphraseRef = old.PassphraseRef
				}
			}
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

// createHost 表单明文凭据 → secrets 库换引用后入库;引用不落 hosts.json。
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
	passwordRef, keyPath, passphraseRef := "", req.KeyPath, ""
	jumpID, startupCmd, follow := req.JumpID, req.StartupCmd, req.FollowSSHPath
	if ct == store.ConnTelnet {
		username, authType = "", ""
		keyPath = ""
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
		if req.Password != "" {
			ref, err := a.st.PutPassword("", req.Password)
			if err != nil {
				fail(c, 500, err.Error())
				return
			}
			passwordRef = ref
		}
		if req.Passphrase != "" {
			ref, err := a.st.PutPassphrase("", req.Passphrase)
			if err != nil {
				fail(c, 500, err.Error())
				return
			}
			passphraseRef = ref
		}
	}
	h, err := a.st.Put(store.Host{
		Name: strings.TrimSpace(req.Name), Host: strings.TrimSpace(req.Host),
		Port: orDefault(req.Port, defaultPort(ct)), ConnType: ct,
		Username: username,
		AuthType: authType, PasswordRef: passwordRef, KeyPath: keyPath,
		PassphraseRef: passphraseRef, JumpID: jumpID,
		Group: strings.TrimSpace(req.Group), StartupCmd: startupCmd,
		FollowSSHPath: follow,
	})
	if err != nil {
		fail(c, 500, err.Error())
		return
	}
	c.JSON(200, toView(h))
}

// updateHost:密码/口令留空 = 保留原引用;填写 = 原位替换密文记录。
// 私钥路径留空 = 保留原值。
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
	passwordRef, keyPath, passphraseRef := old.PasswordRef,
		pickSecret(req.KeyPath, old.KeyPath), old.PassphraseRef
	jumpID, startupCmd, follow := req.JumpID, req.StartupCmd, req.FollowSSHPath
	if ct == store.ConnTelnet {
		username, authType = "", ""
		passwordRef, keyPath, passphraseRef = "", "", ""
		jumpID, startupCmd, follow = "", "", false
	}
	if ct == store.ConnSSH {
		if req.AuthType == store.AuthPassword && req.Password == "" && old.PasswordRef == "" {
			fail(c, 400, "密码认证必须填写密码")
			return
		}
		if req.AuthType == store.AuthKey && req.KeyPath == "" && old.KeyPath == "" {
			fail(c, 400, "私钥认证必须填写私钥路径")
			return
		}
		if req.Password != "" {
			ref, err := a.st.PutPassword(old.PasswordRef, req.Password)
			if err != nil {
				fail(c, 500, err.Error())
				return
			}
			passwordRef = ref
		}
		if req.Passphrase != "" {
			ref, err := a.st.PutPassphrase(old.PassphraseRef, req.Passphrase)
			if err != nil {
				fail(c, 500, err.Error())
				return
			}
			passphraseRef = ref
		}
	}
	h, err := a.st.Put(store.Host{
		ID:   id,
		Name: strings.TrimSpace(req.Name), Host: strings.TrimSpace(req.Host),
		Port: orDefault(req.Port, defaultPort(ct)), ConnType: ct,
		Username:      username,
		AuthType:      authType,
		PasswordRef:   passwordRef,
		KeyPath:       keyPath,
		PassphraseRef: passphraseRef,
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

// duplicateHost 复制主机为一份新配置(凭据引用与源主机共享同一条加密记录),
// 名称追加 " - copy"。
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
		PasswordRef:   old.PasswordRef,
		KeyPath:       old.KeyPath,
		PassphraseRef: old.PassphraseRef,
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
// 表单新输入的明文凭据临时入库即测即删( ForgetSecret),不覆盖已存引用。
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
		KeyPath: req.KeyPath,
		JumpID:  req.JumpID, Group: strings.TrimSpace(req.Group), StartupCmd: req.StartupCmd,
	}
	if req.ID != "" && ct == store.ConnSSH {
		if old, ok := a.st.Get(req.ID); ok {
			if h.KeyPath == "" {
				h.KeyPath = old.KeyPath
			}
			if req.Password == "" {
				h.PasswordRef = old.PasswordRef
			}
			if req.Passphrase == "" {
				h.PassphraseRef = old.PassphraseRef
			}
		}
	}
	// 临时凭据:Put 新记录拿引用,测试完删除;不写回 hosts.json
	if req.Password != "" {
		ref, err := a.st.PutPassword("", req.Password)
		if err != nil {
			fail(c, 500, err.Error())
			return
		}
		h.PasswordRef = ref
		defer a.st.ForgetSecret(ref)
	}
	if req.Passphrase != "" {
		ref, err := a.st.PutPassphrase("", req.Passphrase)
		if err != nil {
			fail(c, 500, err.Error())
			return
		}
		h.PassphraseRef = ref
		defer a.st.ForgetSecret(ref)
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
