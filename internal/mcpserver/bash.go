package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/iamyounglee/remote-workspace-mcp/internal/workspace"
)

// bash 在并发、超时和输出限制下执行允许的命令。
func (s *Server) bash(ctx context.Context, raw json.RawMessage) (any, error) {
	if !s.cfg.Bash.Enabled {
		return nil, errors.New("bash is disabled")
	}
	var a struct {
		Command   string `json:"command"`
		CWD       string `json:"cwd"`
		Timeout   int    `json:"timeout_seconds"`
		MaxOutput int64  `json:"max_output_bytes"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.Command) == "" {
		return nil, errors.New("command is required")
	}
	// 检测 bash 可执行文件是否可用（Windows 未安装 Git Bash/WSL 时常见）。
	if _, err := exec.LookPath(resolvedBash); err != nil {
		slog.Error("bash executable not found; the bash tool requires bash on PATH",
			"resolved", resolvedBash,
			"hint", "on Windows install Git Bash or WSL and add bash to PATH; on macOS ensure /bin/bash exists")
		return nil, fmt.Errorf("bash executable not found: %w", err)
	}
	if a.CWD == "" {
		a.CWD = "."
	}
	cwd, err := s.resolver.Resolve(a.CWD, workspace.Read)
	if err != nil {
		return nil, err
	}
	// 命令始终经 `bash -c` 执行（不使用 -l：login shell 会重置 PATH/HOME 等环境，
	// 破坏服务进程环境继承），支持完整 shell 语法、管道、重定向、命令替换等。
	// 约束完全依赖 Bubblewrap 沙箱（见 cfg.Bash.Sandbox.Mode）与运行用户权限，而非命令级白/黑名单。
	argv := []string{resolvedBash, "-c", a.Command}
	if a.Timeout <= 0 || a.Timeout > s.cfg.Bash.TimeoutSeconds {
		a.Timeout = s.cfg.Bash.TimeoutSeconds
	}
	if a.MaxOutput <= 0 || a.MaxOutput > s.cfg.Bash.MaxOutputBytes {
		a.MaxOutput = s.cfg.Bash.MaxOutputBytes
	}
	select {
	case s.bashSem <- struct{}{}:
		defer func() { <-s.bashSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// bash 并发受 bashSem 限制（cfg.Bash.MaxConcurrent）。bash 与文件写之间不加互斥锁：
	// 服务器无从预知 bash 会触碰哪些文件，也不对其做文件级隔离；并发安全性交由调用方
	// （agent）通过顺序调用 + re-read 保证，与主流 agent 一致。
	cmdCtx, cancel := context.WithTimeout(ctx, time.Duration(a.Timeout)*time.Second)
	defer cancel()
	cmd, err := s.bashCommand(cmdCtx, cwd, argv)
	if err != nil {
		return nil, err
	}
	// 设置工作目录。Windows 上 cwd 为反斜杠原生路径（如 C:\Users\dev\workspace），
	// 直接传给 Git Bash 时反斜杠会被 shell 当作转义符，故转换为正斜杠形式
	cmdDir := cwd
	if runtime.GOOS == "windows" {
		cmdDir = filepath.ToSlash(cwd)
	}
	cmd.Dir = cmdDir
	var stdout, stderr limitedBuffer
	stdout.max, stderr.max = a.MaxOutput, a.MaxOutput
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	started := time.Now()
	err = cmd.Run()
	timedOut := errors.Is(cmdCtx.Err(), context.DeadlineExceeded)
	exitCode := 0
	if err != nil {
		exitCode = 1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exitCode = ee.ExitCode()
		}
	}
	return map[string]any{
		"stdout":      stdout.String(),
		"stderr":      stderr.String(),
		"exit_code":   exitCode,
		"timed_out":   timedOut,
		"truncated":   stdout.truncated || stderr.truncated,
		"duration_ms": time.Since(started).Milliseconds(),
	}, nil
}

// resolvedBash 缓存 bash 可执行文件的绝对路径，避免运行时 PATH（例如测试中被 t.Setenv
// 改写的 PATH）影响命令解析。包加载时解析一次，找不到时回退为 "bash" 交由 exec 查找。
var resolvedBash string

func init() {
	if p, err := exec.LookPath("bash"); err == nil {
		resolvedBash = p
	} else {
		resolvedBash = "bash"
	}
}

// bashCommand 根据沙箱配置构造受控命令。
func (s *Server) bashCommand(ctx context.Context, cwd string, argv []string) (*exec.Cmd, error) {
	if len(argv) == 0 {
		return nil, errors.New("command is empty")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// 继承服务进程环境（含 PATH、HOME 等），确保非沙箱模式命令能找到工具并保留服务环境
	// （见 TestBashPreservesServiceEnvironmentWithoutSandbox）。Bubblewrap 沙箱会在此
	// 基础上进一步裁剪/绑定文件系统视图。
	cmd.Env = os.Environ()
	cmd.WaitDelay = bashWaitDelay
	// 将命令放入独立进程组，超时/取消时 kill 整个进程组，避免 bash -c
	// 派生的子进程（管道、后台任务）在 bash 被 SIGKILL 后成为孤儿继续运行。
	setProcessGroup(cmd)
	if s.cfg.Bash.Sandbox.Mode != "bubblewrap" {
		return cmd, nil
	}
	// Bubblewrap 仅 Linux 可用。macOS/Windows 请求沙箱属于无效配置，需显式处理并记录日志，
	// 避免静默降级导致用户误以为命令在沙箱中执行。
	if runtime.GOOS != "linux" {
		if s.cfg.Bash.Sandbox.Require {
			return nil, fmt.Errorf("bubblewrap sandbox is only supported on Linux, but sandbox.require is set on platform %s", runtime.GOOS)
		}
		slog.Warn("bubblewrap sandbox requested but unsupported on this platform; falling back to unsandboxed execution",
			"platform", runtime.GOOS,
			"sandbox", s.cfg.Bash.Sandbox.Mode)
		return cmd, nil
	}
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		if s.cfg.Bash.Sandbox.Require {
			return nil, fmt.Errorf("bubblewrap is required but unavailable: %w", err)
		}
		slog.Warn("bubblewrap requested but bwrap not found; falling back to unsandboxed execution",
			"require", s.cfg.Bash.Sandbox.Require)
		return cmd, nil
	}
	args := s.bubblewrapArgs(cwd, argv)
	bcmd := exec.CommandContext(ctx, bwrap, args...)
	bcmd.WaitDelay = bashWaitDelay
	// 与上面非沙箱命令一致：超时/取消时 kill 进程组，清理 bwrap 及其子进程。
	setProcessGroup(bcmd)
	return bcmd, nil
}

// bashWaitDelay 限制 Wait 等待 stdout/stderr 管道关闭的时间。
// bash 因超时被杀（SIGKILL）后，其子进程（如 sleep）仍持有管道写端，
// 会导致 exec.Cmd.Wait 挂起直至管道自然关闭（表现为超时"失效"、
// duration 等于命令完整执行时长）。设置 WaitDelay 确保超时后能及时返回
// （Go 1.20+ 的 WaitDelay 从进程退出后开始计时，正常命令的管道会即时
// 关闭不受影响）。值取 500ms：既给输出排空留出余量，又避免超时返回
// 被过度拖延。
const bashWaitDelay = 500 * time.Millisecond

// bubblewrapArgs 根据网络策略和路径权限构造 Bubblewrap 参数。
func (s *Server) bubblewrapArgs(cwd string, argv []string) []string {
	return s.bubblewrapArgsWithNetworkPaths(cwd, argv, []string{
		"/etc/ssl/certs",
		"/etc/pki/tls/certs",
		"/etc/resolv.conf",
		"/etc/hosts",
		"/etc/host.conf",
		"/etc/nsswitch.conf",
		"/etc/gai.conf",
		"/etc/ca-certificates.conf",
	})
}

// bubblewrapArgsWithNetworkPaths 使用指定网络配置路径构造可测试的 Bubblewrap 参数。
func (s *Server) bubblewrapArgsWithNetworkPaths(cwd string, argv, networkPaths []string) []string {
	args := []string{"--die-with-parent", "--new-session", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp"}
	if !s.cfg.Bash.Sandbox.AllowNetwork {
		args = append(args, "--unshare-net")
	}
	args = appendExistingReadOnlyBinds(args, []string{"/usr", "/bin", "/lib", "/lib64"})
	if s.cfg.Bash.Sandbox.AllowNetwork {
		args = appendExistingReadOnlyBinds(args, networkPaths)
	}
	args = append(args, "--bind", s.resolver.Workspace(), s.resolver.Workspace())
	for _, path := range s.resolver.Readable() {
		args = append(args, "--ro-bind", path, path)
	}
	for _, path := range s.resolver.Writable() {
		args = append(args, "--bind", path, path)
	}
	args = append(args, "--chdir", cwd)
	return append(args, argv...)
}

// appendExistingReadOnlyBinds 将存在的主机路径以只读方式加入 Bubblewrap 参数。
func appendExistingReadOnlyBinds(args []string, paths []string) []string {
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			args = append(args, "--ro-bind", path, path)
		}
	}
	return args
}

// limitedBuffer 限制命令输出大小并记录截断状态
type limitedBuffer struct {
	buf       bytes.Buffer
	max       int64
	truncated bool
}

// Write 向限长缓冲区写入数据并记录是否发生截断。
func (b *limitedBuffer) Write(p []byte) (int, error) {
	remain := b.max - int64(b.buf.Len())
	if remain <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if int64(len(p)) > remain {
		_, _ = b.buf.Write(p[:remain])
		b.truncated = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

// String 返回限长缓冲区当前保存的文本。
func (b *limitedBuffer) String() string { return b.buf.String() }


