package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/iamyounglee/remote-workspace-mcp/internal/config"
)

// TestNormalizeArgsDefaultsToServe 验证无参数执行使用默认服务配置。
func TestNormalizeArgsDefaultsToServe(t *testing.T) {
	want := []string{"serve", "--config", "config.yaml"}
	if got := normalizeArgs(nil); !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizeArgs(nil) = %v, want %v", got, want)
	}
}

// TestNormalizeArgsPreservesExplicitCommand 验证显式子命令保持不变。
func TestNormalizeArgsPreservesExplicitCommand(t *testing.T) {
	args := []string{"token", "show", "--config", "custom.yaml"}
	if got := normalizeArgs(args); !reflect.DeepEqual(got, args) {
		t.Fatalf("normalizeArgs() = %v, want %v", got, args)
	}
}

// TestSetupLoggingWritesConfiguredFile 验证服务日志仅写入配置的轮转文件。
func TestSetupLoggingWritesConfiguredFile(t *testing.T) {
	previous := slog.Default()
	defer slog.SetDefault(previous)

	path := filepath.Join(t.TempDir(), "logs", "remote-workspace-mcpd.log")
	cfg := config.Defaults()
	cfg.Logging.File = path
	closeLog, err := setupLogging(cfg)
	if err != nil {
		t.Fatal(err)
	}
	slog.Info("configured file logging", "test", true)
	if err := closeLog(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "configured file logging") {
		t.Fatalf("log file does not contain test message: %q", data)
	}
}
