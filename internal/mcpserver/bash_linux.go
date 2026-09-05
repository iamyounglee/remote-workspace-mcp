//go:build linux

package mcpserver

import (
	"os/exec"
	"syscall"
)

// setProcessGroup 将命令放入独立进程组，并在 ctx 取消/超时
// 时由 cmd.Cancel 向整个进程组发送 SIGKILL，确保 bash -c 派生的全部子进程
// （管道、后台任务、sleep 等）随命令一起被清理，避免成为孤儿进程持续占用资源。
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// 负 PID 表示向进程组（含直接进程及其子进程）发送信号。
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return nil
	}
}
