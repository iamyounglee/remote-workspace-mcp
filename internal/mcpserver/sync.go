package mcpserver

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/iamyounglee/remote-workspace-mcp/internal/workspace"
)

const syncManifestFile = ".remote-workspace-mcp-sync-manifest.json"

// syncManifest 描述客户端提交的文件同步清单
type syncManifest struct {
	Files []syncFile `json:"files"`
}

// syncFile 描述同步清单中的单个文件
type syncFile struct {
	Path           string `json:"path"`
	Size           int64  `json:"size"`
	SHA256         string `json:"sha256"`
	ExpectedSHA256 string `json:"expected_sha256,omitempty"`
}

// syncPlan 汇总同步前需要新增、更新、保留和清理的文件
type syncPlan struct {
	Missing   []string `json:"missing"`
	Changed   []string `json:"changed"`
	Unchanged []string `json:"unchanged"`
	Extra     []string `json:"extra"`
}

// stagedSyncFile 记录同步应用阶段已暂存文件的元数据
type stagedSyncFile struct {
	path   string
	hash   string
	size   int64
	source string
}

// syncHTTP 按路径将同步请求分派到规划或应用处理器。
func (s *Server) syncHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/sync/plan":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.planSync(w, r)
	case "/sync/apply":
		if r.Method != http.MethodPut {
			w.Header().Set("Allow", "PUT")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.applySync(w, r)
	default:
		http.NotFound(w, r)
	}
}

// planSync 解析本地清单并返回与远端工作区的同步差异。
func (s *Server) planSync(w http.ResponseWriter, r *http.Request) {
	root, err := s.resolver.Resolve(r.URL.Query().Get("path"), workspace.Read)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, s.cfg.Files.MaxSyncManifestBytes+1))
	if err != nil {
		http.Error(w, "failed to read manifest", http.StatusBadRequest)
		return
	}
	if int64(len(data)) > s.cfg.Files.MaxSyncManifestBytes {
		http.Error(w, "manifest exceeds max_sync_manifest_bytes", http.StatusRequestEntityTooLarge)
		return
	}
	manifest, err := decodeSyncManifest(data, s.cfg.Files.MaxArchiveEntries)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	plan, err := s.compareSyncManifest(root, manifest)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeHTTPJSON(w, http.StatusOK, plan)
}

// decodeSyncManifest 解码并校验同步清单中的路径、大小和哈希。
func decodeSyncManifest(data []byte, maxEntries int) (syncManifest, error) {
	var manifest syncManifest
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, fmt.Errorf("invalid sync manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return manifest, fmt.Errorf("invalid sync manifest: trailing JSON content")
	}
	if len(manifest.Files) > maxEntries {
		return manifest, fmt.Errorf("manifest exceeds max_archive_entries")
	}
	seen := make(map[string]struct{}, len(manifest.Files))
	for i := range manifest.Files {
		file := &manifest.Files[i]
		clean, err := safeSyncPath(file.Path)
		if err != nil {
			return manifest, err
		}
		file.Path = clean
		file.SHA256 = strings.ToLower(file.SHA256)
		file.ExpectedSHA256 = strings.ToLower(file.ExpectedSHA256)
		if file.Size < 0 || !validSHA256(file.SHA256) {
			return manifest, fmt.Errorf("invalid size or sha256 for %q", file.Path)
		}
		if file.ExpectedSHA256 != "" && !validSHA256(file.ExpectedSHA256) {
			return manifest, fmt.Errorf("invalid expected_sha256 for %q", file.Path)
		}
		if _, ok := seen[file.Path]; ok {
			return manifest, fmt.Errorf("duplicate manifest path %q", file.Path)
		}
		seen[file.Path] = struct{}{}
	}
	return manifest, nil
}

// compareSyncManifest 比较清单与工作区文件并生成创建、更新和冲突计划。
func (s *Server) compareSyncManifest(root string, manifest syncManifest) (syncPlan, error) {
	plan := syncPlan{Missing: []string{}, Changed: []string{}, Unchanged: []string{}, Extra: []string{}}
	local := make(map[string]struct{}, len(manifest.Files))
	for _, file := range manifest.Files {
		local[file.Path] = struct{}{}
		destination := filepath.Join(root, filepath.FromSlash(file.Path))
		if err := ensureNoSymlinkComponents(root, destination); err != nil {
			return plan, err
		}
		info, err := os.Stat(destination)
		if errors.Is(err, os.ErrNotExist) {
			plan.Missing = append(plan.Missing, file.Path)
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Size() != file.Size {
			plan.Changed = append(plan.Changed, file.Path)
			continue
		}
		hash, err := fileSHA256(destination)
		if err != nil {
			return plan, err
		}
		if hash == file.SHA256 {
			plan.Unchanged = append(plan.Unchanged, file.Path)
		} else {
			plan.Changed = append(plan.Changed, file.Path)
		}
	}
	if info, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return plan, nil
	} else if err != nil {
		return plan, err
	} else if !info.IsDir() {
		return plan, fmt.Errorf("sync path is not a directory")
	}
	err := filepath.Walk(root, func(filePath string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filePath == root {
			return nil
		}
		rel, err := filepath.Rel(root, filePath)
		if err != nil {
			return err
		}
		if shouldIgnoreTransfer(rel, info.IsDir(), s.cfg.Files.IgnoreDirectories) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symbolic links are not allowed: %s", rel)
		}
		if info.Mode().IsRegular() {
			rel = filepath.ToSlash(rel)
			if _, ok := local[rel]; !ok {
				plan.Extra = append(plan.Extra, rel)
			}
		}
		return nil
	})
	if err != nil {
		return plan, err
	}
	sort.Strings(plan.Missing)
	sort.Strings(plan.Changed)
	sort.Strings(plan.Unchanged)
	sort.Strings(plan.Extra)
	return plan, nil
}

