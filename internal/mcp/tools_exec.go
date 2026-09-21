package mcp

// exec 域工具:ssh_exec —— 唯一执行入口。接受任意命令字符串,经 SSH exec
// 通道执行并返回 stdout/stderr/exit_code。超时与输出上限由 mcp.json defaults
// 控制(ExecTimeout/ReadMaxBytes)。

import (
	"context"
	"errors"
	"fmt"
"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"webssh/internal/sshmgr"
)

type execIn struct {
	Host    string `json:"host" jsonschema:"主机名称或 ID"`
	Command string `json:"command" jsonschema:"要执行的命令(经登录 shell 执行)"`
}

type execOut struct {
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	ExitCode  int    `json:"exit_code"`
	Truncated bool   `json:"truncated"`
}

func (s *Server) toolExec(ctx context.Context, req *mcp.CallToolRequest, in execIn) (*mcp.CallToolResult, any, error) {
	start := time.Now()
	var (
		out  *execOut
		sess *sshmgr.Session
	)
	err := func() error {
		h, err := s.resolveHost(in.Host)
		if err != nil {
			return err
		}
		if err = rejectTelnet(h); err != nil {
			return err
		}
		cmd := strings.TrimSpace(in.Command)
		if cmd == "" {
			return errors.New("缺少 command 参数;请给出要在远端执行的命令字符串")
		}

		sess, err = s.acquire(ctx, h)
		if err != nil {
			return err
		}
		// 白名单打开时:提取命令中的路径形 token(~ 或 / 开头)逐个校验
		if s.guard.Enabled() {
			sc, err := sess.SFTP()
			if err != nil {
				return fmt.Errorf("主机 %s 的 SFTP 通道不可用:%v", h.Name, err)
			}
			home, _ := sess.Home()
			for _, cand := range extractCommandPaths(cmd) {
				if _, err := s.guard.CheckRead(sc, cand, home); err != nil {
					return fmt.Errorf("命令包含白名单外路径:%v", err)
				}
			}
		}
		timeout := s.conf.Defaults.ExecTimeout
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		res, err := sess.RunExec(runCtx, cmd, s.conf.Defaults.ReadMaxBytes)
		if err != nil {
			if runCtx.Err() != nil {
				return fmt.Errorf("命令执行超过 %s 已被中止;可拆分命令或调整 mcp.json 的 defaults.exec_timeout", timeout)
			}
			return fmt.Errorf("执行失败:%v", err)
		}
		out = &execOut{
			Stdout:    res.Stdout,
			Stderr:    res.Stderr,
			ExitCode:  res.ExitCode,
			Truncated: res.Truncated,
		}
		if res.Truncated {
			out.Stderr += fmt.Sprintf("\n[输出超过 %d 字节上限已被截断;可缩小查询范围]", s.conf.Defaults.ReadMaxBytes)
		}
		return nil
	}()
	release(sess)
	label := in.Host + "#" + firstLine(in.Command)
	defer func() { callLog("ssh_exec", label, err, start) }()
	if err != nil {
		return nil, nil, err
	}
	return nil, out, nil
}

// firstLine 取命令首行作日志标签,超长截断。
func firstLine(cmd string) string {
	for i, r := range cmd {
		if r == '\n' || r == ';' {
			if i > 80 {
				return cmd[:80] + "…"
			}
			return cmd[:i]
		}
		if i == 80 {
			return cmd[:80] + "…"
		}
	}
	return cmd
}

// extractCommandPaths 从命令字符串中提取路径形 token:按空白与常见 shell
// 分隔符切分,再去掉引号与 "=",":" 等粘连前缀;以 / 或 ~ 开头的 token 视为
// 路径候选。启发式,只做白名单的粗筛防线(细粒度由 pathguard 符号链接解析兜底)。
func extractCommandPaths(cmd string) []string {
	fields := strings.FieldsFunc(cmd, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', ';', '|', '&', '(', ')', '<', '>', '"', '\'', '`':
			return true
		}
		return false
	})
	seen := map[string]bool{}
	var paths []string
	for _, f := range fields {
		if i := strings.IndexAny(f, "=:"); i >= 0 {
			f = f[i+1:]
		}
		f = strings.TrimRight(f, ",:()")
		if (strings.HasPrefix(f, "/") || strings.HasPrefix(f, "~")) && !seen[f] {
			seen[f] = true
			paths = append(paths, f)
		}
	}
	return paths
}
