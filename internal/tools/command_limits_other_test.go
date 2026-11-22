//go:build !linux

package tools

import (
	"os/exec"
	"testing"
)

func TestGuardCommandIsNoOpOffLinux(t *testing.T) {
	cmd := exec.Command("go", "version")
	path, args := cmd.Path, append([]string(nil), cmd.Args...)
	guard := guardCommand(cmd, CommandLimits{CPUSeconds: u64(1)}, t.TempDir(), false)
	if cmd.Path != path || len(cmd.Args) != len(args) || cmd.SysProcAttr != nil || cmd.Cancel != nil || len(cmd.ExtraFiles) != 0 {
		t.Fatalf("command was changed: %+v", cmd)
	}
	if hit := guard.limitHit(&exec.ExitError{}, "too many open files"); hit != nil {
		t.Fatalf("reported a limit that is not enforced: %+v", hit)
	}
}
