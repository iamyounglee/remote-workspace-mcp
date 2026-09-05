package config

// Package config 定义 remote-workspace-mcpd 的全部配置项、默认值加载与校验逻辑。

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/iamyounglee/remote-workspace-mcp/internal/audit"
	"gopkg.in/yaml.v3"
)

// Config 汇总服务运行所需的全部配置项
type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Auth      AuthConfig      `yaml:"auth"`
	Workspace WorkspaceConfig `yaml:"workspace"`
	Paths     PathsConfig     `yaml:"paths"`
	Files     FilesConfig     `yaml:"files"`
	Bash      BashConfig      `yaml:"bash"`
	Logging   LoggingConfig   `yaml:"logging"`
}

// ServerConfig 定义服务监听、路由、状态目录与 TLS 配置
type ServerConfig struct {
	Name     string `yaml:"name"`
	Listen   string `yaml:"listen"`
	MCPPath  string `yaml:"mcp_path"`
	StateDir string `yaml:"state_dir"`
	// TrustedProxies 为受信任代理的 IP 或 CIDR 列表。
	// 仅当请求来自这些来源时，审计才采信 X-Forwarded-For 中的客户端 IP。
	// 留空表示不信任任何代理，审计一律使用对端 IP。
	TrustedProxies []string `yaml:"trusted_proxies"`
	// TLS 为用户自备证书文件路径；配置后即仅以 TLS 监听，不再提供明文端口。
	TLS TLSConfig `yaml:"tls"`
}

// TLSConfig 定义用户自备证书文件（cert_file、key_file）的路径。
// 两者必须同时配置；配置后服务仅以 TLS 监听，且证书支持热加载。
type TLSConfig struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// AuthConfig 定义访问令牌文件配置
type AuthConfig struct {
	TokenFile string `yaml:"token_file"`
}

// WorkspaceConfig 定义工作区根目录配置
type WorkspaceConfig struct {
	Root string `yaml:"root"`
}

// PathsConfig 定义工作区外的可读和可写路径白名单
type PathsConfig struct {
	Readable []string `yaml:"readable"`
	Writable []string `yaml:"writable"`
}

// FilesConfig 定义文件操作、归档和传输的容量限制
type FilesConfig struct {
	MaxReadBytes         int64    `yaml:"max_read_bytes"`
	MaxWriteBytes        int64    `yaml:"max_write_bytes"`
	MaxResultBytes       int64    `yaml:"max_result_bytes"`
	MaxUploadBytes       int64    `yaml:"max_upload_bytes"`
	MaxDownloadBytes     int64    `yaml:"max_download_bytes"`
	MaxArchiveEntries    int      `yaml:"max_archive_entries"`
	MaxExtractedBytes    int64    `yaml:"max_extracted_bytes"`
	MaxSyncManifestBytes int64    `yaml:"max_sync_manifest_bytes"`
	CreateParentDirs     bool     `yaml:"create_parent_directories"`
	IgnoreDirectories    []string `yaml:"ignore_directories"`
}

// BashConfig 定义命令执行的开关、沙箱、超时与输出限制
type BashConfig struct {
	Enabled        bool          `yaml:"enabled"`
	TimeoutSeconds int           `yaml:"timeout_seconds"`
	MaxOutputBytes int64         `yaml:"max_output_bytes"`
	MaxConcurrent  int           `yaml:"max_concurrent"`
	Sandbox        SandboxConfig `yaml:"sandbox"`
}

// SandboxConfig 定义 Bash 命令执行的沙箱隔离配置
type SandboxConfig struct {
	Mode         string `yaml:"mode"`
	Require      bool   `yaml:"require"`
	AllowNetwork bool   `yaml:"allow_network"`
}

// LoggingConfig 定义日志级别、文件位置与审计开关
type LoggingConfig struct {
	Level        string `yaml:"level"`
	File         string `yaml:"file"`
	MaxSizeMB    int64  `yaml:"max_size_mb"`
	MaxFiles     int    `yaml:"max_files"`
	AuditEnabled bool   `yaml:"audit_enabled"`
}

// Defaults 返回一份包含默认值的完整配置。
func Defaults() Config {
	return Config{
		Server: ServerConfig{Name: "remote-workspace-mcpd", Listen: "0.0.0.0:8080", MCPPath: "/mcp", StateDir: "./state"},
		Auth:   AuthConfig{TokenFile: "./state/access-token"},
		Files: FilesConfig{
			MaxReadBytes: 1 << 20, MaxWriteBytes: 10 << 20, MaxResultBytes: 1 << 20,
			MaxUploadBytes: 100 << 20, MaxDownloadBytes: 1 << 30,
			MaxArchiveEntries: 10000, MaxExtractedBytes: 1 << 30, MaxSyncManifestBytes: 10 << 20,
			CreateParentDirs: true, IgnoreDirectories: []string{".git", "node_modules", "vendor", "dist"},
		},
		Bash: BashConfig{
			Enabled: true, TimeoutSeconds: 30,
			MaxOutputBytes: 1 << 20, MaxConcurrent: 4,
			Sandbox: SandboxConfig{Mode: "none", Require: false, AllowNetwork: false},
		},
		Logging: LoggingConfig{Level: "info", File: "./logs/remote-workspace-mcpd.log", MaxSizeMB: 20, MaxFiles: 5, AuditEnabled: false},
	}
}

