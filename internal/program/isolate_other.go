//go:build !linux

package program

import "fmt"

func applyWorkerLimits() (map[string]uint64, bool, error) {
	return nil, false, fmt.Errorf("rlimits are not applied on this OS")
}

func burnCPU() {}

func readProcLimits(int) (map[string]string, error) {
	return nil, fmt.Errorf("procfs limits are unavailable")
}

func readProcEnviron(int) (string, error) {
	return "", fmt.Errorf("procfs environ is unavailable")
}

func environHasSecrets(string) bool { return true }

func probeCgroup() (bool, string) {
	return false, "cgroup probe is implemented for linux"
}
