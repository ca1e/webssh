package transfer

import (
	"io"
	"os"
	"path"
	"path/filepath"

	"github.com/pkg/sftp"
)

// Side 传输一侧的文件系统抽象(远程 SFTP 或本机 os),路径拼接约定由实现负责:
// 远程侧使用 POSIX path,本机侧使用 filepath。
type Side interface {
	Name() string
	Join(elem ...string) string
	Dir(p string) string
	Stat(p string) (os.FileInfo, error)
	ReadDir(p string) ([]os.FileInfo, error)
	MkdirAll(p string) error
	Open(p string) (io.ReadCloser, error)
	Create(p string) (io.WriteCloser, error)
}

// ---------- 远程侧(SFTP) ----------

type remoteSide struct {
	sc   *sftp.Client
	name string
}

// NewRemoteSide 创建远程(SFTP)侧。name 用于任务展示。
func NewRemoteSide(sc *sftp.Client, name string) Side {
	return &remoteSide{sc: sc, name: name}
}

func (s *remoteSide) Name() string                            { return s.name }
func (s *remoteSide) Join(elem ...string) string              { return path.Join(elem...) }
func (s *remoteSide) Dir(p string) string                     { return path.Dir(p) }
func (s *remoteSide) Stat(p string) (os.FileInfo, error)      { return s.sc.Stat(p) }
func (s *remoteSide) ReadDir(p string) ([]os.FileInfo, error) { return s.sc.ReadDir(p) }
func (s *remoteSide) Open(p string) (io.ReadCloser, error)    { return s.sc.Open(p) }
func (s *remoteSide) Create(p string) (io.WriteCloser, error) { return s.sc.Create(p) }

func (s *remoteSide) MkdirAll(dir string) error {
	if fi, err := s.sc.Stat(dir); err == nil {
		if fi.IsDir() {
			return nil
		}
		return &pathExistsError{path: dir}
	}
	parent := path.Dir(dir)
	if parent != dir && parent != "/" && parent != "." {
		if err := s.MkdirAll(parent); err != nil {
			return err
		}
	}
	if err := s.sc.Mkdir(dir); err != nil {
		// 并发建目录时可能已被其他 worker 创建,二次确认
		if fi, err2 := s.sc.Stat(dir); err2 == nil && fi.IsDir() {
			return nil
		}
		return err
	}
	return nil
}

// ---------- 本机侧(os) ----------

type localSide struct{}

// NewLocalSide 创建本机侧。
func NewLocalSide() Side { return &localSide{} }

func (s *localSide) Name() string                            { return "本机" }
func (s *localSide) Join(elem ...string) string              { return filepath.Join(elem...) }
func (s *localSide) Dir(p string) string                     { return filepath.Dir(p) }
func (s *localSide) Stat(p string) (os.FileInfo, error)      { return os.Stat(p) }
func (s *localSide) Open(p string) (io.ReadCloser, error)    { return os.Open(p) }
func (s *localSide) Create(p string) (io.WriteCloser, error) { return os.Create(p) }
func (s *localSide) MkdirAll(dir string) error               { return os.MkdirAll(dir, 0o755) }

func (s *localSide) ReadDir(p string) ([]os.FileInfo, error) {
	entries, err := os.ReadDir(p)
	if err != nil {
		return nil, err
	}
	infos := make([]os.FileInfo, 0, len(entries))
	for _, e := range entries {
		if fi, err := e.Info(); err == nil {
			infos = append(infos, fi)
		}
	}
	return infos, nil
}

type pathExistsError struct{ path string }

func (e *pathExistsError) Error() string { return e.path + " 已存在且不是目录" }
