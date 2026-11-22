//go:build !linux

package tools

import "os/exec"

// commandGuard runs one run_command process. Resource limits are enforced on
// Linux only; elsewhere the command runs as it did before limits existed.
type commandGuard struct {
	cmd *exec.Cmd
}

func guardCommand(cmd *exec.Cmd, _ CommandLimits, _ string) *commandGuard {
	return &commandGuard{cmd: cmd}
}

func (g *commandGuard) Run() error { return g.cmd.Run() }

func (g *commandGuard) limitHit(error, string) *limitHit { return nil }
