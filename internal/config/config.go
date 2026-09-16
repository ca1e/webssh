package config

import (
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type Config struct {
	// ListenAddr HTTP 监听地址
	ListenAddr string
	// ConfigDir XDG 配置目录(hosts.json、known_hosts.json 存放处)
	ConfigDir string
	// KeysDir 本地私钥目录(默认 ~/.ssh)
	KeysDir string
	// MaxSessions 同时活动会话数量上限
	MaxSessions int
	// OrphanGrace 会话最后一个终端断开后的存活宽限期(覆盖页面刷新/瞬时断网
	// 的重连窗口);归零超时后自动回收孤儿会话
	OrphanGrace time.Duration
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

func Load() *Config {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "."
	}
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		xdg = filepath.Join(home, ".config")
	}
	return &Config{
		// 默认仅监听本机回环:全部 API 无鉴权,绑定 0.0.0.0 等于把所有已存主机的
		// shell 与本机文件读写暴露给局域网。确需外部访问请交给反向代理并自行加鉴权。
		ListenAddr:  envOr("WEBSSH_LISTEN_ADDR", "127.0.0.1:8090"),
		ConfigDir:   filepath.Join(xdg, "webssh"),
		KeysDir:     envOr("WEBSSH_SSH_KEYS_DIR", filepath.Join(home, ".ssh")),
		MaxSessions: envInt("WEBSSH_MAX_SESSIONS", 64),
		OrphanGrace: envDur("WEBSSH_ORPHAN_GRACE", 5*time.Minute),
	}
}
