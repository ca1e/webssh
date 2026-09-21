package mcp

// hosts 域工具:ssh_list_hosts / ssh_host_info / ssh_test_connection /
// ssh_create_host / ssh_edit_host / ssh_duplicate_host / ssh_delete_host。
// 输出统一走 store.PublicHost 脱敏投影:无凭据字段,auth 类型仅以
// "secret"|"key" 标识 + Has* 布尔回答。凭据字段只进不出:password/passphrase
// 仅作为 ssh_create_host 入参,提交后立即加密进 secrets 库换引用;任何返回
// 都不回显,错误文案亦不得提及明文。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"webssh/internal/store"
)

// hostIn 单主机入参(name 或 id)。
type hostIn struct {
	Host string `json:"host" jsonschema:"主机名称或 ID,可用 ssh_list_hosts 查询"`
}

// mcpHostView MCP 侧主机脱敏投影:在 store.PublicHost 之上再把词形映射掉
// (auth_type 的字面值 "password"/"passphrase" 一律不进入 MCP 输出,快照
// 测试据此断言凭据零暴露;Web 端 hostView 契约不受影响)。
func mcpHostView(h store.Host) map[string]any {
	p := store.PublicHost(h)
	auth := "none"
	switch p.AuthType {
	case store.AuthPassword:
		auth = "secret"
	case store.AuthKey:
		auth = "key"
	}
	return map[string]any{
		"id": p.ID, "name": p.Name, "host": p.Host, "port": p.Port,
		"conn_type": p.ConnType, "username": p.Username,
		"auth": auth,
		// 密码型凭据是否存在 / 私钥口令是否存在(仅布尔,不含词形)
		"has_secret":     p.HasPassword,
		"has_key_secret": p.HasPassphrase,
		"jump_id":        p.JumpID, "group": p.Group, "startup_cmd": p.StartupCmd,
	}
}

func (s *Server) toolListHosts(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	start := time.Now()
	hosts := s.st.List()
	out := make([]map[string]any, 0, len(hosts))
	for _, h := range hosts {
		if !s.hostVisible(h) {
			continue
		}
		out = append(out, mcpHostView(h))
	}
	var err error
	defer func() { callLog("ssh_list_hosts", "-", err, start) }()
	return nil, out, err
}

func (s *Server) toolHostInfo(ctx context.Context, req *mcp.CallToolRequest, in hostIn) (*mcp.CallToolResult, any, error) {
	start := time.Now()
	h, err := s.resolveHost(in.Host)
	var out any
	if err == nil {
		err = rejectTelnet(h)
	}
	if err == nil {
		out = mcpHostView(h)
	}
	defer func() { callLog("ssh_host_info", in.Host, err, start) }()
	return nil, out, err
}

func (s *Server) toolTestConnection(ctx context.Context, req *mcp.CallToolRequest, in hostIn) (*mcp.CallToolResult, any, error) {
	start := time.Now()
	h, err := s.resolveHost(in.Host)
	var out any
	if err == nil {
		err = rejectTelnet(h)
	}
	if err == nil {
		var jump *store.Host
		if h.JumpID != "" {
			j, ok := s.st.Get(h.JumpID)
			if !ok {
				err = fmt.Errorf("跳板机不存在(可能已被删除);请在 Web 端修正该主机的跳板配置")
			} else {
				jump = &j
			}
		}
		if err == nil {
			testStart := time.Now()
			if err = s.mgr.TestConnection(ctx, h, jump); err != nil {
				err = fmt.Errorf("连接 %s 失败:%v;请检查网络、端口、凭据与主机密钥(首次变更需在 Web 端确认)", h.Name, err)
			} else {
				out = map[string]any{"ok": true, "elapsed_ms": time.Since(testStart).Milliseconds()}
			}
		}
	}
	defer func() { callLog("ssh_test_connection", in.Host, err, start) }()
	return nil, out, err
}

