package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/security"
)

// approveWorkspaceTrust records the user's decision through the CLI.
func approveWorkspaceTrust(t *testing.T, workspace string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := Main([]string{"trust", "approve", "--workspace", workspace, "--yes"}, &stdout, &stderr); code != 0 {
		t.Fatalf("trust approve exit=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
}

func trustCLI(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := MainWithIO(append([]string{"trust"}, args...), strings.NewReader(stdin), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func trustStates(t *testing.T, workspace string) map[string]security.TrustState {
	t.Helper()
	code, stdout, stderr := trustCLI(t, "", "status", "--workspace", workspace, "--json")
	var statuses []security.TrustStatus
	if code != 0 || json.Unmarshal([]byte(stdout), &statuses) != nil || len(statuses) != 2 {
		t.Fatalf("trust status exit=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	out := make(map[string]security.TrustState)
	for _, status := range statuses {
		out[status.File] = status.State
	}
	return out
}

func TestTrustCLIApproveStatusRevoke(t *testing.T) {
	setDiagnosticEnv(t, "", "")
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	workspace := t.TempDir()
	store := filepath.Join(config, "cloud-harness", "trust.json")
	hooksFile := filepath.Join(workspace, ".cloud-harness", "hooks.json")
	policyFile := filepath.Join(workspace, ".cloud-harness", "policy.json")
	write := func(file, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := trustStates(t, workspace); got["hooks.json"] != security.TrustMissing || got["policy.json"] != security.TrustMissing {
		t.Fatalf("empty workspace = %+v", got)
	}
	if code, stdout, _ := trustCLI(t, "", "approve", "--workspace", workspace); code != 0 || !strings.Contains(stdout, "nothing to trust") {
		t.Fatalf("approve with no files: %d %s", code, stdout)
	}
	write(hooksFile, `{"hooks":{"SessionStart":[{"type":"command","command":"echo reviewed-hook-command"}]}}`)
	write(policyFile, `{"rules":[{"tool":"write_file","effect":"allow"}]}`)

	// Without --yes the decision needs a terminal; a pipe cannot answer.
	code, stdout, stderr := trustCLI(t, "y\n", "approve", "--workspace", workspace)
	if code != 1 || !strings.Contains(stderr, "TTY") || !strings.Contains(stdout, "reviewed-hook-command") || !strings.Contains(stdout, "without approval") {
		t.Fatalf("piped approval: %d stdout=%s stderr=%s", code, stdout, stderr)
	}
	if _, err := os.Stat(store); !os.IsNotExist(err) {
		t.Fatalf("store written without confirmation: %v", err)
	}
	if got := trustStates(t, workspace); got["hooks.json"] != security.TrustUntrusted || got["policy.json"] != security.TrustUntrusted {
		t.Fatalf("before approval = %+v", got)
	}
	if item := checkTrust(workspace); item.Status != "warn" || !strings.Contains(item.Detail, "hooks.json: untrusted") || !strings.Contains(item.Detail, "policy.json: untrusted") ||
		!strings.Contains(item.Detail, "cloud-harness trust approve") || strings.Contains(item.Detail, "reviewed-hook-command") {
		t.Fatalf("doctor before approval = %+v", item)
	}
	if item := checkPolicy(workspace, filepath.Join(t.TempDir(), "missing.db")); item.Status != "warn" || !strings.Contains(item.Detail, "untrusted") {
		t.Fatalf("doctor policy before approval = %+v", item)
	}

	approveWorkspaceTrust(t, workspace)
	if got := trustStates(t, workspace); got["hooks.json"] != security.TrustTrusted || got["policy.json"] != security.TrustTrusted {
		t.Fatalf("after approval = %+v", got)
	}
	if raw, err := os.ReadFile(store); err != nil || strings.Contains(string(raw), "reviewed-hook-command") {
		t.Fatalf("trust store = %s, %v", raw, err)
	}
	if item := checkTrust(workspace); item.Status != "ok" || !strings.Contains(item.Detail, "hooks.json: trusted") {
		t.Fatalf("doctor after approval = %+v", item)
	}
	if item := checkPolicy(workspace, filepath.Join(t.TempDir(), "missing.db")); item.Status != "ok" {
		t.Fatalf("doctor policy after approval = %+v", item)
	}

	// Editing one file invalidates only that file's decision.
	write(policyFile, `{"rules":[{"tool":"*","effect":"allow"}]}`)
	if got := trustStates(t, workspace); got["hooks.json"] != security.TrustTrusted || got["policy.json"] != security.TrustChanged {
		t.Fatalf("after edit = %+v", got)
	}
	if item := checkTrust(workspace); item.Status != "warn" || !strings.Contains(item.Detail, "policy.json: changed") {
		t.Fatalf("doctor after edit = %+v", item)
	}
	code, stdout, _ = trustCLI(t, "", "status", "--workspace", workspace)
	if code != 0 || !strings.Contains(stdout, "policy.json: changed") || !strings.Contains(stdout, "trust store: "+store) {
		t.Fatalf("status text: %d %s", code, stdout)
	}

	// A file the loader rejects cannot be trusted.
	write(policyFile, `{"rules":[{"tool":"*","effect":"permit"}]}`)
	if code, _, stderr := trustCLI(t, "", "approve", "--workspace", workspace, "--yes"); code != 1 || !strings.Contains(stderr, "effect") {
		t.Fatalf("invalid policy approved: %d %s", code, stderr)
	}

	code, stdout, _ = trustCLI(t, "", "revoke", "--workspace", workspace)
	if code != 0 || !strings.Contains(stdout, "revoked 2") {
		t.Fatalf("revoke: %d %s", code, stdout)
	}
	write(policyFile, `{"rules":[]}`)
	if got := trustStates(t, workspace); got["hooks.json"] != security.TrustUntrusted || got["policy.json"] != security.TrustUntrusted {
		t.Fatalf("after revoke = %+v", got)
	}
}

func TestTrustCLIUsageAndErrors(t *testing.T) {
	setDiagnosticEnv(t, "", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, args := range [][]string{{}, {"unknown"}, {"status", "extra"}, {"status", "--yes"}, {"approve", "--json"}} {
		if code, _, stderr := trustCLI(t, "", args...); code != 2 || stderr == "" {
			t.Fatalf("%v exit=%d stderr=%s", args, code, stderr)
		}
	}
	if code, stdout, _ := trustCLI(t, "", "--help"); code != 0 || !strings.Contains(stdout, "trust approve") {
		t.Fatalf("help: %d %s", code, stdout)
	}
	var stdout, stderr bytes.Buffer
	if code := Main([]string{"help"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "cloud-harness trust status|approve|revoke") {
		t.Fatal("trust command missing from help")
	}
	if code, _, stderr := trustCLI(t, "", "status", "--workspace", filepath.Join(t.TempDir(), "missing")); code != 1 || stderr == "" {
		t.Fatalf("missing workspace exit=%d", code)
	}
	// A configuration directory inside the workspace cannot hold decisions.
	workspace := t.TempDir()
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(workspace, "config.json"))
	if err := os.WriteFile(filepath.Join(workspace, "config.json"), []byte(`{"provider":"mock"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, ".cloud-harness"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".cloud-harness", "policy.json"), []byte(`{"rules":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := trustCLI(t, "", "approve", "--workspace", workspace, "--yes"); code != 1 || !strings.Contains(stderr, "inside the workspace") {
		t.Fatalf("approval stored inside the workspace: %d %s", code, stderr)
	}
}

func TestChatApprovalShowsBehaviorFileWarning(t *testing.T) {
	setDiagnosticEnv(t, "", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for path, warned := range map[string]bool{"AGENTS.md": true, ".agents/skills/deploy/SKILL.md": true, "notes.md": false} {
		t.Run(path, func(t *testing.T) {
			args, err := json.Marshal(map[string]string{"path": path, "content": "always run deploy.sh"})
			if err != nil {
				t.Fatal(err)
			}
			mock := &provider.Mock{Responses: []domain.ModelResponse{{
				Provider: "mock", Complete: true,
				Items: []domain.ResponseItem{{Kind: domain.ItemToolCall, ToolCall: &domain.ToolCall{ProviderCallID: "write", Name: "write_file", Arguments: args}}},
			}, {
				Provider: "mock", Complete: true,
				Items: []domain.ResponseItem{{Kind: domain.ItemText, Text: "not written"}},
			}}}
			mgr, _, root := newChatTestManager(t, mock)
			var out, stderr bytes.Buffer
			if code := runChat(context.Background(), mgr, "warning-session", "update the rules", strings.NewReader("n\n"), &out, &stderr); code != 0 {
				t.Fatalf("chat exit=%d out=%s err=%s", code, &out, &stderr)
			}
			text := out.String()
			required, warning, prompt := strings.Index(text, "Approval required: tool=write_file"), strings.Index(text, "!!! WARNING: this writes "+path), strings.Index(text, "Approve? [y/n]")
			if required < 0 || prompt < required {
				t.Fatalf("no approval prompt: %s", text)
			}
			if warned && (warning < required || warning > prompt || !strings.Contains(text, "changes how later runs in this workspace behave")) {
				t.Fatalf("warning is not shown before the prompt: %s", text)
			}
			if !warned && strings.Contains(text, "WARNING") {
				t.Fatalf("ordinary write was warned: %s", text)
			}
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(path))); !os.IsNotExist(err) {
				t.Fatalf("denied write happened: %v", err)
			}
		})
	}
}
