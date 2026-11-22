package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/agent"
	"github.com/BrianOGG/cloud-harness/internal/config"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/storage"
	"github.com/BrianOGG/cloud-harness/internal/storage/sqlite"
	"github.com/BrianOGG/cloud-harness/internal/transport"
	"github.com/BrianOGG/cloud-harness/internal/worktree"
)

func lifecycleFixture(t *testing.T) (string, *sqlite.DB) {
	t.Helper()
	root := newReviewRepo(t)
	writeReviewFile(t, root, "note.txt", "before\n")
	writeReviewFile(t, root, "expected.txt", "before\n")
	writeReviewFile(t, root, "go.mod", "module example.invalid/worktreefixture\n\ngo 1.23\n")
	writeReviewFile(t, root, "isolation_test.go", `package fixture
import("os";"testing")
func TestIsolated(t *testing.T){
 got,e:=os.ReadFile("note.txt");if e!=nil{t.Fatal(e)}
 want,e:=os.ReadFile("expected.txt");if e!=nil{t.Fatal(e)}
 if string(got)!=string(want){t.Fatalf("other run interfered: got %q want %q",got,want)}
}
`)
	reviewTestGit(t, root, "add", ".")
	reviewTestGit(t, root, "commit", "-qm", "fixture base")
	if err := os.MkdirAll(filepath.Join(root, ".cloud-harness"), 0755); err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(context.Background(), filepath.Join(root, config.DefaultDB))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return root, db
}

func lifecycleRun(t *testing.T, root string, db *sqlite.DB, id string, status domain.RunStatus) storage.Run {
	t.Helper()
	ctx := context.Background()
	info, err := worktree.Create(ctx, root, id)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	session := storage.Session{ID: "ses_" + id, CreatedAt: now, UpdatedAt: now}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	run := storage.Run{ID: id, SessionID: session.ID, Status: status, Budget: domain.DefaultBudget(), Version: 1, CreatedAt: now, UpdatedAt: now, WorktreePath: info.Path, WorktreeBranch: info.Branch, WorktreeBaseSHA: info.BaseSHA}
	if err := db.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	return run
}

func callWorktrees(root, action, id string, flags ...string) (int, string, string) {
	args := []string{"worktrees", action}
	if id != "" {
		args = append(args, id)
	}
	args = append(args, "--workspace", root)
	args = append(args, flags...)
	var out, err bytes.Buffer
	code := Main(args, &out, &err)
	return code, out.String(), err.String()
}

func TestWorktreesListDiffAndDiscard(t *testing.T) {
	setDiagnosticEnv(t, "", "")
	root, db := lifecycleFixture(t)
	a := lifecycleRun(t, root, db, "run_a", domain.RunCompleted)
	b := lifecycleRun(t, root, db, "run_b", domain.RunRunning)
	writeReviewFile(t, a.WorktreePath, "note.txt", "left\n")
	code, out, stderr := callWorktrees(root, "list", "", "--json")
	if code != 0 {
		t.Fatalf("list: %d %s", code, stderr)
	}
	var rows []worktreeRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].State != "pending" || rows[1].State != "running" || rows[0].BaseSHA != a.WorktreeBaseSHA || rows[1].Branch != b.WorktreeBranch {
		t.Fatalf("rows: %+v", rows)
	}
	code, out, stderr = callWorktrees(root, "diff", a.ID, "--json")
	if code != 0 {
		t.Fatalf("diff: %s", stderr)
	}
	var summary worktree.Summary
	if err := json.Unmarshal([]byte(out), &summary); err != nil {
		t.Fatal(err)
	}
	want, err := worktree.Summarize(context.Background(), rows[0].Info)
	if err != nil || !reflect.DeepEqual(want, summary) {
		t.Fatalf("diff != Summarize: %+v != %+v, %v", summary, want, err)
	}
	if code, _, stderr := callWorktrees(root, "discard", a.ID); code == 0 || !strings.Contains(stderr, "--force") {
		t.Fatalf("unforced discard: %d %s", code, stderr)
	}
	if code, _, stderr := callWorktrees(root, "discard", b.ID, "--force"); code == 0 || !strings.Contains(stderr, "running or waiting") {
		t.Fatalf("active discard: %d %s", code, stderr)
	}
	if code, _, stderr := callWorktrees(root, "discard", a.ID, "--force"); code != 0 {
		t.Fatal(stderr)
	}
	stored, err := db.GetRun(context.Background(), a.ID)
	if err != nil || stored.WorktreeState != "discarded" || stored.Status != domain.RunCompleted {
		t.Fatalf("discard record: %+v %v", stored, err)
	}
	assertLifecycleGone(t, root, a)
	if code, _, stderr := callWorktrees(root, "diff", a.ID); code == 0 || !strings.Contains(stderr, "already discarded") {
		t.Fatalf("destroyed diff: %d %s", code, stderr)
	}
	if code, _, stderr := callWorktrees(root, "discard", a.ID); code != 0 {
		t.Fatalf("idempotent discard: %s", stderr)
	}
}

