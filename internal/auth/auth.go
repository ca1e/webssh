// Package auth 管理启动密码与解锁令牌。
// 密码以 bcrypt 哈希落盘 auth.json;解锁签发的 token 同步落盘(服务重启不失效),
// 有效期一天,到期或登出后需重新输入密码。
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// TokenTTL 解锁有效期:一天。
const TokenTTL = 24 * time.Hour

// state 是 auth.json 的磁盘格式:当前密码哈希与唯一有效令牌。
type state struct {
	PasswordHash string    `json:"password_hash,omitempty"`
	Token        string    `json:"token,omitempty"`
	TokenExpires time.Time `json:"token_expires,omitempty"`
}

type Store struct {
	mu    sync.Mutex
	path  string
	state state
}

// Open 加载配置目录下的 auth.json;文件不存在表示尚未设置启动密码。
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "auth.json")}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &s.state); err != nil {
			return nil, fmt.Errorf("解析 %s 失败: %w", s.path, err)
		}
	}
	return s, nil
}

// HasPassword 报告是否已设置启动密码。
func (s *Store) HasPassword() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.PasswordHash != ""
}

// SetPassword 首次设置启动密码,成功后签发新 token。
func (s *Store) SetPassword(pw string) (string, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.PasswordHash != "" {
		return "", time.Time{}, fmt.Errorf("启动密码已设置")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return "", time.Time{}, err
	}
	s.state.PasswordHash = string(hash)
	return s.issueLocked()
}

// Verify 校验启动密码,通过则签发新 token(旧 token 立即失效)。
func (s *Store) Verify(pw string) (string, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.PasswordHash == "" {
		return "", time.Time{}, fmt.Errorf("尚未设置启动密码")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(s.state.PasswordHash), []byte(pw)); err != nil {
		return "", time.Time{}, fmt.Errorf("密码错误")
	}
	return s.issueLocked()
}

// ChangePassword 校验当前密码后改为新密码,成功后签发新 token(旧 token 立即失效)。
func (s *Store) ChangePassword(current, newpw string) (string, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.PasswordHash == "" {
		return "", time.Time{}, fmt.Errorf("尚未设置启动密码")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(s.state.PasswordHash), []byte(current)); err != nil {
		return "", time.Time{}, fmt.Errorf("当前密码错误")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newpw), bcrypt.DefaultCost)
	if err != nil {
		return "", time.Time{}, err
	}
	s.state.PasswordHash = string(hash)
	return s.issueLocked()
}

// ValidToken 校验 token 是否为当前有效令牌(未过期)。
func (s *Store) ValidToken(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return token != "" && s.state.Token != "" &&
		s.state.Token == token && time.Now().Before(s.state.TokenExpires)
}

// Logout 注销当前 token,下次访问需重新解锁。
func (s *Store) Logout() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Token, s.state.TokenExpires = "", time.Time{}
	_ = s.saveLocked()
}

// issueLocked 签发新 token 并落盘;调用方须已持有 s.mu。
func (s *Store) issueLocked() (string, time.Time, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	token, exp := hex.EncodeToString(b), time.Now().Add(TokenTTL)
	s.state.Token, s.state.TokenExpires = token, exp
	if err := s.saveLocked(); err != nil {
		return "", time.Time{}, err
	}
	return token, exp, nil
}

// saveLocked 原子写入(临时文件 + rename,权限 0600);调用方须已持有 s.mu。
func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
