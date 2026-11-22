//go:build !linux && !darwin

package tools

import "os/exec"

// commandGuard runs one run_command process. Resource limits are enforced on
// Linux only, and control files are hidden from the command on Linux and macOS
// only; elsewhere the command runs as it did before either existed.
type commandGuard struct {
	cmd *exec.Cmd
}

func guardCommand(cmd *exec.Cmd, _ CommandLimits, _ string, _ bool) *commandGuard {
	return &commandGuard{cmd: cmd}
}

func warmCommandSandbox() {}

func (g *commandGuard) Run() error { return g.cmd.Run() }

func (g *commandGuard) limitHit(error, string) *limitHit { return nil }