func TestWorktreesCleanMergeAndMergedDiscard(t *testing.T) {
	setDiagnosticEnv(t, "", "")
	root, db := lifecycleFixture(t)
	run := lifecycleRun(t, root, db, "run_merge", domain.RunCompleted)
	writeReviewFile(t, run.WorktreePath, "note.txt", "merged\n")
	writeReviewFile(t, run.WorktreePath, "expected.txt", "merged\n")
	if code, _, stderr := callWorktrees(root, "merge", run.ID); code != 0 {
		t.Fatalf("merge: %s", stderr)
	}
	stored, err := db.GetRun(context.Background(), run.ID)
	if err != nil || stored.WorktreeState != "merged" {
		t.Fatalf("merged state: %+v %v", stored, err)
	}
	if string(readReviewTestFile(t, filepath.Join(root, "note.txt"))) != "merged\n" {
		t.Fatal("main missing change")
	}
	assertLifecycleGone(t, root, run)
	if code, _, stderr := callWorktrees(root, "diff", run.ID); code == 0 || !strings.Contains(stderr, "already merged") {
		t.Fatalf("merged diff: %d %s", code, stderr)
	}
	if code, _, stderr := callWorktrees(root, "discard", run.ID); code != 0 {
		t.Fatalf("merged discard: %s", stderr)
	}
	if code, out, stderr := callWorktrees(root, "list", "", "--json"); code != 0 || !strings.Contains(out, `"state":"merged"`) || strings.Contains(out, `"orphan"`) {
		t.Fatalf("merged list: %d %s %s", code, out, stderr)
	}
}

func TestWorktreesConflictStopsAndManualRetry(t *testing.T) {
	setDiagnosticEnv(t, "", "")
	root, db := lifecycleFixture(t)
	run := lifecycleRun(t, root, db, "run_conflict", domain.RunCompleted)
	writeReviewFile(t, run.WorktreePath, "note.txt", "agent\n")
	writeReviewFile(t, run.WorktreePath, "expected.txt", "agent\n")
	writeReviewFile(t, root, "note.txt", "main\n")
	writeReviewFile(t, root, "expected.txt", "main\n")
	reviewTestGit(t, root, "commit", "-qam", "main advances")
	before := reviewTestGit(t, root, "rev-parse", "HEAD")
	if code, _, stderr := callWorktrees(root, "merge", run.ID); code == 0 || !strings.Contains(stderr, "conflict files:") || !strings.Contains(stderr, "note.txt") {
		t.Fatalf("conflict: %d %s", code, stderr)
	}
	if got := reviewTestGit(t, root, "rev-parse", "HEAD"); got != before {
		t.Fatal("main changed on conflict")
	}
	stored, err := db.GetRun(context.Background(), run.ID)
	if err != nil || stored.WorktreeState != "" {
		t.Fatalf("conflict marked merged: %+v %v", stored, err)
	}
	writeReviewFile(t, run.WorktreePath, "note.txt", "both\n")
	writeReviewFile(t, run.WorktreePath, "expected.txt", "both\n")
	reviewTestGit(t, run.WorktreePath, "add", ".")
	reviewTestGit(t, run.WorktreePath, "commit", "--no-edit")
	if code, _, stderr := callWorktrees(root, "merge", run.ID); code != 0 {
		t.Fatalf("manual retry: %s", stderr)
	}
	if string(readReviewTestFile(t, filepath.Join(root, "note.txt"))) != "both\n" {
		t.Fatal("manual resolution lost")
	}
}

