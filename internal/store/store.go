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

	"webssh/internal/secrets"
)

const (
	AuthPassword = "password"
	AuthKey      = "key"

	ConnSSH    = "ssh"
	ConnTelnet = "telnet"
)

// NormalizeConnType 归一化连接方式:空值/未知值回落 SSH。
func NormalizeConnType(t string) string {
	if t == ConnTelnet {
		return ConnTelnet
	}
	return ConnSSH
}

type Host struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Host string `json:"host"`
	Port int    `json:"port"`
	// ConnType 连接协议,取值 NormalizeConnType 归一化后的结果
	ConnType string `json:"conn_type"`
	Username string `json:"username"`
	AuthType string `json:"auth_type"`
	// PasswordRef/PassphraseRef 指向 secrets 库中的加密凭据记录;
	// Host 结构体上不存在明文字段,任何序列化出口天然凭据零暴露。
	PasswordRef   string `json:"password_ref,omitempty"`
	KeyPath       string `json:"key_path,omitempty"`
	PassphraseRef string `json:"passphrase_ref,omitempty"`
	JumpID        string `json:"jump_id,omitempty"`
	Group         string `json:"group,omitempty"`
	StartupCmd    string `json:"startup_cmd,omitempty"`
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

// HostPublic 是主机配置的对外脱敏投影:只含元数据与"凭据是否存在"标识,
// Web hostView 与 MCP host 序列化共用同一函数,保证凭据零暴露口径一致。
type HostPublic struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	ConnType      string `json:"conn_type"`
	Username      string `json:"username"`
	AuthType      string `json:"auth_type"`
	KeyPath       string `json:"key_path"`
	JumpID        string `json:"jump_id"`
	Group         string `json:"group"`
	StartupCmd    string `json:"startup_cmd"`
	FollowSSHPath bool   `json:"follow_ssh_path"`
	HasPassword   bool   `json:"has_password"`
	HasPassphrase bool   `json:"has_passphrase"`
}

// PublicHost 生成主机的可序列化视图;ref 字段被折叠为 Has* 布尔,值本身不外泄。
func PublicHost(h Host) HostPublic {
	port := h.Port
	if port <= 0 {
		port = 22
	}
	return HostPublic{
		ID: h.ID, Name: h.Name, Host: h.Host, Port: port,
		ConnType: h.ConnType, Username: h.Username, AuthType: h.AuthType,
		KeyPath: h.KeyPath, JumpID: h.JumpID, Group: h.Group,
		StartupCmd: h.StartupCmd, FollowSSHPath: h.FollowSSHPath,
		HasPassword: h.PasswordRef != "", HasPassphrase: h.PassphraseRef != "",
	}
}

// Store 保存主机列表到 XDG 配置目录下的 hosts.json;凭据在 secrets 库。
type Store struct {
	mu    sync.Mutex
	path  string
	sec   *secrets.Store
	hosts []Host
}

// Open 加载 hosts.json;凭据只存 secrets 库引用,磁盘上无任何凭据密文。
// sec 必须先于本函数打开。
func Open(dir string, sec *secrets.Store) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "hosts.json"), sec: sec}
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
			s.hosts[i] = hostFromWire(w)
		}
	}
	return s, nil
}

// PutPassword/PutPassphrase 把表单明文凭据加密入库,返回引用 id。
// ref 非空时原位替换(更新不产生孤儿记录)。Web 表单保存路径经此进,
// handler 不直接接触 secrets 包。
func (s *Store) PutPassword(ref, plaintext string) (string, error) {
	return s.putSecret(ref, secrets.KindPassword, plaintext)
}

func (s *Store) PutPassphrase(ref, plaintext string) (string, error) {
	return s.putSecret(ref, secrets.KindPassphrase, plaintext)
}

func (s *Store) putSecret(ref, kind, plaintext string) (string, error) {
	if s.sec == nil {
		return "", errors.New("凭据库未初始化")
	}
	return s.sec.Put(ref, kind, plaintext)
}

// ForgetSecret 删除一条凭据记录,用于"测试连接"临时入库的表单明文收尾;
// 失败静默(孤儿密文无害:已加密且无任何主机引用)。
func (s *Store) ForgetSecret(ref string) {
	if s.sec != nil && ref != "" {
		_ = s.sec.Delete(ref)
	}
}

// FindByNameHost 按 (Name, Host) 精确查找现有主机(bundle 导入的凭据保留
// 判断需要读取旧引用)。
func (s *Store) FindByNameHost(name, host string) (Host, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.hosts {
		if h.Name == name && h.Host == host {
			return h, true
		}
	}
	return Host{}, false
}

// ExportWires 返回 bundle 序列化用的 wire:凭据从 secrets 库解密后按 base64
// 内嵌(加密模式逐条封入信封、明文模式整体写文件,两种出口都只在用户显式
// 触发导出时出现)。内部 hosts.json 从不以该形态落盘。
func (s *Store) ExportWires() ([]HostWire, error) {
	hosts := s.List()
	out := make([]HostWire, 0, len(hosts))
	for _, h := range hosts {
		w := HostWire{hostWire: toWire(h)}
		if h.PasswordRef != "" {
			pt, err := s.sec.Reveal(h.PasswordRef)
			if err != nil {
				return nil, fmt.Errorf("主机 %q 密码导出失败: %w", h.Name, err)
			}
			w.Password = b64enc(pt)
		}
		if h.PassphraseRef != "" {
			pt, err := s.sec.Reveal(h.PassphraseRef)
			if err != nil {
				return nil, fmt.Errorf("主机 %q 私钥口令导出失败: %w", h.Name, err)
			}
			w.Passphrase = b64enc(pt)
		}
		out = append(out, w)
	}
	return out, nil
}

