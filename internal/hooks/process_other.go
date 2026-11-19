//go:build !linux

package hooks

import "os/exec"

func configureProcess(cmd *exec.Cmd) {}
func cleanupProcess(cmd *exec.Cmd)   {}
