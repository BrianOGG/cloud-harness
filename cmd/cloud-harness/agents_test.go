package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/agents"
)

func TestAgentsListHumanAndJSON(t *testing.T) {
	root := t.TempDir()
	writeAgentFixture(t, root, "custom", "---\ndescription: Custom code reader\ntools: read_file, grep\nmodel: example-model\n---\nCustom system.")
	writeAgentFixture(t, root, "empty", "---\ndescription: No tools\ntools: []\n---")
	writeAgentFixture(t, root, "inherited", "---\ndescription: Inherited tools\n---")
	code, out, stderr := invokeAgents("--workspace", root)
	if code != 0 || stderr != "" {
		t.Fatalf("agents code=%d stdout=%s stderr=%s", code, out, stderr)
	}
	assertAgentsContains(t, out, "NAME", "SOURCE", "OVERRIDES_BUILTIN", "DESCRIPTION", "TOOLS", "MODEL",
		"explorer", "reviewer", "builtin", "custom", "file", "Custom code reader", "read_file, grep", "example-model", "inherit", "none", "—")
	if strings.Index(out, "custom") > strings.Index(out, "empty") || strings.Index(out, "empty") > strings.Index(out, "explorer") {
		t.Fatalf("unsorted list: %s", out)
	}
	code, out, stderr = invokeAgents("list", "--workspace", root, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("agents JSON code=%d stdout=%s stderr=%s", code, out, stderr)
	}
	var defs []agents.Definition
	if err := json.Unmarshal([]byte(out), &defs); err != nil {
		t.Fatal(err)
	}
	if len(defs) != 5 || defs[0].Name != "custom" || defs[0].Source != "file" || defs[0].Model != "example-model" || defs[0].System != "Custom system." {
		t.Fatalf("list JSON = %+v", defs)
	}
	if defs[1].Tools == nil || len(defs[1].Tools) != 0 || defs[3].Name != "inherited" || defs[3].Tools != nil {
		t.Fatalf("JSON lost tools nil/empty distinction: %+v", defs)
	}
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatal(err)
	}
	for _, item := range raw {
		assertAgentJSONFields(t, item)
	}
}

func TestAgentsShowFileAndBuiltin(t *testing.T) {
	root := t.TempDir()
	body := "Read this code.\n\n---\nKeep the entire system body."
	writeAgentFixture(t, root, "custom", "---\nname: custom\ndescription: Full role\ntools: []\nmodel: example-model\nmax_model_calls: 3\nmax_tool_calls: 9\n---\n\n"+body+"\n")
	code, out, stderr := invokeAgents("show", "custom", "--workspace", root)
	if code != 0 || stderr != "" {
		t.Fatalf("show code=%d stdout=%s stderr=%s", code, out, stderr)
	}
	assertAgentsContains(t, out, "Name: custom", "Source: file", "Overrides builtin: false", "Description: Full role", "Tools: none",
		"Model: example-model", "Max model calls: 3", "Max tool calls: 9", "System:\n"+body)
	code, out, stderr = invokeAgents("show", "custom", "--workspace", root, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("show JSON code=%d stdout=%s stderr=%s", code, out, stderr)
	}
	var def agents.Definition
	if err := json.Unmarshal([]byte(out), &def); err != nil {
		t.Fatal(err)
	}
	if def.System != body || def.MaxModelCalls != 3 || def.MaxToolCalls != 9 || def.Source != "file" || def.Tools == nil {
		t.Fatalf("show file definition = %+v", def)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatal(err)
	}
	assertAgentJSONFields(t, raw)
	code, out, stderr = invokeAgents("show", "explorer", "--workspace", root, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("show builtin code=%d stdout=%s stderr=%s", code, out, stderr)
	}
	def = agents.Definition{}
	if err := json.Unmarshal([]byte(out), &def); err != nil {
		t.Fatal(err)
	}
	want := []string{"read_file", "list_files", "grep", "git_status", "git_diff", "git_log"}
	if def.Name != "explorer" || def.Source != "builtin" || def.OverridesBuiltin || !slices.Equal(def.Tools, want) {
		t.Fatalf("builtin explorer = %+v", def)
	}
	for _, forbidden := range []string{"write_file", "edit_file", "run_command", "ask_user", "delegate_task"} {
		if slices.Contains(def.Tools, forbidden) {
			t.Fatalf("builtin explorer contains %s", forbidden)
		}
	}
}

