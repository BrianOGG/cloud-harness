package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func putFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMissingAndLayerOverrides(t *testing.T) {
	workspace, global := t.TempDir(), t.TempDir()
	cfg, err := LoadFrom(workspace, global)
	if err != nil || len(cfg.Hooks) != 0 || !New(cfg).GoCheckEnabled() {
		t.Fatalf("missing config: %+v, %v", cfg, err)
	}
	putFile(t, filepath.Join(global, "hooks.json"), `{"go_check":false,"hooks":{
		"SessionStart":[{"type":"command","command":"echo global"}],
		"PreToolUse":[{"type":"command","command":"exit 2"}],
		"Stop":[{"type":"command","command":"echo stop"}]}}`)
	putFile(t, filepath.Join(workspace, ".cloud-harness", "hooks.json"), `{"hooks":{
		"SessionStart":[{"type":"command","command":"echo workspace"}],"PreToolUse":[]}}`)
	cfg, err = LoadFrom(workspace, global)
	if err != nil || cfg.Hooks[SessionStart][0].Command != "echo workspace" || len(cfg.Hooks[PreToolUse]) != 0 || len(cfg.Hooks[Stop]) != 1 || New(cfg).GoCheckEnabled() {
		t.Fatalf("merged config: %+v, %v", cfg, err)
	}
	putFile(t, filepath.Join(workspace, ".cloud-harness", "hooks.json"), `{"go_check":true}`)
	cfg, err = LoadFrom(workspace, global)
	if err != nil || !New(cfg).GoCheckEnabled() {
		t.Fatalf("workspace boolean override: %+v, %v", cfg, err)
	}
}

func TestLoadConfigDirectoryMatchesPolicy(t *testing.T) {
	workspace, global := t.TempDir(), t.TempDir()
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(global, "settings.json"))
	putFile(t, filepath.Join(global, "hooks.json"), `{"go_check":false}`)
	cfg, err := Load(workspace)
	if err != nil || New(cfg).GoCheckEnabled() {
		t.Fatalf("explicit config directory: %+v, %v", cfg, err)
	}
	t.Setenv("CLOUD_HARNESS_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", global)
	putFile(t, filepath.Join(global, "cloud-harness", "hooks.json"), `{"go_check":false}`)
	cfg, err = Load(workspace)
	if err != nil || New(cfg).GoCheckEnabled() {
		t.Fatalf("platform config directory: %+v, %v", cfg, err)
	}
}

func TestInvalidConfigurationNamesFileAndProblem(t *testing.T) {
	for _, tc := range []struct{ body, problem string }{
		{`{`, "unexpected EOF"},
		{`null`, "object"},
		{`{} {}`, "one JSON"},
		{`{"hook":{}}`, "unknown field"},
		{`{"go_check":null}`, "boolean"},
		{`{"go_check":"false"}`, "boolean"},
		{`{"hooks":null}`, "object"},
		{`{"hooks":{"Unknown":[]}}`, "unknown hook point"},
		{`{"hooks":{"PreToolUse":null}}`, "array"},
		{`{"hooks":{"PreToolUse":[{}]}}`, "action 1"},
		{`{"hooks":{"PreToolUse":[{"type":"command","command":"true","extra":1}]}}`, "unknown field"},
		{`{"hooks":{"PreToolUse":[{"type":"command","command":"true","timeout_ms":null}]}}`, "must not be null"},
		{`{"hooks":{"PreToolUse":[{"type":"command","command":"true","timeout_ms":-1}]}}`, "timeout_ms"},
		{`{"hooks":{"PreToolUse":[{"type":"command","command":"true","shell":"/tmp/sh"}]}}`, "shell must"},
		{`{"hooks":{"PreToolUse":[{"type":"command","command":"true","cwd":".."}]}}`, "inside the workspace"},
		{`{"hooks":{"PreToolUse":[{"type":"starlark","script":"def main(): return 0","command":"true"}]}}`, "forbids"},
	} {
		t.Run(tc.body, func(t *testing.T) {
			workspace, global := t.TempDir(), t.TempDir()
			file := filepath.Join(global, "hooks.json")
			putFile(t, file, tc.body)
			// Invalid overridden actions must still fail to load.
			putFile(t, filepath.Join(workspace, ".cloud-harness", "hooks.json"), `{"hooks":{"PreToolUse":[]}}`)
			_, err := LoadFrom(workspace, global)
			if err == nil || !strings.Contains(err.Error(), file) || !strings.Contains(err.Error(), tc.problem) {
				t.Fatalf("error = %v, want file and %q", err, tc.problem)
			}
		})
	}
}

func TestCwdSymlinkIsRecheckedAtExecution(t *testing.T) {
	workspace, outside := t.TempDir(), t.TempDir()
	inside := filepath.Join(workspace, "inside")
	if err := os.Mkdir(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(workspace, "link")
	if err := os.Symlink(inside, link); err != nil {
		t.Fatal(err)
	}
	action := Action{Type: "command", Command: "echo escaped > marker", Cwd: "link"}
	if err := validateAction(workspace, action); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	engine := New(&Config{Workspace: workspace, Hooks: map[Point][]Action{PreToolUse: {action}}})
	out, err := engine.Execute(t.Context(), Input{Point: PreToolUse}, nil)
	if err != nil || !out.Blocked || !strings.Contains(out.Message, "inside the workspace") {
		t.Fatalf("cwd recheck: %+v, %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "marker")); !os.IsNotExist(err) {
		t.Fatalf("hook executed outside workspace: %v", err)
	}
}

func TestNonDirectoryConfigParentIsMissing(t *testing.T) {
	workspace := t.TempDir()
	putFile(t, filepath.Join(workspace, ".cloud-harness"), "not a directory")
	cfg, err := LoadFrom(workspace, t.TempDir())
	if err != nil || len(cfg.Hooks) != 0 {
		t.Fatalf("missing hooks under non-directory: %+v, %v", cfg, err)
	}
}
