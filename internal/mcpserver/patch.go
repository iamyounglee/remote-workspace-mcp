package mcpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/iamyounglee/remote-workspace-mcp/internal/workspace"
)

const (
	patchMaxEdits = 1000
	patchMaxFiles = 100
	// patchMaxRequestBytes 与 HTTP 层 MaxBytesReader(10<<20)保持一致：
	// 由于 post 已在读取阶段用 10MB 上限拦截，此处原有 16MB 常量不可达，
	// 统一为同一上限来源，避免两处不一致。
	patchMaxRequestBytes     = 10 << 20
	patchMaxTotalInputBytes  = 64 << 20
	patchMaxTotalOutputBytes = 64 << 20
)

// patchEditResult 记录单个编辑的替换统计。
type patchEditResult struct {
	Index        int `json:"index"`
	Replacements int `json:"replacements"`
}

// pendingPatchFile 保存单个文件的批量编辑规划和提交数据。
type pendingPatchFile struct {
	path        string
	displayPath string
	original    []byte
	data        []byte
	originalSHA string
	// newSHA 在规划阶段计算一次，结果构造与提交阶段复用（避免重复哈希）。
	newSHA string
	edits  []patchEditResult
}

// patchError 为批量编辑错误补充索引、路径和处理阶段。
func patchError(index int, path, stage string, err error) error {
	return fmt.Errorf("edit %d path %q stage %s: %w", index, path, stage, err)
}

