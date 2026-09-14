package store

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

const (
	AuthPassword = "password"
	AuthKey      = "key"
)

type Host struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Username   string `json:"username"`
	AuthType   string `json:"auth_type"`
	Password   string `json:"password,omitempty"`
	KeyPath    string `json:"key_path,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
	JumpID     string `json:"jump_id,omitempty"`
	Group      string `json:"group,omitempty"`
	StartupCmd string `json:"startup_cmd,omitempty"`
	// FollowSSHPath 勾选后注入 OSC 7 提示符钩子,SFTP 面板跟随终端 cd;
	// 注入路径经 exec 启动 shell,sshd 不打印 Last login/MOTD 横幅。
	FollowSSHPath bool `json:"follow_ssh_path"`
}

func NewID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Store 保存主机列表到 XDG 配置目录下的 hosts.json。
type Store struct {
	mu    sync.Mutex
	path  string
	hosts []Host
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "hosts.json")}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) > 0 {
		var wires []hostWire
		if err := json.Unmarshal(data, &wires); err != nil {
			return nil, fmt.Errorf("解析 %s 失败: %w", s.path, err)
		}
		s.hosts = make([]Host, len(wires))
		for i, w := range wires {
			h, err := fromWire(w)
			if err != nil {
				return nil, err
			}
			s.hosts[i] = h
		}
	}
	return s, nil
}

func (s *Store) List() []Host {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Host, len(s.hosts))
	copy(out, s.hosts)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Store) Get(id string) (Host, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.hosts {
		if h.ID == id {
			return h, true
		}
	}
	return Host{}, false
}

// Put 新增或按 ID 更新主机;ID 为空时自动生成。
func (s *Store) Put(h Host) (Host, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h.ID == "" {
		h.ID = NewID()
		s.hosts = append(s.hosts, h)
	} else {
		found := false
		for i := range s.hosts {
			if s.hosts[i].ID == h.ID {
				s.hosts[i] = h
				found = true
				break
			}
		}
		if !found {
			return Host{}, fmt.Errorf("主机 %s 不存在", h.ID)
		}
	}
	if err := s.save(); err != nil {
		return Host{}, err
	}
	return h, nil
}

func (s *Store) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.hosts {
		if s.hosts[i].ID == id {
			s.hosts = append(s.hosts[:i], s.hosts[i+1:]...)
			if err := s.save(); err != nil {
				return false
			}
			return true
		}
	}
	return false
}

// hostWire 是 hosts.json 的磁盘格式:密码/口令以 base64 编码存储(混淆,非加密)。
type hostWire struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Username   string `json:"username"`
	AuthType   string `json:"auth_type"`
	Password   string `json:"password,omitempty"`
	KeyPath    string `json:"key_path,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
	JumpID     string `json:"jump_id,omitempty"`
	Group      string `json:"group,omitempty"`
	StartupCmd string `json:"startup_cmd,omitempty"`
}

func b64enc(s string) string {
	if s == "" {
		return ""
	}
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// b64dec 严格解码 base64;解码失败视为非法配置(不支持旧版明文格式)。
func b64dec(field, host string, s string) (string, error) {
	if s == "" {
		return "", nil
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", fmt.Errorf("主机 %q 的 %s 不是有效的 base64 编码(旧版明文配置已不再支持,请删除 hosts.json 中该主机后重建)", host, field)
	}
	return string(b), nil
}

func toWire(h Host) hostWire {
	return hostWire{
		ID: h.ID, Name: h.Name, Host: h.Host, Port: h.Port,
		Username: h.Username, AuthType: h.AuthType,
		Password: b64enc(h.Password), KeyPath: h.KeyPath,
		Passphrase: b64enc(h.Passphrase), JumpID: h.JumpID,
		Group: h.Group, StartupCmd: h.StartupCmd,
	}
}

func fromWire(w hostWire) (Host, error) {
	password, err := b64dec("密码", w.Name, w.Password)
	if err != nil {
		return Host{}, err
	}
	passphrase, err := b64dec("私钥口令", w.Name, w.Passphrase)
	if err != nil {
		return Host{}, err
	}
	return Host{
		ID: w.ID, Name: w.Name, Host: w.Host, Port: w.Port,
		Username: w.Username, AuthType: w.AuthType,
		Password: password, KeyPath: w.KeyPath,
		Passphrase: passphrase, JumpID: w.JumpID,
		Group: w.Group, StartupCmd: w.StartupCmd,
	}, nil
}

// save 原子写入:先写临时文件再 rename,权限 0600(内容含密码,base64 编码)。
func (s *Store) save() error {
	wires := make([]hostWire, len(s.hosts))
	for i, h := range s.hosts {
		wires[i] = toWire(h)
	}
	data, err := json.MarshalIndent(wires, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
