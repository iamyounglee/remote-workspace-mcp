package logging

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRotatingWriterLimitsFiles 验证日志按大小轮转且文件总数不超过上限。
func TestRotatingWriterLimitsFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "remote-workspace-mcpd.log")
	writer, err := Open(path, 10, 3)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, err := writer.Write([]byte("123456\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{path, path + ".1", path + ".2"} {
		if _, err := os.Stat(name); err != nil {
			t.Fatalf("expected log file %s: %v", name, err)
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("unexpected fourth log file: %v", err)
	}
}

// TestRotatingWriterAppendsExistingFile 验证服务重启后沿用已有日志大小。
func TestRotatingWriterAppendsExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remote-workspace-mcpd.log")
	if err := os.WriteFile(path, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	writer, err := Open(path, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("next")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("expected rotated existing log: %v", err)
	}
}

// TestRotatingWriterKeepsOnlyActiveFile 验证 max_files 为一时不会保留历史日志。
func TestRotatingWriterKeepsOnlyActiveFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remote-workspace-mcpd.log")
	writer, err := Open(path, 5, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("second")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("unexpected backup log: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "second" {
		t.Fatalf("active log = %q, want second", data)
	}
}

// TestRotatingWriterKeepsOversizedRecordWhole 验证单条超限日志保持完整并在下一次写入前轮转。
func TestRotatingWriterKeepsOversizedRecordWhole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remote-workspace-mcpd.log")
	writer, err := Open(path, 5, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("oversized")); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("next")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != "oversized" {
		t.Fatalf("backup log = %q, want oversized", backup)
	}
}
