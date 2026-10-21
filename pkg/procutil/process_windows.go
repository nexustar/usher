//go:build windows

package procutil

import "os/exec"

func ConfigureGroup(cmd *exec.Cmd) {}

func KillGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
