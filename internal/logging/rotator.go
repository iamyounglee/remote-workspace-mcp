package logging

// Package logging 提供按大小轮转的本地文件日志写入器。

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// RotatingWriter 将日志写入文件，并按大小保留固定数量的日志文件
type RotatingWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	maxFiles int
	file     *os.File
	size     int64
}

// Open 创建日志目录并打开支持轮转的日志写入器。
func Open(path string, maxBytes int64, maxFiles int) (*RotatingWriter, error) {
	if path == "" {
		return nil, errors.New("logging.file is required")
	}
	if maxBytes < 1 {
		return nil, errors.New("logging.max_size_mb must be positive")
	}
	if maxFiles < 1 {
		return nil, errors.New("logging.max_files must be positive")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	w := &RotatingWriter{path: path, maxBytes: maxBytes, maxFiles: maxFiles}
	if err := w.openFile(); err != nil {
		return nil, err
	}
	return w, nil
}

// Write 写入日志，并在写入前按配置执行轮转。
func (w *RotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, errors.New("log writer is closed")
	}
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// Close 同步并关闭当前日志文件。
func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := errors.Join(w.file.Sync(), w.file.Close())
	w.file = nil
	return err
}

// openFile 以追加模式打开当前日志文件并记录已有大小。
func (w *RotatingWriter) openFile() error {
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("stat log file: %w", err)
	}
	w.file = file
	w.size = info.Size()
	return nil
}

// rotate 关闭当前文件并将历史日志依次后移。
func (w *RotatingWriter) rotate() error {
	closeErr := errors.Join(w.file.Sync(), w.file.Close())
	w.file = nil
	if closeErr != nil {
		return w.reopenAfterRotationError(fmt.Errorf("close log before rotation: %w", closeErr))
	}
	oldest := w.maxFiles - 1
	if oldest > 0 {
		_ = os.Remove(w.backupPath(oldest))
		for i := oldest - 1; i >= 1; i-- {
			from := w.backupPath(i)
			to := w.backupPath(i + 1)
			if err := os.Rename(from, to); err != nil && !errors.Is(err, os.ErrNotExist) {
				return w.reopenAfterRotationError(fmt.Errorf("rotate log backup: %w", err))
			}
		}
		if err := os.Rename(w.path, w.backupPath(1)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return w.reopenAfterRotationError(fmt.Errorf("rotate active log: %w", err))
		}
	} else {
		if err := os.Remove(w.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return w.reopenAfterRotationError(fmt.Errorf("remove active log: %w", err))
		}
	}
	return w.openFile()
}

// reopenAfterRotationError 尝试恢复活动日志，并保留最初的轮转错误。
func (w *RotatingWriter) reopenAfterRotationError(rotationErr error) error {
	if err := w.openFile(); err != nil {
		return errors.Join(rotationErr, fmt.Errorf("reopen log after rotation failure: %w", err))
	}
	return rotationErr
}

// backupPath 返回指定序号的历史日志路径。
func (w *RotatingWriter) backupPath(index int) string {
	return fmt.Sprintf("%s.%d", w.path, index)
}

var _ io.WriteCloser = (*RotatingWriter)(nil)
