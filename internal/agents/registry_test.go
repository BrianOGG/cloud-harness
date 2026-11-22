package agents

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestLoadAllFieldsAndBody(t *testing.T) {
	root := t.TempDir()
	writeDefinition(t, root, "code-reader.md", `---
name: code-reader
description: Locate relevant code: read only
tools: [read_file, grep, finish_turn]
model: example-model
max_model_calls: 7
max_tool_calls: 13
---

Inspect the relevant files.

---
Keep this internal separator.
  Keep internal indentation.

`)
	r := mustLoad(t, root)
	got, ok := r.Get("code-reader")
	want := Definition{
		Name: "code-reader", Description: "Locate relevant code: read only",
		Tools: []string{"read_file", "grep", "finish_turn"}, Model: "example-model",
		MaxModelCalls: 7, MaxToolCalls: 13, Source: "file",
		System: "Inspect the relevant files.\n\n---\nKeep this internal separator.\n  Keep internal indentation.",
	}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("Get = %+v, %v; want %+v", got, ok, want)
	}
}

func TestLoadMinimalAndToolForms(t *testing.T) {
	root := t.TempDir()
	for name, fields := range map[string]string{
		"minimal": "",
		"comma":   "tools: read_file, grep\n",
		"bracket": "tools: [read_file, grep]\n",
		"empty":   "tools: []\n",
		"spaces":  "tools: [  ]\n",
	} {
		writeDefinition(t, root, name+".md", "---\ndescription: A role\n"+fields+"---\n")
	}
	r := mustLoad(t, root)
	minimal, _ := r.Get("minimal")
	if minimal.Tools != nil || minimal.System != "" || minimal.Model != "" || minimal.MaxModelCalls != 0 || minimal.MaxToolCalls != 0 {
		t.Fatalf("minimal definition = %+v", minimal)
	}
	comma, _ := r.Get("comma")
	bracket, _ := r.Get("bracket")
	if !reflect.DeepEqual(comma.Tools, bracket.Tools) || !slices.Equal(comma.Tools, []string{"read_file", "grep"}) {
		t.Fatalf("comma tools %v != bracket tools %v", comma.Tools, bracket.Tools)
	}
	for _, name := range []string{"empty", "spaces"} {
		def, _ := r.Get(name)
		if def.Tools == nil || len(def.Tools) != 0 {
			t.Fatalf("%s tools = %#v; want non-nil empty slice", name, def.Tools)
		}
	}
	for _, tc := range []struct {
		def  Definition
		want string
	}{{minimal, "null"}, {mustGet(t, r, "empty"), "[]"}} {
		data, err := json.Marshal(tc.def)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields["tools"]) != tc.want {
			t.Fatalf("tools JSON = %s; want %s", fields["tools"], tc.want)
		}
	}
}

func TestLoadAllKnownChildTools(t *testing.T) {
	root := t.TempDir()
	names := []string{
		"read_file", "write_file", "edit_file", "list_dir", "list_files", "grep", "git_status", "git_diff", "git_log", "run_command",
		"finish_turn", "update_plan", "retrieve_history", "retrieve_resource", "execute_program", "poll_tool",
	}
	writeDefinition(t, root, "all.md", "---\ndescription: All eligible tools\ntools: "+strings.Join(names, ", ")+"\n---\n")
	def := mustGet(t, mustLoad(t, root), "all")
	if !slices.Equal(def.Tools, names) {
		t.Fatalf("tools = %v; want %v", def.Tools, names)
	}
}

