package mcp

// MCP 设置读写:供 Web 设置页(/api/mcp/settings)查看/修改路径白名单、
// 可见主机与工具过滤。持久化 = 以 map 合并方式改写 mcp.json(保留 token 等
// 其余字段),随后走 Reload 热生效;写失败或重载失败均返回错误,不产生半套
// 状态(Reload 失败时内存保持旧策略)。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Settings Web 设置页可见的 MCP 配置投影。
type Settings struct {
	PathWhitelist PathWhitelistCfg `json:"path_whitelist"`
	VisibleHosts  VisibleHostsCfg  `json:"visible_hosts"`
	ToolFilter    ToolFilterCfg    `json:"tool_filter"`
}

// Settings 返回当前生效设置(内存态,含热重载结果)。
func (s *Server) Settings() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Settings{
		PathWhitelist: s.conf.Defaults.PathWhitelist,
		VisibleHosts:  s.conf.VisibleHosts,
		ToolFilter:    s.conf.ToolFilter,
	}
}

// UpdateSettings 持久化新设置并热重载;任一步失败返回错误且内存保持旧策略。
func (s *Server) UpdateSettings(set Settings) error {
	if err := validateSettings(set); err != nil {
		return err
	}
	if err := s.persistSettings(set); err != nil {
		return err
	}
	if err := s.Reload(); err != nil {
		return fmt.Errorf("设置已写入但热重载失败(内存保持旧策略):%w", err)
	}
	return nil
}

// validateSettings 写盘前校验:工具名必须可识别(主机 ID 不在此校验——主机
// 可能先于设置删除,运行时过滤时自然忽略)。
func validateSettings(set Settings) error {
	known := map[string]bool{}
	for _, n := range allToolNames {
		known[n] = true
	}
	for _, n := range set.ToolFilter.Tools {
		if !known[n] {
			return fmt.Errorf("未知工具 %q;可用: %v", n, allToolNames)
		}
	}
	return nil
}

// persistSettings 读取现有 mcp.json(可不存在)→ 合并三段配置 → 写回。
func (s *Server) persistSettings(set Settings) error {
	file := filepath.Join(s.cfgDir, "mcp.json")
	doc := map[string]any{}
	if data, err := os.ReadFile(file); err == nil {
		if err := json.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("现有 mcp.json 解析失败,拒绝覆盖:%w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	defaults, _ := doc["defaults"].(map[string]any)
	if defaults == nil {
		defaults = map[string]any{}
	}
	defaults["path_whitelist"] = map[string]any{
		"enabled": set.PathWhitelist.Enabled,
		"paths":   set.PathWhitelist.Paths,
	}
	doc["defaults"] = defaults
	doc["visible_hosts"] = map[string]any{
		"enabled": set.VisibleHosts.Enabled,
		"ids":     set.VisibleHosts.IDs,
	}
	doc["tool_filter"] = map[string]any{
		"tools": set.ToolFilter.Tools,
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(file, data, 0o600)
}
