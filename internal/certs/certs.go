// Package certs 提供基于用户自备证书文件的 TLS 证书热加载能力。
package certs

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Reloader 持有来自用户自备证书文件的 TLS 证书，并支持在不重启进程的前提下热加载。
// GetCertificate 的签名由标准库 crypto/tls 约束，是热加载的最佳挂载点：
// 每次 TLS 握手都会回调该函数，返回当前内存中的证书，无需替换 *tls.Config。
type Reloader struct {
	certFile string
	keyFile  string
	mu       sync.RWMutex
	cert     *tls.Certificate
}

// New 从用户自备的证书与私钥文件加载证书并构造 Reloader。
// 文件缺失或格式错误会立即返回错误，避免带着无效证书启动。
func New(certFile, keyFile string) (*Reloader, error) {
	r := &Reloader{certFile: certFile, keyFile: keyFile}
	if err := r.reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// GetCertificate 返回当前加载的证书，供 tls.Config.GetCertificate 调用。
func (r *Reloader) GetCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.cert == nil {
		return nil, errors.New("tls certificate not loaded")
	}
	return r.cert, nil
}

// Watch 定期检查证书文件变化并重新加载，直至收到停止信号。
// 热加载策略与 auth.Store.Watch 保持一致：定时重新加载，失败时记录告警而不中断服务。
func (r *Reloader) Watch(stop <-chan struct{}, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if err := r.reload(); err != nil {
				slog.Warn("failed to reload tls certificate", "cert_file", r.certFile, "key_file", r.keyFile, "error", err)
			}
		}
	}
}

// reload 从文件加载证书并更新内存中的证书；任何失败都不会覆盖已有的有效证书。
func (r *Reloader) reload() error {
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("load x509 key pair cert=%q key=%q: %w", r.certFile, r.keyFile, err)
	}
	r.mu.Lock()
	r.cert = &cert
	r.mu.Unlock()
	return nil
}
