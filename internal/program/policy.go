// Package program runs a Starlark script in a separate worker process.
// The parent never interprets the script. If kernel isolation cannot be
// verified, execution is disabled and the script is not started.
package program

import (
	"crypto/sha256"
	"encoding/hex"
	"runtime"

	"github.com/cloud-harness/cloud-harness/internal/domain"
)

const (
	// LimitAddressSpace is a finite virtual-memory cap. Go's runtime reserves
	// large arenas, so a cap below about 2 GiB kills the worker before the
	// script starts. 8 GiB still leaves headroom for a race-instrumented
	// worker and is not "unlimited".
	LimitAddressSpace uint64 = 8 << 30
	LimitCPUSeconds   uint64 = 5
	LimitFileSize     uint64 = 0
	LimitOpenFiles    uint64 = 128
	MaxScriptBytes           = 64 << 10
	DefaultStepLimit  uint64 = 100_000
	MaxStepLimit      uint64 = 1_000_000
)

// Probe is the isolation evidence collected for one worker.
type Probe struct {
	GOOS                string `json:"goos"`
	SeparateProcess     bool   `json:"separate_process"`
	RlimitAS            bool   `json:"rlimit_as"`
	RlimitCPU           bool   `json:"rlimit_cpu"`
	RlimitFsize         bool   `json:"rlimit_fsize"`
	RlimitNofile        bool   `json:"rlimit_nofile"`
	FsizeWriteBlocked   bool   `json:"fsize_write_blocked"`
	CgroupMemory        bool   `json:"cgroup_memory"`
	CgroupDetail        string `json:"cgroup_detail,omitempty"`
	ParentVerified      bool   `json:"parent_verified"`
	SecretEnvBlocked    bool   `json:"secret_env_blocked"`
	ProgrammaticEnabled bool   `json:"programmatic_enabled"`
	Reason              string `json:"reason,omitempty"`
}

// Decide reports whether the probe meets the M0 minimum.
// A step limit is not isolation. cgroup is recorded and preferred when the
// kernel delegates it, but a verified rlimit set is the minimum on Linux.
// Every other OS stays disabled until the same checks have been run there.
func Decide(p Probe) (bool, string) {
	if p.GOOS != "linux" {
		return false, "programmatic execution is disabled on " + p.GOOS + " until process isolation is verified on that OS"
	}
	if !p.SeparateProcess {
		return false, "worker is not a separate process"
	}
	if !p.RlimitCPU || !p.RlimitAS || !p.RlimitFsize || !p.RlimitNofile {
		return false, "kernel rlimits were not applied"
	}
	if !p.FsizeWriteBlocked {
		return false, "RLIMIT_FSIZE did not block a file write"
	}
	if !p.ParentVerified {
		return false, "parent did not observe the worker limits"
	}
	if !p.SecretEnvBlocked {
		return false, "worker environment still contains credentials"
	}
	return true, ""
}

func staticDisableReason() string {
	if runtime.GOOS != "linux" {
		ok, reason := Decide(Probe{GOOS: runtime.GOOS})
		if !ok {
			return reason
		}
	}
	return ""
}

func hashScript(script string) string {
	sum := sha256.Sum256([]byte(script))
	return hex.EncodeToString(sum[:])
}

func disabled(reason string) error {
	if reason == "" {
		reason = "programmatic execution is disabled"
	}
	return domain.E(domain.CodeProgramDisabled, reason)
}
