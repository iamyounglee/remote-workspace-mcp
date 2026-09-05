package audit

import "unicode/utf8"

// TruncateUTF8 在 UTF-8 字符边界处将 s 截断到 limit 字节，
// 返回截断结果与是否发生截断。
//
// 按字节截断必须回退到字符起始字节，否则会切断多字节字符（如中文）
// 产生无效 UTF-8 字节，导致日志记录不可读甚至破坏日志解析。
// limit 小于等于 0 或 s 未超过 limit 时原样返回。
func TruncateUTF8(s string, limit int) (string, bool) {
	if limit <= 0 || len(s) <= limit {
		return s, false
	}
	end := limit
	// 回退到最近的 UTF-8 字符起始字节（非 0x10xxxxxx 续字节）。
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end], true
}
