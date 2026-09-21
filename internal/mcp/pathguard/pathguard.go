// Package pathguard 实现 MCP 的可选读侧路径白名单:开关关闭时不做任何
// 路径限制(仅做 ~ 展开/规范化);开关打开时,ssh_download_file 的 path 与
// ssh_exec 命令中出现的路径均须落在白名单前缀内——服务侧统一做 ~ 展开、
// path.Clean、前缀匹配与逐级符号链接解析,绝不信任传入的已规范化路径。
package pathguard

import (
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/pkg/sftp"
)

// Guard 白名单持有者:enabled=false 时全部检查直通(仅规范化)。
type Guard struct {
	enabled  bool
	prefixes []string // 已规范化、以 / 结尾的前缀
}

// New 校验并构建 Guard;任何相对/未规范化前缀直接报错(配置非法)。
func New(enabled bool, prefixes []string) (*Guard, error) {
	g := &Guard{enabled: enabled}
	for _, p := range prefixes {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !path.IsAbs(p) || path.Clean(p) != strings.TrimSuffix(p, "/") {
			return nil, fmt.Errorf("白名单路径 %q 必须是规范化绝对路径(如 /var/log/)", p)
		}
		g.prefixes = append(g.prefixes, path.Clean(p)+"/")
	}
	return g, nil
}

// Enabled 白名单开关是否打开。
func (g *Guard) Enabled() bool { return g.enabled }

// Prefixes 当前生效的规范化前缀(UI 展示/错误文案用)。
func (g *Guard) Prefixes() []string { return g.prefixes }

// Expand 展开 ~/ 并 Clean;绝对路径原样 Clean。不做白名单校验。
func (g *Guard) Expand(p, home string) string {
	if strings.HasPrefix(p, "~") {
		p = home + strings.TrimPrefix(p, "~")
	}
	return path.Clean(p)
}

// CheckStatic 纯静态校验(无需会话):展开 + 前缀匹配,返回物理路径。
func (g *Guard) CheckStatic(p, home string) (string, error) {
	physical := g.Expand(p, home)
	if err := g.checkPrefix(physical); err != nil {
		return "", err
	}
	return physical, nil
}

// CheckRead 完整校验:展开 + 逐级符号链接解析 + 前缀匹配。
func (g *Guard) CheckRead(sc *sftp.Client, p, home string) (string, error) {
	physical, err := g.CheckStatic(p, home)
	if err != nil {
		return "", err
	}
	resolved, err := resolveSymlinks(sc, physical)
	if err != nil {
		return "", err
	}
	if err := g.checkPrefix(resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

func (g *Guard) checkPrefix(physical string) error {
	if !g.enabled {
		return nil
	}
	for _, pre := range g.prefixes {
		if physical == strings.TrimSuffix(pre, "/") || strings.HasPrefix(physical, pre) {
			return nil
		}
	}
	return fmt.Errorf("路径 %s 不在允许的可读范围内;允许前缀: %v", physical, g.prefixes)
}

// resolveSymlinks 逐级解析符号链接(最多 16 跳防环),返回物理路径。
// 任一级 Lstat 失败(如文件不存在)按原样返回,交由后续 Stat/Open 报错。
func resolveSymlinks(sc *sftp.Client, p string) (string, error) {
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	cur := ""
	for _, part := range parts {
		cur = path.Join(cur, part)
		for range 16 {
			fi, err := sc.Lstat(cur)
			if err != nil {
				return p, nil // 不存在/不可 Lstat:交给调用方后续操作报错
			}
			if fi.Mode()&os.ModeSymlink == 0 {
				break
			}
			target, err := sc.ReadLink(cur)
			if err != nil {
				return "", fmt.Errorf("解析符号链接 %s 失败:%v", cur, err)
			}
			if path.IsAbs(target) {
				cur = path.Clean(target)
			} else {
				cur = path.Clean(path.Join(path.Dir(cur), target))
			}
		}
	}
	return cur, nil
}
