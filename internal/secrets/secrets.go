// Package secrets 提供字段级 AES-256-GCM 凭据库:主机密码/私钥口令不再
// 落在 hosts.json,而是加密后存入独立的 secrets.json。
//
// 设计不变式:
//   - master.key 永不出本机(32B 随机,首启自动生成);
//   - 每条凭据独立 nonce 与 AAD(绑定 secret id),密文无法跨记录调换;
//   - Reveal 只允许连接层(sshmgr)调用,任何 Web/MCP 序列化路径不可达。
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Kind 凭据类型标识(与历史 base64 字段语义对齐)。
const (
	KindPassword   = "password"
	KindPassphrase = "passphrase"
)

const (
	keyFileName   = "master.key"
	storeFileName = "secrets.json"
	keyBytes      = 32 // AES-256
	nonceBytes    = 12 // GCM 标准 nonce
)

// ErrKeyMissing secrets.json 存在而主密钥缺失:显式报错,绝不静默重建
// (重建会让全部已存凭据不可解,静默降级等于数据损毁)。
var ErrKeyMissing = errors.New("master.key 缺失但 secrets.json 已存在:请恢复 master.key 或设置 WEBSSH_MASTER_KEY,否则已存凭据不可解")

type record struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
}

// Store 并发安全的凭据库,落盘格式 {"secrets":[record...]}。
type Store struct {
	mu      sync.Mutex
	path    string
	key     []byte
	records map[string]record
}

// Open 加载(或首次生成)主密钥并读取 secrets.json。
// WEBSSH_MASTER_KEY(base64 的 32B 密钥)优先于 master.key 文件,供容器注入。
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, storeFileName), records: map[string]record{}}

	key, err := loadKey(dir)
	if err != nil {
		return nil, err
	}
	s.key = key

	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var file struct {
		Secrets []record `json:"secrets"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", s.path, err)
	}
	for _, r := range file.Secrets {
		if r.ID == "" || len(r.Nonce) != nonceBytes || len(r.Ciphertext) == 0 {
			return nil, fmt.Errorf("%s 中存在非法凭据记录(id=%q),拒绝加载", s.path, r.ID)
		}
		s.records[r.ID] = r
	}
	return s, nil
}

// loadKey 按 WEBSSH_MASTER_KEY → master.key 文件 → 首启自动生成 的顺序取密钥。
func loadKey(dir string) ([]byte, error) {
	if v := os.Getenv("WEBSSH_MASTER_KEY"); v != "" {
		key, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("WEBSSH_MASTER_KEY 不是合法 base64: %w", err)
		}
		if len(key) != keyBytes {
			return nil, fmt.Errorf("WEBSSH_MASTER_KEY 解码后应为 %d 字节,实际 %d", keyBytes, len(key))
		}
		return key, nil
	}
	keyPath := filepath.Join(dir, keyFileName)
	key, err := os.ReadFile(keyPath)
	if err == nil {
		if len(key) != keyBytes {
			return nil, fmt.Errorf("%s 长度异常(期望 %d 字节,实际 %d),拒绝加载", keyPath, keyBytes, len(key))
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// 首启:secrets.json 已存在说明密钥曾被生成过(可能被误删),不得静默重建。
	if _, statErr := os.Stat(filepath.Join(dir, storeFileName)); statErr == nil {
		return nil, ErrKeyMissing
	}
	key = make([]byte, keyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

func (s *Store) aead() (cipher.AEAD, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Put 加密存储凭据并返回引用 id;id 为空时自动生成,非空时原位替换
// (更新主机密码复用同一 id,不产生孤儿记录)。
func (s *Store) Put(id, kind, plaintext string) (string, error) {
	if kind != KindPassword && kind != KindPassphrase {
		return "", fmt.Errorf("未知凭据类型 %q", kind)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		id = hex.EncodeToString(b)
	}
	a, err := s.aead()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, nonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	// AAD 绑定 id:整条记录被调换到另一 id 下时 GCM 校验必失败。
	ct := a.Seal(nil, nonce, []byte(plaintext), []byte(id))
	s.records[id] = record{ID: id, Kind: kind, Nonce: nonce, Ciphertext: ct}
	if err := s.saveLocked(); err != nil {
		delete(s.records, id)
		return "", err
	}
	return id, nil
}

// Reveal 解密返回凭据明文。仅供连接层(sshmgr 拨号)调用;
// Web/MCP 的任何序列化出口不得经过此方法。
func (s *Store) Reveal(ref string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[ref]
	if !ok {
		return "", fmt.Errorf("凭据引用 %s 不存在(可能已被删除,请重新配置该主机密码)", ref)
	}
	a, err := s.aead()
	if err != nil {
		return "", err
	}
	pt, err := a.Open(nil, r.Nonce, r.Ciphertext, []byte(r.ID))
	if err != nil {
		// GCM 校验失败 = 密文/nonce 被篡改或密钥不匹配;不区分细节避免探测。
		return "", fmt.Errorf("凭据 %s 解密失败(数据损坏或主密钥不匹配)", ref)
	}
	return string(pt), nil
}

// Delete 移除凭据记录。
func (s *Store) Delete(ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.records[ref]; !ok {
		return fmt.Errorf("凭据引用 %s 不存在", ref)
	}
	delete(s.records, ref)
	return s.saveLocked()
}

// saveLocked 原子写(tmp+rename),权限 0600。调用方须持有 s.mu。
func (s *Store) saveLocked() error {
	list := make([]record, 0, len(s.records))
	for _, r := range s.records {
		list = append(list, r)
	}
	// 稳定排序,避免 map 遍历顺序造成无意义 diff。
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].ID < list[j-1].ID; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
	data, err := json.MarshalIndent(struct {
		Secrets []record `json:"secrets"`
	}{list}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
