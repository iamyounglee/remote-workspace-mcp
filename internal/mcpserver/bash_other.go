//go:build !linux

package mcpserver

import "os/exec"

// setProcessGroup 在非 Linux 平台为空实现。进程组级清理为 Linux 专属能力
// （syscall.SysProcAttr.Setpgid / syscall.Kill 在其他平台不可用）；其余平台
// 沿用 Go 运行时默认的 cmd.Cancel 行为（向直接子进程发送 SIGKILL），足以
// 覆盖主要场景，且不影响 cross-platform 构建。
func setProcessGroup(cmd *exec.Cmd) {}