func TestLoadParseErrorsIncludePathAndReason(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want string
	}{
		{"missing-frontmatter", "description: A role\n", "missing frontmatter"},
		{"leading-blank", "\n---\ndescription: A role\n---", "missing frontmatter"},
		{"unclosed", "---\ndescription: A role\n", "unclosed frontmatter"},
		{"unknown-field", "---\ndescription: A role\ntoolz: read_file\n---", `unknown field "toolz"`},
		{"name-mismatch", "---\ndescription: A role\nname: Other\n---", `must match filename stem "role" exactly`},
		{"name-empty", "---\ndescription: A role\nname:\n---", "must match filename stem"},
		{"unknown-tool", "---\ndescription: A role\ntools: [read_file, mystery_tool]\n---", `unknown tool "mystery_tool"`},
		{"ask-user", "---\ndescription: A role\ntools: ask_user\n---", `"ask_user" is forbidden`},
		{"delegate-task", "---\ndescription: A role\ntools: [read_file, delegate_task]\n---", `"delegate_task" is forbidden`},
		{"missing-description", "---\nmodel: example\n---", "description is required"},
		{"empty-description", "---\ndescription:\n---", "description is required"},
		{"whitespace-description", "---\ndescription:   \t\n---", "description is required"},
		{"empty-quoted-description", "---\ndescription: \"\"\n---", "description is required"},
		{"space-quoted-description", "---\ndescription: '  '\n---", "description is required"},
		{"duplicate", "---\ndescription: A role\ndescription: Other\n---", `duplicate field "description"`},
		{"malformed-field", "---\ndescription: A role\nnot a key value\n---", "expected single-line key: value"},
		{"multiline-field", "---\ndescription: A role\n  continuation\n---", "expected single-line key: value"},
		{"empty-tools", "---\ndescription: A role\ntools:\n---", "empty tools value"},
		{"missing-close-bracket", "---\ndescription: A role\ntools: [read_file\n---", "matching [ and ]"},
		{"missing-open-bracket", "---\ndescription: A role\ntools: read_file]\n---", "matching [ and ]"},
		{"trailing-comma", "---\ndescription: A role\ntools: [read_file,]\n---", "empty tool name"},
		{"bad-quotes", "---\ndescription: \"A role\n---", "unterminated quoted string"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := writeDefinition(t, root, "role.md", tc.text)
			_, err := Load(root)
			if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load error = %v; want path %q and %q", err, path, tc.want)
			}
		})
	}
	for _, key := range []string{"max_model_calls", "max_tool_calls"} {
		for _, value := range []string{"0", "-1", "abc", "1.5", "", "999999999999999999999999999999"} {
			t.Run(key+"/"+value, func(t *testing.T) {
				root := t.TempDir()
				path := writeDefinition(t, root, "role.md", fmt.Sprintf("---\ndescription: A role\n%s: %s\n---", key, value))
				_, err := Load(root)
				if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "integer > 0") {
					t.Fatalf("Load error = %v; want path, %s, and positive-integer diagnostic", err, key)
				}
			})
		}
	}
}

func TestLoadNameValidation(t *testing.T) {
	for _, name := range []string{"Upper", "_start", "-start", "a.b", "two words", "汉字", ""} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := writeDefinition(t, root, name+".md", "---\ndescription: A role\n---")
			_, err := Load(root)
			if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "invalid agent name") {
				t.Fatalf("Load error = %v; want invalid stem and path %q", err, path)
			}
		})
	}
	root := t.TempDir()
	for _, name := range []string{"a", "0", "a-b_c", "9_role-"} {
		writeDefinition(t, root, name+".md", "---\ndescription: A role\n---")
	}
	if got := len(mustLoad(t, root).List()); got != 6 {
		t.Fatalf("valid stems: got %d definitions; want 6", got)
	}
}

func TestLoadCRLFAndQuotedStrings(t *testing.T) {
	root := t.TempDir()
	writeDefinition(t, root, "role.md", "---\r\n\r\nname: 'role'\r\ndescription: \"Literal # text: here\"\r\nmodel: 'example-model'\r\n---\r\n\r\nFirst.\r\n---\r\nLast.\r\n")
	def := mustGet(t, mustLoad(t, root), "role")
	if def.Description != "Literal # text: here" || def.Model != "example-model" || def.System != "First.\r\n---\r\nLast." {
		t.Fatalf("CRLF/quoted definition = %+v", def)
	}
}

func TestLoadOnlyBuiltins(t *testing.T) {
	root := t.TempDir()
	for _, workspace := range []string{"", root, filepath.Join(root, "missing")} {
		r := mustLoad(t, workspace)
		defs := r.List()
		if len(defs) != 2 || defs[0].Name != "explorer" || defs[1].Name != "reviewer" {
			t.Fatalf("built-ins for %q = %+v", workspace, defs)
		}
		readonly := []string{"read_file", "list_dir", "list_files", "grep", "git_status", "git_diff", "git_log"}
		for _, def := range defs {
			if def.Source != "builtin" || def.OverridesBuiltin || !slices.Equal(def.Tools, readonly) || def.Description == "" {
				t.Fatalf("builtin = %+v", def)
			}
		}
		if _, ok := r.Get("missing"); ok {
			t.Fatal("unknown Get returned true")
		}
	}
	writeDefinition(t, root, "custom.md", "---\ndescription: A role\n---")
	t.Chdir(root)
	if got := len(mustLoad(t, "").List()); got != 2 {
		t.Fatalf("empty workspace read cwd definitions: %d", got)
	}
}

