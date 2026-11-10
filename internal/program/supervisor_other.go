//go:build !linux

package program

import (
	"context"
	"os/exec"
)

func applySysProc(*exec.Cmd) {}

func killGroup(*exec.Cmd) error { return nil }

func supervise(context.Context, wire, Invoker) (Result, error) {
	reason := staticDisableReason()
	return Result{Probe: Probe{GOOS: "disabled", Reason: reason}}, disabled(reason)
}
