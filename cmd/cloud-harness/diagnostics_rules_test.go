package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorProjectRules(t *testing.T) {
	for _, populated := range []bool{false, true} {
		t.Run(fmt.Sprint(populated), func(t *testing.T) {
			setDiagnosticEnv(t, "", "")
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			workspace := t.TempDir()
			var expected []string
			if populated {
				global := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "cloud-harness")
				for _, file := range []struct{ path, body string }{
					{filepath.Join(global, "AGENTS.md"), "global rule"},
					{filepath.Join(workspace, "AGENTS.md"), "root rule"},
					{filepath.Join(workspace, "src/AGENTS.md"), "子目录规则"},
				} {
					if err := os.MkdirAll(filepath.Dir(file.path), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(file.path, []byte(file.body), 0o644); err != nil {
						t.Fatal(err)
					}
					expected = append(expected, fmt.Sprintf("%s (%d bytes)", file.path, len(file.body)))
				}
			}
			for _, asJSON := range []bool{false, true} {
				args := []string{"doctor", "--workspace", workspace, "--db", filepath.Join(workspace, "missing.db"), "--provider", "mock"}
				if asJSON {
					args = append(args, "--json")
				}
				var out, stderr bytes.Buffer
				if code := Main(args, &out, &stderr); code != 0 {
					t.Fatalf("doctor exit=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
				}
				detail := out.String()
				if asJSON {
					var items []doctorItem
					if err := json.Unmarshal(out.Bytes(), &items); err != nil {
						t.Fatal(err)
					}
					item := findDoctor(items, "project_rules")
					if item == nil || item.Status != "ok" {
						t.Fatalf("missing rule diagnostics: %+v", item)
					}
					detail = item.Detail
				}
				if !populated && !strings.Contains(detail, "AGENTS.md: none") {
					t.Fatalf("missing explicit none: %s", detail)
				}
				previous := -1
				for _, want := range expected {
					at := strings.Index(detail, want)
					if at <= previous {
						t.Fatalf("missing or unordered %q: %s", want, detail)
					}
					previous = at
				}
				if strings.Contains(detail, "global rule") || strings.Contains(detail, "root rule") || strings.Contains(detail, "子目录规则") {
					t.Fatal("doctor printed rule bodies")
				}
			}
			if _, err := os.Stat(filepath.Join(workspace, "missing.db")); !os.IsNotExist(err) {
				t.Fatalf("doctor created database: %v", err)
			}
		})
	}
}

func TestDoctorProjectRulesTruncationAndReadFailure(t *testing.T) {
	setDiagnosticEnv(t, "", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	workspace := t.TempDir()
	path := filepath.Join(workspace, "AGENTS.md")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 40*1024)), 0o644); err != nil {
		t.Fatal(err)
	}
	item := checkProjectRules(workspace)
	if item.Status != "warn" || !strings.Contains(item.Detail, "40960 bytes") || !strings.Contains(item.Detail, "truncated at 32 KiB") {
		t.Fatalf("truncation check: %+v", item)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("AGENTS.md", path); err != nil {
		t.Fatal(err)
	}
	item = checkProjectRules(workspace)
	if item.Status != "warn" || !strings.Contains(item.Detail, "skipped") || !strings.Contains(item.Detail, path) {
		t.Fatalf("read failure check: %+v", item)
	}
}
