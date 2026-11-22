package main

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/config"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
)

func TestRunContextIncludesBoundedRepoMapAfterRules(t *testing.T) {
	setDiagnosticEnv(t, "", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	writeRepoMapTestFile(t, root, "AGENTS.md", "Follow project conventions.\n")
	writeRepoMapTestFile(t, root, "main.go", "package main\n\ntype App struct{}\nfunc NewApp() *App { return &App{} }\n")
	writeRepoMapTestFile(t, root, "script.py", "def hello():\n    pass\n")
	for i := 0; i < 500; i++ {
		name := fmt.Sprintf("src_%04d.py", i)
		writeRepoMapTestFile(t, root, name, fmt.Sprintf("def function_%04d():\n    pass\n", i))
	}

	manager, db, err := openManager(context.Background(), config.Config{
		Provider: "mock", DBPath: filepath.Join(t.TempDir(), "run.db"), DisableSnapshots: true,
	}, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	mock := &provider.Mock{Responses: []domain.ModelResponse{{
		Provider: "mock", Complete: true,
		Items: []domain.ResponseItem{{Ordinal: 0, Kind: domain.ItemText, Text: "done"}},
	}}}
	manager.Provider = mock
	manager.SkipPlanner = true
	manager.DisableSnapshots = true
	session, err := manager.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	run, err := manager.RunMessage(context.Background(), session.ID, "inspect the project")
	if err != nil || run.Status != domain.RunCompleted {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	if len(mock.Requests) != 1 {
		t.Fatalf("model requests=%d", len(mock.Requests))
	}
	request := mock.Requests[0]
	if len(request.Prefix) != 3 || !strings.Contains(request.Prefix[2].Content, "Project rules") || !strings.Contains(request.Prefix[2].Content, "Follow project conventions") {
		t.Fatalf("project rules changed or disappeared from prefix: %+v", request.Prefix)
	}
	if len(request.Messages) <= len(request.Prefix) || request.Messages[len(request.Prefix)].Role != "system" || !strings.HasPrefix(request.Messages[len(request.Prefix)].Content, "Repo-Map:\n") {
		t.Fatalf("Repo-Map is not immediately after project rules: %+v", request.Messages)
	}
	var cliMap, cliErr bytes.Buffer
	if code := cmdRepoMap([]string{"--dir", root, "--max-bytes", "8192"}, &cliMap, &cliErr); code != 0 {
		t.Fatalf("repomap CLI exited %d: stdout=%s stderr=%s", code, cliMap.String(), cliErr.String())
	}
	mapSection := request.Messages[len(request.Prefix)].Content
	if got, want := strings.TrimPrefix(mapSection, "Repo-Map:\n"), cliMap.String(); got != want {
		t.Fatalf("run map differs from repomap CLI output\ngot: %q\nwant: %q", got, want)
	}
	if !strings.Contains(cliMap.String(), "main.go") || !strings.Contains(cliMap.String(), "script.py") || !strings.Contains(cliMap.String(), "# TRUNCATED omitted_files=") {
		t.Fatalf("map lacks Go/Python symbols or truncation marker: %s", cliMap.String())
	}
	if len(cliMap.Bytes()) > 8192 {
		t.Fatalf("rendered map has %d bytes; limit is 8192", cliMap.Len())
	}
}

func TestRunNoRepoMapFlag(t *testing.T) {
	setDiagnosticEnv(t, "", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	workspace := t.TempDir()
	writeRepoMapTestFile(t, workspace, "main.go", "package main\nfunc Main() {}\n")
	var out, stderr bytes.Buffer
	code := Main([]string{
		"run", "--no-repomap", "--message", "hello", "--workspace", workspace,
		"--db", filepath.Join(t.TempDir(), "disabled.db"), "--provider", "mock",
	}, &out, &stderr)
	if code != 0 || !strings.Contains(out.String(), "COMPLETED") {
		t.Fatalf("run --no-repomap exited %d: stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
}

func TestEmptyProjectDoesNotAddRepoMapThroughRunManager(t *testing.T) {
	root := t.TempDir()
	writeRepoMapTestFile(t, root, "README.txt", "plain text workspace")
	manager, db, err := openManager(context.Background(), config.Config{
		Provider: "mock", DBPath: filepath.Join(t.TempDir(), "empty.db"), DisableSnapshots: true,
	}, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	mock := &provider.Mock{}
	manager.Provider = mock
	manager.SkipPlanner = true
	manager.DisableSnapshots = true
	session, err := manager.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RunMessage(context.Background(), session.ID, "hello"); err != nil {
		t.Fatal(err)
	}
	if len(mock.Requests) != 1 || strings.Contains(mock.Requests[0].Messages[0].Content, "Repo-Map:") {
		t.Fatalf("empty workspace injected a map: %+v", mock.Requests)
	}
	for _, message := range mock.Requests[0].Messages {
		if strings.HasPrefix(message.Content, "Repo-Map:\n") {
			t.Fatal("empty workspace injected a Repo-Map section")
		}
	}
}
