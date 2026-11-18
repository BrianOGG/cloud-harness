//go:build !linux

package tools

import (
	"os/exec"
)

func applySysProc(*exec.Cmd) {}

func killGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
