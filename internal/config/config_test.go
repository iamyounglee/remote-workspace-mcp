package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadYAML 验证 YAML 配置可覆盖默认监听地址和文件限制。
func TestLoadYAML(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "remote-workspace-mcp.yaml")
	data := fmt.Sprintf("workspace:\n  root: %q\nserver:\n  listen: 127.0.0.1:9090\nfiles:\n  max_upload_bytes: 2048\n", root)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != "127.0.0.1:9090" || cfg.Files.MaxUploadBytes != 2048 {
		t.Fatalf("YAML overrides not loaded: %#v", cfg)
	}
	if cfg.Files.MaxReadBytes != 1<<20 {
		t.Fatalf("default max_read_bytes lost: %d", cfg.Files.MaxReadBytes)
	}
}

// TestLoadYAMLRejectsUnknownFields 验证加载配置时拒绝未知 YAML 字段。
func TestLoadYAMLRejectsUnknownFields(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "remote-workspace-mcp.yaml")
	data := fmt.Sprintf("workspace:\n  root: %q\nserver:\n  unknown_option: true\n", root)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown_option") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

// TestLoadYAMLRejectsMultipleDocuments 验证加载配置时拒绝多个 YAML 文档。
func TestLoadYAMLRejectsMultipleDocuments(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "remote-workspace-mcp.yaml")
	data := fmt.Sprintf("workspace:\n  root: %q\n---\nworkspace:\n  root: %q\n", root, root)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "multiple documents") {
		t.Fatalf("expected multiple document error, got %v", err)
	}
}

// TestDefaultsConfiguresFileLogging 验证默认配置启用指定路径和轮转限制。
func TestDefaultsConfiguresFileLogging(t *testing.T) {
	cfg := Defaults()
	if cfg.Logging.File != "./logs/remote-workspace-mcpd.log" {
		t.Fatalf("logging.file = %q", cfg.Logging.File)
	}
	if cfg.Logging.MaxSizeMB != 20 || cfg.Logging.MaxFiles != 5 {
		t.Fatalf("logging rotation = %d MB, %d files", cfg.Logging.MaxSizeMB, cfg.Logging.MaxFiles)
	}
}

// TestValidateRestoresLoggingLimits 验证非正日志轮转限制恢复为默认值。
func TestValidateRestoresLoggingLimits(t *testing.T) {
	cfg := Defaults()
	cfg.Workspace.Root = t.TempDir()
	cfg.Logging.MaxSizeMB = 0
	cfg.Logging.MaxFiles = -1
	if err := Validate(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Logging.MaxSizeMB != 20 || cfg.Logging.MaxFiles != 5 {
		t.Fatalf("logging rotation = %d MB, %d files", cfg.Logging.MaxSizeMB, cfg.Logging.MaxFiles)
	}
}

// TestValidateRejectsInvalidLogging 验证日志路径和级别必须有效。
func TestValidateRejectsInvalidLogging(t *testing.T) {
	t.Run("empty file", func(t *testing.T) {
		cfg := Defaults()
		cfg.Workspace.Root = t.TempDir()
		cfg.Logging.File = ""
		if err := Validate(&cfg); err == nil || !strings.Contains(err.Error(), "logging.file") {
			t.Fatalf("expected logging.file error, got %v", err)
		}
	})
	t.Run("unknown level", func(t *testing.T) {
		cfg := Defaults()
		cfg.Workspace.Root = t.TempDir()
		cfg.Logging.Level = "trace"
		if err := Validate(&cfg); err == nil || !strings.Contains(err.Error(), "logging.level") {
			t.Fatalf("expected logging.level error, got %v", err)
		}
	})
}
