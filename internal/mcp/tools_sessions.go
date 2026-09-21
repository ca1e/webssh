package mcp

// sessions 域工具:ssh_list_sessions —— 当前活跃会话快照。
// 会话 = sshmgr.Manager 中的活连接(Web 端打开的终端/传输,以及 MCP 工具
// 调用建立的连接)。只读投影:仅元数据,无任何凭据字段;不可见主机开关
// 名单外的主机会话按「不存在」处理,不出现在结果中(与其他工具的可见性
// 口径一致)。

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *Server) toolListSessions(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	start := time.Now()
	sessions := s.mgr.List()
	out := make([]map[string]any, 0, len(sessions))
	for _, sess := range sessions {
		if !s.hostVisible(sess.Host) {
			continue
		}
		v := map[string]any{
			"id":             sess.ID,
			"name":           sess.Name(),
			"host":           sess.IP(),
			"addr":           sess.Addr(),
			"username":       sess.Username(),
			"conn_type":      sess.ConnType(),
			"opened_at":      sess.OpenedAt.UTC().Format(time.RFC3339),
			"uptime_seconds": int(time.Since(sess.OpenedAt).Seconds()),
		}
		if sess.JumpHost != nil {
			v["jump_name"] = sess.JumpHost.Name
		}
		out = append(out, v)
	}
	var err error
	defer func() { callLog("ssh_list_sessions", "-", err, start) }()
	return nil, out, err
}
