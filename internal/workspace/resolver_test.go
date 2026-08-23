package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/iamyounglee/remote-workspace-mcp/internal/config"
)

// TestResolveWorkspaceAndWhitelists 验证工作区及读写白名单的权限边界。
func TestResolveWorkspaceAndWhitelists(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	readOnly := filepath.Join(base, "read-only")
	writable := filepath.Join(base, "writable")
	outside := filepath.Join(base, "outside")
	for _, path := range []string{root, readOnly, writable, outside} {
		if err := os.MkdirAll(path, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Defaults()
	cfg.Workspace.Root = root
	cfg.Paths.Readable = []string{readOnly}
	cfg.Paths.Writable = []string{writable}
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve("src/main.go", Write); err != nil {
		t.Fatalf("workspace relative write: %v", err)
	}
	if _, err := r.Resolve(filepath.Join(readOnly, "a.txt"), Read); err != nil {
		t.Fatalf("read whitelist: %v", err)
	}
	if _, err := r.Resolve(filepath.Join(readOnly, "a.txt"), Write); err == nil {
		t.Fatal("write to read-only root should fail")
	}
	if _, err := r.Resolve(filepath.Join(writable, "a.txt"), Write); err != nil {
		t.Fatalf("write whitelist: %v", err)
	}
	if _, err := r.Resolve(filepath.Join(outside, "a.txt"), Read); err == nil {
		t.Fatal("outside read should fail")
	}
	if _, err := r.Resolve("../outside/a.txt", Read); err == nil {
		t.Fatal("relative path traversal should fail")
	}
}

// TestResolveRejectsEscapingSymlink 验证路径解析拒绝经符号链接逃逸工作区。
func TestResolveRejectsEscapingSymlink(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Workspace.Root = root
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve("escape/secret.txt", Read); err == nil {
		t.Fatal("symlink escape should fail")
	}
}
