package mcpserver

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/iamyounglee/remote-workspace-mcp/internal/auth"
	"github.com/iamyounglee/remote-workspace-mcp/internal/workspace"
)

// transferHTTP 校验令牌并将传输或同步请求路由到对应处理器。
func (s *Server) transferHTTP(w http.ResponseWriter, r *http.Request) {
	if token, ok := auth.Bearer(r.Header.Get("Authorization")); !ok || !s.tokens.Validate(token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// 限制请求体读取量，防止超大上传耗尽内存。下载(GET)无 body，不受影响。
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.Files.MaxUploadBytes)
	switch r.URL.Path {
	case "/files":
		s.fileTransfer(w, r)
	case "/directories":
		s.directoryTransfer(w, r)
	case "/sync/plan", "/sync/apply":
		s.syncHTTP(w, r)
	default:
		http.NotFound(w, r)
	}
}

// fileTransfer 解析文件路径并按请求方法执行上传或下载。
func (s *Server) fileTransfer(w http.ResponseWriter, r *http.Request) {
	filePath, err := s.resolver.Resolve(r.URL.Query().Get("path"), workspace.Read)
	if r.Method == http.MethodPut {
		filePath, err = s.resolver.Resolve(r.URL.Query().Get("path"), workspace.Write)
		r.Body = http.MaxBytesReader(w, r.Body, s.cfg.Files.MaxUploadBytes)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodPut:
		s.uploadFile(w, r, filePath)
	case http.MethodGet:
		s.downloadFile(w, filePath)
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// uploadFile 在大小和并发写入约束下接收并原子保存文件。
func (s *Server) uploadFile(w http.ResponseWriter, r *http.Request, filePath string) {
	if r.ContentLength > s.cfg.Files.MaxUploadBytes {
		http.Error(w, "upload exceeds max_upload_bytes", http.StatusRequestEntityTooLarge)
		return
	}
	// 与 write/edit/apply_patch/sync 共享分片锁，避免上传覆盖与这些写路径并发竞争；
	// 取路径分片锁（fileMu）串行化同文件写操作；bash 与文件写之间不再互斥，并发安全性由调用方保证。
	s.lockFiles([]string{filePath})
	defer s.unlockFiles([]string{filePath})

	if info, err := os.Stat(filePath); err == nil && info.IsDir() {
		http.Error(w, "target is a directory", http.StatusConflict)
		return
	} else if err != nil && !os.IsNotExist(err) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if s.cfg.Files.CreateParentDirs {
		if err := os.MkdirAll(filepath.Dir(filePath), 0o750); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(filePath), ".remote-workspace-mcp-upload-*")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	_ = tmp.Chmod(0o600)
	// 限制读取量为 MaxUploadBytes+1：若客户端发送超过上限，io.Copy 会在恰好读到
	// MaxUploadBytes+1 字节处停止，使 n == MaxUploadBytes+1 成为“明确超限”的可靠信号，
	// 避免此前仅靠 copyErr 无法区分“恰好传满上限”与“被截断”的歧义。
	limited := io.LimitReader(r.Body, s.cfg.Files.MaxUploadBytes+1)
	n, copyErr := io.Copy(tmp, limited)
	closeErr := tmp.Close()
	if copyErr != nil || closeErr != nil {
		http.Error(w, "failed to receive upload", http.StatusBadRequest)
		return
	}
	if n > s.cfg.Files.MaxUploadBytes {
		http.Error(w, "upload exceeds max_upload_bytes", http.StatusRequestEntityTooLarge)
		return
	}
	if err := os.Rename(tmpName, filePath); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// downloadFile 以附件形式返回受读取上限约束的文件内容。
func (s *Server) downloadFile(w http.ResponseWriter, filePath string) {
	file, err := os.Open(filePath)
	if err != nil {
		status := http.StatusInternalServerError
		if os.IsNotExist(err) {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		http.Error(w, "path is not a regular file", http.StatusBadRequest)
		return
	}
	if info.Size() > s.cfg.Files.MaxDownloadBytes {
		http.Error(w, "download exceeds max_download_bytes", http.StatusRequestEntityTooLarge)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(info.Size()))
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(filePath)+`"`)
	// 通过限长读取器约束实际传输量，避免超大文件拖垮连接。
	limited := io.LimitReader(file, s.cfg.Files.MaxDownloadBytes)
	if _, err := io.Copy(w, limited); err != nil {
		slog.Warn("download interrupted", "path", filePath, "error", err)
	}
}

// directoryTransfer 解析目录路径与归档格式并执行上传或下载。
func (s *Server) directoryTransfer(w http.ResponseWriter, r *http.Request) {
	format, err := archiveFormat(r.URL.Query().Get("format"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.Method == http.MethodPut {
		target, err := s.resolver.Resolve(r.URL.Query().Get("path"), workspace.Write)
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		s.uploadDirectory(w, r, target, format)
		return
	}
	if r.Method == http.MethodGet {
		target, err := s.resolver.Resolve(r.URL.Query().Get("path"), workspace.Read)
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		s.downloadDirectory(w, target, format)
		return
	}
	w.Header().Set("Allow", "GET, PUT")
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

// uploadDirectory 将上传的归档解压到临时目录并原子替换目标目录。
func (s *Server) uploadDirectory(w http.ResponseWriter, r *http.Request, target, format string) {
	if r.ContentLength > s.cfg.Files.MaxUploadBytes {
		http.Error(w, "upload exceeds max_upload_bytes", http.StatusRequestEntityTooLarge)
		return
	}
	// 取路径分片锁（fileMu）串行化同文件写操作；bash 与文件写之间不再互斥，并发安全性由调用方保证。
	// 注意：分片锁按“目标路径”归一化取模，
	// uploadDirectory 仅对 target 目录路径本身加锁，无法覆盖其下子文件（子项落在不同分片，互不排斥）。
	// 目录上传以整体 rename 替换目录，与并发的单文件写存在理论竞争窗口；该限制结构性存在
	// （分片锁无法同时覆盖“目录 + 其全部后代”），由沙箱/隔离兜底，且约定目录上传目标不应已存在。
	s.lockFiles([]string{target})
	defer s.unlockFiles([]string{target})

	if _, err := os.Stat(target); err == nil {
		http.Error(w, "target directory already exists", http.StatusConflict)
		return
	} else if !os.IsNotExist(err) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if s.cfg.Files.CreateParentDirs {
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	tmp, err := os.MkdirTemp(filepath.Dir(target), ".remote-workspace-mcp-directory-*")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(tmp)
	limited := io.LimitReader(r.Body, s.cfg.Files.MaxUploadBytes+1)
	archive, err := os.CreateTemp("", ".remote-workspace-mcp-archive-*")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	archiveName := archive.Name()
	defer os.Remove(archiveName)
	n, err := io.Copy(archive, limited)
	if err != nil {
		_ = archive.Close()
		http.Error(w, "failed to receive archive", http.StatusBadRequest)
		return
	}
	if err := archive.Close(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if n > s.cfg.Files.MaxUploadBytes {
		http.Error(w, "upload exceeds max_upload_bytes", http.StatusRequestEntityTooLarge)
		return
	}
	if err := extractTar(archiveName, tmp, format, s.cfg.Files.MaxArchiveEntries, s.cfg.Files.MaxExtractedBytes); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := os.Rename(tmp, target); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// extractTar 在条目数和总大小限制下安全解压 tar 归档。
func extractTar(archiveName, target, format string, maxEntries int, maxBytes int64) error {
	file, err := os.Open(archiveName)
	if err != nil {
		return err
	}
	defer file.Close()
	var reader io.Reader = file
	var gz *gzip.Reader
	if format == "tar.gz" {
		gz, err = gzip.NewReader(file)
		if err != nil {
			return fmt.Errorf("invalid gzip archive: %w", err)
		}
		defer gz.Close()
		reader = gz
	}
	tr := tar.NewReader(reader)
	var total int64
	entries := 0
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("invalid tar archive: %w", err)
		}
		entries++
		if entries > maxEntries {
			return fmt.Errorf("archive exceeds max_archive_entries")
		}
		if header.Typeflag == tar.TypeDir && path.Clean(strings.ReplaceAll(header.Name, `\`, "/")) == "." {
			continue
		}
		name, err := safeArchiveName(header.Name)
		if err != nil {
			return err
		}
		destination := filepath.Join(target, filepath.FromSlash(name))
		if !withinPath(target, destination) {
			return fmt.Errorf("archive entry escapes target: %q", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(destination, 0o750); err != nil {
				return err
			}
		case tar.TypeReg:
			remaining := maxBytes - total
			if remaining < 0 || header.Size < 0 || header.Size > remaining {
				return fmt.Errorf("archive exceeds max_extracted_bytes")
			}
			if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
				return err
			}
			out, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return err
			}
			n, copyErr := io.Copy(out, io.LimitReader(tr, remaining+1))
			closeErr := out.Close()
			if copyErr != nil || closeErr != nil {
				return fmt.Errorf("extract %q failed", header.Name)
			}
			if n != header.Size || n > remaining {
				return fmt.Errorf("invalid size for archive entry %q", header.Name)
			}
			total += n
		default:
			return fmt.Errorf("unsupported tar entry type for %q", header.Name)
		}
	}
}

// safeArchiveName 规范化归档条目名并拒绝绝对路径和路径穿越。
func safeArchiveName(name string) (string, error) {
	normalized := strings.ReplaceAll(name, `\`, "/")
	clean := path.Clean(normalized)
	windowsAbsolute := len(clean) >= 3 && ((clean[0] >= 'A' && clean[0] <= 'Z') || (clean[0] >= 'a' && clean[0] <= 'z')) && clean[1] == ':' && clean[2] == '/'
	if name == "" || strings.HasPrefix(normalized, "/") || windowsAbsolute || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("unsafe archive entry %q", name)
	}
	return clean, nil
}

// downloadDirectory 将目录打包为指定格式的 tar 归档并写入响应。
func (s *Server) downloadDirectory(w http.ResponseWriter, target, format string) {
	info, err := os.Stat(target)
	if err != nil {
		status := http.StatusInternalServerError
		if os.IsNotExist(err) {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}
	if !info.IsDir() {
		http.Error(w, "path is not a directory", http.StatusBadRequest)
		return
	}

	// 第一遍：基于 filepath.WalkDir（不跟随符号链接目录）遍历并累加总大小，
	// 在写入响应体之前就完成 max_download_bytes 校验，避免写出半截损坏的归档。
	var total int64
	var entries []dirEntry
	err = filepath.WalkDir(target, func(filePath string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filePath == target {
			return nil
		}
		rel, err := filepath.Rel(target, filePath)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		if shouldIgnoreTransfer(relSlash, d.IsDir(), s.cfg.Files.IgnoreDirectories) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// 符号链接：作为链接本身归档，绝不跟随，避免顺着软链逃逸出工作区。
		if d.Type()&os.ModeSymlink != 0 {
			entries = append(entries, dirEntry{rel: relSlash, symlink: true})
			return nil
		}
		if d.IsDir() {
			entries = append(entries, dirEntry{rel: relSlash, dir: true})
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			// 跳过其他非常规文件（设备文件、管道等）。
			return nil
		}
		total += fi.Size()
		if total > s.cfg.Files.MaxDownloadBytes {
			return fmt.Errorf("download exceeds max_download_bytes")
		}
		entries = append(entries, dirEntry{rel: relSlash, info: fi})
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	filename := filepath.Base(target) + ".tar"
	var writer io.Writer = w
	var gz *gzip.Writer
	if format == "tar.gz" {
		filename += ".gz"
		w.Header().Set("Content-Type", "application/gzip")
		gz = gzip.NewWriter(w)
		defer gz.Close()
		writer = gz
	} else {
		w.Header().Set("Content-Type", "application/x-tar")
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	tw := tar.NewWriter(writer)
	defer tw.Close()
	for _, e := range entries {
		if e.symlink {
			linkTarget, err := os.Readlink(filepath.Join(target, filepath.FromSlash(e.rel)))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			header := &tar.Header{
				Name:     e.rel,
				Typeflag: tar.TypeSymlink,
				Linkname: linkTarget,
				Mode:     0o644,
			}
			if err := tw.WriteHeader(header); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			continue
		}
		fi := e.info
		if e.dir {
			fi = dirInfo{e.rel}
		}
		header, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		header.Name = e.rel
		if e.dir {
			header.Name += "/"
		}
		if err := tw.WriteHeader(header); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if e.dir {
			continue
		}
		in, err := os.Open(filepath.Join(target, filepath.FromSlash(e.rel)))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, copyErr := io.Copy(tw, in)
		_ = in.Close()
		if copyErr != nil {
			http.Error(w, copyErr.Error(), http.StatusInternalServerError)
			return
		}
	}
}

// dirEntry 是 downloadDirectory 第一遍遍历时收集的可序列化条目。
type dirEntry struct {
	rel     string
	dir     bool
	symlink bool
	info    os.FileInfo
}

// dirInfo 为仅含名称的目录构造一个最小 os.FileInfo，供 tar.FileInfoHeader 生成目录头。
type dirInfo struct{ name string }

func (d dirInfo) Name() string       { return d.name }
func (d dirInfo) Size() int64        { return 0 }
func (d dirInfo) Mode() os.FileMode  { return os.ModeDir | 0o755 }
func (d dirInfo) ModTime() time.Time { return time.Time{} }
func (d dirInfo) IsDir() bool        { return true }
func (d dirInfo) Sys() any           { return nil }

// archiveFormat 校验并返回请求指定的归档格式。
func archiveFormat(value string) (string, error) {
	switch strings.ToLower(value) {
	case "", "tar.gz", "tgz":
		return "tar.gz", nil
	case "tar":
		return "tar", nil
	default:
		return "", fmt.Errorf("format must be tar or tar.gz")
	}
}

// shouldIgnoreTransfer 判断路径是否应被目录传输忽略。
// 仅对目录生效；只要路径的任一组成部分命中忽略名单即忽略（此前仅比较末级 base，
// 对形如 sub/.git 的嵌套忽略目录可能漏判）。
func shouldIgnoreTransfer(rel string, isDir bool, ignored []string) bool {
	if !isDir || len(ignored) == 0 {
		return false
	}
	for _, comp := range strings.Split(rel, "/") {
		for _, name := range ignored {
			if comp == name {
				return true
			}
		}
	}
	return false
}

// withinPath 判断目标路径是否位于指定根目录内。
func withinPath(root, candidate string) bool {
	root, candidate = filepath.Clean(root), filepath.Clean(candidate)
	return candidate == root || strings.HasPrefix(candidate, root+string(filepath.Separator))
}
