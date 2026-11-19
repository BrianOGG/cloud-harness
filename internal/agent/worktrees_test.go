package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/provider"
	workspacesnapshot "github.com/cloud-harness/cloud-harness/internal/snapshot"
	"github.com/cloud-harness/cloud-harness/internal/storage"
	"github.com/cloud-harness/cloud-harness/internal/storage/sqlite"
	"github.com/cloud-harness/cloud-harness/internal/tools"
	"github.com/cloud-harness/cloud-harness/internal/worktree"
)

func isolationGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func isolationManager(t *testing.T, prov provider.Provider) (*Manager, *sqlite.DB, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	root := t.TempDir()
	isolationGit(t, root, "init", "-q", "--template=")
	isolationGit(t, root, "config", "user.name", "Isolation Test")
	isolationGit(t, root, "config", "user.email", "isolation@example.invalid")
	isolationGit(t, root, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("before\n"), 0644); err != nil {
		t.Fatal(err)
	}
	isolationGit(t, root, "add", ".")
	isolationGit(t, root, "commit", "-qm", "base")
	if err := os.MkdirAll(filepath.Join(root, ".cloud-harness"), 0755); err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(context.Background(), filepath.Join(root, ".cloud-harness", "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	broker := tools.NewBroker(db, db)
	if err := tools.RegisterLocal(broker, root); err != nil {
		t.Fatal(err)
	}
	return &Manager{Store: db, Provider: prov, Broker: broker, Workspace: root, Worktree: true, OwnerID: "worktree-test", SkipPlanner: true}, db, root
}

func isolationSummaries(t *testing.T, db *sqlite.DB, runID string) []worktree.Summary {
	t.Helper()
	events, err := db.ListEvents(context.Background(), runID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var summaries []worktree.Summary
	for _, event := range events {
		if event.Type != "worktree.summary" {
			continue
		}
		var summary worktree.Summary
		if err := json.Unmarshal(event.Payload, &summary); err != nil {
			t.Fatal(err)
		}
		summaries = append(summaries, summary)
	}
	return summaries
}

func isolationRead(t *testing.T, root, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestWorktreeApprovalResumeAndSnapshotsStayIsolated(t *testing.T) {
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(snapshotWrite("write", "note.txt", "after\n")), textOnly("done"),
	}}
	m, db, root := isolationManager(t, mock)
	run := runSnapshotMessage(t, m)
	if run.Status != domain.RunWaiting || run.WaitReason != domain.WaitApproval || len(isolationSummaries(t, db, run.ID)) != 0 {
		t.Fatalf("expected approval without a summary: %+v", run)
	}
	if isolationRead(t, run.WorktreePath, "note.txt") != "before\n" || isolationRead(t, root, "note.txt") != "before\n" {
		t.Fatal("unapproved call changed files")
	}
	gitPointer := isolationRead(t, run.WorktreePath, ".git")
	// Simulate a new CLI process with no --worktree option. Resume must use
	// the saved run binding, including for snapshots and broker executors.
	resumer := &Manager{Store: db, Provider: mock, Workspace: root, Broker: m.Broker, OwnerID: m.OwnerID}
	run, err := resumer.Resume(context.Background(), run.ID, ResumeInput{Decision: "approve"})
	if err != nil || run.Status != domain.RunCompleted {
		t.Fatalf("resume: %+v, %v", run, err)
	}
	if isolationRead(t, root, "note.txt") != "before\n" || isolationRead(t, run.WorktreePath, "note.txt") != "after\n" || isolationGit(t, root, "status", "--porcelain=v1", "--untracked-files=all") != "" {
		t.Fatal("file tools escaped the worktree")
	}
	if m.Workspace != root {
		t.Fatal("parent manager workspace mutated")
	}
	resources, err := m.Broker.ExtractResources("write_file", json.RawMessage(`{"path":"note.txt","content":"unused"}`))
	if err != nil || len(resources) != 1 || resources[0] != filepath.Join(root, "note.txt") {
		t.Fatalf("parent broker mutated: %v, %v", resources, err)
	}
	summaries := isolationSummaries(t, db, run.ID)
	if len(summaries) != 1 || summaries[0].RunID != run.ID || summaries[0].BaseSHA != run.WorktreeBaseSHA || summaries[0].Branch != run.WorktreeBranch || len(summaries[0].Files) != 1 || summaries[0].Files[0] != "note.txt" || !strings.Contains(summaries[0].Diff, "+after") || !strings.Contains(summaries[0].Stat, "1 insertion(+), 1 deletion(-)") || summaries[0].Error != "" {
		t.Fatalf("summary: %+v", summaries)
	}
	s, records := workspaceRecords(t, db, run.WorktreePath)
	assertSnapshotLabels(t, records, run.ID, "batch-1/pre", "batch-1/post")
	if len(snapshotSkips(t, db, run.ID)) != 0 {
		t.Fatal("worktree snapshots were skipped")
	}
	for _, record := range records {
		if record.WorkspacePath != run.WorktreePath || record.WorkspaceID != run.WorkspaceID || record.RunID != run.ID {
			t.Fatalf("snapshot binding: %+v", record)
		}
	}
	if err := s.Restore(context.Background(), records[0]); err != nil {
		t.Fatal(err)
	}
	if isolationRead(t, run.WorktreePath, "note.txt") != "before\n" || isolationRead(t, run.WorktreePath, ".git") != gitPointer || isolationRead(t, root, "note.txt") != "before\n" {
		t.Fatal("snapshot restore changed main files or the worktree Git pointer")
	}
	if _, err := os.Stat(filepath.Join(run.WorktreePath, ".cloud-harness", "harness.db")); !os.IsNotExist(err) {
		t.Fatalf("worktree has its own database: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".cloud-harness", "snapshots")); !os.IsNotExist(err) {
		t.Fatalf("worktree snapshots polluted main shadow repository: %v", err)
	}
	if _, err := os.Stat(run.WorktreePath); err != nil {
		t.Fatalf("worktree was removed: %v", err)
	}
}

func TestWorktreeDelegateAndCommandShareParentExecutionRoot(t *testing.T) {
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "delegate", Name: domain.ToolDelegateTask, Arguments: json.RawMessage(`{"goal":"read note.txt","agent":"explorer"}`)}),
		toolResp(domain.ToolCall{ProviderCallID: "read", Name: "read_file", Arguments: json.RawMessage(`{"path":"note.txt"}`)}),
		textOnly("before"),
		toolResp(domain.ToolCall{ProviderCallID: "cwd", Name: "run_command", Arguments: json.RawMessage(`{"command":"pwd"}`)}),
		textOnly("done"),
	}}
	m, db, root := isolationManager(t, mock)
	// Even if runtime tools were already registered on the parent, executors
	// that close over a manager must be rebound for the worktree.
	m.ensureTools()
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("dirty main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run := runSnapshotMessage(t, m)
	if run.Status != domain.RunCompleted {
		t.Fatalf("run: %+v", run)
	}
	runs, err := db.ListRuns(context.Background(), run.SessionID)
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs: %+v, %v", runs, err)
	}
	for _, child := range runs {
		if child.ParentRunID == "" {
			continue
		}
		if child.ParentRunID != run.ID || child.WorkspaceID != run.WorkspaceID || child.WorktreePath != "" || child.FinalAnswer != "before" {
			t.Fatalf("child binding: %+v", child)
		}
		batches, err := db.LoadBatches(context.Background(), child.ID)
		if err != nil || len(batches) != 1 || !strings.Contains(string(batches[0].Invocations[0].Result.Data), "before") || strings.Contains(string(batches[0].Invocations[0].Result.Data), "dirty main") {
			t.Fatalf("child read wrong root: %+v, %v", batches, err)
		}
		if len(isolationSummaries(t, db, child.ID)) != 0 {
			t.Fatal("child created its own worktree summary")
		}
	}
	batches, err := db.LoadBatches(context.Background(), run.ID)
	if err != nil || len(batches) != 2 || !strings.Contains(string(batches[1].Invocations[0].Result.Data), run.WorktreePath) {
		t.Fatalf("command cwd: %+v, %v", batches, err)
	}
	if isolationRead(t, root, "note.txt") != "dirty main\n" || isolationGit(t, root, "status", "--porcelain=v1", "--untracked-files=all") != "M note.txt" {
		t.Fatal("main workspace was polluted")
	}
}