// applyPatch 先按文件规划顺序编辑，再对每个文件执行一次原子写入。
func (s *Server) applyPatch(raw json.RawMessage) (any, error) {
	// 规划阶段不持锁，仅校验与读取文件内容；提交阶段再按序取分片锁，避免持锁时间过长。
	if len(raw) > patchMaxRequestBytes {
		return nil, fmt.Errorf("stage validate: request exceeds %d bytes", patchMaxRequestBytes)
	}
	var a struct {
		Edits           []patchEdit `json:"edits"`
		DryRun          bool        `json:"dry_run"`
		RollbackOnError bool        `json:"rollback_on_error"`
	}
	if err := decodeStrict(raw, &a); err != nil {
		return nil, fmt.Errorf("stage decode: %w", err)
	}
	if len(a.Edits) == 0 {
		return nil, errors.New("stage validate: edits must not be empty")
	}
	if len(a.Edits) > patchMaxEdits {
		return nil, fmt.Errorf("stage validate: edits exceeds limit %d", patchMaxEdits)
	}

	filesByPath := make(map[string]*pendingPatchFile)
	files := make([]*pendingPatchFile, 0)
	totalInput := int64(len(raw))
	for index, edit := range a.Edits {
		if edit.Path == "" {
			return nil, patchError(index, edit.Path, "validate", errors.New("path must not be empty"))
		}
		if edit.Expected != "" {
			if err := validateExpectedSHA(edit.Expected); err != nil {
				return nil, patchError(index, edit.Path, "validate", err)
			}
		}
		path, err := s.resolver.Resolve(edit.Path, workspace.Write)
		if err != nil {
			return nil, patchError(index, edit.Path, "resolve", err)
		}
		file := filesByPath[path]
		if file == nil {
			if len(files) >= patchMaxFiles {
				return nil, patchError(index, edit.Path, "validate", fmt.Errorf("unique files exceeds limit %d", patchMaxFiles))
			}
			data, err := readEditableFile(path, s.cfg.Files.MaxReadBytes)
			if err != nil {
				return nil, patchError(index, edit.Path, "read", err)
			}
			totalInput += int64(len(data))
			if totalInput > patchMaxTotalInputBytes {
				return nil, patchError(index, edit.Path, "validate", fmt.Errorf("total input exceeds %d bytes", patchMaxTotalInputBytes))
			}
			file = &pendingPatchFile{
				path:        path,
				displayPath: s.resolver.Display(path),
				original:    data,
				data:        data,
				originalSHA: sha256Hex(data),
			}
			filesByPath[path] = file
			files = append(files, file)
		}
		if edit.Expected != "" && !strings.EqualFold(edit.Expected, file.originalSHA) {
			return nil, patchError(index, edit.Path, "expected_sha256", errors.New("file changed since expected_sha256 was calculated"))
		}
		updated, count, err := applyByteEdit(file.data, edit, s.cfg.Files.MaxWriteBytes)
		if err != nil {
			return nil, patchError(index, edit.Path, "apply", err)
		}
		file.data = updated
		file.edits = append(file.edits, patchEditResult{Index: index, Replacements: count})
	}

	var totalOutput int64
	for _, file := range files {
		totalOutput += int64(len(file.data))
		if totalOutput > patchMaxTotalOutputBytes {
			return nil, fmt.Errorf("stage validate: total output exceeds %d bytes", patchMaxTotalOutputBytes)
		}
	}

	fileResults := make([]map[string]any, 0, len(files))
	for _, file := range files {
		// 规划阶段即计算最终哈希，提交阶段直接复用，避免重复哈希。
		file.newSHA = sha256Hex(file.data)
		fileResults = append(fileResults, map[string]any{
			"path":         file.displayPath,
			"old_sha256":   file.originalSHA,
			"new_sha256":   file.newSHA,
			"old_bytes":    len(file.original),
			"new_bytes":    len(file.data),
			"edits":        file.edits,
			"replacements": patchReplacementCount(file.edits),
		})
	}
	result := map[string]any{
		"dry_run":            a.DryRun,
		"rollback_on_error":  a.RollbackOnError,
		"edit_count":         len(a.Edits),
		"file_count":         len(files),
		"total_input_bytes":  totalInput,
		"total_output_bytes": totalOutput,
		"files":              fileResults,
	}
	if a.DryRun {
		result["committed"] = false
		return result, nil
	}

	// 提交阶段：按路径字典序取分片锁，避免多文件编辑因加锁顺序不一致而死锁。
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.path)
	}
	s.lockFiles(paths)
	defer s.unlockFiles(paths)

	for _, file := range files {
		current, err := readEditableFile(file.path, s.cfg.Files.MaxReadBytes)
		if err != nil {
			return nil, fmt.Errorf("path %q stage precommit_read: %w", file.displayPath, err)
		}
		if !strings.EqualFold(sha256Hex(current), file.originalSHA) {
			return nil, fmt.Errorf("path %q stage precommit_hash: file changed after planning", file.displayPath)
		}
	}

	committed := make([]*pendingPatchFile, 0, len(files))
	for _, file := range files {
		// 批量提交阶段不逐个 fsync（提升吞吐），统一在提交后同步父目录。
		if err := atomicWrite(file.path, file.data, false); err != nil {
			if !a.RollbackOnError {
				return nil, fmt.Errorf("path %q stage commit: %w", file.displayPath, err)
			}
			if rollbackErr := rollbackPatchFiles(committed); rollbackErr != nil {
				return nil, fmt.Errorf("path %q stage commit: %v; stage rollback: %w", file.displayPath, err, rollbackErr)
			}
			return nil, fmt.Errorf("path %q stage commit: %w; committed files rolled back", file.displayPath, err)
		}
		committed = append(committed, file)
	}
	result["committed"] = true
	return result, nil
}

// patchReplacementCount 汇总文件内所有编辑的替换次数。
func patchReplacementCount(edits []patchEditResult) int {
	total := 0
	for _, edit := range edits {
		total += edit.Replacements
	}
	return total
}

// rollbackPatchFiles 按提交逆序恢复已经写入的文件。
func rollbackPatchFiles(files []*pendingPatchFile) error {
	var rollbackErrors []string
	for index := len(files) - 1; index >= 0; index-- {
		file := files[index]
		// 回滚写入也需同步父目录，确保崩溃后目录项持久化。
		if err := atomicWrite(file.path, file.original, false); err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Sprintf("%s: %v", file.displayPath, err))
		}
	}
	if len(rollbackErrors) > 0 {
		return errors.New(strings.Join(rollbackErrors, "; "))
	}
	return nil
}