// Load 读取单份 YAML 配置并在默认值基础上完成校验。
func Load(path string) (Config, error) {
	cfg := Defaults()
	file, err := os.Open(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	defer file.Close()
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("parse YAML config: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return cfg, fmt.Errorf("parse YAML config: multiple documents are not supported")
	}
	if cfg.Server.MCPPath == "" {
		cfg.Server.MCPPath = "/mcp"
	}
	if cfg.Server.Name == "" {
		cfg.Server.Name = "remote-workspace-mcpd"
	}
	if cfg.Server.Listen == "" {
		cfg.Server.Listen = "0.0.0.0:8080"
	}
	if cfg.Server.StateDir == "" {
		cfg.Server.StateDir = "./state"
	}
	if cfg.Auth.TokenFile == "" {
		cfg.Auth.TokenFile = filepath.Join(cfg.Server.StateDir, "access-token")
	}
	if err := Validate(&cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Validate 校验配置必填项、数值范围和命令执行模式。
func Validate(cfg *Config) error {
	if cfg.Workspace.Root == "" {
		return fmt.Errorf("workspace.root is required")
	}
	root, err := filepath.Abs(cfg.Workspace.Root)
	if err != nil {
		return fmt.Errorf("workspace.root: %w", err)
	}
	cfg.Workspace.Root = filepath.Clean(root)
	if cfg.Server.MCPPath == "" || cfg.Server.MCPPath[0] != '/' {
		return fmt.Errorf("server.mcp_path must start with /")
	}
	if _, err := audit.NewIPResolver(cfg.Server.TrustedProxies); err != nil {
		return fmt.Errorf("server.trusted_proxies: %w", err)
	}
	if cfg.Bash.Sandbox.Mode != "none" && cfg.Bash.Sandbox.Mode != "bubblewrap" {
		return fmt.Errorf("bash.sandbox.mode must be none or bubblewrap")
	}
	if cfg.Bash.Sandbox.Require && cfg.Bash.Sandbox.Mode == "bubblewrap" {
		if _, err := exec.LookPath("bwrap"); err != nil {
			return fmt.Errorf("bash.sandbox.require is true but bwrap is not available: %w", err)
		}
	}
	if cfg.Bash.MaxConcurrent < 1 {
		cfg.Bash.MaxConcurrent = 1
	}
	if cfg.Bash.TimeoutSeconds < 1 {
		cfg.Bash.TimeoutSeconds = 30
	}
	if cfg.Files.MaxUploadBytes < 1 {
		cfg.Files.MaxUploadBytes = 100 << 20
	}
	if cfg.Files.MaxDownloadBytes < 1 {
		cfg.Files.MaxDownloadBytes = 1 << 30
	}
	if cfg.Files.MaxArchiveEntries < 1 {
		cfg.Files.MaxArchiveEntries = 10000
	}
	if cfg.Files.MaxExtractedBytes < 1 {
		cfg.Files.MaxExtractedBytes = 1 << 30
	}
	if cfg.Files.MaxSyncManifestBytes < 1 {
		cfg.Files.MaxSyncManifestBytes = 10 << 20
	}
	if cfg.Logging.File == "" {
		return errors.New("logging.file is required")
	}
	if cfg.Logging.MaxSizeMB < 1 {
		cfg.Logging.MaxSizeMB = 20
	}
	if cfg.Logging.MaxFiles < 1 {
		cfg.Logging.MaxFiles = 5
	}
	if cfg.Logging.Level != "debug" && cfg.Logging.Level != "info" && cfg.Logging.Level != "warn" && cfg.Logging.Level != "error" {
		return fmt.Errorf("logging.level must be debug, info, warn, or error")
	}
	for i := range cfg.Paths.Readable {
		cfg.Paths.Readable[i], err = filepath.Abs(cfg.Paths.Readable[i])
		if err != nil {
			return fmt.Errorf("paths.readable: %w", err)
		}
	}
	for i := range cfg.Paths.Writable {
		cfg.Paths.Writable[i], err = filepath.Abs(cfg.Paths.Writable[i])
		if err != nil {
			return fmt.Errorf("paths.writable: %w", err)
		}
	}
	// 启用 TLS 时 cert_file 与 key_file 必须成对配置；启用后服务不再提供明文端口。
	if cfg.Server.TLS.CertFile != "" || cfg.Server.TLS.KeyFile != "" {
		if cfg.Server.TLS.CertFile == "" || cfg.Server.TLS.KeyFile == "" {
			return fmt.Errorf("server.tls.cert_file and server.tls.key_file must be set together")
		}
	}
	return nil
}
