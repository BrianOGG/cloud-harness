package rules

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func writeRule(t *testing.T, root, relative, body string) string {
	t.Helper()
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func openRule(path string) (io.ReadCloser, error) { return os.Open(path) }

func TestLayeredRulesCanonicalDedupAndIgnoredDirectories(t *testing.T) {
	workspace, global := t.TempDir(), t.TempDir()
	globalFile := writeRule(t, global, "AGENTS.md", "global rule")
	rootFile := writeRule(t, workspace, "AGENTS.md", "root rule")
	shallowFile := writeRule(t, workspace, "z/AGENTS.md", "shallow rule")
	deepFile := writeRule(t, workspace, "a/b/AGENTS.md", "deep rule")
	for _, directory := range []string{".git", ".cloud-harness", "node_modules", "vendor", "a/node_modules", "a/b/vendor"} {
		writeRule(t, workspace, directory+"/AGENTS.md", "ignored rule")
	}
	if err := os.MkdirAll(filepath.Join(workspace, "alias"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Hit the same file via two relative paths and a symlinked workspace root.
	if err := os.Symlink(".././AGENTS.md", filepath.Join(workspace, "alias/AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	workspaceLink := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(workspace, workspaceLink); err != nil {
		t.Fatal(err)
	}
	// Directory symlinks, including cycles, must not recurse.
	if err := os.Symlink(".", filepath.Join(workspace, "cycle")); err != nil {
		t.Fatal(err)
	}
	loaded := load(filepath.Join(workspaceLink, "a", ".."), global, openRule)
	wantPaths := []string{globalFile, rootFile, shallowFile, deepFile}
	wantSources := []string{"global", "AGENTS.md", "z/AGENTS.md", "a/b/AGENTS.md"}
	if len(loaded.Files) != 4 || len(loaded.Loaded) != 4 || len(loaded.Omitted) != 0 || len(loaded.Diagnostics) != 0 {
		t.Fatalf("unexpected discovery: %+v", loaded)
	}
	previous := -1
	for i, file := range loaded.Files {
		if file.Path != wantPaths[i] || file.Source != wantSources[i] {
			t.Fatalf("source %d: %+v", i, file)
		}
		marker := "<!-- AGENTS.md: " + wantSources[i] + " -->"
		at := strings.Index(loaded.Content, marker)
		if at <= previous || strings.Count(loaded.Content, marker) != 1 {
			t.Fatalf("wrong order or duplicate marker %q: %s", marker, loaded.Content)
		}
		previous = at
	}
	if strings.Contains(loaded.Content, "ignored rule") || strings.Count(loaded.Content, "root rule") != 1 {
		t.Fatalf("ignored or duplicate body: %s", loaded.Content)
	}
	if again := load(workspace, global, openRule); !reflect.DeepEqual(loaded, again) {
		t.Fatal("path aliases changed the frozen result")
	}
}

func TestGlobalAndWorkspaceCanonicalDedup(t *testing.T) {
	workspace := t.TempDir()
	writeRule(t, workspace, "AGENTS.md", "one rule")
	global := filepath.Join(t.TempDir(), "global")
	if err := os.Symlink(workspace, global); err != nil {
		t.Fatal(err)
	}
	loaded := load(workspace, global, openRule)
	if len(loaded.Files) != 1 || loaded.Files[0].Source != "global" || strings.Count(loaded.Content, "one rule") != 1 {
		t.Fatalf("global/root duplicate: %+v", loaded)
	}
}

func TestRulesByteLimitAndOmittedSources(t *testing.T) {
	workspace, global := t.TempDir(), t.TempDir()
	writeRule(t, global, "AGENTS.md", "keep global")
	writeRule(t, workspace, "AGENTS.md", strings.Repeat("规则", MaxBytes))
	writeRule(t, workspace, "src/AGENTS.md", "omitted body")
	loaded := load(workspace, global, openRule)
	if len(loaded.Content) > MaxBytes || len(loaded.Content) < MaxBytes-4 || !utf8.ValidString(loaded.Content) {
		t.Fatalf("bad byte boundary: %d bytes, valid UTF-8=%v", len(loaded.Content), utf8.ValidString(loaded.Content))
	}
	for _, text := range []string{"keep global", "<!-- AGENTS.md: global -->", "<!-- AGENTS.md: AGENTS.md -->", "AGENTS.md truncated", `"AGENTS.md"`, `"src/AGENTS.md"`} {
		if !strings.Contains(loaded.Content, text) {
			t.Fatalf("missing %q in truncated section", text)
		}
	}
	if strings.Contains(loaded.Content, "omitted body") || !strings.HasSuffix(loaded.Content, " -->\n") {
		t.Fatal("missing final truncation marker or included a later body")
	}
	if len(loaded.Loaded) != 2 || loaded.Loaded[1].Bytes <= 0 || loaded.Loaded[1].Bytes >= loaded.Files[1].Bytes || len(loaded.Omitted) != 2 {
		t.Fatalf("bad truncation metadata: %+v", loaded)
	}
}

func TestRulesExactLimitAndMarkerAccounting(t *testing.T) {
	for _, extra := range []int{0, 1} {
		t.Run(string(rune('0'+extra)), func(t *testing.T) {
			workspace := t.TempDir()
			size := MaxBytes - len(heading) - len("<!-- AGENTS.md: AGENTS.md -->\n") - 2
			writeRule(t, workspace, "AGENTS.md", strings.Repeat("x", size+extra))
			loaded := load(workspace, "", openRule)
			if len(loaded.Content) != MaxBytes {
				t.Fatalf("size %d: rendered %d bytes", size+extra, len(loaded.Content))
			}
			if truncated := strings.Contains(loaded.Content, "AGENTS.md truncated"); truncated != (extra > 0) {
				t.Fatalf("size %d: truncated=%v", size+extra, truncated)
			}
		})
	}
	// Many small files can exceed the cap through source markers alone.
	workspace := t.TempDir()
	for i := 0; i < 250; i++ {
		name := strings.Repeat("directory", 20) + string(rune(0x400+i))
		writeRule(t, workspace, filepath.Join(name, "AGENTS.md"), "")
	}
	loaded := load(workspace, "", openRule)
	if len(loaded.Content) > MaxBytes || len(loaded.Omitted) == 0 || !strings.HasSuffix(loaded.Content, " -->\n") || !utf8.ValidString(loaded.Content) {
		t.Fatalf("marker overhead bypassed cap: %d bytes, %d omitted", len(loaded.Content), len(loaded.Omitted))
	}
}

func TestUnreadableFileSkippedAndReadOnce(t *testing.T) {
	workspace := t.TempDir()
	unreadable := writeRule(t, workspace, "AGENTS.md", "unreadable body")
	readable := writeRule(t, workspace, "src/AGENTS.md", "readable body")
	reads := make(map[string]int)
	loaded := load(workspace, "", func(path string) (io.ReadCloser, error) {
		reads[path]++
		if path == unreadable {
			return nil, fs.ErrPermission
		}
		return openRule(path)
	})
	if len(loaded.Diagnostics) != 1 || loaded.Diagnostics[0].Path != unreadable || !strings.Contains(loaded.Diagnostics[0].Error, "permission") {
		t.Fatalf("missing non-fatal permission diagnostic: %+v", loaded.Diagnostics)
	}
	if strings.Contains(loaded.Content, "unreadable body") || !strings.Contains(loaded.Content, "readable body") || len(loaded.Loaded) != 1 || loaded.Loaded[0].Path != readable {
		t.Fatalf("did not skip unreadable file: %+v", loaded)
	}
	if reads[unreadable] != 1 || reads[readable] != 1 {
		t.Fatalf("repeated disk reads: %v", reads)
	}
	failed := load(workspace, "", func(string) (io.ReadCloser, error) { return nil, fs.ErrPermission })
	if failed.Content != "" || len(failed.Diagnostics) != 2 {
		t.Fatalf("all unreadable files should inject nothing: %+v", failed)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func (failingReader) Close() error             { return nil }

func TestReadFailureSkipped(t *testing.T) {
	workspace := t.TempDir()
	writeRule(t, workspace, "AGENTS.md", "unused")
	loaded := load(workspace, "", func(string) (io.ReadCloser, error) { return failingReader{}, nil })
	if loaded.Content != "" || len(loaded.Diagnostics) != 1 || loaded.Diagnostics[0].Error != "read failed" {
		t.Fatalf("read failure was not isolated: %+v", loaded)
	}
}

func TestNoRulesIsEmpty(t *testing.T) {
	loaded := load(t.TempDir(), t.TempDir(), func(string) (io.ReadCloser, error) {
		t.Fatal("opened a nonexistent rule")
		return nil, nil
	})
	if loaded.Content != "" || len(loaded.Files) != 0 || len(loaded.Diagnostics) != 0 {
		t.Fatalf("empty workspace changed behavior: %+v", loaded)
	}
}

func TestConfigDirectory(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", base)
	t.Setenv("CLOUD_HARNESS_CONFIG", "")
	directory, err := ConfigDir()
	if err != nil || directory != filepath.Join(base, "cloud-harness") {
		t.Fatalf("default config dir=%q err=%v", directory, err)
	}
	writeRule(t, directory, "AGENTS.md", "default global")
	if loaded := Load(t.TempDir()); len(loaded.Loaded) != 1 || loaded.Loaded[0].Source != "global" || !strings.Contains(loaded.Content, "default global") {
		t.Fatalf("default global discovery: %+v", loaded)
	}
	custom := t.TempDir()
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(custom, "settings.json"))
	directory, err = ConfigDir()
	if err != nil || directory != custom {
		t.Fatalf("override config dir=%q err=%v", directory, err)
	}
	writeRule(t, custom, "AGENTS.md", "custom global")
	if loaded := Load(t.TempDir()); len(loaded.Loaded) != 1 || !strings.Contains(loaded.Content, "custom global") || strings.Contains(loaded.Content, "default global") {
		t.Fatalf("custom global discovery: %+v", loaded)
	}
}

func TestScopeAncestorsFreezeAndStayInsideWorkspace(t *testing.T) {
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(t.TempDir(), "config.yaml"))
	root := t.TempDir()
	writeRule(t, root, "AGENTS.md", "root rule")
	writeRule(t, root, "src/AGENTS.md", "parent rule")
	writeRule(t, root, "src/ui/AGENTS.md", "frontend rule")
	writeRule(t, root, "src/ui/node_modules/AGENTS.md", "vendor rule")
	outside := writeRule(t, t.TempDir(), "AGENTS.md", "external")
	writeRule(t, root, "lib/file.txt", "data")
	if err := os.Symlink(outside, filepath.Join(root, "lib", "AGENTS.md")); err != nil {
		t.Skip(err)
	}
	s := Scope{}
	got := s.Next(root, []string{"src/ui/component.tsx", "src/ui/node_modules/file", "lib/file.txt", outside}, nil)
	if len(got.Loaded) != 3 || got.Loaded[2].Source != "src/ui/AGENTS.md" || strings.Contains(got.Content, "vendor rule") || strings.Contains(got.Content, "external") {
		t.Fatalf("unexpected scoped rules: %+v", got)
	}
	writeRule(t, root, "src/AGENTS.md", "changed")
	if again := s.Next(root, []string{"src/file.go", "src/ui"}, nil); again.Content != "" {
		t.Fatal("reread active rules")
	}
	writeRule(t, root, "other/AGENTS.md", strings.Repeat("large rule", MaxBytes))
	late := s.Next(root, []string{"other/file.txt"}, nil)
	if len(late.Omitted) != 1 || len(got.Content)+len(late.Content) > MaxBytes {
		t.Fatal("run-level rules cap not enforced")
	}
}

func TestScopeUsesResolvedWorkspaceAndHonorsReadPolicy(t *testing.T) {
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(t.TempDir(), "config"))
	root := t.TempDir()
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(root, workspace); err != nil {
		t.Skip(err)
	}
	writeRule(t, root, "web/AGENTS.md", "web only")
	writeRule(t, root, "web/private/AGENTS.md", "restricted")
	s := Scope{}
	result := s.Next(workspace, []string{filepath.Join(workspace, "web/private/new.txt")}, func(path string) bool {
		return filepath.Base(filepath.Dir(path)) != "private"
	})
	if len(result.Loaded) != 1 || result.Loaded[0].Source != "web/AGENTS.md" || strings.Contains(result.Content, "restricted") || len(result.Diagnostics) != 1 {
		t.Fatalf("workspace scope or read policy lost: %+v", result)
	}
}