// ---------- 主机写操作(create / duplicate / delete) ----------
//
// 凭据只进不出:password/passphrase 仅作入参,提交即加密进 secrets 库换
// 引用;返回一律 mcpHostView 脱敏投影。错误文案不得含 "password"/
// "passphrase" 词形(快照测试 secretPattern 断言),中文"口令"不受限。
// 写操作与读面同一口径:仅 SSH 主机(create 无 telnet 入参;duplicate/delete
// 对 telnet 源/目标一律拒绝)。telnet 全生命周期管理留在 Web 端。
// 可编辑字段(名称/IP/用户名/端口/分组/启动命令)按指针入参区分"未提供/
// 显式清空";认证方式与凭据不在 edit_host 面内(输入 schema 封闭,未知
// 字段直接拒绝)——改密/换认证方式仍只能走 Web 端。

// createHostIn 新建主机入参。password/passphrase 为明文字段:仅存在于
// 单次调用的参数对象,入 store 前即换成引用。
type createHostIn struct {
	Name       string `json:"name" jsonschema:"主机显示名称"`
	Host       string `json:"host" jsonschema:"主机 IP 或域名"`
	Username   string `json:"username" jsonschema:"登录用户名"`
	AuthType   string `json:"auth_type" jsonschema:"认证方式:password 或 key"`
	Port       int    `json:"port,omitempty" jsonschema:"SSH 端口,默认 22"`
	Password   string `json:"password,omitempty" jsonschema:"登录口令明文;认证方式为口令时必填;提交后立即加密入库,不回显"`
	KeyPath    string `json:"key_path,omitempty" jsonschema:"服务端私钥路径(如 ~/.ssh/id_ed25519);认证方式为私钥时必填"`
	Passphrase string `json:"passphrase,omitempty" jsonschema:"私钥口令明文(可选,仅私钥认证有意义);不回显"`
	Jump       string `json:"jump,omitempty" jsonschema:"跳板机名称或 ID(可选)"`
	Group      string `json:"group,omitempty" jsonschema:"分组名(可选)"`
}

func (s *Server) toolCreateHost(ctx context.Context, req *mcp.CallToolRequest, in createHostIn) (*mcp.CallToolResult, any, error) {
	start := time.Now()
	var out any
	err := func() error {
		name, host, username := strings.TrimSpace(in.Name), strings.TrimSpace(in.Host), strings.TrimSpace(in.Username)
		if name == "" || host == "" || username == "" {
			return errors.New("name/host/username 均不能为空")
		}
		if in.AuthType != store.AuthPassword && in.AuthType != store.AuthKey {
			return errors.New("auth_type 非法:仅支持口令认证与私钥认证两种取值(字面量见参数说明)")
		}
		if in.Port < 0 || in.Port > 65535 {
			return fmt.Errorf("port 非法:%d(须为 1-65535 或留空取默认 22)", in.Port)
		}
		switch {
		case in.AuthType == store.AuthPassword && in.Password == "":
			return errors.New("口令认证必须提交对应凭据字段(字面量见参数说明;明文仅本次调用内使用,入库即加密)")
		case in.AuthType == store.AuthKey && strings.TrimSpace(in.KeyPath) == "":
			return errors.New("私钥认证必须提交 key_path(服务端可读的私钥路径)")
		case in.AuthType == store.AuthKey && in.Password != "":
			return errors.New("口令字段仅对口令认证有意义;请改用口令认证或删除该字段")
		case in.Passphrase != "" && in.AuthType != store.AuthKey:
			return errors.New("私钥口令字段仅对私钥认证有意义;请改用私钥认证或删除该字段")
		}
		jumpID := ""
		if j := strings.TrimSpace(in.Jump); j != "" {
			jh, err := s.resolveHost(j)
			if err != nil {
				return fmt.Errorf("跳板机解析失败:%w", err)
			}
			if err := rejectTelnet(jh); err != nil {
				return fmt.Errorf("跳板机 %s 不支持(telnet):%w", jh.Name, err)
			}
			jumpID = jh.ID
		}
		h := store.Host{
			Name: name, Host: host, Port: in.Port, ConnType: store.ConnSSH,
			Username: username, AuthType: in.AuthType,
			KeyPath: strings.TrimSpace(in.KeyPath), JumpID: jumpID,
			Group: strings.TrimSpace(in.Group),
		}
		if h.Port == 0 {
			h.Port = 22
		}
		// 明文凭据 → secrets 库引用;失败即中止,不留半套主机记录。
		if in.Password != "" {
			ref, err := s.st.PutPassword("", in.Password)
			if err != nil {
				return fmt.Errorf("凭据入库失败:%w", err)
			}
			h.PasswordRef = ref
		}
		if in.Passphrase != "" {
			ref, err := s.st.PutPassphrase("", in.Passphrase)
			if err != nil {
				s.st.ForgetSecret(h.PasswordRef)
				return fmt.Errorf("私钥口令入库失败:%w", err)
			}
			h.PassphraseRef = ref
		}
		saved, err := s.st.Put(h)
		if err != nil {
			s.st.ForgetSecret(h.PasswordRef)
			s.st.ForgetSecret(h.PassphraseRef)
			return fmt.Errorf("主机保存失败:%w", err)
		}
		// 可见开关开启:新主机自动入名单(持久化+热生效);失败则整体回滚,
		// 不留对 MCP 不可管理的僵尸配置。
		if err := s.addVisibleHost(saved.ID); err != nil {
			s.st.Delete(saved.ID)
			s.st.ForgetSecret(h.PasswordRef)
			s.st.ForgetSecret(h.PassphraseRef)
			return fmt.Errorf("新主机加入可见名单失败,已回滚:%w", err)
		}
		out = mcpHostView(saved)
		return nil
	}()
	defer func() { callLog("ssh_create_host", in.Name, err, start) }()
	if err != nil {
		return nil, nil, err
	}
	return nil, out, nil
}

