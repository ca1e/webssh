package mcp

// files 域工具:ssh_download_file —— 唯一文件工具。SFTP 流式下载到服务端
// 暂存目录($ConfigDir/mcp-downloads/),返回本机路径 + sha256 + 大小;
// 不整读进内存,流式写盘同时计数,超限即中止清理。路径无白名单限制,
// 仅要求绝对路径或 ~/ 形态(~/ 展开为远端主目录)。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"webssh/internal/sshmgr"
)

type downloadIn struct {
	Host string `json:"host" jsonschema:"主机名称或 ID"`
	Path string `json:"path" jsonschema:"远端文件路径(绝对路径或 ~/)"`
}

type downloadOut struct {
	LocalPath  string `json:"local_path"`
	RemotePath string `json:"remote_path"`
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
}

// downloadDirName 服务端暂存目录名(位于配置目录下)。
const downloadDirName = "mcp-downloads"

func (s *Server) toolDownloadFile(ctx context.Context, req *mcp.CallToolRequest, in downloadIn) (*mcp.CallToolResult, any, error) {
	start := time.Now()
	var out *downloadOut
	var sess *sshmgr.Session
	err := func() error {
		h, err := s.resolveHost(in.Host)
		if err != nil {
			return err
		}
		if err = rejectTelnet(h); err != nil {
			return err
		}
		if strings.TrimSpace(in.Path) == "" {
			return errors.New("缺少 path 参数;请给出远端文件的绝对路径或以 ~/ 开头的路径")
		}
		if !strings.HasPrefix(in.Path, "~") && !path.IsAbs(in.Path) {
			return fmt.Errorf("path 必须是绝对路径或以 ~/ 开头,得到 %q", in.Path)
		}
		sess, err = s.acquire(ctx, h)
		if err != nil {
			return err
		}
		sc, err := sess.SFTP()
		if err != nil {
			return fmt.Errorf("主机 %s 的 SFTP 通道不可用:%v", h.Name, err)
		}
		home, _ := sess.Home()
		var physical string
		if s.guard.Enabled() {
			// 白名单:展开 ~/、Clean、前缀匹配并逐级符号链接解析
			physical, err = s.guard.CheckRead(sc, in.Path, home)
			if err != nil {
				return fmt.Errorf("路径越界:%w", err)
			}
		} else {
			physical = path.Clean(in.Path)
			if strings.HasPrefix(in.Path, "~") {
				physical = path.Clean(home + strings.TrimPrefix(in.Path, "~"))
			}
		}
		remote, err := sc.Stat(physical)
		if err != nil {
			return fmt.Errorf("远端文件不可访问:%s(可能是权限不足或文件不存在)", physical)
		}
		if remote.IsDir() {
			return fmt.Errorf("%s 是目录;仅支持下载单个文件,可先用 ssh_exec(ls)查看目录内容", physical)
		}
		limit := s.conf.Defaults.DownloadMaxBytes
		if remote.Size() > limit {
			return fmt.Errorf("文件大小 %d 字节超过下载上限 %d;可先经 ssh_exec(head/tail)在远端裁剪后分片查看", remote.Size(), limit)
		}

		// 暂存目录 + 消毒文件名 + CreateTemp 随机后缀防碰撞
		dir := path.Join(s.cfgDir, downloadDirName)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("创建下载暂存目录失败:%v", err)
		}
		local, err := os.CreateTemp(dir, sanitizeName(path.Base(physical))+"-*")
		if err != nil {
			return fmt.Errorf("创建本地暂存文件失败:%v", err)
		}
		tmpName := local.Name()
		cleanup := func() { local.Close(); os.Remove(tmpName) }

		rf, err := sc.Open(physical)
		if err != nil {
			cleanup()
			return fmt.Errorf("打开远端文件失败:%v", err)
		}
		defer rf.Close()

		dig := sha256.New()
		counting := &countWriter{w: io.MultiWriter(local, dig), limit: limit}
		n, err := io.Copy(counting, rf)
		if err != nil {
			cleanup()
			return fmt.Errorf("下载中断:%v", err)
		}
		if counting.over {
			cleanup()
			return fmt.Errorf("文件实际大小超过上限 %d 字节,已中止并清理;可先在远端裁剪后分片下载", limit)
		}
		if err := local.Close(); err != nil {
			os.Remove(tmpName)
			return fmt.Errorf("写入本地暂存文件失败:%v", err)
		}
		out = &downloadOut{
			LocalPath:  tmpName,
			RemotePath: physical,
			SHA256:     hex.EncodeToString(dig.Sum(nil)),
			Size:       n,
		}
		return nil
	}()
	release(sess)
	defer func() { callLog("ssh_download_file", in.Host, err, start) }()
	if err != nil {
		return nil, nil, err
	}
	return nil, out, nil
}

// countWriter 计数并在超限后翻转 over(数据仍继续排空,尽早拿到错误返回)。
type countWriter struct {
	w     io.Writer
	limit int64
	n     int64
	over  bool
}

func (c *countWriter) Write(p []byte) (int, error) {
	if !c.over && c.n+int64(len(p)) > c.limit {
		c.over = true
	}
	c.n += int64(len(p))
	return c.w.Write(p)
}

// sanitizeName 远端 basename 消毒:仅保留文件名安全字符,空结果回退 file。
func sanitizeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := b.String()
	if s == "" || s == "." || s == ".." {
		return "file"
	}
	if len(s) > 48 {
		s = s[len(s)-48:]
	}
	return s
}