func TestAgentsErrorsAndHelp(t *testing.T) {
	root := t.TempDir()
	writeAgentFixture(t, root, "custom", "---\ndescription: A role\n---")
	for _, jsonFlag := range [][]string{nil, {"--json"}} {
		args := append([]string{"show", "missing", "--workspace", root}, jsonFlag...)
		code, out, stderr := invokeAgents(args...)
		if code != 1 || out != "" {
			t.Fatalf("missing show code=%d stdout=%s stderr=%s", code, out, stderr)
		}
		assertAgentsContains(t, stderr, `unknown agent "missing"`, "available agents: custom, explorer, reviewer")
	}
	for _, args := range [][]string{
		{"show"}, {"show", "--json"}, {"show", "custom", "extra"}, {"list", "extra"}, {"unexpected"}, {"--bad"}, {"--workspace"},
	} {
		code, out, stderr := invokeAgents(args...)
		if code != 2 || out != "" || stderr == "" {
			t.Fatalf("invalid args %v: code=%d stdout=%s stderr=%s", args, code, out, stderr)
		}
	}
	for _, args := range [][]string{{"--help"}, {"list", "--help"}, {"show", "--help"}} {
		code, _, stderr := invokeAgents(args...)
		if code != 0 || !strings.Contains(stderr, "agents show <name>") {
			t.Fatalf("help %v: code=%d stderr=%s", args, code, stderr)
		}
	}
	path := writeAgentFixture(t, root, "broken", "---\ndescription: Broken\ntoolz: grep\n---")
	code, out, stderr := invokeAgents("--workspace", root, "--json")
	if code != 1 || out != "" || !strings.Contains(stderr, path) || !strings.Contains(stderr, `unknown field "toolz"`) {
		t.Fatalf("invalid definition code=%d stdout=%s stderr=%s", code, out, stderr)
	}
	var help, helpErr bytes.Buffer
	if code := Main([]string{"--help"}, &help, &helpErr); code != 0 || !strings.Contains(help.String(), "cloud-harness agents") {
		t.Fatalf("main help code=%d stdout=%s stderr=%s", code, &help, &helpErr)
	}
}

func TestAgentsDefaultWorkspaceAndReadOnly(t *testing.T) {
	root := t.TempDir()
	writeAgentFixture(t, root, "custom", "---\ndescription: From cwd\n---\nCwd system.")
	t.Chdir(root)
	// Invalid config and a non-database sentinel prove this command bypasses
	// config loading and database opening; no subprocess is available on PATH.
	configPath := filepath.Join(root, "invalid-config.json")
	if err := os.WriteFile(configPath, []byte("not JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	dbDir := filepath.Join(root, ".cloud-harness")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dbDir, "harness.db")
	if err := os.WriteFile(dbPath, []byte("do not open or modify"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOUD_HARNESS_CONFIG", configPath)
	t.Setenv("CLOUD_HARNESS_DB", dbPath)
	t.Setenv("PATH", t.TempDir())
	before := agentWorkspaceState(t, root)
	for _, args := range [][]string{nil, {"--json"}, {"list"}, {"show", "custom"}, {"show", "--json", "custom"}, {"show", "explorer", "--json"}} {
		code, out, stderr := invokeAgents(args...)
		if code != 0 || stderr != "" || out == "" {
			t.Fatalf("read-only %v: code=%d stdout=%s stderr=%s", args, code, out, stderr)
		}
		if !slices.Contains(args, "explorer") && !strings.Contains(out, "custom") {
			t.Fatalf("default workspace ignored: %s", out)
		}
	}
	if after := agentWorkspaceState(t, root); !reflect.DeepEqual(before, after) {
		t.Fatalf("agents modified workspace: before=%v after=%v", before, after)
	}
	// The same check in a fresh workspace catches creation of .cloud-harness.
	fresh := t.TempDir()
	code, _, stderr := invokeAgents("--workspace", fresh)
	if code != 0 || stderr != "" {
		t.Fatalf("fresh list code=%d stderr=%s", code, stderr)
	}
	if entries, err := os.ReadDir(fresh); err != nil || len(entries) != 0 {
		t.Fatalf("fresh workspace changed: entries=%v err=%v", entries, err)
	}
}

func TestAgentsOutputErrors(t *testing.T) {
	for _, args := range [][]string{{}, {"--json"}, {"show", "explorer"}, {"show", "explorer", "--json"}} {
		root := t.TempDir()
		args = append(args, "--workspace", root)
		var stderr bytes.Buffer
		if code := cmdAgents(args, agentFailWriter{}, &stderr); code != 1 {
			t.Fatalf("output failure for %v: code=%d stderr=%s", args, code, &stderr)
		}
	}
}

type agentFailWriter struct{}

func (agentFailWriter) Write([]byte) (int, error) { return 0, errors.New("output unavailable") }

func invokeAgents(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := Main(append([]string{"agents"}, args...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func writeAgentFixture(t *testing.T, root, name, text string) string {
	t.Helper()
	dir := filepath.Join(root, ".agents", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+".md")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertAgentsContains(t *testing.T, output string, values ...string) {
	t.Helper()
	for _, value := range values {
		if !strings.Contains(output, value) {
			t.Errorf("output missing %q: %s", value, output)
		}
	}
}

func assertAgentJSONFields(t *testing.T, item map[string]json.RawMessage) {
	t.Helper()
	for _, field := range []string{"name", "description", "tools", "model", "max_model_calls", "max_tool_calls", "system", "source", "overrides_builtin"} {
		if _, ok := item[field]; !ok {
			t.Errorf("JSON definition missing %q: %s", field, item)
		}
	}
}

func agentWorkspaceState(t *testing.T, root string) map[string]any {
	t.Helper()
	state := make(map[string]any)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		var data []byte
		if !entry.IsDir() {
			data, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		state[rel] = []any{info.Mode(), info.ModTime(), data}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}
