package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

func TestOutsideCommandResourcesUseExactCommandScope(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	for _, command := range []string{"ls -la /", "wc " + filepath.Join(outside, "sample"), "ls . " + outside} {
		argv, dangerous, err := parseCommand(root, command)
		if err != nil || !dangerous || len(argv) == 0 {
			t.Fatalf("parse %q: argv=%v dangerous=%v err=%v", command, argv, dangerous, err)
		}
		resources, err := commandResources(root, command)
		if err != nil || len(resources) != 1 || resources[0] != command {
			t.Fatalf("resources %q: %v err=%v", command, resources, err)
		}
	}
	control := filepath.Join(root, ".cloud-harness")
	if err := os.MkdirAll(control, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(outside, "alias")
	if err := os.Symlink(control, alias); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"ls / .cloud-harness", "ls " + alias, "wc " + filepath.Join(alias, "harness.db")} {
		if _, err := commandResources(root, command); !domain.IsCode(err, domain.CodeAuthDenied) {
			t.Fatalf("control argument %q: %v", command, err)
		}
	}
}

func TestRunCommandShellRequiresFrozenModeAndExactGrant(t *testing.T) {
	root := t.TempDir()
	command := "printf shell-result | cat"
	grants := &memGrants{grants: []domain.Grant{
		{Tool: "run_command", Op: "call", State: "active", RunID: "run", ResourceScope: root},
		{Tool: "run_command", Op: "call", State: "active", RunID: "run", InvocationID: "other", ResourceScope: command},
	}}
	b := NewBroker(&memCharge{b: domain.Budget{MaxToolCalls: 20}}, grants)
	if err := RegisterLocal(b, root); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]string{"command": command, "approval_mode": "full-auto", "ApprovalMode": "full-auto"})
	req := Request{RunID: "run", InvocationID: "inv", Tool: "run_command", Args: args, ApprovalMode: "full-auto"}
	res, err := b.Invoke(t.Context(), req)
	if err != nil || res.OK || res.Dispatched || res.Error == nil || res.Error.Code != domain.CodeAuthDenied {
		t.Fatalf("broad/other invocation grant allowed shell: %+v %v", res, err)
	}
	grants.grants = append(grants.grants, domain.Grant{Tool: "run_command", Op: "call", State: "active", RunID: "run", InvocationID: "inv", ResourceScope: command})
	for _, mode := range []string{"", "ask", "safe", "workspace-write", "full-auto"} {
		req.ApprovalMode = mode
		res, err = b.Invoke(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		if mode != "full-auto" {
			if res.OK || res.Error == nil || res.Error.Code != domain.CodeAuthDenied {
				t.Fatalf("model arguments enabled shell in %q: %+v", mode, res)
			}
			continue
		}
		if !res.OK || !res.Dispatched || !strings.Contains(string(res.Data), "shell-result") {
			t.Fatalf("full-auto shell failed: %+v", res)
		}
	}
	// The executor itself cannot be called with model arguments as authority.
	spec, _ := b.spec("run_command")
	if _, err := spec.Execute(context.Background(), args); !domain.IsCode(err, domain.CodeAuthDenied) {
		t.Fatalf("direct executor bypassed trusted request: %v", err)
	}
	req.Args = json.RawMessage(`{"command":"printf different-result | cat"}`)
	res, err = b.Invoke(t.Context(), req)
	if err != nil || res.OK || res.Dispatched {
		t.Fatalf("exact grant authorized another command: %+v %v", res, err)
	}
}

func TestRunCommandFullAutoCwdBoundaries(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	configDir := t.TempDir()
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(configDir, "settings.json"))
	configAlias := filepath.Join(outside, "config-alias")
	if err := os.Symlink(configDir, configAlias); err != nil {
		t.Fatal(err)
	}
	control := filepath.Join(root, ".cloud-harness")
	if err := os.MkdirAll(control, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(outside, "control-alias")
	if err := os.Symlink(control, alias); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(outside, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"", "ask", "safe", "workspace-write", "full-auto"} {
		resources, err := commandResourcesForMode(root, outside, "pwd", mode)
		if mode == "full-auto" {
			if err != nil || len(resources) != 1 || resources[0] != "pwd" {
				t.Fatalf("full-auto cwd resources=%v err=%v", resources, err)
			}
		} else if err == nil || ValidCommandCwd(root, outside, mode) {
			t.Fatalf("outside cwd allowed in %q", mode)
		}
		for _, cwd := range []string{"..", control, alias, configDir, configAlias, file, filepath.Join(outside, "missing")} {
			if _, err := CommandWorkDir(root, cwd, mode); err == nil {
				t.Fatalf("cwd=%q allowed in %q", cwd, mode)
			}
		}
	}
}

func TestFullAutoShellStillFailsClosedWithoutControlSandbox(t *testing.T) {
	root := t.TempDir()
	// A config directory containing the workspace cannot be hidden without
	// hiding the workspace; even full-auto must not launch the shell there.
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(root, "settings.json"))
	command := "printf leaked > started | cat"
	b := NewBroker(&memCharge{b: domain.Budget{MaxToolCalls: 1}}, &memGrants{grants: []domain.Grant{
		{Tool: "run_command", RunID: "run", InvocationID: "inv", ResourceScope: command, State: "active"},
	}})
	if err := RegisterLocal(b, root); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]string{"command": command})
	res, err := b.Invoke(t.Context(), Request{RunID: "run", InvocationID: "inv", Tool: "run_command", Args: args, ApprovalMode: "full-auto"})
	if err != nil || res.OK || res.Error == nil || !strings.Contains(res.Error.Message, "sandbox") {
		t.Fatalf("sandbox failure=%+v err=%v", res, err)
	}
	if _, err := os.Stat(filepath.Join(root, "started")); !os.IsNotExist(err) {
		t.Fatalf("shell started despite unavailable sandbox: %v", err)
	}
}

func TestFullAutoCannotUseOriginalControlDirectoryAsCwd(t *testing.T) {
	original := t.TempDir()
	control := filepath.Join(original, ".cloud-harness")
	root := filepath.Join(control, "worktrees", "run")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if !ValidCommandCwd(root, root, "full-auto") {
		t.Fatal("preserved worktree was denied")
	}
	for _, cwd := range []string{control, filepath.Join(control, "worktrees")} {
		if ValidCommandCwd(root, cwd, "full-auto") {
			t.Fatalf("original control cwd allowed: %s", cwd)
		}
	}
	if _, err := commandResourcesForMode(root, "", "ls "+control, "full-auto"); !domain.IsCode(err, domain.CodeAuthDenied) {
		t.Fatalf("original control argument allowed: %v", err)
	}
}
