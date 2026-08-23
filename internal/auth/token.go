package auth

// Package auth 管理持久化静态 Bearer 令牌的生成、校验、热加载与轮换。

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const tokenBytes = 32

// Store 管理持久化访问令牌及其并发读取和热加载状态
type Store struct {
	path  string
	mu    sync.RWMutex
	token string
}

// Open 打开令牌存储，并在令牌文件不存在时创建初始令牌。
func Open(path string) (*Store, bool, error) {
	s := &Store{path: path}
	created := false
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		token, err := generate()
		if err != nil {
			return nil, false, err
		}
		if err := writeAtomic(path, token); err != nil {
			return nil, false, err
		}
		created = true
	} else if err != nil {
		return nil, false, fmt.Errorf("stat token file: %w", err)
	}
	if err := s.reload(); err != nil {
		return nil, false, err
	}
	return s, created, nil
}

// Validate 使用常量时间比较校验候选令牌是否有效。
func (s *Store) Validate(candidate string) bool {
	s.mu.RLock()
	current := s.token
	s.mu.RUnlock()
	a := []byte(candidate)
	b := []byte(current)
	if len(a) != len(b) {
		b = make([]byte, len(a))
	}
	return subtle.ConstantTimeCompare(a, b) == 1
}

// Token 返回当前加载的访问令牌。
func (s *Store) Token() string { s.mu.RLock(); defer s.mu.RUnlock(); return s.token }

// Fingerprint 返回当前令牌哈希的短指纹。
func (s *Store) Fingerprint() string {
	sum := sha256.Sum256([]byte(s.Token()))
	return fmt.Sprintf("%x", sum[:6])
}

// Watch 定期检查令牌文件变化，直至收到停止信号。
func (s *Store) Watch(stop <-chan struct{}, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			// reload 失败（如文件暂时不可读或权限异常）向上传播为告警，
			// 不再静默吞掉，便于运维及时发现令牌文件问题。
			if err := s.ReloadIfChanged(); err != nil {
				slog.Warn("failed to reload token file", "error", err)
			}
		}
	}
}

// ReloadIfChanged 重新读取令牌文件，以内容为准处理时间戳未变化的原子替换。
func (s *Store) ReloadIfChanged() error {
	return s.reload()
}

// reload 从文件读取并校验令牌后更新存储状态。
func (s *Store) reload() error {
	if err := enforceOwnerOnly(s.path); err != nil {
		return err
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("read token file: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if len(token) < 32 {
		return fmt.Errorf("token must be at least 32 characters")
	}
	s.mu.Lock()
	s.token = token
	s.mu.Unlock()
	return nil
}

// Rotate 生成新令牌并将其原子写入指定文件。
func Rotate(path string) (string, error) {
	token, err := generate()
	if err != nil {
		return "", err
	}
	if err := writeAtomic(path, token); err != nil {
		return "", err
	}
	return token, nil
}

// Read 读取令牌文件并去除首尾空白。
func Read(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// generate 生成具有安全随机性的 URL 安全令牌。
func generate() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// writeAtomic 以受限权限将令牌原子写入目标文件。
func writeAtomic(path, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create token directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".access-token-*")
	if err != nil {
		return fmt.Errorf("create temporary token file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(token + "\n"); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace token file: %w", err)
	}
	return nil
}

// Bearer 从 Authorization 请求头中提取 Bearer 令牌。
func Bearer(header string) (string, bool) {
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
		return "", false
	}
	return strings.TrimSpace(parts[1]), true
}
