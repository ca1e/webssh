package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDefaultsWhenAbsent(t *testing.T) {
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled || cfg.Token != "" {
		t.Fatalf("默认应为启用且无 token: %+v", cfg)
	}
	for _, d := range []string{"hosts", "exec", "files", "monitor"} {
		if !cfg.Domains[d] {
			t.Fatalf("域 %s 默认应开启", d)
		}
	}
	if cfg.Defaults.ExecTimeout != DefaultExecTimeout || cfg.Defaults.ReadMaxBytes != DefaultReadMaxBytes ||
		cfg.Defaults.DownloadMaxBytes != DefaultDownloadMaxBytes {
		t.Fatalf("默认限额不符: %+v", cfg.Defaults)
	}
	if cfg.Defaults.PathWhitelist.Enabled || len(cfg.Defaults.PathWhitelist.Paths) != 0 {
		t.Fatalf("路径白名单默认应关闭且为空: %+v", cfg.Defaults.PathWhitelist)
	}
	if len(cfg.ToolFilter.Tools) != 12 {
		t.Fatalf("工具白名单默认应显式列出全部 12 个工具: %v", cfg.ToolFilter.Tools)
	}
}

func TestLoadFullConfig(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "mcp.json", `{
		"enabled": true,
		"token": "tok-1",
		"domains": {"files": false},
		"defaults": {"exec_timeout": "5s", "read_max_bytes": 4096, "download_max_bytes": 8192,
			"path_whitelist": {"enabled": true, "paths": ["/var/log/", "  ", "/srv/data/"]}}
	}`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "tok-1" || cfg.Domains["files"] {
		t.Fatalf("token/域门控不符: %+v", cfg)
	}
	if cfg.Defaults.ExecTimeout != 5*time.Second ||
		cfg.Defaults.ReadMaxBytes != 4096 || cfg.Defaults.DownloadMaxBytes != 8192 {
		t.Fatalf("defaults 不符: %+v", cfg.Defaults)
	}
	pw := cfg.Defaults.PathWhitelist
	if !pw.Enabled || len(pw.Paths) != 2 || pw.Paths[0] != "/var/log/" || pw.Paths[1] != "/srv/data/" {
		t.Fatalf("path_whitelist 不符: %+v", pw)
	}
}

func TestLoadRejects(t *testing.T) {
	cases := []struct {
		name, json, want string
	}{
		{"未知字段", `{"tokn":"x"}`, "mcp.json 解析失败"},
		{"坏时长", `{"defaults":{"exec_timeout":"abc"}}`, "exec_timeout"},
		{"负上限", `{"defaults":{"read_max_bytes":-1}}`, "read_max_bytes"},
		{"负下载上限", `{"defaults":{"download_max_bytes":0}}`, "download_max_bytes"},
		{"白名单相对路径", `{"defaults":{"path_whitelist":{"paths":["var/log"]}}}`, "非法路径"},
	}
	for _, c := range cases {
		dir := t.TempDir()
		writeFile(t, dir, "mcp.json", c.json)
		if _, err := Load(dir); err == nil {
			t.Fatalf("%s 应拒绝启动", c.name)
		} else if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s 错误文案不符: %v", c.name, err)
		}
	}
}

func TestLoadHostsAndToolsFilter(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "mcp.json", `{
		"visible_hosts": {"enabled": true, "ids": ["h1", " ", "h2"]},
		"tool_filter": {"tools": ["ssh_exec", "ssh_list_hosts", ""]}
	}`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.VisibleHosts.Enabled || len(cfg.VisibleHosts.IDs) != 2 {
		t.Fatalf("visible_hosts 不符: %+v", cfg.VisibleHosts)
	}
	if len(cfg.ToolFilter.Tools) != 2 {
		t.Fatalf("tool_filter 不符: %+v", cfg.ToolFilter)
	}

	writeFile(t, dir, "mcp.json", `{"tool_filter":{"tools":["nope"]}}`)
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "未知工具") {
		t.Fatalf("未知工具应拒绝: %v", err)
	}
}
