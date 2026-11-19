package hooks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cloud-harness/cloud-harness/internal/program"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "worker" {
		program.ServeWorker(os.Stdin, os.Stdout)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func engineFor(t *testing.T, actions ...Action) *Engine {
	t.Helper()
	return New(&Config{Workspace: t.TempDir(), Hooks: map[Point][]Action{PreToolUse: actions}})
}

func TestCommandExitProtocolAndActionOrder(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		code          int
		block, warn   bool
		count         int
	}{
		{"allow", "printf allowed", 0, false, false, 2},
		{"block", "echo repair-this >&2; exit 2", 2, true, false, 1},
		{"warn", "echo warning; exit 7", 7, false, true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := engineFor(t, Action{Type: "command", Command: tc.command}, Action{Type: "command", Shell: "bash", Command: "echo second"})
			var events []Event
			out, err := engine.Execute(t.Context(), Input{Point: PreToolUse, InvocationID: "one"}, func(_ context.Context, e Event) error {
				events = append(events, e)
				return nil
			})
			if err != nil || out.Blocked != tc.block || len(events) != tc.count || events[0].ExitCode != tc.code || events[0].Warning != tc.warn || events[0].InvocationID != "one" {
				t.Fatalf("out=%+v events=%+v err=%v", out, events, err)
			}
			if tc.block && !strings.Contains(out.Message, "repair-this") {
				t.Fatalf("blocked error missing output: %s", out.Message)
			}
		})
	}
}

func TestTimeoutKillsDescendantsAndBlocks(t *testing.T) {
	engine := engineFor(t, Action{Type: "command", Command: "sleep 0.2; echo escaped > late.txt", TimeoutMS: 25})
	var event Event
	started := time.Now()
	out, err := engine.Execute(t.Context(), Input{Point: PreToolUse}, func(_ context.Context, e Event) error { event = e; return nil })
	if err != nil || !out.Blocked || !event.TimedOut || event.ExitCode != 2 || !strings.Contains(out.Message, "timed out") || time.Since(started) > time.Second {
		t.Fatalf("timeout=%+v event=%+v err=%v", out, event, err)
	}
	<-time.After(250 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(engine.Config.Workspace, "late.txt")); !os.IsNotExist(err) {
		t.Fatalf("timed-out descendant survived: %v", err)
	}
}

func TestMinimalEnvironmentStdinAndRedactedOutput(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "secret-from-parent-999")
	t.Setenv("BASH_ENV", "/must-not-be-loaded")
	t.Setenv("CUSTOM_ENV", "must-not-inherit")
	engine := engineFor(t, Action{Type: "command", Command: `env; cat; printf '\nsecret-from-parent-999\nTOKEN=literal-credential\nAuthorization: Bearer another-secret\nhttps://u:p@example.com/path\n'`})
	var event Event
	_, err := engine.Execute(t.Context(), Input{Point: PreToolUse, Tool: "read_file", Prompt: "submitted text"}, func(_ context.Context, e Event) error { event = e; return nil })
	output := event.Stdout + event.Stderr
	if err != nil || event.ExitCode != 0 || !strings.Contains(output, `"tool":"read_file"`) || !strings.Contains(output, "PATH=") || !strings.Contains(output, "submitted text") {
		t.Fatalf("command input/env: %+v %v", event, err)
	}
	for _, forbidden := range []string{"OPENAI_API_KEY", "secret-from-parent-999", "BASH_ENV", "CUSTOM_ENV", "must-not-inherit", "literal-credential", "another-secret", "u:p@"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("credential or inherited variable %q in event: %s", forbidden, output)
		}
	}
}

func TestOutputHasCombinedFourKiBLimit(t *testing.T) {
	engine := engineFor(t, Action{Type: "command", Command: "head -c 20000 /dev/zero | tr '\\0' x; head -c 20000 /dev/zero >&2"})
	var event Event
	_, err := engine.Execute(t.Context(), Input{Point: PreToolUse}, func(_ context.Context, e Event) error { event = e; return nil })
	if err != nil || event.ExitCode != 0 || len(event.Stdout)+len(event.Stderr) > MaxOutputBytes || !strings.HasSuffix(event.Stdout, "…") || !utf8.ValidString(event.Stdout+event.Stderr) {
		t.Fatalf("output cap: stdout=%d stderr=%d err=%v event=%+v", len(event.Stdout), len(event.Stderr), err, event)
	}
}

func TestStarlarkUsesIsolatedWorkerAndInputBridge(t *testing.T) {
	for _, code := range []int{0, 2} {
		script := `def main():
    input = broker.invoke("hook.input", {})["data"]
    denied = broker.invoke("run_command", {"command": "echo forbidden"})
    if denied["ok"]:
        return {"exit_code": 2, "stderr": "unexpected broker authority"}
    return {"exit_code": ` + string(rune('0'+code)) + `, "stdout": input["tool"] + ":" + denied["error"]["code"]}
`
		engine := engineFor(t, Action{Type: "starlark", Script: script})
		var event Event
		out, err := engine.Execute(t.Context(), Input{Point: PreToolUse, Tool: "read_file"}, func(_ context.Context, e Event) error { event = e; return nil })
		if err != nil || out.Blocked != (code == 2) || event.ExitCode != code || event.Stdout != "read_file:auth_denied" {
			t.Fatalf("Starlark: out=%+v event=%+v err=%v", out, event, err)
		}
	}
}

func TestAuditFailureStopsActionSequence(t *testing.T) {
	engine := engineFor(t, Action{Type: "command", Command: "true"}, Action{Type: "command", Command: "touch should-not-execute"})
	want := errors.New("audit unavailable")
	_, err := engine.Execute(t.Context(), Input{Point: PreToolUse}, func(context.Context, Event) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("audit error lost: %v", err)
	}
	if _, err := os.Stat(filepath.Join(engine.Config.Workspace, "should-not-execute")); !os.IsNotExist(err) {
		t.Fatalf("continued after audit failure: %v", err)
	}
}