// applySync 解包同步归档、校验内容并将变更提交到工作区。
func (s *Server) applySync(w http.ResponseWriter, r *http.Request) {
	root, err := s.resolver.Resolve(r.URL.Query().Get("path"), workspace.Write)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	format, err := archiveFormat(r.URL.Query().Get("format"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.ContentLength > s.cfg.Files.MaxUploadBytes {
		http.Error(w, "upload exceeds max_upload_bytes", http.StatusRequestEntityTooLarge)
		return
	}
	stage, err := os.MkdirTemp("", ".remote-workspace-mcp-sync-*")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(stage)
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.Files.MaxUploadBytes)
	manifest, staged, err := extractSyncArchive(
		r.Body,
		stage,
		format,
		s.cfg.Files.MaxArchiveEntries,
		s.cfg.Files.MaxExtractedBytes,
		s.cfg.Files.MaxSyncManifestBytes,
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	created, updated, err := s.commitSync(root, manifest, staged)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errSyncConflict) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeHTTPJSON(w, http.StatusOK, map[string]any{"created": created, "updated": updated})
}

// extractSyncArchive 在资源限制下解包并核对同步清单与暂存文件。
func extractSyncArchive(
	body io.Reader,
	stage, format string,
	maxEntries int,
	maxBytes, maxManifestBytes int64,
) (syncManifest, map[string]stagedSyncFile, error) {
	var manifest syncManifest
	reader := body
	var gz *gzip.Reader
	var err error
	if format == "tar.gz" {
		gz, err = gzip.NewReader(body)
		if err != nil {
			return manifest, nil, fmt.Errorf("invalid gzip archive: %w", err)
		}
		defer gz.Close()
		reader = gz
	}
	tr := tar.NewReader(reader)
	staged := make(map[string]stagedSyncFile)
	var manifestData []byte
	var total int64
	entries := 0
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return manifest, nil, fmt.Errorf("invalid tar archive: %w", err)
		}
		entries++
		if entries > maxEntries+1 {
			return manifest, nil, fmt.Errorf("archive exceeds max_archive_entries")
		}
		if header.Typeflag == tar.TypeDir && strings.TrimSuffix(strings.ReplaceAll(header.Name, `\`, "/"), "/") == "." {
			continue
		}
		name, err := safeArchiveName(header.Name)
		if err != nil {
			return manifest, nil, err
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return manifest, nil, fmt.Errorf("unsupported tar entry type for %q", header.Name)
		}
		if name == syncManifestFile {
			if manifestData != nil || header.Size > maxManifestBytes {
				return manifest, nil, fmt.Errorf("invalid sync manifest entry")
			}
			manifestData, err = io.ReadAll(io.LimitReader(tr, maxManifestBytes+1))
			if err != nil || int64(len(manifestData)) != header.Size || int64(len(manifestData)) > maxManifestBytes {
				return manifest, nil, fmt.Errorf("invalid sync manifest entry")
			}
			continue
		}
		clean, err := safeSyncPath(name)
		if err != nil {
			return manifest, nil, err
		}
		if _, ok := staged[clean]; ok {
			return manifest, nil, fmt.Errorf("duplicate archive path %q", clean)
		}
		remaining := maxBytes - total
		if header.Size < 0 || header.Size > remaining {
			return manifest, nil, fmt.Errorf("archive exceeds max_extracted_bytes")
		}
		destination := filepath.Join(stage, filepath.FromSlash(clean))
		if err := os.MkdirAll(filepath.Dir(destination), 0750); err != nil {
			return manifest, nil, err
		}
		out, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			return manifest, nil, err
		}
		hasher := sha256.New()
		n, copyErr := io.Copy(io.MultiWriter(out, hasher), io.LimitReader(tr, remaining+1))
		closeErr := out.Close()
		if copyErr != nil || closeErr != nil || n != header.Size {
			return manifest, nil, fmt.Errorf("extract %q failed", header.Name)
		}
		total += n
		staged[clean] = stagedSyncFile{path: clean, hash: hex.EncodeToString(hasher.Sum(nil)), size: n, source: destination}
	}
	if manifestData == nil {
		return manifest, nil, fmt.Errorf("archive is missing %s", syncManifestFile)
	}
	manifest, err = decodeSyncManifest(manifestData, maxEntries)
	if err != nil {
		return manifest, nil, err
	}
	if len(manifest.Files) != len(staged) {
		return manifest, nil, fmt.Errorf("archive files do not match sync manifest")
	}
	for _, file := range manifest.Files {
		item, ok := staged[file.Path]
		if !ok || item.size != file.Size || item.hash != file.SHA256 {
			return manifest, nil, fmt.Errorf("archive content does not match manifest for %q", file.Path)
		}
	}
	return manifest, staged, nil
}

var errSyncConflict = errors.New("sync conflict")

// commitSync 校验目标文件预期状态后原子提交暂存文件。
func (s *Server) commitSync(root string, manifest syncManifest, staged map[string]stagedSyncFile) ([]string, []string, error) {
	if err := os.MkdirAll(root, 0750); err != nil {
		return nil, nil, err
	}
	// 按目标路径取分片锁：commitSync 是 read-check-then-rename 的写路径，
	// 必须与 write/edit/apply_patch 共享同一套锁，否则校验与提交之间存在 TOCTOU。
	paths := make([]string, 0, len(manifest.Files))
	for _, file := range manifest.Files {
		paths = append(paths, filepath.Join(root, filepath.FromSlash(file.Path)))
	}
	s.lockFiles(paths)
	defer s.unlockFiles(paths)

	created := make([]string, 0)
	updated := make([]string, 0)
	for _, file := range manifest.Files {
		destination := filepath.Join(root, filepath.FromSlash(file.Path))
		if err := ensureNoSymlinkComponents(root, destination); err != nil {
			return nil, nil, err
		}
		info, err := os.Stat(destination)
		if err == nil && info.IsDir() {
			return nil, nil, fmt.Errorf("destination is a directory: %s", file.Path)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, nil, err
		}
		if file.ExpectedSHA256 != "" {
			if errors.Is(err, os.ErrNotExist) {
				return nil, nil, fmt.Errorf("%w: %s no longer exists", errSyncConflict, file.Path)
			}
			current, hashErr := fileSHA256(destination)
			if hashErr != nil {
				return nil, nil, hashErr
			}
			if current != file.ExpectedSHA256 {
				return nil, nil, fmt.Errorf("%w: %s changed after planning", errSyncConflict, file.Path)
			}
		}
		if errors.Is(err, os.ErrNotExist) {
			created = append(created, file.Path)
		} else {
			updated = append(updated, file.Path)
		}
	}
	// 第二遍：真正落地文件。为保证 apply 的原子性感知（此前任一文件写入失败时，
	// 已写入的文件不会回滚，却仍回传了 created/updated 列表，误导调用方以为整体成功），
	// 这里先尝试写入全部文件，任一失败则尽力回滚已写入的文件，并返回空列表 + 错误，
	// 绝不暴露“部分成功”的假象。
	applied := make([]string, 0, len(manifest.Files))
	for _, file := range manifest.Files {
		destination := filepath.Join(root, filepath.FromSlash(file.Path))
		if err := atomicCopyFile(staged[file.Path].source, destination, s.cfg.Files.CreateParentDirs); err != nil {
			for _, done := range applied {
				_ = os.Remove(done)
			}
			return nil, nil, fmt.Errorf("sync apply failed and partial writes were rolled back: %w (client should retry the full plan)", err)
		}
		applied = append(applied, destination)
	}
	sort.Strings(created)
	sort.Strings(updated)
	return created, updated, nil
}

// safeSyncPath 规范化同步路径并拒绝绝对路径和路径穿越。
func safeSyncPath(name string) (string, error) {
	clean, err := safeArchiveName(name)
	if err != nil {
		return "", err
	}
	if clean == syncManifestFile {
		return "", fmt.Errorf("reserved sync path %q", clean)
	}
	return clean, nil
}

// validSHA256 判断字符串是否为有效的 SHA-256 十六进制摘要。
func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// fileSHA256 计算受大小上限约束的文件 SHA-256 摘要。
func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// ensureNoSymlinkComponents 确保根目录到目标路径之间不含符号链接，且目标确实位于根目录内。
// 路径归属判定统一交由 withinPath（此前这里自行用 filepath.Rel 重复实现了一套
//  containment 判断，与 withinPath 语义重复且易漂移）。
func ensureNoSymlinkComponents(root, destination string) error {
	if !withinPath(root, destination) {
		return fmt.Errorf("path escapes sync root")
	}
	rel, err := filepath.Rel(root, destination)
	if err != nil {
		return fmt.Errorf("invalid sync path")
	}
	current := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symbolic links are not allowed: %s", rel)
		}
	}
	return nil
}

// atomicCopyFile 将源文件复制到同目录临时文件后原子替换目标。
func atomicCopyFile(source, destination string, makeParents bool) error {
	if makeParents {
		if err := os.MkdirAll(filepath.Dir(destination), 0750); err != nil {
			return err
		}
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(destination), ".remote-workspace-mcp-sync-file-*")
	if err != nil {
		return err
	}
	tmp := out.Name()
	defer os.Remove(tmp)
	if err := out.Chmod(0600); err != nil {
		out.Close()
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, destination)
}

// writeHTTPJSON 以指定状态码写入 JSON HTTP 响应。
func writeHTTPJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
