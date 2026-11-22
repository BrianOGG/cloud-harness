package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIValidatesBothHookLayersAtStartup(t *testing.T) {
	for _, layer := range []string{"global", "workspace"} {
		t.Run(layer, func(t *testing.T) {
			global, workspace := t.TempDir(), t.TempDir()
			t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(global, "config.json"))
			if err := os.WriteFile(filepath.Join(global, "config.json"), []byte(`{"provider":"mock"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(global, "hooks.json")
			if layer == "workspace" {
				if err := os.Mkdir(filepath.Join(workspace, ".cloud-harness"), 0o700); err != nil {
					t.Fatal(err)
				}
				file = filepath.Join(workspace, ".cloud-harness", "hooks.json")
			}
			if err := os.WriteFile(file, []byte(`{"hooks":{"PreToolUse":[{"type":"command"}]}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			var out, stderr bytes.Buffer
			code := Main([]string{"run", "--workspace", workspace, "--db", filepath.Join(t.TempDir(), "h.db"), "--message", "blocked at startup"}, &out, &stderr)
			if code != 1 || !strings.Contains(stderr.String(), file) || !strings.Contains(stderr.String(), "action 1") || strings.Contains(out.String(), "Mock answer") {
				t.Fatalf("invalid hook layer: code=%d out=%s err=%s", code, &out, &stderr)
			}
		})
	}
}

func TestCLIUserPromptHookReturnsRejection(t *testing.T) {
	workspace, global := t.TempDir(), t.TempDir()
	t.Setenv("CLOUD_HARNESS_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", global)
	if err := os.Mkdir(filepath.Join(workspace, ".cloud-harness"), 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(workspace, ".cloud-harness", "hooks.json")
	if err := os.WriteFile(file, []byte(`{"hooks":{"UserPromptSubmit":[{"type":"command","command":"echo submission-rejected >&2; exit 2"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	// Until the user trusts the file, its hook does not run and the CLI says so.
	code := Main([]string{"run", "--provider", "mock", "--workspace", workspace, "--db", filepath.Join(t.TempDir(), "h.db"), "--message", "not rejected"}, &out, &stderr)
	if code != 0 || !strings.Contains(out.String(), "Mock answer") || strings.Contains(stderr.String(), "submission-rejected") ||
		!strings.Contains(stderr.String(), "hooks.json is not trusted") || !strings.Contains(stderr.String(), "cloud-harness trust approve") {
		t.Fatalf("untrusted hook: code=%d out=%s err=%s", code, &out, &stderr)
	}
	approveWorkspaceTrust(t, workspace)
	out.Reset()
	stderr.Reset()
	code = Main([]string{"run", "--provider", "mock", "--workspace", workspace, "--db", filepath.Join(t.TempDir(), "h.db"), "--message", "rejected"}, &out, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "submission-rejected") || out.Len() != 0 {
		t.Fatalf("submission rejection: code=%d out=%s err=%s", code, &out, &stderr)
	}
}