func TestWorktreesRollbackFailingGoTest(t *testing.T) {
	setDiagnosticEnv(t, "", "")
	root, db := lifecycleFixture(t)
	run := lifecycleRun(t, root, db, "run_rollback", domain.RunCompleted)
	// The fixture's real test passes in the isolated worktree, then fails only
	// after integration in main. This exercises the post-merge test gate.
	writeReviewFile(t, run.WorktreePath, "main_gate_test.go", `package fixture
import("os";"strings";"testing")
func TestMainGate(t *testing.T){cwd,e:=os.Getwd();if e!=nil{t.Fatal(e)};if !strings.Contains(cwd,"/worktrees/"){t.Fatal("main integration gate rejected")}}
`)
	before := reviewTestGit(t, root, "rev-parse", "HEAD")
	if code, _, stderr := callWorktrees(root, "merge", run.ID); code == 0 || !strings.Contains(stderr, "rolled back to") || !strings.Contains(stderr, "main integration gate rejected") {
		t.Fatalf("rollback: %d %s", code, stderr)
	}
	if reviewTestGit(t, root, "rev-parse", "HEAD") != before || reviewTestGit(t, root, "status", "--porcelain") != "" {
		t.Fatal("main not restored")
	}
	if _, err := os.Stat(filepath.Join(root, "main_gate_test.go")); !os.IsNotExist(err) {
		t.Fatal("failed test leaked into main")
	}
	stored, err := db.GetRun(context.Background(), run.ID)
	if err != nil || stored.WorktreeState != "" {
		t.Fatalf("rollback state: %+v %v", stored, err)
	}
	if _, exists, err := worktree.Lookup(context.Background(), root, run.ID); err != nil || !exists {
		t.Fatal("rollback lost worktree")
	}
	// Fix the gate in the retained worktree, then retry successfully.
	if err := os.Remove(filepath.Join(run.WorktreePath, "main_gate_test.go")); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := callWorktrees(root, "merge", run.ID); code != 0 {
		t.Fatalf("rollback retry: %s", stderr)
	}
}