func TestLoadOverrideAndDeterminism(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"zeta", "explorer", "alpha"} {
		writeDefinition(t, root, name+".md", "---\ndescription: Workspace "+name+"\ntools: []\nmodel: custom\nmax_model_calls: 2\n---\nWorkspace system for "+name)
	}
	r := mustLoad(t, root)
	def := mustGet(t, r, "explorer")
	if def.Source != "file" || !def.OverridesBuiltin || def.Description != "Workspace explorer" || def.System != "Workspace system for explorer" || def.Model != "custom" || def.MaxModelCalls != 2 || def.Tools == nil || len(def.Tools) != 0 {
		t.Fatalf("override = %+v", def)
	}
	if custom := mustGet(t, r, "alpha"); custom.OverridesBuiltin || custom.Source != "file" {
		t.Fatalf("custom source = %+v", custom)
	}
	want := []string{"alpha", "explorer", "reviewer", "zeta"}
	var names []string
	for _, def := range r.List() {
		names = append(names, def.Name)
	}
	if !slices.Equal(names, want) {
		t.Fatalf("List names = %v; want %v", names, want)
	}
	for i := 0; i < 5; i++ {
		if got := mustLoad(t, root).List(); !reflect.DeepEqual(got, r.List()) {
			t.Fatalf("Load %d produced different definitions: %+v", i, got)
		}
	}
}

func TestRegistryReturnsIndependentCopies(t *testing.T) {
	r := mustLoad(t, "")
	def := mustGet(t, r, "explorer")
	def.Tools[0] = "write_file"
	defs := r.List()
	defs[0].Tools[1] = "run_command"
	if got := mustGet(t, r, "explorer"); got.Tools[0] != "read_file" || got.Tools[1] != "list_dir" {
		t.Fatalf("returned slice mutated registry: %+v", got)
	}
	if got := mustGet(t, mustLoad(t, ""), "explorer"); got.Tools[0] != "read_file" {
		t.Fatalf("mutation leaked into another Load: %+v", got)
	}
	var zero Registry
	if _, ok := zero.Get("explorer"); ok || len(zero.List()) != 0 {
		t.Fatal("zero registry should be empty")
	}
}

func TestLoadIgnoresNonMarkdownSubdirectoriesAndSymlinks(t *testing.T) {
	root := t.TempDir()
	writeDefinition(t, root, "bad.txt", "not frontmatter")
	writeDefinition(t, root, "Upper.MD", "not frontmatter")
	writeDefinition(t, root, "sub.md/Bad.md", "not frontmatter")
	writeDefinition(t, root, "nested/also-bad.md", "not frontmatter")
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("not frontmatter"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".agents", "agents")
	for name, target := range map[string]string{
		"linked.md": outside,
		"Bad.md":    outside,
		"broken.md": filepath.Join(root, "absent"),
		"loop.md":   "loop.md",
		"dir.md":    filepath.Dir(outside),
	} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(mustLoad(t, root).List()); got != 2 {
		t.Fatalf("ignored entries loaded: got %d definitions", got)
	}
}

func TestLoadSkipsSymlinkedDefinitionDirectories(t *testing.T) {
	for _, component := range []string{".agents", ".agents/agents"} {
		t.Run(component, func(t *testing.T) {
			root, target := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(target, "Bad.md"), []byte("bad"), 0o600); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(root, component)
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			if got := len(mustLoad(t, root).List()); got != 2 {
				t.Fatalf("symlink directory was read: %d definitions", got)
			}
		})
	}
}

func TestLoadStopsAtFirstFileError(t *testing.T) {
	root := t.TempDir()
	writeDefinition(t, root, "zeta.md", "---\ndescription: Zeta\nunknown: no\n---")
	first := writeDefinition(t, root, "alpha.md", "not frontmatter")
	_, err := Load(root)
	if err == nil || !strings.Contains(err.Error(), first) || !strings.Contains(err.Error(), "missing frontmatter") || strings.Contains(err.Error(), "zeta") {
		t.Fatalf("first-error result = %v", err)
	}
}

func TestLoadDirectoryErrors(t *testing.T) {
	for _, component := range []string{".agents", ".agents/agents"} {
		t.Run(component, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, component)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(root); err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "not a directory") {
				t.Fatalf("directory error = %v", err)
			}
		})
	}
}

func writeDefinition(t *testing.T, root, name, text string) string {
	t.Helper()
	path := filepath.Join(root, ".agents", "agents", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustLoad(t *testing.T, root string) Registry {
	t.Helper()
	r, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func mustGet(t *testing.T, r Registry, name string) Definition {
	t.Helper()
	def, ok := r.Get(name)
	if !ok {
		t.Fatalf("missing definition %q", name)
	}
	return def
}
