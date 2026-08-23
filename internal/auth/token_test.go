package auth

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestStorePersistsAndRotates 验证令牌存储可持久化、轮换并重新加载令牌。
func TestStorePersistsAndRotates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state", "access-token")
	store, created, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("expected initial token creation")
	}
	first := store.Token()
	if !store.Validate(first) {
		t.Fatal("initial token did not validate")
	}
	secondStore, created, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if created || secondStore.Token() != first {
		t.Fatal("token was not persisted")
	}
	beforeRotateInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := Rotate(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, beforeRotateInfo.ModTime(), beforeRotateInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	if rotated == first {
		t.Fatal("rotation did not change token")
	}
	if err := secondStore.ReloadIfChanged(); err != nil {
		t.Fatal(err)
	}
	if secondStore.Validate(first) || !secondStore.Validate(rotated) {
		t.Fatal("store did not reload rotated token")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows 使用 ACL 而非 POSIX 权限位，无法保证 0o600，仅在其他平台断言。
	if runtime.GOOS != "windows" {
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("token permissions = %o, want 600", info.Mode().Perm())
		}
	}
}
