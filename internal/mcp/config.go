package mcp

// mcp.json 加载与校验:可选文件,不存在 = 全默认(exec 15s,读上限 1MiB,
// 下载上限 100MiB)。启动校验非法即 fatal(具体到字段);SIGHUP 热重载由
// Server.Reload 承接(失败保持旧配置并告警)。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 默认值。
const (
	DefaultExecTimeout      = 15 * time.Second
	DefaultReadMaxBytes     = 1 << 20 // 1 MiB/流,与 RunExec 输出上限同源
	DefaultDownloadMaxBytes = 100 << 20
)

// Config MCP 运行时配置(已校验/已解析形态)。
type Config struct {
	Enabled      bool
	Token        string // mcp.json 的 token 字段;环境变量 WEBSSH_MCP_TOKEN 优先(在 Server 合成)
	Domains      map[string]bool
	Defaults     DefaultsCfg
	VisibleHosts VisibleHostsCfg
	ToolFilter   ToolFilterCfg
}

// VisibleHostsCfg 可见主机:关闭(默认)= 全部主机对 MCP 可见;开启后仅
// ids 内的主机可见,其余在 ssh_list_hosts 中隐藏、按主机操作一律报"不存在"。
type VisibleHostsCfg struct {
	Enabled bool     `json:"enabled"`
	IDs     []string `json:"ids"`
}

// ToolFilterCfg 工具白名单:显式列出的工具才对 MCP 暴露;空列表 = 全部暴露。
type ToolFilterCfg struct {
	Tools []string `json:"tools"`
}

// allToolNames 全部工具名(config 校验与设置页目录共用)。
var allToolNames = []string{
	"ssh_list_hosts", "ssh_host_info", "ssh_test_connection",
	"ssh_create_host", "ssh_duplicate_host", "ssh_delete_host",
	"ssh_exec", "ssh_download_file",
	"ssh_system_info", "ssh_cpu_memory", "ssh_disk_usage",
	"ssh_network_status", "ssh_service_status", "ssh_docker_status", "ssh_k8s_status",
}

// DefaultsCfg 全局默认策略。
type DefaultsCfg struct {
	ExecTimeout      time.Duration // ssh_exec 执行上限
	ReadMaxBytes     int64         // exec 输出上限(字节/流)
	DownloadMaxBytes int64         // 下载大小上限
	PathWhitelist    PathWhitelistCfg
}

// PathWhitelistCfg 可选路径白名单:关闭时路径不受限(默认);打开后
// ssh_download_file 的 path 与 ssh_exec 命令中的路径均须落在前缀内。
type PathWhitelistCfg struct {
	Enabled bool     `json:"enabled"`
	Paths   []string `json:"paths"`
}

// rawConfig JSON 形态(DisallowUnknownFields,拼错字段名直接拒绝)。
type rawConfig struct {
	Enabled *bool  `json:"enabled"`
	Token   string `json:"token"`
	Domains *struct {
		Hosts   *bool `json:"hosts"`
		Exec    *bool `json:"exec"`
		Files   *bool `json:"files"`
		Monitor *bool `json:"monitor"`
	} `json:"domains"`
	Defaults *struct {
		ExecTimeout      string `json:"exec_timeout"`
		ReadMaxBytes     *int64 `json:"read_max_bytes"`
		DownloadMaxBytes *int64 `json:"download_max_bytes"`
		PathWhitelist    *struct {
			Enabled *bool     `json:"enabled"`
			Paths   *[]string `json:"paths"`
		} `json:"path_whitelist"`
	} `json:"defaults"`
	VisibleHosts *struct {
		Enabled *bool     `json:"enabled"`
		IDs     *[]string `json:"ids"`
	} `json:"visible_hosts"`
	ToolFilter *struct {
		Tools *[]string `json:"tools"`
	} `json:"tool_filter"`
}

