package workspace

// Package workspace 根据工作区根目录与路径白名单解析并校验文件访问路径，防止越权访问。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/iamyounglee/remote-workspace-mcp/internal/config"
)

// Access 表示路径解析所需的访问权限类型。
type Access int

const (
	Read Access = iota
	Write
)

// Resolver 根据工作区和路径白名单解析并校验文件路径
type Resolver struct {
	workspace string
	readable  []string
	writable  []string
}

// New 根据配置创建完成规范化的工作区路径解析器。
func New(cfg config.Config) (*Resolver, error) {
	workspace, err := canonicalExisting(cfg.Workspace.Root)
	if err != nil {
		return nil, fmt.Errorf("workspace root: %w", err)
	}
	r := &Resolver{workspace: workspace}
	for _, path := range cfg.Paths.Readable {
		p, err := canonicalExisting(path)
		if err != nil {
			return nil, fmt.Errorf("readable path %q: %w", path, err)
		}
		r.readable = append(r.readable, p)
	}
	for _, path := range cfg.Paths.Writable {
		p, err := canonicalExisting(path)
		if err != nil {
			return nil, fmt.Errorf("writable path %q: %w", path, err)
		}
		r.writable = append(r.writable, p)
	}
	return r, nil
}

// Workspace 返回规范化后的工作区根目录。
func (r *Resolver) Workspace() string { return r.workspace }

// Readable 返回工作区外可读路径白名单的副本。
func (r *Resolver) Readable() []string { return append([]string(nil), r.readable...) }

// Writable 返回工作区外可写路径白名单的副本。
func (r *Resolver) Writable() []string { return append([]string(nil), r.writable...) }

// Resolve 规范化输入路径并校验其是否具备指定访问权限。
func (r *Resolver) Resolve(input string, access Access) (string, error) {
	if strings.TrimSpace(input) == "" {
		input = "."
	}
	var candidate string
	if filepath.IsAbs(input) {
		candidate = filepath.Clean(input)
	} else {
		candidate = filepath.Join(r.workspace, filepath.Clean(input))
		if !within(r.workspace, candidate) {
			return "", fmt.Errorf("relative path escapes workspace")
		}
	}
	resolved, err := canonicalCandidate(candidate)
	if err != nil {
		return "", err
	}
	if r.allowed(resolved, access) {
		return resolved, nil
	}
	kind := "read"
	if access == Write {
		kind = "write"
	}
	return "", fmt.Errorf("path is outside configured %s roots", kind)
}

// Display 将工作区内路径转换为相对展示路径。
func (r *Resolver) Display(path string) string {
	if within(r.workspace, path) {
		rel, err := filepath.Rel(r.workspace, path)
		if err == nil {
			return rel
		}
	}
	return filepath.Clean(path)
}

// allowed 判断路径是否位于工作区或对应权限白名单内。
func (r *Resolver) allowed(path string, access Access) bool {
	if within(r.workspace, path) {
		return true
	}
	if access == Write {
		return withinAny(r.writable, path)
	}
	return withinAny(r.readable, path) || withinAny(r.writable, path)
}

// withinAny 判断路径是否位于任一指定根目录内。
func withinAny(roots []string, path string) bool {
	for _, root := range roots {
		if within(root, path) {
			return true
		}
	}
	return false
}

// within 判断路径是否等于或位于指定根目录内。
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// canonicalExisting 返回已存在路径解析符号链接后的绝对规范路径。
func canonicalExisting(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("not a directory")
	}
	return filepath.Clean(resolved), nil
}

// canonicalCandidate 在解析现有父目录符号链接后规范化候选路径。
func canonicalCandidate(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	current := abs
	var suffix []string
	for {
		_, err := os.Lstat(current)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no existing parent for path")
		}
		suffix = append([]string{filepath.Base(current)}, suffix...)
		current = parent
	}
	resolved, err := filepath.EvalSymlinks(current)
	if err != nil {
		return "", err
	}
	for _, part := range suffix {
		resolved = filepath.Join(resolved, part)
	}
	return filepath.Clean(resolved), nil
}
