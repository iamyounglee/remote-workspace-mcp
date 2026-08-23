package mcpserver

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// globMatch 判断路径是否匹配支持双星号的通配模式。
func globMatch(pattern, name string) bool {
	pattern = filepath.ToSlash(pattern)
	name = filepath.ToSlash(name)
	if !strings.Contains(pattern, "/") {
		name = filepath.Base(name)
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				if i+2 < len(pattern) && pattern[i+2] == '/' {
					b.WriteString("(?:.*/)?")
					i += 2
				} else {
					b.WriteString(".*")
					i++
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(pattern[i])))
		}
	}
	b.WriteString("$")
	matched, err := regexp.MatchString(b.String(), name)
	return err == nil && matched
}

// readLimited 读取文件并拒绝超过指定字节上限的内容。返回读取到的数据与是否发生截断。
// 截断判定基于是否多读到一个字节，避免“恰好等于上限”被误判为截断。
func readLimited(path string, max int64) ([]byte, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, false, err
	}
	truncated := int64(len(data)) > max
	if truncated {
		data = data[:max]
	}
	return data, truncated, nil
}

// readEditableFile 在读取上限内加载待编辑文件，避免编辑超大文件时完整读入内存。
// 与 readLimited 不同，它超限直接报错而非截断，因为编辑需要完整内容。
func readEditableFile(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// 只读 max+1 字节：若文件超过上限，第 max+1 字节一定会被读到，从而触发报错。
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errors.New("file exceeds max_read_bytes")
	}
	return data, nil
}

// fileExists 仅判断文件是否存在，不读取内容。
func fileExists(path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// existingContent 受限读取已有文件内容，用于 expected_sha256 校验。
// 返回内容、是否存在以及错误。文件不存在时返回 (nil, false, nil)。
func existingContent(path string, max int64) ([]byte, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > max {
		return nil, true, errors.New("file exceeds max_read_bytes")
	}
	return data, true, nil
}

// validateExpectedSHA 校验 expected_sha256 是否为合法的 64 位十六进制 SHA-256 摘要。
func validateExpectedSHA(sum string) error {
	if sum == "" {
		return nil
	}
	decoded, err := hex.DecodeString(sum)
	if err != nil || len(decoded) != sha256.Size {
		return errors.New("expected_sha256 must be a 64-character hexadecimal SHA-256")
	}
	return nil
}

// applyByteEdit 对原始字节执行精确替换，不要求文件使用 UTF-8 编码。
// 采用单遍扫描：从已扫描位置继续查找 old_text，边拷贝边替换并计数，避免两次完整遍历。
func applyByteEdit(data []byte, edit patchEdit, maxWrite int64) ([]byte, int, error) {
	oldBytes := []byte(edit.OldText)
	if len(oldBytes) == 0 {
		return nil, 0, errors.New("old_text must not be empty")
	}
	limit := 1
	if edit.ReplaceAll {
		limit = -1
	}
	var out bytes.Buffer
	count := 0
	i := 0
	for i < len(data) {
		idx := bytes.Index(data[i:], oldBytes)
		if idx < 0 {
			out.Write(data[i:])
			break
		}
		out.Write(data[i : i+idx])
		count++
		// 达到替换上限后停止替换，但继续统计剩余匹配数用于 ambiguous 检测。
		if limit < 0 || count <= limit {
			out.WriteString(edit.NewText)
		}
		i += idx + len(oldBytes)
		if limit >= 0 && count >= limit {
			// 已替换 limit 次，剩余内容原样保留，并统计剩余匹配数。
			out.Write(data[i:])
			count += bytes.Count(data[i:], oldBytes)
			break
		}
	}
	if count == 0 {
		return nil, 0, errors.New("old_text was not found")
	}
	if !edit.ReplaceAll && count > 1 {
		return nil, 0, fmt.Errorf("old_text matched %d times; set replace_all or provide a unique match", count)
	}
	updated := out.Bytes()
	if int64(len(updated)) > maxWrite {
		return nil, 0, errors.New("edited content exceeds max_write_bytes")
	}
	return updated, count, nil
}

// atomicWrite 通过同目录临时文件原子写入，并保留已有文件权限。
func atomicWrite(path string, data []byte, makeParents bool) error {
	if makeParents {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return err
		}
	}
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".remote-workspace-mcp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	// Windows 不支持 POSIX 权限位：盲目 Chmod 继承来的只读位（如 0o444）会导致
	// 后续 Write 失败，且 os.CreateTemp 在 Windows 上默认仅当前用户可访问，故 Windows 上跳过 Chmod。
	if runtime.GOOS != "windows" {
		if err := tmp.Chmod(mode); err != nil {
			_ = tmp.Close()
			return err
		}
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return nil
}

// sha256Hex 计算数据的 SHA-256 十六进制摘要。
func sha256Hex(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
