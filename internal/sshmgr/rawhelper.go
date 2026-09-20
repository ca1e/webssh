package sshmgr

import (
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"fmt"
	"io"
	"path"
	"strings"
)

// 内嵌的远端辅助程序(linux 静态二进制,gzip 压缩):
// 由 `make helper`(tools/socks5-helper)交叉编译产出,构建产物随源码
// 入库以便 `go build`/`go test` 开箱即用;Makefile 的 build 目标会先重新
// 生成,保证与服务端代码一致。
//
//go:embed rawhelper/*.gz
var rawHelperFS embed.FS

// rawHelperVersion 辅助程序协议版本;helper 与本文件同步演进,
// 内容寻址的部署文件名使其自动失效重传,无需手工清理。
const rawHelperVersion = "1"

// rawHelperFiles 按 GOARCH 索引的内嵌产物。
var rawHelperFiles = map[string]string{
	"amd64": "rawhelper/socks5-helper-linux-amd64.gz",
	"arm64": "rawhelper/socks5-helper-linux-arm64.gz",
}

// rawHelperHashes 各架构 gz 内容 sha256 前 8 位,用作远端部署文件名:
// 内容寻址 → 版本升级/架构变化自动换名,远端永不残留旧版可执行。
var rawHelperHashes = map[string]string{}

func init() {
	for arch, name := range rawHelperFiles {
		b, err := rawHelperFS.ReadFile(name)
		if err != nil {
			panic("rawhelper 内嵌产物缺失(先运行 make helper): " + err.Error())
		}
		rawHelperHashes[arch] = fmt.Sprintf("%x", sha256.Sum256(b))[:8]
	}
}

// deployRawHelper 确保远端存在可执行的辅助程序并返回其路径。
// 策略:按内容哈希命名部署于 ~/.webssh/bin(回退 /tmp),已存在且
// 可执行(版本校验通过)则直接复用;否则优先经 SFTP 上传,SFTP 不可用
// 时回退 exec 通道 base64 注入。调用方(raw_forwarder.start)已串行化,
// 无需再防并发。
func (s *Session) deployRawHelper() (string, error) {
	arch, err := s.remoteArch()
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("webssh-rawfwd-v%s-%s-%s", rawHelperVersion, rawHelperHashes[arch], arch)

	// 部署目录:主目录不可得(如 SFTP 被禁)时回退 /tmp。
	dir := "/tmp/.webssh-bin"
	if home, err := s.Home(); err == nil && home != "" {
		dir = path.Join(home, ".webssh", "bin")
	}
	target := path.Join(dir, name)

	// 已存在且能正确执行 → 复用,省去每次 1-2MB 的上传。
	if out, err := s.Run(fmt.Sprintf("test -x %s && %s version", shQuote(target), shQuote(target))); err == nil &&
		strings.TrimSpace(out) == rawHelperVersionOutput() {
		return target, nil
	}

	upErr := s.uploadHelperSFTP(arch, target)
	if upErr != nil {
		// SFTP 通道失败(子系统被禁用等):回退 exec + base64。
		if err2 := s.uploadHelperBase64(arch, target); err2 != nil {
			return "", fmt.Errorf("辅助程序上传失败(SFTP: %s;exec 回退: %s)",
				uploadErrMsg(upErr), uploadErrMsg(err2))
		}
	}

	// 上传后校验可执行性与架构匹配(错误架构的二进制 exec 会失败)。
	out, err := s.Run(shQuote(target) + " version")
	if err != nil || strings.TrimSpace(out) != rawHelperVersionOutput() {
		return "", fmt.Errorf("辅助程序部署后校验失败: %s", strings.TrimSpace(out))
	}
	// 尽力清理同目录旧版本(升级/重编译后内容寻址改名,旧文件不再被引用);
	// 正在被运行的旧进程不受影响(Linux 删除已打开文件安全)。
	_, _ = s.Run(fmt.Sprintf("find %s -name 'webssh-rawfwd-*' ! -name %s -type f -delete 2>/dev/null",
		shQuote(dir), shQuote(path.Base(target))))
	return target, nil
}