func TestWorktreeSummaryOnFailureAndWaitingCancellation(t *testing.T) {
	t.Run("provider failure", func(t *testing.T) {
		m, db, _ := isolationManager(t, &provider.Mock{Err: errors.New("fixture provider failure")})
		run := runSnapshotMessage(t, m)
		if run.Status != domain.RunFailed || len(isolationSummaries(t, db, run.ID)) != 1 {
			t.Fatalf("failure did not emit summary: %+v", run)
		}
	})
	t.Run("cancel waiting", func(t *testing.T) {
		m, db, _ := isolationManager(t, &provider.Mock{Responses: []domain.ModelResponse{toolResp(snapshotWrite("write", "note.txt", "never"))}})
		run := runSnapshotMessage(t, m)
		if run.Status != domain.RunWaiting {
			t.Fatalf("expected wait: %+v", run)
		}
		var err error
		for range 2 {
			run, err = m.Cancel(context.Background(), run.ID)
			if err != nil || run.Status != domain.RunCancelled {
				t.Fatalf("cancel: %+v, %v", run, err)
			}
		}
		if summaries := isolationSummaries(t, db, run.ID); len(summaries) != 1 || len(summaries[0].Files) != 0 {
			t.Fatalf("cancel summaries: %+v", summaries)
		}
	})
}