// Load 读取并校验 $ConfigDir/mcp.json;文件不存在返回全默认。
func Load(dir string) (*Config, error) {
	cfg := defaultConfig()
	data, err := os.ReadFile(filepath.Join(dir, "mcp.json"))
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var raw rawConfig
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("mcp.json 解析失败: %w", err)
	}
	if raw.Enabled != nil {
		cfg.Enabled = *raw.Enabled
	}
	cfg.Token = raw.Token
	if raw.Domains != nil {
		if raw.Domains.Hosts != nil {
			cfg.Domains["hosts"] = *raw.Domains.Hosts
		}
		if raw.Domains.Exec != nil {
			cfg.Domains["exec"] = *raw.Domains.Exec
		}
		if raw.Domains.Files != nil {
			cfg.Domains["files"] = *raw.Domains.Files
		}
		if raw.Domains.Monitor != nil {
			cfg.Domains["monitor"] = *raw.Domains.Monitor
		}
	}
	if raw.Defaults != nil {
		d := raw.Defaults
		if d.ExecTimeout != "" {
			dur, err := time.ParseDuration(d.ExecTimeout)
			if err != nil || dur <= 0 {
				return nil, fmt.Errorf("mcp.json: defaults.exec_timeout %q 不是正的时长(如 15s)", d.ExecTimeout)
			}
			cfg.Defaults.ExecTimeout = dur
		}
		if d.ReadMaxBytes != nil {
			if *d.ReadMaxBytes <= 0 {
				return nil, fmt.Errorf("mcp.json: defaults.read_max_bytes 必须为正,得到 %d", *d.ReadMaxBytes)
			}
			cfg.Defaults.ReadMaxBytes = *d.ReadMaxBytes
		}
		if d.DownloadMaxBytes != nil {
			if *d.DownloadMaxBytes <= 0 {
				return nil, fmt.Errorf("mcp.json: defaults.download_max_bytes 必须为正,得到 %d", *d.DownloadMaxBytes)
			}
			cfg.Defaults.DownloadMaxBytes = *d.DownloadMaxBytes
		}
		if d.PathWhitelist != nil {
			pw := d.PathWhitelist
			if pw.Enabled != nil {
				cfg.Defaults.PathWhitelist.Enabled = *pw.Enabled
			}
			if pw.Paths != nil {
				for _, p := range *pw.Paths {
					p = strings.TrimSpace(p)
					if p == "" {
						continue
					}
					if !filepath.IsAbs(p) || filepath.Clean(p) != strings.TrimSuffix(p, "/") {
						return nil, fmt.Errorf("mcp.json: defaults.path_whitelist.paths 含非法路径 %q(须为规范化绝对路径,如 /var/log/)", p)
					}
					cfg.Defaults.PathWhitelist.Paths = append(cfg.Defaults.PathWhitelist.Paths, p)
				}
			}
		}
	}
	if raw.VisibleHosts != nil {
		vh := raw.VisibleHosts
		if vh.Enabled != nil {
			cfg.VisibleHosts.Enabled = *vh.Enabled
		}
		if vh.IDs != nil {
			for _, id := range *vh.IDs {
				if id = strings.TrimSpace(id); id != "" {
					cfg.VisibleHosts.IDs = append(cfg.VisibleHosts.IDs, id)
				}
			}
		}
	}
	if raw.ToolFilter != nil {
		tf := raw.ToolFilter
		if tf.Tools != nil {
			known := map[string]bool{}
			for _, n := range allToolNames {
				known[n] = true
			}
			tools := make([]string, 0, len(*tf.Tools))
			for _, n := range *tf.Tools {
				n = strings.TrimSpace(n)
				if n == "" {
					continue
				}
				if !known[n] {
					return nil, fmt.Errorf("mcp.json: tool_filter.tools 含未知工具 %q;可用: %v", n, allToolNames)
				}
				tools = append(tools, n)
			}
			// 显式白名单:整体替换,不做默认合并
			cfg.ToolFilter.Tools = tools
		}
	}
	return cfg, nil
}

// defaultConfig 全默认。
func defaultConfig() *Config {
	return &Config{
		Enabled: true,
		Domains: map[string]bool{"hosts": true, "exec": true, "files": true, "monitor": true},
		Defaults: DefaultsCfg{
			ExecTimeout:      DefaultExecTimeout,
			ReadMaxBytes:     DefaultReadMaxBytes,
			DownloadMaxBytes: DefaultDownloadMaxBytes,
		},
		// 工具白名单默认显式列出全部工具:空名单 = 不暴露任何工具
		ToolFilter: ToolFilterCfg{Tools: append([]string(nil), allToolNames...)},
	}
}
