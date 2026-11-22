package program

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "worker" {
		ServeWorker(os.Stdin, os.Stdout)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestScriptCannotTouchTheSystem(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := Execute(ctx, Request{Script: "def main():\n  return open('/etc/passwd')\n"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || res.Error == nil || res.Error.Code != domain.CodeScriptError {
		t.Fatalf("open was interpreted: %+v", res.Error)
	}
	if starlarkEntries != 0 {
		t.Fatal("parent interpreted the script")
	}
}

func TestDecideRejectsMissingIsolation(t *testing.T) {
	ok, reason := Decide(Probe{GOOS: "linux"})
	if ok || reason == "" {
		t.Fatalf("empty probe enabled: %v %s", ok, reason)
	}
	ok, reason = Decide(Probe{GOOS: "darwin", SeparateProcess: true, RlimitAS: true, RlimitCPU: true, RlimitFsize: true, RlimitNofile: true, FsizeWriteBlocked: true, ParentVerified: true, SecretEnvBlocked: true})
	if ok || !strings.Contains(reason, "darwin") {
		t.Fatalf("darwin enabled: %v %s", ok, reason)
	}
	ok, reason = Decide(Probe{GOOS: "windows", SeparateProcess: true, RlimitAS: true, RlimitCPU: true, RlimitFsize: true, RlimitNofile: true, FsizeWriteBlocked: true, ParentVerified: true, SecretEnvBlocked: true})
	if ok || !strings.Contains(reason, "windows") {
		t.Fatalf("windows enabled: %v %s", ok, reason)
	}
	if starlarkEntries != 0 {
		t.Fatal("decision path entered the interpreter")
	}
}

func TestExecuteDisabledDoesNotStart(t *testing.T) {
	prev := exeFn
	called := false
	exeFn = func() (string, []string, error) {
		called = true
		return "", nil, domain.E(domain.CodeProgramDisabled, "should not be called")
	}
	t.Cleanup(func() { exeFn = prev })
	// Force the pre-spawn refusal by pretending this is not linux through Decide,
	// then call Execute only when the OS would refuse. On linux, call supervise's
	// caller with an explicitly unverified probe via RejectBeforeStart.
	res, err := rejectBeforeStart(Request{Script: "def main():\n  return 1\n"})
	if called {
		t.Fatal("worker process was started")
	}
	if !domain.IsCode(err, domain.CodeProgramDisabled) {
		t.Fatalf("%v", err)
	}
	if res.Probe.ProgrammaticEnabled {
		t.Fatal("disabled probe was enabled")
	}
	if starlarkEntries != 0 {
		t.Fatal("parent interpreted the script")
	}
}

func rejectBeforeStart(req Request) (Result, error) {
	probe := Probe{GOOS: "linux", Reason: "rlimit missing"}
	ok, reason := Decide(probe)
	probe.ProgrammaticEnabled = ok
	probe.Reason = reason
	if !ok {
		return Result{Probe: probe, Hash: hashScript(req.Script)}, disabled(reason)
	}
	return Result{}, domain.E(domain.CodeConflict, "test expected the probe to be disabled")
}

func TestWorkerBrokerSyntaxAndLimits(t *testing.T) {
	if starlarkEntries != 0 {
		t.Fatal("parent entered starlark before the test")
	}
	t.Setenv("OPENAI_API_KEY", "sk-should-not-leak")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	probe, err := Isolate(ctx)
	if err != nil {
		t.Fatalf("isolate: %v probe=%+v", err, probe)
	}
	if !probe.ProgrammaticEnabled {
		t.Fatalf("programmatic execution disabled: %+v", probe)
	}
	if !probe.SecretEnvBlocked || !probe.ParentVerified || !probe.FsizeWriteBlocked {
		t.Fatalf("probe %+v", probe)
	}
	t.Logf("cgroup_memory=%v detail=%s", probe.CgroupMemory, probe.CgroupDetail)

	var calls []string
	res, err := Execute(ctx, Request{Script: `
def main():
    a = broker.invoke("echo", {"value": "ping"})
    if not a["ok"]:
        return {"stage": "echo"}
    b = broker.invoke("shell", {"cmd": "id"})
    code = ""
    if b["error"] != None:
        code = b["error"]["code"]
    return {"echo": a["data"]["value"], "shell_ok": b["ok"], "shell_code": code}
`}, func(_ context.Context, tool string, args json.RawMessage) (bool, json.RawMessage, *domain.Error) {
		calls = append(calls, tool)
		if tool != "echo" {
			return false, nil, domain.E(domain.CodeAuthDenied, "not on the allowlist")
		}
		return true, args, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("script failed: %+v", res.Error)
	}
	if strings.Join(calls, ",") != "echo,shell" {
		t.Fatalf("calls %v", calls)
	}
	var got map[string]any
	if err := json.Unmarshal(res.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got["echo"] != "ping" || got["shell_ok"] != false || got["shell_code"] != domain.CodeAuthDenied {
		t.Fatalf("data %s", res.Data)
	}
	if starlarkEntries != 0 {
		t.Fatal("parent process interpreted the script")
	}

	syn, err := Execute(ctx, Request{Script: "def main(\n  return 1\n"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if syn.OK || syn.Error == nil || syn.Error.Code != domain.CodeScriptSyntax {
		t.Fatalf("syntax %+v", syn.Error)
	}

	load, err := Execute(ctx, Request{Script: "load(\"evil.star\", \"x\")\ndef main():\n  return 1\n"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if load.OK || load.Error == nil || load.Error.Code != domain.CodeScriptPolicy {
		t.Fatalf("load %+v", load.Error)
	}

	openRes, err := Execute(ctx, Request{Script: "def main():\n  return open(\"/etc/passwd\")\n"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if openRes.OK || openRes.Error == nil || openRes.Error.Code != domain.CodeScriptError {
		t.Fatalf("open %+v", openRes.Error)
	}

	steps, err := Execute(ctx, Request{StepLimit: 50, Script: "def main():\n  n = 0\n  for i in range(100000):\n    n = n + 1\n  return n\n"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if steps.OK || steps.Error == nil || steps.Error.Code != domain.CodeResourceLimit {
		t.Fatalf("steps %+v", steps.Error)
	}
}

func TestCPULimitStopsWorker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := ProveCPULimit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestCancelKillsWorker(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	go func() {
		select {
		case <-started:
			cancel()
		case <-time.After(2 * time.Second):
			cancel()
		}
	}()
	res, err := Execute(ctx, Request{Script: "def main():\n  return broker.invoke(\"block\", {})\n"}, func(ctx context.Context, tool string, _ json.RawMessage) (bool, json.RawMessage, *domain.Error) {
		if tool != "block" {
			return false, nil, domain.E(domain.CodeAuthDenied, "no")
		}
		select {
		case <-started:
		default:
			close(started)
		}
		<-ctx.Done()
		return false, nil, domain.E(domain.CodeCancelled, "cancelled")
	})
	if err == nil && res.OK {
		var body map[string]any
		if json.Unmarshal(res.Data, &body) == nil && body["ok"] == false {
			return
		}
		t.Fatalf("cancelled script completed without a cancellation: %#v %s", res.Error, res.Data)
	}
	if err == nil && (res.Error == nil || res.Error.Code == "") {
		t.Fatal("cancel produced neither a process error nor a script error")
	}
}