// editHostIn 编辑主机入参:仅开放 名称/IP/用户名/端口/分组/启动命令 六个
// 字段。指针语义 = 未提供即保持原值;字符串字段传 "" 即清空(分组/启动
// 命令可清空,name/host/username 不可清空)。auth_type/凭据字段不在 schema
// 内(封闭对象,未知属性直接拒绝)——凭据只进不出不变式不受影响。
type editHostIn struct {
	Host       string  `json:"host" jsonschema:"要编辑的主机名称或 ID(定位目标,不参与修改)"`
	Name       *string `json:"name,omitempty" jsonschema:"新主机显示名称(可选;留空不改)"`
	Addr       *string `json:"addr,omitempty" jsonschema:"新主机 IP 或域名(可选;留空不改)"`
	Username   *string `json:"username,omitempty" jsonschema:"新登录用户名(可选;留空不改)"`
	Port       *int    `json:"port,omitempty" jsonschema:"新 SSH 端口(可选;留空不改)"`
	Group      *string `json:"group,omitempty" jsonschema:"新分组名(可选;传空字符串清空分组)"`
	StartupCmd *string `json:"startup_cmd,omitempty" jsonschema:"新启动命令,连接后自动执行(可选;传空字符串清空)"`
}

func (s *Server) toolEditHost(ctx context.Context, req *mcp.CallToolRequest, in editHostIn) (*mcp.CallToolResult, any, error) {
	start := time.Now()
	var out any
	err := func() error {
		h, err := s.resolveHost(in.Host)
		if err != nil {
			return err
		}
		if err := rejectTelnet(h); err != nil {
			return err
		}
		if in.Name == nil && in.Addr == nil && in.Username == nil &&
			in.Port == nil && in.Group == nil && in.StartupCmd == nil {
			return errors.New("未提供任何可改字段(name/addr/username/port/group/startup_cmd);请先用 ssh_host_info 查看现状")
		}
		if in.Name != nil {
			name := strings.TrimSpace(*in.Name)
			if name == "" {
				return errors.New("name 不能为空;如需改名请传入非空名称")
			}
			h.Name = name
		}
		if in.Addr != nil {
			addr := strings.TrimSpace(*in.Addr)
			if addr == "" {
				return errors.New("addr(IP/域名)不能为空;如需修改请传入非空地址")
			}
			h.Host = addr
		}
		if in.Username != nil {
			username := strings.TrimSpace(*in.Username)
			if username == "" {
				return errors.New("username 不能为空;如需修改请传入非空用户名")
			}
			h.Username = username
		}
		if in.Port != nil {
			if *in.Port < 1 || *in.Port > 65535 {
				return fmt.Errorf("port 非法:%d(须为 1-65535)", *in.Port)
			}
			h.Port = *in.Port
		}
		if in.Group != nil {
			h.Group = strings.TrimSpace(*in.Group)
		}
		if in.StartupCmd != nil {
			h.StartupCmd = strings.TrimSpace(*in.StartupCmd)
		}
		saved, err := s.st.Put(h)
		if err != nil {
			return fmt.Errorf("主机保存失败:%w", err)
		}
		out = mcpHostView(saved)
		return nil
	}()
	defer func() { callLog("ssh_edit_host", in.Host, err, start) }()
	if err != nil {
		return nil, nil, err
	}
	return nil, out, nil
}

