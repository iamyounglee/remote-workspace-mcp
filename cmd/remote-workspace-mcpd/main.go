package main

// Package main 是 remote-workspace-mcpd 的入口，负责 CLI 子命令分发与服务启动。

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/iamyounglee/remote-workspace-mcp/internal/audit"
	"github.com/iamyounglee/remote-workspace-mcp/internal/auth"
	"github.com/iamyounglee/remote-workspace-mcp/internal/certs"
	"github.com/iamyounglee/remote-workspace-mcp/internal/config"
	"github.com/iamyounglee/remote-workspace-mcp/internal/logging"
	"github.com/iamyounglee/remote-workspace-mcp/internal/mcpserver"
	"github.com/iamyounglee/remote-workspace-mcp/internal/workspace"
)

const usage = `remote-workspace-mcpd commands:
  serve            Run the Streamable HTTP MCP service
  token show       Print the persisted access token
  token rotate     Generate, persist, and activate a new token
  validate-config  Validate configuration and path policy
`

// main 使用默认参数启动服务，或分发显式 CLI 子命令。
func main() {
	// 顶层版本查询，避免被 normalizeArgs 转换为默认 serve。
	for _, a := range os.Args[1:] {
		if a == "-version" || a == "--version" || a == "version" {
			fmt.Println(mcpserver.Version)
			return
		}
	}

	args := normalizeArgs(os.Args[1:])
	var err error
	switch args[0] {
	case "serve":
		err = serve(args[1:])
	case "token":
		err = tokenCommand(args[1:])
	case "validate-config":
		err = validateConfig(args[1:])
	case "help", "-h", "--help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprint(os.Stderr, usage)
		err = fmt.Errorf("unknown command %q", args[0])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// normalizeArgs 将无参数调用转换为默认服务启动参数。
func normalizeArgs(args []string) []string {
	if len(args) == 0 {
		return []string{"serve", "--config", "config.yaml"}
	}
	return args
}

// configFlag 解析配置文件参数并加载 YAML 配置。
func configFlag(name string, args []string) (config.Config, string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	path := fs.String("config", "./config.yaml", "path to YAML configuration")
	if err := fs.Parse(args); err != nil {
		return config.Config{}, "", err
	}
	cfg, err := config.Load(*path)
	return cfg, *path, err
}

// serve 加载配置并运行 Streamable HTTP MCP 服务。
func serve(args []string) error {
	cfg, _, err := configFlag("serve", args)
	if err != nil {
		return err
	}
	closeLog, err := setupLogging(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = closeLog() }()
	resolver, err := workspace.New(cfg)
	if err != nil {
		return err
	}
	store, created, err := auth.Open(cfg.Auth.TokenFile)
	if err != nil {
		return err
	}
	if created {
		slog.Info("generated initial access token", "fingerprint", store.Fingerprint(), "next", "remote-workspace-mcpd token show --config <path>")
	}
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go store.Watch(stopWatch, time.Second)

	// 受信任代理来自 server.trusted_proxies，已在配置校验阶段验证合法性；
	// 未配置时表示不信任任何代理，审计一律使用对端 IP。
	ips, err := audit.NewIPResolver(cfg.Server.TrustedProxies)
	if err != nil {
		return err
	}
	recorder := audit.NewRecorder(slog.Default(), audit.Options{Enabled: cfg.Logging.AuditEnabled})
	mcp := mcpserver.New(cfg, resolver, store, recorder, ips)
	mux := http.NewServeMux()
	mux.Handle(cfg.Server.MCPPath, mcp)
	mux.Handle("/files", mcp)
	mux.Handle("/directories", mcp)
	mux.Handle("/sync/plan", mcp)
	mux.Handle("/sync/apply", mcp)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})

	httpServer := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           securityHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}

	// 启用 TLS 时，服务仅以 TLS 监听，不再提供明文端口（见配置校验与文档）。
	// 证书来自用户自备文件，并通过 Watch 定时热加载，无需重启进程。
	var tlsConfig *tls.Config
	if cfg.Server.TLS.CertFile != "" {
		cr, cerr := certs.New(cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile)
		if cerr != nil {
			return fmt.Errorf("load tls certificate: %w", cerr)
		}
		stopCertWatch := make(chan struct{})
		defer close(stopCertWatch)
		go cr.Watch(stopCertWatch, time.Second)
		tlsConfig = &tls.Config{GetCertificate: cr.GetCertificate, MinVersion: tls.VersionTLS12}
	}

	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Server.Listen, err)
	}
	errCh := make(chan error, 1)
	go func() {
		if tlsConfig != nil {
			httpServer.TLSConfig = tlsConfig
			slog.Info("remote-workspace-mcp gateway started", "tls", true, "listen", cfg.Server.Listen, "path", cfg.Server.MCPPath, "token_fingerprint", store.Fingerprint())
			errCh <- httpServer.ServeTLS(ln, "", "")
			return
		}
		slog.Info("remote-workspace-mcp gateway started", "tls", false, "listen", cfg.Server.Listen, "path", cfg.Server.MCPPath, "token_fingerprint", store.Fingerprint())
		errCh <- httpServer.Serve(ln)
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-signals:
		slog.Info("shutting down", "signal", sig.String())
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpServer.Shutdown(ctx)
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

// tokenCommand 执行访问令牌的查看或轮换子命令。
func tokenCommand(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("token command requires show or rotate")
	}
	cfg, _, err := configFlag("token "+args[0], args[1:])
	if err != nil {
		return err
	}
	switch args[0] {
	case "show":
		token, err := auth.Read(cfg.Auth.TokenFile)
		if err != nil {
			return err
		}
		fmt.Println(token)
		return nil
	case "rotate":
		token, err := auth.Rotate(cfg.Auth.TokenFile)
		if err != nil {
			return err
		}
		fmt.Println(token)
		return nil
	default:
		return fmt.Errorf("unknown token command %q", args[0])
	}
}

// validateConfig 加载并校验配置及工作区路径策略。
func validateConfig(args []string) error {
	cfg, path, err := configFlag("validate-config", args)
	if err != nil {
		return err
	}
	if _, err := workspace.New(cfg); err != nil {
		return err
	}
	fmt.Printf("configuration is valid: %s\n", path)
	return nil
}

// setupLogging 创建仅写文件的结构化日志，并返回关闭函数。
func setupLogging(cfg config.Config) (func() error, error) {
	writer, err := logging.Open(cfg.Logging.File, cfg.Logging.MaxSizeMB<<20, cfg.Logging.MaxFiles)
	if err != nil {
		return nil, err
	}
	levels := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	}
	handler := slog.NewTextHandler(writer, &slog.HandlerOptions{Level: levels[cfg.Logging.Level]})
	slog.SetDefault(slog.New(handler))
	return writer.Close, nil
}

// securityHeaders 为所有 HTTP 响应增加基础安全头。
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
