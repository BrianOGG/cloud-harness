package program

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

// Invoker is the parent-side broker. The worker cannot call a tool except
// through this function.
type Invoker func(ctx context.Context, tool string, args json.RawMessage) (ok bool, data json.RawMessage, err *domain.Error)

// Request is one programmatic execution.
type Request struct {
	Script    string
	StepLimit uint64
}

// Result is the worker's structured result. A protocol completion returns a
// nil error even when the script itself failed; crash and isolation failures
// are returned as errors.
type Result struct {
	OK    bool
	Data  json.RawMessage
	Error *domain.Error
	Hash  string
	Probe Probe
}

func defaultExe() (string, []string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", nil, err
	}
	return exe, []string{"worker"}, nil
}

var exeFn = defaultExe

// Execute runs the script only after the worker proves its limits.
// On failure it does not interpret the script in this process.
func Execute(ctx context.Context, req Request, invoke Invoker) (Result, error) {
	if starlarkEntries != 0 {
		return Result{}, domain.E(domain.CodeProgramDisabled, "refusing to run: this process has already entered the interpreter")
	}
	if reason := staticDisableReason(); reason != "" {
		return Result{Probe: Probe{GOOS: "other", Reason: reason}}, disabled(reason)
	}
	if strings.TrimSpace(req.Script) == "" {
		return Result{}, domain.E(domain.CodeInvalidArgument, "script is empty")
	}
	if len(req.Script) > MaxScriptBytes {
		return Result{}, domain.E(domain.CodeScriptPolicy, "script exceeds the size limit")
	}
	return supervise(ctx, wire{Type: "exec", Script: req.Script, StepLimit: req.StepLimit}, invoke)
}

// Isolate probes a worker and sends no script when isolation is missing.
func Isolate(ctx context.Context) (Probe, error) {
	if reason := staticDisableReason(); reason != "" {
		return Probe{GOOS: "other", Reason: reason, ProgrammaticEnabled: false}, disabled(reason)
	}
	res, err := supervise(ctx, wire{Type: "stop"}, nil)
	return res.Probe, err
}

// ProveCPULimit starts a worker that lowers RLIMIT_CPU and busy-loops.
// A nil error means the operating system stopped that process.
func ProveCPULimit(ctx context.Context) error {
	if reason := staticDisableReason(); reason != "" {
		return disabled(reason)
	}
	_, err := supervise(ctx, wire{Type: "burn_cpu"}, nil)
	if err == nil {
		return domain.E(domain.CodeResourceLimit, "cpu limit did not stop the worker")
	}
	text := err.Error()
	if strings.Contains(text, "signal") || strings.Contains(text, "killed") || strings.Contains(text, "CPU") {
		return nil
	}
	return err
}