func rawHelperVersionOutput() string { return "webssh-rawfwd " + rawHelperVersion }

// remoteArch 探测远端机器架构,映射到内嵌产物的 GOARCH。
func (s *Session) remoteArch() (string, error) {
	out, err := s.Run("uname -m")
	if err != nil {
		return "", fmt.Errorf("探测远端架构失败: %s", strings.TrimSpace(out))
	}
	switch m := strings.TrimSpace(out); m {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("远端架构 %q 暂无预编译辅助程序(支持 amd64/arm64)", m)
	}
}

// uploadHelperSFTP 经既有 SFTP 通道上传:gz 流式解压写盘,0700 权限,
// 先写 .part 再原子改名,远端不会出现半个可执行文件。
func (s *Session) uploadHelperSFTP(arch, target string) error {
	sc, err := s.SFTP()
	if err != nil {
		return err
	}
	gz, err := rawHelperFS.Open(rawHelperFiles[arch])
	if err != nil {
		return err
	}
	defer gz.Close()
	zr, err := gzip.NewReader(gz)
	if err != nil {
		return err
	}
	defer zr.Close()

	dir := path.Dir(target)
	if err := sc.MkdirAll(dir); err != nil {
		return fmt.Errorf("创建远端目录 %s 失败: %w", dir, err)
	}
	part := target + ".part"
	f, err := sc.Create(part)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, zr); err != nil {
		f.Close()
		sc.Remove(part)
		return fmt.Errorf("写入远端文件失败: %w", err)
	}
	if err := f.Chmod(0o700); err != nil {
		f.Close()
		sc.Remove(part)
		return err
	}
	if err := f.Close(); err != nil {
		sc.Remove(part)
		return err
	}
	// 目标已存在(校验失败的旧版)先删,再原子落位。
	_ = sc.Remove(target)
	return sc.Rename(part, target)
}

// uploadHelperBase64 exec 通道回退:把 gz 内容 base64 后经 session stdin
// 注入远端 `base64 -d` 解码落盘。远端无需任何额外工具(base64 属 coreutils);
// 相比拼接超长命令行(受 MAX_ARG_STRLEN 128KB 限制),走 stdin 无长度上限。
func (s *Session) uploadHelperBase64(arch, target string) error {
	gz, err := rawHelperFS.Open(rawHelperFiles[arch])
	if err != nil {
		return err
	}
	defer gz.Close()
	zr, err := gzip.NewReader(gz)
	if err != nil {
		return err
	}
	defer zr.Close()

	if err := s.checkOpen(); err != nil {
		return err
	}
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	sess, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("创建执行会话失败: %w", err)
	}
	defer sess.Close()
	stdin, err := sess.StdinPipe()
	if err != nil {
		return err
	}
	dir := path.Dir(target)
	cmd := fmt.Sprintf(`mkdir -p %s && base64 -d > %s && chmod 700 %s && mv -f %s %s`,
		shQuote(dir), shQuote(target+".part"), shQuote(target+".part"),
		shQuote(target+".part"), shQuote(target))
	if err := sess.Start(cmd); err != nil {
		stdin.Close()
		return fmt.Errorf("启动解码命令失败: %w", err)
	}
	enc := base64.NewEncoder(base64.StdEncoding, stdin)
	if _, err := io.Copy(enc, zr); err != nil {
		enc.Close()
		stdin.Close()
		return fmt.Errorf("传输辅助程序失败: %w", err)
	}
	// Close 编码器冲刷尾部 padding,再关 stdin 触发远端解码收尾。
	if err := enc.Close(); err != nil {
		stdin.Close()
		return err
	}
	if err := stdin.Close(); err != nil {
		return err
	}
	if err := sess.Wait(); err != nil {
		return fmt.Errorf("远端解码执行失败: %w", err)
	}
	return nil
}

// uploadErrMsg 压缩错误信息为单行片段,便于拼接展示。
func uploadErrMsg(err error) string {
	msg := strings.TrimSpace(err.Error())
	if len(msg) > 120 {
		msg = msg[:120] + "…"
	}
	return msg
}