// duplicateHostIn 复制主机入参;凭据引用与源共享同一条加密记录(与 Web 端
// duplicateHost 语义一致),新名缺省 = 源名 + " - copy"。
type duplicateHostIn struct {
	Host string `json:"host" jsonschema:"源主机名称或 ID"`
	Name string `json:"name,omitempty" jsonschema:"新主机名称(可选,默认 源名 - copy)"`
}

func (s *Server) toolDuplicateHost(ctx context.Context, req *mcp.CallToolRequest, in duplicateHostIn) (*mcp.CallToolResult, any, error) {
	start := time.Now()
	var out any
	err := func() error {
		old, err := s.resolveHost(in.Host)
		if err != nil {
			return err
		}
		if err := rejectTelnet(old); err != nil {
			return err
		}
		// 副本是新 ID,开关开启时与 create 同样自动加入可见名单。
		name := strings.TrimSpace(in.Name)
		if name == "" {
			name = old.Name + " - copy"
		}
		cp := old
		cp.ID, cp.Name = "", name
		saved, err := s.st.Put(cp)
		if err != nil {
			return fmt.Errorf("复制失败:%w", err)
		}
		if err := s.addVisibleHost(saved.ID); err != nil {
			s.st.Delete(saved.ID)
			return fmt.Errorf("副本加入可见名单失败,已回滚:%w", err)
		}
		out = mcpHostView(saved)
		return nil
	}()
	defer func() { callLog("ssh_duplicate_host", in.Host, err, start) }()
	if err != nil {
		return nil, nil, err
	}
	return nil, out, nil
}

// deleteHostIn 删除主机入参;confirm 必须显式 true —— 删除不可逆,给
// Agent 一次自我复核的机会。
type deleteHostIn struct {
	Host    string `json:"host" jsonschema:"要删除的主机名称或 ID"`
	Confirm bool   `json:"confirm,omitempty" jsonschema:"二次确认:必须显式传 true 才会删除;缺省或 false 一律拒绝"`
}

func (s *Server) toolDeleteHost(ctx context.Context, req *mcp.CallToolRequest, in deleteHostIn) (*mcp.CallToolResult, any, error) {
	start := time.Now()
	var out any
	err := func() error {
		if !in.Confirm {
			return errors.New("删除主机不可逆:请复核目标后以 confirm=true 重新调用")
		}
		h, err := s.resolveHost(in.Host)
		if err != nil {
			return err
		}
		if err := rejectTelnet(h); err != nil {
			return err
		}
		// 跳板保护:其他主机仍引用它时拒删(删除会让下游主机连接静默失效)。
		for _, o := range s.st.List() {
			if o.ID != h.ID && o.JumpID == h.ID {
				return fmt.Errorf("主机 %s 仍被 %q 用作跳板机;请先在 Web 端调整该主机的跳板配置,或改删无下游引用的目标", h.Name, o.Name)
			}
		}
		if !s.st.Delete(h.ID) {
			return fmt.Errorf("主机 %q 删除失败(可能已被并发删除);请用 ssh_list_hosts 复核", in.Host)
		}
		out = map[string]any{"ok": true, "deleted_id": h.ID, "deleted_name": h.Name}
		return nil
	}()
	defer func() { callLog("ssh_delete_host", in.Host, err, start) }()
	if err != nil {
		return nil, nil, err
	}
	return nil, out, nil
}