func TestWorktreeCancellationSummaryIncludesLastToolWrite(t *testing.T) {
	m, db, root := isolationManager(t, &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "block", Name: "block_write", Arguments: json.RawMessage(`{}`)}),
	}})
	started := make(chan string, 1)
	m.Broker.Register(tools.Spec{
		Name: "block_write", SideEffectClass: "reversible",
		Execute: func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
			runs, err := db.ListRuns(ctx, "")
			if err != nil || len(runs) != 1 {
				return nil, errors.New("missing run")
			}
			path := filepath.Join(runs[0].WorktreePath, "note.txt")
			if err := os.WriteFile(path, []byte("first write\n"), 0644); err != nil {
				return nil, err
			}
			started <- runs[0].ID
			<-ctx.Done()
			if err := os.WriteFile(path, []byte("last write after cancellation\n"), 0644); err != nil {
				return nil, err
			}
			return nil, ctx.Err()
		},
	})
	m.Grants = []domain.Grant{{Tool: "block_write", Op: "call", State: "active", Subject: "test"}}
	session, err := m.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type result struct {
		run storage.Run
		err error
	}
	done := make(chan result, 1)
	go func() {
		run, err := m.RunMessage(ctx, session.ID, "block then cancel")
		done <- result{run, err}
	}()
	var runID string
	select {
	case runID = <-started:
	case <-ctx.Done():
		t.Fatal("tool did not start")
	}
	if _, err := m.Cancel(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.err != nil || result.run.Status != domain.RunCancelled {
			t.Fatalf("cancel result: %+v, %v", result.run, result.err)
		}
		summaries := isolationSummaries(t, db, runID)
		if len(summaries) != 1 || !strings.Contains(summaries[0].Diff, "+last write after cancellation") {
			t.Fatalf("summary missed last write: %+v", summaries)
		}
		if isolationRead(t, root, "note.txt") != "before\n" {
			t.Fatal("cancelled tool wrote main file")
		}
		id, err := workspacesnapshot.WorkspaceIDForPath(result.run.WorktreePath)
		if err != nil {
			t.Fatal(err)
		}
		records, err := db.ListWorkspaceSnapshots(context.Background(), id)
		if err != nil || len(records) != 2 {
			t.Fatalf("cancel snapshots: %+v, %v", records, err)
		}
	case <-ctx.Done():
		t.Fatal("cancelled run did not finish")
	}
}
