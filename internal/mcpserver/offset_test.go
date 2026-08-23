package mcpserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iamyounglee/remote-workspace-mcp/internal/auth"
	"github.com/iamyounglee/remote-workspace-mcp/internal/config"
	"github.com/iamyounglee/remote-workspace-mcp/internal/workspace"
)

// utf8File 构造一个含多字节内容、跨多行的文件，用于验证行号偏移与字节扫描的一致性。
// 每行均为「3个中文字符 + 换行」，确保任意行都含多字节字符，若按 rune 或按 byte 错位都会暴露。
const utf8Content = "第一行内容\n第二行内容\n第三行内容\n第四行内容\n第五行内容\n"

func utf8Server(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cn.txt"), []byte(utf8Content), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Workspace.Root = dir
	resolver, err := workspace.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	store, _, err := auth.Open(filepath.Join(dir, ".state", "token"))
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, resolver, store)
}

// TestReadOffsetIsLineNumberWithMultibyte 验证 read 的 offset/limit 是 1-based 行号，
// 且对多字节 UTF-8 内容行计数依然准确（lineBoundaries 按 '\n' 字节扫描，续字节不会是 0x0A）。
func TestReadOffsetIsLineNumberWithMultibyte(t *testing.T) {
	s := utf8Server(t)

	// 读取第 2 行起、共 2 行，应得到「第二行内容」「第三行内容」。
	raw, _ := json.Marshal(map[string]any{"path": "cn.txt", "offset": 2, "limit": 2})
	res, err := s.read(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	content := m["content"].(string)
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %q", len(lines), content)
	}
	if lines[0] != "2\t第二行内容" {
		t.Fatalf("line1 = %q, want %q", lines[0], "2\t第二行内容")
	}
	if lines[1] != "3\t第三行内容" {
		t.Fatalf("line2 = %q, want %q", lines[1], "3\t第三行内容")
	}
	if m["offset"].(int) != 2 || m["limit"].(int) != 2 {
		t.Fatalf("echoed offset/limit = %v/%v", m["offset"], m["limit"])
	}

	// 验证 lineBoundaries 的字节扫描对多字节文件给出正确行数。
	// 注意：文件以 '\n' 结尾时，会多计一个末尾空行（bounds 末两位均为 len(data)），
	// 即 "a\nb\n" 被计为 3 行（含末行空串）。这是当前实现的真实语义。
	bounds := lineBoundaries([]byte(utf8Content))
	if len(bounds)-1 != 6 {
		t.Fatalf("lineBoundaries reported %d lines, want 6 (5 内容行 + 1 末尾空行, bounds=%v)", len(bounds)-1, bounds)
	}
}

// TestEditHasNoOffsetJustTextMatch 验证 edit 不依赖任何偏移量，仅靠 old_text/new_text 字节匹配，
// 且对多字节内容替换精确无误。
func TestEditHasNoOffsetJustTextMatch(t *testing.T) {
	s := utf8Server(t)

	raw, _ := json.Marshal(map[string]any{
		"path":     "cn.txt",
		"old_text": "第三行内容",
		"new_text": "替换后的第三行",
	})
	res, err := s.edit(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	if res.(map[string]any)["replacements"].(int) != 1 {
		t.Fatalf("expected 1 replacement, got %v", res.(map[string]any)["replacements"])
	}

	// 回读确认替换结果，且其余行完好。
	readRaw, _ := json.Marshal(map[string]any{"path": "cn.txt", "offset": 1, "limit": 10})
	got, err := s.read(json.RawMessage(readRaw))
	if err != nil {
		t.Fatal(err)
	}
	content := got.(map[string]any)["content"].(string)
	// 文件以 \n 结尾，read 不再把末行空串计入，因此只返回 5 行真实内容。
	want := "1\t第一行内容\n2\t第二行内容\n3\t替换后的第三行\n4\t第四行内容\n5\t第五行内容\n"
	if content != want {
		t.Fatalf("after edit content = %q, want %q", content, want)
	}
}

// TestReadOffsetOutOfRangeClamps 验证 offset 越界时被钳制，不会 panic 或越界。
func TestReadOffsetOutOfRangeClamps(t *testing.T) {
	s := utf8Server(t)
	raw, _ := json.Marshal(map[string]any{"path": "cn.txt", "offset": 999, "limit": 5})
	res, err := s.read(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	if res.(map[string]any)["content"].(string) != "" {
		t.Fatalf("expected empty content for out-of-range offset, got %q", res.(map[string]any)["content"])
	}
}