// ImportWire 把 bundle wire(凭据为 base64 字段)转换为 Host:凭据换入
// secrets 库成引用;base64 非法时返回错误,供导入侧逐条跳过。
func (s *Store) ImportWire(w HostWire) (Host, error) {
	h := hostFromWire(w.hostWire)
	if w.Password != "" {
		pt, err := b64dec("密码", w.Name, w.Password)
		if err != nil {
			return Host{}, err
		}
		if pt != "" {
			ref, err := s.PutPassword("", pt)
			if err != nil {
				return Host{}, err
			}
			h.PasswordRef = ref
		}
	}
	if w.Passphrase != "" {
		pt, err := b64dec("私钥口令", w.Name, w.Passphrase)
		if err != nil {
			return Host{}, err
		}
		if pt != "" {
			ref, err := s.PutPassphrase("", pt)
			if err != nil {
				return Host{}, err
			}
			h.PassphraseRef = ref
		}
	}
	return h, nil
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

// HostWire 是 bundle 导入/导出的 wire 载荷:主机字段 + base64 凭据。
// hosts.json 磁盘格式(hostWire)只存引用,两者同构但凭据载体不同。
type HostWire struct {
	hostWire
	Password   string `json:"password,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
}

// hostWire 是 hosts.json 的磁盘格式:凭据只存 secrets 库引用。
type hostWire struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	ConnType      string `json:"conn_type,omitempty"`
	Username      string `json:"username"`
	AuthType      string `json:"auth_type"`
	PasswordRef   string `json:"password_ref,omitempty"`
	KeyPath       string `json:"key_path,omitempty"`
	PassphraseRef string `json:"passphrase_ref,omitempty"`
	JumpID        string `json:"jump_id,omitempty"`
	Group         string `json:"group,omitempty"`
	StartupCmd    string `json:"startup_cmd,omitempty"`
	FollowSSHPath bool   `json:"follow_ssh_path,omitempty"`
}

func b64enc(s string) string {
	if s == "" {
		return ""
	}
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// b64dec 严格解码 base64;解码失败视为非法数据(明文裸存不被接受)。
func b64dec(field, host string, s string) (string, error) {
	if s == "" {
		return "", nil
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", fmt.Errorf("主机 %q 的 %s 不是有效的 base64 编码", host, field)
	}
	return string(b), nil
}

func toWire(h Host) hostWire {
	return hostWire{
		ID: h.ID, Name: h.Name, Host: h.Host, Port: h.Port,
		ConnType: h.ConnType,
		Username: h.Username, AuthType: h.AuthType,
		PasswordRef: h.PasswordRef, KeyPath: h.KeyPath,
		PassphraseRef: h.PassphraseRef, JumpID: h.JumpID,
		Group: h.Group, StartupCmd: h.StartupCmd,
		FollowSSHPath: h.FollowSSHPath,
	}
}

func hostFromWire(w hostWire) Host {
	return Host{
		ID: w.ID, Name: w.Name, Host: w.Host, Port: w.Port,
		ConnType: NormalizeConnType(w.ConnType),
		Username: w.Username, AuthType: w.AuthType,
		PasswordRef: w.PasswordRef, KeyPath: w.KeyPath,
		PassphraseRef: w.PassphraseRef, JumpID: w.JumpID,
		Group: w.Group, StartupCmd: w.StartupCmd,
		FollowSSHPath: w.FollowSSHPath,
	}
}

// UpsertMany 批量合并导入主机:按 (Name, Host) 匹配,命中则整条覆盖但保留
// 现有 ID(不破坏会话/跳板引用),且新条目无凭据时保留原有引用(对齐
// pickSecret 的"留空=保留原值");未命中则追加,ID 为空或与现有主机冲突时
// 重新生成。同批内同名同主机的多条按后者覆盖前者。一次性落盘,返回写入条数。
func (s *Store) UpsertMany(hosts []Host) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range hosts {
		replaced := false
		for i := range s.hosts {
			if s.hosts[i].Name == h.Name && s.hosts[i].Host == h.Host {
				h.ID = s.hosts[i].ID
				if h.PasswordRef == "" {
					h.PasswordRef = s.hosts[i].PasswordRef
				}
				if h.PassphraseRef == "" {
					h.PassphraseRef = s.hosts[i].PassphraseRef
				}
				s.hosts[i] = h
				replaced = true
				break
			}
		}
		if !replaced {
			taken := h.ID == ""
			for _, e := range s.hosts {
				if e.ID == h.ID {
					taken = true
					break
				}
			}
			if taken {
				h.ID = NewID()
			}
			s.hosts = append(s.hosts, h)
		}
	}
	if err := s.save(); err != nil {
		return 0, err
	}
	return len(hosts), nil
}

// save 原子写入:先写临时文件再 rename,权限 0600(引用可留盘;凭据密文在
// secrets.json,由 secrets 包自行原子落盘)。
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
