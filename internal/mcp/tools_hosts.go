package mcp

// hosts 域工具:ssh_list_hosts / ssh_host_info / ssh_test_connection。
// 输出统一走 store.PublicHost 脱敏投影:无凭据字段,auth 类型仅以
// "password"|"key" 标识 + Has* 布尔回答。

import (
	"context"
	"fmt"
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
		"jump_id":        p.JumpID, "group": p.Group,
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
