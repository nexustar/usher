//go:build !windows

package procutil

import (
	"os/exec"
	"syscall"
)

// ConfigureGroup gives a backend worker its own process group so forced
// shutdown also reaches MCP servers, shells, and other descendants.
func ConfigureGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func KillGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
