package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestRepoMapCLIAndInvalidDirectory(t *testing.T) {
	emptyRoot := t.TempDir()
	var emptyOut, emptyErr bytes.Buffer
	if code := Main([]string{"repomap", "--dir", emptyRoot}, &emptyOut, &emptyErr); code != 0 || emptyOut.Len() != 0 || emptyErr.Len() != 0 {
		t.Fatalf("empty repomap exited %d: stdout=%s stderr=%s", code, emptyOut.String(), emptyErr.String())
	}

	root := t.TempDir()
	writeRepoMapTestFile(t, root, "main.go", "package main\n\ntype App struct{}\nfunc NewApp() *App { return &App{} }\n")
	writeRepoMapTestFile(t, root, "script.py", "def hello():\n    pass\n")

	var stdout, stderr bytes.Buffer
	if code := Main([]string{"repomap", "--dir", root}, &stdout, &stderr); code != 0 {
		t.Fatalf("repomap exited %d: stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"main.go", "package=main", "type:App", "func:NewApp", "script.py", "def:hello"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected diagnostics: %s", stderr.String())
	}

	writeRepoMapTestFile(t, root, "broken.go", "package broken\nfunc Incomplete( {\n")
	stdout.Reset()
	stderr.Reset()
	if code := Main([]string{"repomap", "--dir", root}, &stdout, &stderr); code != 0 || !strings.Contains(stderr.String(), "skipped broken.go") || strings.Contains(stdout.String(), "broken.go") {
		t.Fatalf("syntax-error CLI exited %d: stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Main([]string{"repomap", "--dir", root + "/missing"}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "--dir") {
		t.Fatalf("invalid directory exited %d: stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Main([]string{"repomap", "--dir", root + "/main.go"}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "not a directory") {
		t.Fatalf("non-directory exited %d: stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

func TestRepoMapCLITruncation(t *testing.T) {
	root := t.TempDir()
	writeRepoMapTestFile(t, root, "a.py", "def a():\n")
	writeRepoMapTestFile(t, root, "b.py", "def b():\n")
	writeRepoMapTestFile(t, root, "c.py", "def c():\n")

	var stdout, stderr bytes.Buffer
	if code := Main([]string{"repomap", "--dir", root, "--max-bytes", "70"}, &stdout, &stderr); code != 0 {
		t.Fatalf("repomap exited %d: stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "a.py") || strings.Contains(stdout.String(), "b.py") || strings.Contains(stdout.String(), "c.py") || !strings.Contains(stdout.String(), "# TRUNCATED omitted_files=2") {
		t.Fatalf("unexpected truncated output: %q", stdout.String())
	}
	if stdout.Len() > 70 {
		t.Fatalf("output is %d bytes, limit is 70: %q", stdout.Len(), stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Main([]string{"repomap", "--dir", root, "--max-bytes", "1"}, &stdout, &stderr); code == 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "too small") {
		t.Fatalf("too-small limit exited %d: stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

func writeRepoMapTestFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := root + "/" + name
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
