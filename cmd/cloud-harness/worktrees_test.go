package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloud-harness/cloud-harness/internal/agent"
	"github.com/cloud-harness/cloud-harness/internal/config"
	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/provider"
	"github.com/cloud-harness/cloud-harness/internal/storage/sqlite"
	"github.com/cloud-harness/cloud-harness/internal/worktree"
)

func TestChatWorktreeMultipleTurnsWithApprovedMockWrites(t *testing.T) {
	root := newReviewRepo(t)
	writeReviewFile(t, root, "note.txt", "before\n")
	reviewTestGit(t, root, "add", ".")
	reviewTestGit(t, root, "commit", "-qm", "base")
	mgr, db, err := openRunManager(context.Background(), config.Config{Provider: "mock", DBPath: config.DefaultDB}, root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	response := func(call domain.ToolCall) domain.ModelResponse {
		return domain.ModelResponse{Provider: "mock", Complete: true, Items: []domain.ResponseItem{{Ordinal: 0, Kind: domain.ItemToolCall, ToolCall: &call}}}
	}
	finish := func(answer string) domain.ModelResponse {
		args, _ := json.Marshal(map[string]string{"final_answer": answer})
		return response(domain.ToolCall{ProviderCallID: "finish-" + answer, Name: domain.ToolFinishTurn, Arguments: args})
	}
	mgr.Provider = &provider.Mock{Responses: []domain.ModelResponse{
		response(domain.ToolCall{ProviderCallID: "first", Name: "write_file", Arguments: json.RawMessage(`{"path":"note.txt","content":"first\n"}`)}), finish("first done"),
		response(domain.ToolCall{ProviderCallID: "second", Name: "write_file", Arguments: json.RawMessage(`{"path":"note.txt","content":"second\n"}`)}), finish("second done"),
	}}
	mgr.SkipPlanner = true
	var stdout, stderr bytes.Buffer
	if code := runChat(context.Background(), mgr, "", "", strings.NewReader("first turn\ny\nsecond turn\ny\n"), &stdout, &stderr); code != 0 {
		t.Fatalf("chat code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	runs, err := db.ListRuns(context.Background(), "")
	if err != nil || len(runs) != 2 || runs[0].WorktreePath == runs[1].WorktreePath || runs[0].WorktreeBaseSHA != runs[1].WorktreeBaseSHA {
		t.Fatalf("chat runs: %+v, %v", runs, err)
	}
	for i, run := range runs {
		want := []string{"first\n", "second\n"}[i]
		if run.Status != domain.RunCompleted || string(readReviewTestFile(t, filepath.Join(run.WorktreePath, "note.txt"))) != want {
			t.Fatalf("turn %d: %+v", i, run)
		}
		events, err := db.ListEvents(context.Background(), run.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, event := range events {
			if event.Type == "worktree.summary" {
				count++
				var summary worktree.Summary
				if err := json.Unmarshal(event.Payload, &summary); err != nil || len(summary.Files) != 1 || summary.Files[0] != "note.txt" || !strings.Contains(summary.Diff, "-before") || summary.Error != "" {
					t.Fatalf("turn summary: %+v, %v", summary, err)
				}
			}
		}
		if count != 1 {
			t.Fatalf("turn got %d summaries", count)
		}
	}
	if !strings.Contains(stdout.String(), "first done") || !strings.Contains(stdout.String(), "second done") || string(readReviewTestFile(t, filepath.Join(root, "note.txt"))) != "before\n" || reviewTestGit(t, root, "status", "--porcelain=v1", "--untracked-files=all") != "" {
		t.Fatalf("chat result stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	// Invalid resumes must preserve the terminal record and existing summary.
	if _, err := mgr.Resume(context.Background(), runs[0].ID, agent.ResumeInput{Decision: "approve"}); err == nil {
		t.Fatal("terminal run accepted resume")
	}
}

func TestRunAndChatWorktreeCLI(t *testing.T) {
	for _, command := range []string{"run", "chat"} {
		t.Run(command, func(t *testing.T) {
			setDiagnosticEnv(t, "", "")
			root := newReviewRepo(t)
			writeReviewFile(t, root, "note.txt", "committed\n")
			writeReviewFile(t, root, ".gitignore", ".cloud-harness/\n")
			reviewTestGit(t, root, "add", ".")
			reviewTestGit(t, root, "commit", "-qm", "base")
			base := strings.TrimSpace(reviewTestGit(t, root, "rev-parse", "HEAD"))
			mainBranch := reviewTestGit(t, root, "branch", "--show-current")
			var stdout, stderr bytes.Buffer
			if code := MainWithIO([]string{command, "--worktree", "--workspace", root, "--provider", "mock", "--message", "hello"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
				t.Fatalf("%s code=%d stdout=%s stderr=%s", command, code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String(), "Mock answer: hello") {
				t.Fatalf("missing answer: %s", stdout.String())
			}
			db, err := sqlite.Open(context.Background(), filepath.Join(root, config.DefaultDB))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			runs, err := db.ListRuns(context.Background(), "")
			if err != nil || len(runs) != 1 {
				t.Fatalf("runs: %+v, %v", runs, err)
			}
			run := runs[0]
			if run.WorktreeBaseSHA != base || run.WorktreeBranch != "cloud-harness/"+run.ID || run.WorktreePath != filepath.Join(root, ".cloud-harness", "worktrees", run.ID) {
				t.Fatalf("metadata: %+v", run)
			}
			events, err := db.ListEvents(context.Background(), run.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, event := range events {
				if event.Type != "worktree.summary" {
					continue
				}
				count++
				var payload map[string]json.RawMessage
				if err := json.Unmarshal(event.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				for _, key := range []string{"run_id", "base_sha", "branch", "worktree_path", "files", "stat", "diff", "truncated"} {
					if _, ok := payload[key]; !ok {
						t.Fatalf("summary missing %s: %s", key, event.Payload)
					}
				}
				var summary worktree.Summary
				if err := json.Unmarshal(event.Payload, &summary); err != nil || summary.RunID != run.ID || summary.BaseSHA != base || summary.Branch != run.WorktreeBranch || summary.Path != run.WorktreePath || len(summary.Files) != 0 || summary.Diff != "" || summary.Error != "" {
					t.Fatalf("summary: %+v, %v", summary, err)
				}
			}
			if count != 1 {
				t.Fatalf("got %d summaries", count)
			}
			if _, err := os.Stat(filepath.Join(run.WorktreePath, ".git")); err != nil {
				t.Fatalf("worktree not retained: %v", err)
			}
			if _, err := os.Stat(filepath.Join(run.WorktreePath, config.DefaultDB)); !os.IsNotExist(err) {
				t.Fatalf("worktree created its own DB: %v", err)
			}
			if got := reviewTestGit(t, root, "status", "--porcelain=v1", "--untracked-files=all"); got != "" {
				t.Fatalf("main workspace dirty: %q", got)
			}
			if string(readReviewTestFile(t, filepath.Join(root, "note.txt"))) != "committed\n" || reviewTestGit(t, root, "branch", "--show-current") != mainBranch {
				t.Fatal("main workspace changed")
			}
		})
	}
}

func TestWorktreeCLIRejectsNonGitBeforeOpeningStore(t *testing.T) {
	for _, command := range []string{"run", "chat"} {
		t.Run(command, func(t *testing.T) {
			setDiagnosticEnv(t, "", "")
			root := t.TempDir()
			dbPath := filepath.Join(t.TempDir(), "must-not-exist.db")
			var stdout, stderr bytes.Buffer
			code := MainWithIO([]string{command, "--worktree", "--workspace", root, "--db", dbPath, "--message", "hello"}, strings.NewReader(""), &stdout, &stderr)
			if code == 0 || !strings.Contains(stderr.String(), "requires a Git repository") || !strings.Contains(stderr.String(), ".git") {
				t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			for _, path := range []string{dbPath, filepath.Join(root, ".cloud-harness")} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("invalid workspace created %s: %v", path, err)
				}
			}
			// Even an already-existing store must not get a new run or session.
			db, err := sqlite.Open(context.Background(), dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			stdout.Reset()
			stderr.Reset()
			if code := MainWithIO([]string{command, "--worktree", "--workspace", root, "--db", dbPath, "--message", "hello"}, strings.NewReader(""), &stdout, &stderr); code == 0 {
				t.Fatal("non-Git workspace accepted")
			}
			if runs, err := db.ListRuns(context.Background(), ""); err != nil || len(runs) != 0 {
				t.Fatalf("invalid workspace wrote runs: %+v, %v", runs, err)
			}
		})
	}
}

func TestWorktreeCLIRejectsLinkedWorktreeAndUnbornHEAD(t *testing.T) {
	setDiagnosticEnv(t, "", "")
	root := newReviewRepo(t)
	for _, command := range []string{"run", "chat"} {
		var out, stderr bytes.Buffer
		if code := MainWithIO([]string{command, "--worktree", "--workspace", root, "--message", "hello"}, strings.NewReader(""), &out, &stderr); code == 0 || !strings.Contains(stderr.String(), "git rev-parse") || !strings.Contains(stderr.String(), "stderr:") {
			t.Fatalf("unborn HEAD code=%d stderr=%s", code, stderr.String())
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".cloud-harness")); !os.IsNotExist(err) {
		t.Fatalf("unborn HEAD created state: %v", err)
	}
	reviewTestGit(t, root, "commit", "--allow-empty", "-qm", "base")
	info, err := worktree.Create(context.Background(), root, "run_outer")
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"run", "chat"} {
		var out, stderr bytes.Buffer
		if code := MainWithIO([]string{command, "--worktree", "--workspace", info.Path, "--message", "hello"}, strings.NewReader(""), &out, &stderr); code == 0 || !strings.Contains(stderr.String(), "nested worktree") {
			t.Fatalf("nested code=%d stderr=%s", code, stderr.String())
		}
	}
	if _, err := os.Stat(filepath.Join(info.Path, ".cloud-harness")); !os.IsNotExist(err) {
		t.Fatalf("nested worktree created state: %v", err)
	}
}

func TestNoWorktreeFlagPreservesRunAndChatWorkspace(t *testing.T) {
	for _, command := range []string{"run", "chat"} {
		t.Run(command, func(t *testing.T) {
			setDiagnosticEnv(t, "", "")
			root := t.TempDir() // Existing non-Git workspace behavior is supported.
			dbPath := filepath.Join(t.TempDir(), "h.db")
			var stdout, stderr bytes.Buffer
			if code := MainWithIO([]string{command, "--workspace", root, "--db", dbPath, "--provider", "mock", "--message", "ordinary"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
				t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			db, err := sqlite.Open(context.Background(), dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			runs, err := db.ListRuns(context.Background(), "")
			if err != nil || len(runs) != 1 || runs[0].WorktreePath != "" || runs[0].WorktreeBaseSHA != "" || runs[0].WorktreeBranch != "" {
				t.Fatalf("ordinary run: %+v, %v", runs, err)
			}
			events, err := db.ListEvents(context.Background(), runs[0].ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event.Type == "worktree.summary" {
					t.Fatal("ordinary run emitted a worktree summary")
				}
			}
			if _, err := os.Stat(filepath.Join(root, ".cloud-harness")); !os.IsNotExist(err) {
				t.Fatalf("ordinary text run initialized workspace state: %v", err)
			}
		})
	}
}