func TestWorktreesConcurrentMergesSerialize(t *testing.T) {
	setDiagnosticEnv(t, "", "")
	root, db := lifecycleFixture(t)
	a := lifecycleRun(t, root, db, "run_serial_a", domain.RunCompleted)
	b := lifecycleRun(t, root, db, "run_serial_b", domain.RunCompleted)
	writeReviewFile(t, a.WorktreePath, "a.txt", "a\n")
	writeReviewFile(t, b.WorktreePath, "b.txt", "b\n")
	start := make(chan struct{})
	results := make(chan string, 2)
	for _, run := range []storage.Run{a, b} {
		go func(run storage.Run) {
			<-start
			code, _, stderr := callWorktrees(root, "merge", run.ID, "--test-command", "go test -p 1 ./...")
			if code != 0 {
				results <- stderr
			} else {
				results <- ""
			}
		}(run)
	}
	close(start)
	for range 2 {
		if err := <-results; err != "" {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []storage.Run{a, b} {
		stored, err := db.GetRun(context.Background(), r.ID)
		if err != nil || stored.WorktreeState != "merged" {
			t.Fatalf("serial state: %+v %v", stored, err)
		}
		assertLifecycleGone(t, root, r)
	}
	if reviewTestGit(t, root, "status", "--porcelain") != "" {
		t.Fatal("main dirty after serial merges")
	}
}

func TestWorktreeDoctorAndRetainOrphans(t *testing.T) {
	setDiagnosticEnv(t, "", "")
	root, db := lifecycleFixture(t)
	ctx := context.Background()
	missing := lifecycleRun(t, root, db, "run_missing", domain.RunCompleted)
	if err := worktree.Remove(ctx, root, worktree.Info{RunID: missing.ID, BaseSHA: missing.WorktreeBaseSHA, Branch: missing.WorktreeBranch, Path: missing.WorktreePath}); err != nil {
		t.Fatal(err)
	}
	orphan, err := worktree.Create(ctx, root, "run_orphan")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().AddDate(0, 0, -10)
	for _, path := range []string{orphan.Path, orphan.Path + ".json"} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	live := lifecycleRun(t, root, db, "run_live", domain.RunCompleted)
	expired := lifecycleRun(t, root, db, "run_expired", domain.RunCompleted)
	expired.UpdatedAt = old
	if err := db.CompareAndSwapRun(ctx, expired, expired.Version); err != nil {
		t.Fatal(err)
	}
	item := checkWorktrees(ctx, root, filepath.Join(root, config.DefaultDB))
	if item.Status != "warn" || !strings.Contains(item.Detail, "run_orphan: missing_run") || !strings.Contains(item.Detail, "run_missing: missing_worktree") {
		t.Fatalf("doctor: %+v", item)
	}
	if code, out, stderr := callWorktrees(root, "list", "", "--json"); code != 0 || !strings.Contains(out, `"orphan":"missing_run"`) || !strings.Contains(out, `"orphan":"missing_worktree"`) {
		t.Fatalf("orphan list %d %s %s", code, out, stderr)
	}
	var out, stderr bytes.Buffer
	args := []string{"retain", "--workspace", root, "--prune-runs-days", "7"}
	if code := Main(append(args, "--dry-run"), &out, &stderr); code != 0 || !strings.Contains(out.String(), `"run_id":"run_orphan"`) || !strings.Contains(out.String(), `"run_id":"run_expired"`) {
		t.Fatalf("dry retain: %d %s %s", code, out.String(), stderr.String())
	}
	for _, id := range []string{orphan.RunID, expired.ID, live.ID} {
		if _, exists, err := worktree.Lookup(ctx, root, id); err != nil || !exists {
			t.Fatalf("dry-run removed %s: %v", id, err)
		}
	}
	out.Reset()
	stderr.Reset()
	if code := Main(args, &out, &stderr); code != 0 || strings.Contains(out.String(), `"error"`) {
		t.Fatalf("retain: %d %s %s", code, out.String(), stderr.String())
	}
	for _, id := range []string{orphan.RunID, expired.ID} {
		if _, exists, err := worktree.Lookup(ctx, root, id); err != nil || exists {
			t.Fatalf("orphan retained %s: %v", id, err)
		}
	}
	if _, exists, err := worktree.Lookup(ctx, root, live.ID); err != nil || !exists {
		t.Fatal("surviving run removed")
	}
	if _, err := db.GetRun(ctx, missing.ID); err != nil {
		t.Fatal("unexpired missing worktree record pruned")
	}
}

func TestServeWorktreeHTTPApprovalAndSummary(t *testing.T) {
	setDiagnosticEnv(t, "", "")
	root, _ := lifecycleFixture(t)
	// cmdServe uses this exact opener, including validation and DB anchoring.
	mgr, db, err := openRunManager(context.Background(), config.Config{Provider: "mock", DBPath: config.DefaultDB}, root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	mgr.SkipPlanner = true
	mgr.Provider = &provider.Mock{Responses: []domain.ModelResponse{lifecycleTool("write", "write_file", map[string]string{"path": "note.txt", "content": "http\n"}), lifecycleFinish("http done")}}
	handler := (&transport.Server{Manager: mgr}).Handler()
	request := func(path, body string, want int) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("HTTP %s: %d %s", path, rec.Code, rec.Body.String())
		}
		var v map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	session := request("/v1/sessions", "", http.StatusCreated)["id"].(string)
	view := request("/v1/sessions/"+session+"/runs?wait=1", `{"message":"edit via http"}`, http.StatusCreated)
	if view["status"] != string(domain.RunWaiting) {
		t.Fatalf("expected approval: %+v", view)
	}
	id := view["id"].(string)
	view = request("/v1/runs/"+id+"/responses?wait=1", `{"decision":"approve"}`, http.StatusOK)
	if view["status"] != string(domain.RunCompleted) {
		t.Fatalf("resume: %+v", view)
	}
	run, err := db.GetRun(context.Background(), id)
	if err != nil || run.WorktreePath == "" {
		t.Fatalf("HTTP binding: %+v %v", run, err)
	}
	if string(readReviewTestFile(t, filepath.Join(run.WorktreePath, "note.txt"))) != "http\n" || string(readReviewTestFile(t, filepath.Join(root, "note.txt"))) != "before\n" || reviewTestGit(t, root, "status", "--porcelain") != "" {
		t.Fatal("HTTP escaped isolation")
	}
	assertLifecycleSummary(t, db, run.ID)
	for _, action := range []string{"merge", "discard"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/worktrees/"+id+"/"+action, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("unexpected lifecycle HTTP endpoint: %d", rec.Code)
		}
	}
}

// Each conversation is scripted independently; the barrier proves two worktree
// runs overlap without depending on provider scheduling order.
type concurrentWorktreeMock struct {
	mu      sync.Mutex
	started int
	ready   chan struct{}
}

func (*concurrentWorktreeMock) Name() string { return "mock" }
func (p *concurrentWorktreeMock) Complete(ctx context.Context, req provider.Request) (domain.ModelResponse, error) {
	side := ""
	step := 0
	for _, m := range req.Messages {
		if m.Role == "user" && (m.Content == "left" || m.Content == "right") {
			side = m.Content
		}
		for _, call := range m.ToolCalls {
			if call.Name == "write_file" || call.Name == "run_command" {
				step++
			}
		}
	}
	if side == "" {
		return domain.ModelResponse{}, errors.New("fixture lost user conversation")
	}
	if step == 0 {
		p.mu.Lock()
		p.started++
		if p.started == 2 {
			close(p.ready)
		}
		p.mu.Unlock()
		select {
		case <-p.ready:
		case <-ctx.Done():
			return domain.ModelResponse{}, ctx.Err()
		}
	}
	switch step {
	case 0:
		return lifecycleTool(side+"-write", "write_file", map[string]string{"path": "note.txt", "content": side + "\n"}), nil
	case 1:
		return lifecycleTool(side+"-expected", "write_file", map[string]string{"path": "expected.txt", "content": side + "\n"}), nil
	case 2:
		return lifecycleTool(side+"-test", "run_command", map[string]string{"command": "go test -p 1 ./..."}), nil
	default:
		return lifecycleFinish(side + " tested"), nil
	}
}

func TestM1024ConcurrentWorktreeMilestone(t *testing.T) {
	setDiagnosticEnv(t, "", "")
	root, _ := lifecycleFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	mgr, db, err := openRunManager(ctx, config.Config{Provider: "mock", DBPath: config.DefaultDB}, root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	mgr.SkipPlanner = true
	mgr.Provider = &concurrentWorktreeMock{ready: make(chan struct{})}
	type result struct {
		run  storage.Run
		err  error
		side string
	}
	results := make(chan result, 2)
	for _, side := range []string{"left", "right"} {
		session, err := mgr.CreateSession(ctx)
		if err != nil {
			t.Fatal(err)
		}
		go func(side, sessionID string) {
			run, err := mgr.RunMessage(ctx, sessionID, side)
			for approvals := 0; err == nil && run.Status == domain.RunWaiting && approvals < 4; approvals++ {
				run, err = mgr.Resume(ctx, run.ID, agent.ResumeInput{Decision: "approve"})
			}
			results <- result{run: run, err: err, side: side}
		}(side, session.ID)
	}
	runs := map[string]storage.Run{}
	for range 2 {
		res := <-results
		if res.err != nil || res.run.Status != domain.RunCompleted {
			t.Fatalf("concurrent run %s: %+v %v", res.side, res.run, res.err)
		}
		runs[res.side] = res.run
		for _, name := range []string{"note.txt", "expected.txt"} {
			if string(readReviewTestFile(t, filepath.Join(res.run.WorktreePath, name))) != res.side+"\n" {
				t.Fatalf("%s run interfered with %s", res.side, name)
			}
		}
		batches, err := db.LoadBatches(ctx, res.run.ID)
		if err != nil {
			t.Fatal(err)
		}
		tested := false
		for _, b := range batches {
			for _, inv := range b.Invocations {
				if inv.Name == "run_command" && inv.Result != nil && inv.Result.OK {
					var data struct {
						Exit   int    `json:"exit_code"`
						Stdout string `json:"stdout"`
					}
					if err := json.Unmarshal(inv.Result.Data, &data); err != nil {
						t.Fatal(err)
					}
					if data.Exit != 0 || !strings.Contains(data.Stdout, "ok ") {
						t.Fatalf("isolated test failed: %s", inv.Result.Data)
					}
					tested = true
				}
			}
		}
		if !tested {
			t.Fatal("isolated run never executed tests")
		}
		assertLifecycleSummary(t, db, res.run.ID)
	}
	if runs["left"].WorktreePath == runs["right"].WorktreePath || runs["left"].WorktreeBaseSHA != runs["right"].WorktreeBaseSHA {
		t.Fatal("concurrent runs not isolated at same base")
	}
	if reviewTestGit(t, root, "status", "--porcelain=v1", "--untracked-files=all") != "" || string(readReviewTestFile(t, filepath.Join(root, "note.txt"))) != "before\n" {
		t.Fatal("main workspace dirty before integration")
	}
	if code, _, stderr := callWorktrees(root, "merge", runs["left"].ID); code != 0 {
		t.Fatalf("milestone merge: %s", stderr)
	}
	if code, _, stderr := callWorktrees(root, "discard", runs["right"].ID, "--force"); code != 0 {
		t.Fatalf("milestone discard: %s", stderr)
	}
	if string(readReviewTestFile(t, filepath.Join(root, "note.txt"))) != "left\n" || reviewTestGit(t, root, "status", "--porcelain") != "" {
		t.Fatal("main differs from selected run")
	}
	for _, run := range runs {
		assertLifecycleGone(t, root, run)
	}
	left, _ := db.GetRun(ctx, runs["left"].ID)
	right, _ := db.GetRun(ctx, runs["right"].ID)
	if left.WorktreeState != "merged" || right.WorktreeState != "discarded" {
		t.Fatalf("milestone states: %s %s", left.WorktreeState, right.WorktreeState)
	}
}

func lifecycleTool(id, name string, args any) domain.ModelResponse {
	raw, _ := json.Marshal(args)
	call := domain.ToolCall{ProviderCallID: id, Name: name, Arguments: raw}
	return domain.ModelResponse{Provider: "mock", Complete: true, Items: []domain.ResponseItem{{Kind: domain.ItemToolCall, ToolCall: &call}}}
}
func lifecycleFinish(text string) domain.ModelResponse {
	return lifecycleTool("finish", domain.ToolFinishTurn, map[string]string{"final_answer": text})
}
func assertLifecycleGone(t *testing.T, root string, run storage.Run) {
	t.Helper()
	if _, exists, err := worktree.Lookup(context.Background(), root, run.ID); err != nil || exists {
		t.Fatalf("worktree still exists: %s %v", run.ID, err)
	}
	if _, err := os.Stat(run.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree directory remains: %v", err)
	} // Git branch absence also verifies cleanup after a missing directory.
	if strings.Contains(reviewTestGit(t, root, "branch", "--list", run.WorktreeBranch), run.WorktreeBranch) {
		t.Fatal("worktree branch remains")
	}
}
func assertLifecycleSummary(t *testing.T, db *sqlite.DB, id string) {
	t.Helper()
	events, err := db.ListEvents(context.Background(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, ev := range events {
		if ev.Type == "worktree.summary" {
			count++
			var summary worktree.Summary
			if err := json.Unmarshal(ev.Payload, &summary); err != nil || summary.Error != "" {
				t.Fatalf("bad summary: %+v %v", summary, err)
			}
		}
	}
	if count != 1 {
		t.Fatalf("run %s summaries: %d", id, count)
	}
}

func TestMergeRestrictedAgentCanRepairFiles(t *testing.T) {
	setDiagnosticEnv(t, "", "")
	ctx := context.Background()
	root, db := lifecycleFixture(t)
	run := lifecycleRun(t, root, db, "run_model_repair", domain.RunCompleted)
	writeReviewFile(t, run.WorktreePath, "note.txt", "agent\n")
	writeReviewFile(t, run.WorktreePath, "expected.txt", "agent\n")
	writeReviewFile(t, root, "note.txt", "main\n")
	writeReviewFile(t, root, "expected.txt", "main\n")
	reviewTestGit(t, root, "commit", "-qam", "advance main")
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		lifecycleTool("repair-note", "write_file", map[string]string{"path": "note.txt", "content": "both\n"}),
		lifecycleTool("repair-expected", "write_file", map[string]string{"path": "expected.txt", "content": "both\n"}), lifecycleFinish("resolved"),
	}}
	info := worktree.Info{RunID: run.ID, BaseSHA: run.WorktreeBaseSHA, Branch: run.WorktreeBranch, Path: run.WorktreePath}
	err := worktree.Merge(ctx, root, info, worktree.MergeOptions{Resolve: worktreeResolverForProvider(mock, db), Test: func(ctx context.Context, path string) error {
		return runWorktreeTests(ctx, path, "go test -p 1 ./...", io.Discard)
	}, Record: func(ctx context.Context) error { return db.SetWorktreeState(ctx, run, "merged") }})
	if err != nil {
		t.Fatal(err)
	}
	if string(readReviewTestFile(t, filepath.Join(root, "note.txt"))) != "both\n" {
		t.Fatal("model resolution not integrated")
	}
	if len(mock.Requests) != 3 {
		t.Fatalf("repair requests: %d", len(mock.Requests))
	}
	for _, req := range mock.Requests {
		for _, tool := range req.Tools {
			if tool.Name == "run_command" || tool.Name == domain.ToolDelegateTask || tool.Name == domain.ToolExecuteProgram {
				t.Fatalf("repair exposed forbidden tool %s", tool.Name)
			}
		}
	}
}

func TestServeWorktreeFlagValidatesBeforeStore(t *testing.T) {
	setDiagnosticEnv(t, "", "")
	root := t.TempDir()
	dbPath := filepath.Join(root, "must-not-exist.db")
	var out, stderr bytes.Buffer
	code := Main([]string{"serve", "--worktree", "--workspace", root, "--db", dbPath}, &out, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "requires a Git repository") {
		t.Fatalf("serve --worktree: %d %s", code, stderr.String())
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatal("serve initialized DB before worktree validation")
	}
}

func TestServeWorktreeCLIProcessWithHTTP(t *testing.T) {
	if os.Getenv("CLOUD_HARNESS_M1024_SERVE_HELPER") == "1" {
		for i, arg := range os.Args {
			if arg == "--" {
				os.Exit(Main(os.Args[i+1:], os.Stdout, os.Stderr))
			}
		}
		os.Exit(2)
	}
	setDiagnosticEnv(t, "", "")
	root, db := lifecycleFixture(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestServeWorktreeCLIProcessWithHTTP$", "--", "serve", "--worktree", "--workspace", root, "--provider", "mock", "--addr", addr)
	cmd.Env = append(os.Environ(), "CLOUD_HARNESS_M1024_SERVE_HELPER=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			_ = cmd.Process.Signal(os.Interrupt)
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
		})
	}
	defer stop()
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		ready <- scanner.Scan() && strings.Contains(scanner.Text(), "listening on http://")
	}()
	select {
	case ok := <-ready:
		if !ok {
			stop()
			t.Fatalf("serve failed to start: %s", stderr.String())
		}
	case <-ctx.Done():
		stop()
		t.Fatalf("serve startup timeout: %s", stderr.String())
	}
	client := &http.Client{Timeout: 2 * time.Second}
	base := "http://" + addr
	for {
		resp, err := client.Get(base + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal("serve never became healthy")
		}
	}
	post := func(path, body string) map[string]any {
		t.Helper()
		resp, err := client.Post(base+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("HTTP %s: %d %s", path, resp.StatusCode, data)
		}
		var view map[string]any
		if err := json.Unmarshal(data, &view); err != nil {
			t.Fatal(err)
		}
		return view
	}
	session := post("/v1/sessions", "")["id"].(string)
	view := post("/v1/sessions/"+session+"/runs?wait=1", `{"message":"actual serve worktree"}`)
	run, err := db.GetRun(ctx, view["id"].(string))
	if err != nil || run.WorktreePath == "" || run.Status != domain.RunCompleted {
		t.Fatalf("serve --worktree did not isolate: %+v %v", run, err)
	}
	assertLifecycleSummary(t, db, run.ID)
	if reviewTestGit(t, root, "status", "--porcelain") != "" {
		t.Fatal("actual serve dirtied main")
	}
	if _, err := os.Stat(filepath.Join(run.WorktreePath, config.DefaultDB)); !os.IsNotExist(err) {
		t.Fatal("actual serve opened DB inside worktree")
	}
}
