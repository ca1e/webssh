package handler

// MCP 设置接口:路径白名单、可见主机、工具过滤的查看/修改。
// 修改经 mcp.Server.UpdateSettings 持久化到 mcp.json 并热重载;
// PUT 为部分更新——未传的段保持现值。

import (
	"github.com/gin-gonic/gin"

	"webssh/internal/mcp"
)

type mcpSettingsReq struct {
	PathWhitelist *struct {
		Enabled *bool     `json:"enabled"`
		Paths   *[]string `json:"paths"`
	} `json:"path_whitelist"`
	VisibleHosts *struct {
		Enabled *bool     `json:"enabled"`
		IDs     *[]string `json:"ids"`
	} `json:"visible_hosts"`
	ToolFilter *struct {
		Tools *[]string `json:"tools"`
	} `json:"tool_filter"`
}

// getMcpSettings GET /api/mcp/settings
func (a *API) getMcpSettings(c *gin.Context) {
	var set mcp.Settings = a.mcpSrv.Settings()
	c.JSON(200, gin.H{
		"path_whitelist": gin.H{"enabled": set.PathWhitelist.Enabled, "paths": set.PathWhitelist.Paths},
		"visible_hosts":  gin.H{"enabled": set.VisibleHosts.Enabled, "ids": set.VisibleHosts.IDs},
		"tool_filter":    gin.H{"tools": set.ToolFilter.Tools},
	})
}

// putMcpSettings PUT /api/mcp/settings
func (a *API) putMcpSettings(c *gin.Context) {
	var req mcpSettingsReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, 400, "请求体须为 {path_whitelist?, visible_hosts?, tool_filter?} 的 JSON")
		return
	}
	set := a.mcpSrv.Settings()
	if req.PathWhitelist != nil {
		if req.PathWhitelist.Enabled != nil {
			set.PathWhitelist.Enabled = *req.PathWhitelist.Enabled
		}
		if req.PathWhitelist.Paths != nil {
			set.PathWhitelist.Paths = *req.PathWhitelist.Paths
		}
	}
	if req.VisibleHosts != nil {
		if req.VisibleHosts.Enabled != nil {
			set.VisibleHosts.Enabled = *req.VisibleHosts.Enabled
		}
		if req.VisibleHosts.IDs != nil {
			set.VisibleHosts.IDs = *req.VisibleHosts.IDs
		}
	}
	if req.ToolFilter != nil && req.ToolFilter.Tools != nil {
		set.ToolFilter.Tools = *req.ToolFilter.Tools
	}
	if err := a.mcpSrv.UpdateSettings(set); err != nil {
		fail(c, 500, err.Error())
		return
	}
	cur := a.mcpSrv.Settings()
	c.JSON(200, gin.H{
		"path_whitelist": gin.H{"enabled": cur.PathWhitelist.Enabled, "paths": cur.PathWhitelist.Paths},
		"visible_hosts":  gin.H{"enabled": cur.VisibleHosts.Enabled, "ids": cur.VisibleHosts.IDs},
		"tool_filter":    gin.H{"tools": cur.ToolFilter.Tools},
	})
}
