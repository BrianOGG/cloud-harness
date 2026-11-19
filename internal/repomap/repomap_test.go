package repomap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanSymbolsOrderAndSkippedDirectories(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a_type.go", "package demo\n\ntype Alpha struct{}\n")
	writeFile(t, root, "b_func.go", "package demo\n\nfunc Beta() {}\n")
	writeFile(t, root, "c_method.go", "package demo\n\ntype Worker struct{}\n\nfunc (w *Worker) Run() {}\n")
	writeFile(t, root, "d_script.py", "class Script:\n    pass\n\ndef greet():\n    return 'hi'\n")
	for _, directory := range []string{".git", ".cloud-harness", "node_modules", "vendor", "dist", "__pycache__"} {
		writeFile(t, root, filepath.Join(directory, "hidden.go"), "package hidden\nfunc Hidden() {}\n")
	}

	result, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", result.Diagnostics)
	}
	if got, want := len(result.Files), 4; got != want {
		t.Fatalf("file count=%d, want %d: %+v", got, want, result.Files)
	}
	paths := make([]string, 0, len(result.Files))
	for _, file := range result.Files {
		paths = append(paths, file.Path)
	}
	if got, want := strings.Join(paths, ","), "a_type.go,b_func.go,c_method.go,d_script.py"; got != want {
		t.Fatalf("paths=%q, want %q", got, want)
	}
	if got, want := symbolNames(result.Files[0].Symbols), "Alpha"; got != want {
		t.Fatalf("type symbols=%q, want %q", got, want)
	}
	if got, want := symbolNames(result.Files[1].Symbols), "Beta"; got != want {
		t.Fatalf("function symbols=%q, want %q", got, want)
	}
	if got, want := symbolNames(result.Files[2].Symbols), "Worker,Run"; got != want {
		t.Fatalf("method symbols=%q, want %q", got, want)
	}
	if result.Files[2].Symbols[1].Kind != "method" || result.Files[2].Symbols[1].Receiver != "*Worker" {
		t.Fatalf("method=%+v, want receiver *Worker", result.Files[2].Symbols[1])
	}
	if got, want := symbolNames(result.Files[3].Symbols), "Script,greet"; got != want {
		t.Fatalf("python symbols=%q, want %q", got, want)
	}

	rendered := result.Render(DefaultMaxBytes)
	second, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if repeated := second.Render(DefaultMaxBytes).Text; repeated != rendered.Text {
		t.Fatalf("render changed between scans:\nfirst:\n%s\nsecond:\n%s", rendered.Text, repeated)
	}
	for _, hidden := range []string{"hidden.go", ".git", "node_modules", "vendor"} {
		if strings.Contains(rendered.Text, hidden) {
			t.Fatalf("rendered map contains skipped path %q:\n%s", hidden, rendered.Text)
		}
	}
}

func TestScanSyntaxErrorIsDiagnosticAndOtherFilesRemain(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "bad.go", "package broken\nfunc Missing( {\n")
	writeFile(t, root, "good.go", "package good\nfunc Good() {}\n")

	result, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 1 || result.Files[0].Path != "good.go" {
		t.Fatalf("files=%+v, want only good.go", result.Files)
	}
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Path != "bad.go" {
		t.Fatalf("diagnostics=%+v, want bad.go", result.Diagnostics)
	}
	if result.Diagnostics[0].Error == "" {
		t.Fatal("syntax diagnostic is empty")
	}
}

func TestRenderTruncatesByFileAndReportsOmittedCount(t *testing.T) {
	m := Map{Files: []File{
		{Path: "a.go", Lines: 1, Package: "a", Symbols: []Symbol{{Kind: "func", Name: "A"}}},
		{Path: "b.go", Lines: 1, Package: "b", Symbols: []Symbol{{Kind: "func", Name: "B"}}},
		{Path: "c.go", Lines: 1, Package: "c", Symbols: []Symbol{{Kind: "func", Name: "C"}}},
	}}
	all := m.Render(DefaultMaxBytes)
	firstLine := strings.IndexByte(all.Text, '\n') + 1
	limit := firstLine + len(truncationMarker(2))
	truncated := m.Render(limit)
	if !truncated.Truncated {
		t.Fatal("expected truncation")
	}
	if truncated.OmittedFiles != 2 {
		t.Fatalf("omitted=%d, want 2", truncated.OmittedFiles)
	}
	if got := truncated.Text; !strings.Contains(got, "a.go") || strings.Contains(got, "b.go") || strings.Contains(got, "c.go") {
		t.Fatalf("unexpected truncated map: %q", got)
	}
	if !strings.Contains(truncated.Text, "# TRUNCATED omitted_files=2") {
		t.Fatalf("missing marker: %q", truncated.Text)
	}
}

func TestScanEmptyWorkspaceHasEmptyRenderedMap(t *testing.T) {
	result, err := Scan(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 0 || len(result.Diagnostics) != 0 {
		t.Fatalf("result=%+v, want empty map", result)
	}
	if got := result.Render(DefaultMaxBytes).Text; got != "" {
		t.Fatalf("rendered empty map=%q", got)
	}
}

func TestScanRetainsSupportedFileWithoutSymbols(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "empty.py", "# no definitions\n\n")
	result, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 1 || result.Files[0].Path != "empty.py" || len(result.Files[0].Symbols) != 0 {
		t.Fatalf("files=%+v, want path and line count only", result.Files)
	}
	if !strings.Contains(result.Render(DefaultMaxBytes).Text, "empty.py\tlines=2") {
		t.Fatalf("rendered=%q", result.Render(DefaultMaxBytes).Text)
	}
}

func symbolNames(symbols []Symbol) string {
	names := make([]string, 0, len(symbols))
	for _, symbol := range symbols {
		names = append(names, symbol.Name)
	}
	return strings.Join(names, ",")
}

func writeFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
