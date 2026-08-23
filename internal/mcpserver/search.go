package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/iamyounglee/remote-workspace-mcp/internal/workspace"
)

// grep 在工作区文件中搜索固定文本或正则表达式并返回上下文。
// ctx 用于在客户端断开或上游取消时尽早停止遍历。
func (s *Server) grep(ctx context.Context, raw json.RawMessage) (any, error) {
	var a struct {
		Path          string `json:"path"`
		Pattern       string `json:"pattern"`
		Glob          string `json:"glob"`
		Fixed         bool   `json:"fixed_string"`
		CaseSensitive bool   `json:"case_sensitive"`
		ContextBefore int    `json:"context_before"`
		ContextAfter  int    `json:"context_after"`
		Max           int    `json:"max_results"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	if a.Max < 1 {
		a.Max = 200
	}
	path, err := s.resolver.Resolve(a.Path, workspace.Read)
	if err != nil {
		return nil, err
	}
	var re *regexp.Regexp
	if !a.Fixed {
		p := a.Pattern
		if !a.CaseSensitive {
			p = "(?i)" + p
		}
		re, err = regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("invalid pattern: %w", err)
		}
	}
	var matches []map[string]any
	truncatedAny := false
	visit := func(file string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, truncated, err := readLimited(file, s.cfg.Files.MaxReadBytes)
		if err != nil {
			return nil
		}
		if truncated {
			truncatedAny = true
		}
		// 字节级按行扫描，避免对整体 data 做 string(data) 复制。
		bounds := lineBoundaries(data)
		totalLines := len(bounds) - 1
		for i := 0; i < totalLines; i++ {
			lineEnd := bounds[i+1]
			line := data[bounds[i]:lineEnd]
			// 去掉行尾换行，便于匹配与展示。
			if n := len(line); n > 0 && line[n-1] == '\n' {
				line = line[:n-1]
			}
			fixedMatch := false
			if a.Fixed {
				haystack := string(line)
				needle := a.Pattern
				if !a.CaseSensitive {
					haystack = strings.ToLower(haystack)
					needle = strings.ToLower(needle)
				}
				fixedMatch = strings.Contains(haystack, needle)
			}
			found := fixedMatch || (!a.Fixed && re.Match(line))
			if found {
				before := a.ContextBefore
				after := a.ContextAfter
				if before < 0 {
					before = 0
				}
				if after < 0 {
					after = 0
				}
				start, end := i-before, i+after+1
				if start < 0 {
					start = 0
				}
				if end > totalLines {
					end = totalLines
				}
				ctxLines := make([]string, 0, end-start)
				for j := start; j < end; j++ {
					cl := data[bounds[j]:bounds[j+1]]
					if n := len(cl); n > 0 && cl[n-1] == '\n' {
						cl = cl[:n-1]
					}
					ctxLines = append(ctxLines, string(cl))
				}
				matches = append(matches, map[string]any{"path": s.resolver.Display(file), "line": i + 1, "text": string(line), "context": ctxLines})
				if len(matches) >= a.Max {
					return errStopWalk
				}
			}
		}
		return nil
	}
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		_ = visit(path)
	} else {
		err = filepath.WalkDir(path, func(p string, d os.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if walkErr != nil {
				return nil
			}
			if d.IsDir() {
				for _, ignored := range s.cfg.Files.IgnoreDirectories {
					if d.Name() == ignored && p != path {
						return filepath.SkipDir
					}
				}
				return nil
			}
			if a.Glob != "" {
				rel, _ := filepath.Rel(path, p)
				ok := globMatch(a.Glob, rel)
				if !ok {
					return nil
				}
			}
			return visit(p)
		})
		if err != nil && !errors.Is(err, errStopWalk) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
	}
	return map[string]any{"matches": matches, "truncated": len(matches) >= a.Max || truncatedAny}, nil
}

var errStopWalk = errors.New("stop walk")

// glob 按通配模式查找工作区路径。ctx 用于在客户端断开或上游取消时尽早停止遍历。
func (s *Server) glob(ctx context.Context, raw json.RawMessage) (any, error) {
	var a struct {
		Path    string `json:"path"`
		Pattern string `json:"pattern"`
		Max     int    `json:"max_results"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	if a.Max < 1 {
		a.Max = 500
	}
	path, err := s.resolver.Resolve(a.Path, workspace.Read)
	if err != nil {
		return nil, err
	}
	var result []string
	err = filepath.WalkDir(path, func(p string, d os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			for _, ignored := range s.cfg.Files.IgnoreDirectories {
				if d.Name() == ignored && p != path {
					return filepath.SkipDir
				}
			}
			return nil
		}
		rel, _ := filepath.Rel(path, p)
		ok := globMatch(a.Pattern, rel)
		if ok {
			result = append(result, s.resolver.Display(p))
		}
		if len(result) >= a.Max {
			return errStopWalk
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStopWalk) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}
	sort.Strings(result)
	return map[string]any{"paths": result, "truncated": len(result) >= a.Max}, nil
}

// list 列出工作区目录中的条目及其元数据。ctx 用于在客户端断开或上游取消时尽早停止遍历。
func (s *Server) list(ctx context.Context, raw json.RawMessage) (any, error) {
	var a struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
		Max       int    `json:"max_entries"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	if a.Max < 1 {
		a.Max = 200
	}
	path, err := s.resolver.Resolve(a.Path, workspace.Read)
	if err != nil {
		return nil, err
	}
	type entry struct {
		Name, Path, Type string
		Size             int64     `json:"size"`
		Modified         time.Time `json:"modified"`
	}
	var out []entry
	walk := func(p string, info os.FileInfo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		typ := "file"
		if info.IsDir() {
			typ = "directory"
		}
		out = append(out, entry{Name: info.Name(), Path: s.resolver.Display(p), Type: typ, Size: info.Size(), Modified: info.ModTime()})
		if len(out) >= a.Max {
			return errStopWalk
		}
		return nil
	}
	if !a.Recursive {
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if info, err := e.Info(); err == nil {
				_ = walk(filepath.Join(path, e.Name()), info)
			}
		}
	} else {
		err = filepath.WalkDir(path, func(p string, d os.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if walkErr != nil {
				return nil
			}
			if p == path {
				return nil
			}
			if d.IsDir() {
				if info, err := d.Info(); err == nil && info.Mode()&os.ModeSymlink != 0 {
					return filepath.SkipDir
				}
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			return walk(p, info)
		})
		if err != nil && !errors.Is(err, errStopWalk) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
	}
	return map[string]any{"entries": out, "truncated": len(out) >= a.Max}, nil
}
