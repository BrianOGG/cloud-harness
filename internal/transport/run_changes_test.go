package transport

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/agent"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/snapshot"
	"github.com/BrianOGG/cloud-harness/internal/storage"
	"github.com/BrianOGG/cloud-harness/internal/storage/sqlite"
	"github.com/BrianOGG/cloud-harness/internal/tools"
	"github.com/BrianOGG/cloud-harness/internal/worktree"
)

type changesFixture struct {
	t             *testing.T
	h             http.Handler
	m             *agent.Manager
	db            *sqlite.DB
	run           storage.Run
	path          string
	snap          *snapshot.Snapshotter
	before, after storage.WorkspaceSnapshot
}

func changesGit(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func setupChanges(t *testing.T, isolated bool) changesFixture {
	t.Helper()
	m, h := newServer(t)
	m.Workspace = t.TempDir()
	session, err := m.CreateSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	turn := storage.Turn{ID: "turn_review", SessionID: session.ID, UserInput: "edit", Status: "completed", CreatedAt: now}
	if err := m.Store.CreateTurn(t.Context(), turn); err != nil {
		t.Fatal(err)
	}
	run := storage.Run{ID: "run_review", SessionID: session.ID, TurnID: turn.ID, Status: domain.RunCompleted, Version: 1, CreatedAt: now, UpdatedAt: now}
	path := m.Workspace
	if isolated {
		changesGit(t, path, "init", "-q", "--template=")
		changesGit(t, path, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "base")
		info, err := worktree.Create(t.Context(), path, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		run.WorktreeBaseSHA, run.WorktreePath, run.WorktreeBranch = info.BaseSHA, info.Path, info.Branch
		path = info.Path
	}
	db := m.Store.(*sqlite.DB)
	snap, err := snapshot.Open(path, db)
	if err != nil {
		t.Fatal(err)
	}
	run.WorkspaceID = snap.WorkspaceID()
	if err := db.CreateRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	f := changesFixture{t: t, h: h, m: m, db: db, run: run, path: path, snap: snap}
	f.write("review [a].txt", "before\n")
	f.write("removed", "gone\n")
	for _, name := range []string{"review [a].txt", "removed"} {
		if err := os.Chmod(filepath.Join(path, name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	f.before = f.capture("pre")
	f.write("review [a].txt", "after\n")
	f.write("new", "created\n")
	if err := os.Remove(filepath.Join(path, "removed")); err != nil {
		t.Fatal(err)
	}
	f.after = f.capture("post")
	return f
}

func (f changesFixture) write(path, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.path, path), []byte(content), 0644); err != nil {
		f.t.Fatal(err)
	}
}
func (f changesFixture) read(path string) string {
	f.t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.path, path))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(raw)
}
func (f changesFixture) capture(phase string) storage.WorkspaceSnapshot {
	f.t.Helper()
	s, err := f.snap.Snapshot(f.t.Context(), f.run.ID+"/batch-1/exec_test/"+phase)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}
func (f changesFixture) base() string { return "/v1/runs/" + f.run.ID + "/changes" }
func (f changesFixture) restore(path string, confirm bool, status int) string {
	f.t.Helper()
	body := map[string]any{"before": f.before.ID, "after": f.after.ID, "path": path, "confirm": confirm}
	return modelConfigRequest(f.t, f.h, "POST", f.base()+"/restore", body, status, "").Body.String()
}
func (f changesFixture) diff(path string, status int) string {
	f.t.Helper()
	query := url.Values{"before": {f.before.ID}, "after": {f.after.ID}, "path": {path}}
	return modelConfigRequest(f.t, f.h, "GET", f.base()+"/diff?"+query.Encode(), nil, status, "").Body.String()
}

func TestRunChangesRestorePrivatePermissions(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode os.FileMode
	}{
		{"review [a].txt", 0600},
		{"review [a].txt", 0400}, // Tightened by the owner after the batch.
		{"removed", 0600},
	} {
		f := setupChanges(t, false)
		path := filepath.Join(f.path, tc.name)
		if tc.name != "removed" {
			if err := os.Chmod(path, tc.mode); err != nil {
				t.Fatal(err)
			}
		}
		f.restore(tc.name, true, 200)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != tc.mode {
			t.Errorf("%s restored mode %04o, want %04o", tc.name, got, tc.mode)
		}
	}
}

func TestRunChangesHistoryAndSafeRestore(t *testing.T) {
	f := setupChanges(t, false)
	f.write("later", "independent edit")
	f.write("review [a].txt", "human edit\n")
	raw := modelConfigRequest(t, f.h, "GET", f.base(), nil, 200, "").Body
	var list review
	if err := json.Unmarshal(raw.Bytes(), &list); err != nil || !list.Terminal || len(list.Batches) != 1 || len(list.Batches[0].Files) != 3 || strings.Contains(raw.String(), "later") {
		t.Fatalf("list %s: %v", raw, err)
	}
	patch := f.diff("review [a].txt", 200)
	if !strings.Contains(patch, "-before") || !strings.Contains(patch, "+after") || strings.Contains(patch, "human edit") {
		t.Fatalf("patch: %s", patch)
	}
	f.restore("review [a].txt", false, 400)
	f.restore("review [a].txt", true, 409)
	if f.read("review [a].txt") != "human edit\n" {
		t.Fatal("clobbered dirty file")
	}
	f.write("review [a].txt", "after\n")
	if body := f.restore("review [a].txt", true, 200); !strings.Contains(body, `"safety_snapshot":"snap_`) {
		t.Fatal(body)
	}
	if f.read("review [a].txt") != "before\n" || f.read("later") != "independent edit" {
		t.Fatal("wrong restoration")
	}
	f.restore("review [a].txt", true, 409)
	f.restore("new", true, 200)
	if _, err := os.Stat(filepath.Join(f.path, "new")); !os.IsNotExist(err) {
		t.Fatalf("added file still present: %v", err)
	}
	f.restore("removed", true, 200)
	if f.read("removed") != "gone\n" {
		t.Fatal("removed file not restored")
	}
	run, err := f.db.GetRun(t.Context(), f.run.ID)
	if err != nil || run.Version != f.run.Version || run.Status != domain.RunCompleted {
		t.Fatalf("run mutated: %+v %v", run, err)
	}
}

func TestRunChangesRefusesForeignInputsAndStalePair(t *testing.T) {
	f := setupChanges(t, false)
	for _, path := range []string{"../review [a].txt", ".git/config", "later", "*"} {
		f.diff(path, 404)
		f.restore(path, true, 404)
	}
	foreign := f.run
	foreign.ID = "run_other"
	if err := f.db.CreateRun(t.Context(), foreign); err != nil {
		t.Fatal(err)
	}
	other := f
	other.run = foreign
	other.diff("new", 409)
	other.restore("new", true, 409)
	missing := f
	missing.run.ID = "run_missing"
	missing.diff("new", 404)
	f.capture("post")
	f.restore("new", true, 409)
	root := f.m.Workspace
	f.m.Workspace = t.TempDir()
	modelConfigRequest(t, f.h, "GET", f.base(), nil, 409, "")
	f.restore("new", true, 409)
	if _, err := os.Stat(filepath.Join(f.m.Workspace, ".cloud-harness")); !os.IsNotExist(err) {
		t.Fatal("opened a shadow in wrong workspace")
	}
	f.m.Workspace = root
	if f.read("new") != "created\n" {
		t.Fatal("wrong run/path altered file")
	}
}

func TestRunChangesRefusesLegacyApprovalWindow(t *testing.T) {
	f := setupChanges(t, false)
	oldPair := f
	var err error
	oldPair.before, err = f.snap.Snapshot(t.Context(), f.run.ID+"/batch-2/pre")
	if err != nil {
		t.Fatal(err)
	}
	f.write("human-only", "added while waiting")
	f.write("approved-write", "tool result")
	oldPair.after, err = f.snap.Snapshot(t.Context(), f.run.ID+"/batch-2/post")
	if err != nil {
		t.Fatal(err)
	}
	result := modelConfigRequest(t, f.h, "GET", f.base(), nil, 200, "")
	var list review
	if err := json.Unmarshal(result.Body.Bytes(), &list); err != nil || !list.Incomplete || len(list.Batches) != 1 || list.Batches[0].Before != f.before.ID {
		t.Fatalf("unexpected review: %s", result.Body)
	}
	oldPair.diff("human-only", 409)
	oldPair.restore("human-only", true, 409)
	if f.read("human-only") != "added while waiting" {
		t.Fatal("legacy review altered the human edit")
	}
}

func TestRunChangesRefusesActiveWorkspaceAndLease(t *testing.T) {
	f := setupChanges(t, false)
	now := time.Now().UTC()
	if _, err := f.db.AcquireLease(t.Context(), f.run.ID, "cli", now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	f.restore("new", true, 409)
	if err := f.db.ReleaseLease(t.Context(), f.run.ID, "cli"); err != nil {
		t.Fatal(err)
	}
	other := f.run
	other.ID, other.Status = "run_busy", domain.RunRunning
	if err := f.db.CreateRun(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	f.restore("new", true, 409)
	if f.read("new") != "created\n" {
		t.Fatal("active workspace lost file")
	}
}

func TestRunChangesRetainedWorktree(t *testing.T) {
	f := setupChanges(t, true)
	if err := os.WriteFile(filepath.Join(f.m.Workspace, "new"), []byte("parent"), 0644); err != nil {
		t.Fatal(err)
	}
	f.diff("new", 200)
	changesGit(t, f.path, "add", "new")
	f.restore("new", true, 409)
	changesGit(t, f.path, "reset", "-q", "--", "new")
	f.restore("new", true, 200)
	if data, err := os.ReadFile(filepath.Join(f.m.Workspace, "new")); err != nil || string(data) != "parent" {
		t.Fatal("changed parent checkout")
	}
	changesGit(t, f.path, "switch", "-q", "--detach")
	f.diff("new", 409)
	changesGit(t, f.path, "switch", "-q", f.run.WorktreeBranch)
	info := worktree.Info{RunID: f.run.ID, Branch: f.run.WorktreeBranch, Path: f.path, BaseSHA: f.run.WorktreeBaseSHA}
	if err := worktree.Remove(t.Context(), f.m.Workspace, info); err != nil {
		t.Fatal(err)
	}
	f.restore("removed", true, 409)
	if _, err := os.Stat(f.path); !os.IsNotExist(err) {
		t.Fatalf("recreated missing worktree: %v", err)
	}
	if err := f.db.SetWorktreeState(t.Context(), f.run, "discarded"); err != nil {
		t.Fatal(err)
	}
	modelConfigRequest(t, f.h, "GET", f.base(), nil, 409, "")
}

func TestRunChangesDelegatedWorktree(t *testing.T) {
	parent := setupChanges(t, true)
	child := parent
	child.run.ID = "run_child"
	child.run.ParentRunID = parent.run.ID
	child.run.WorktreePath, child.run.WorktreeBaseSHA, child.run.WorktreeBranch = "", "", ""
	if err := child.db.CreateRun(t.Context(), child.run); err != nil {
		t.Fatal(err)
	}
	child.before = child.capture("pre")
	child.write("child.txt", "child\n")
	child.after = child.capture("post")
	child.diff("child.txt", 200)
	child.restore("child.txt", true, 200)
	if _, err := os.Stat(filepath.Join(parent.path, "child.txt")); !os.IsNotExist(err) {
		t.Fatal("child file was not restored in the retained worktree")
	}
	if err := parent.db.SetWorktreeState(t.Context(), parent.run, "merged"); err != nil {
		t.Fatal(err)
	}
	child.restore("child.txt", true, 409)
}

func (f changesFixture) interruptedRun(status domain.RunStatus, start, end time.Time, phase string) string {
	f.t.Helper()
	foreign := f.run
	foreign.ID, foreign.Status = "run_interrupted", status
	foreign.CreatedAt, foreign.UpdatedAt = start, end
	if err := f.db.CreateRun(f.t.Context(), foreign); err != nil {
		f.t.Fatal(err)
	}
	marker := f.before
	marker.ID, marker.RunID = "", foreign.ID
	marker.Label = foreign.ID + "/batch-1/exec_interrupted/" + phase
	marker.CreatedAt = start
	if phase == "post" {
		marker.CreatedAt = end
	}
	if _, err := f.db.CreateWorkspaceSnapshot(f.t.Context(), marker); err != nil {
		f.t.Fatal(err)
	}
	return foreign.ID
}

func TestRunChangesIncompleteForeignDispatch(t *testing.T) {
	const (
		earlier = iota
		atStart
		during
		atEnd
		later
	)
	for _, tc := range []struct {
		name, marker string
		status       domain.RunStatus
		start, end   int
		visible      bool
	}{
		{"failed before review", "pre", domain.RunFailed, earlier, earlier, true},
		{"running with old update", "pre", domain.RunRunning, earlier, earlier, false},
		{"terminal during review", "pre", domain.RunFailed, earlier, during, false},
		{"terminal exactly at review start", "pre", domain.RunCompleted, earlier, atStart, false},
		{"missing pre started later", "post", domain.RunCompleted, later, later, true},
		{"missing pre started at review end", "post", domain.RunCompleted, atEnd, later, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupChanges(t, false)
			before, after := f.before.CreatedAt, f.after.CreatedAt
			times := [...]time.Time{before.Add(-time.Minute), before, before.Add(after.Sub(before) / 2), after, after.Add(time.Minute)}
			f.interruptedRun(tc.status, times[tc.start], times[tc.end], tc.marker)
			raw := modelConfigRequest(t, f.h, "GET", f.base(), nil, 200, "").Body
			var list review
			if err := json.Unmarshal(raw.Bytes(), &list); err != nil || list.Incomplete == tc.visible || (len(list.Batches) == 1) != tc.visible {
				t.Fatalf("unexpected review: %s", raw)
			}
			if tc.visible {
				f.diff("new", 200)
				f.restore("new", true, 200)
			} else {
				f.diff("new", 409)
				f.restore("new", true, 409)
			}
		})
	}
}

func TestRunChangesCancelledForeignDispatch(t *testing.T) {
	f := setupChanges(t, false)
	earlier := f.before.CreatedAt.Add(-time.Minute)
	id := f.interruptedRun(domain.RunCancelled, earlier, earlier, "pre")
	f.diff("new", 200)
	// Even an expired lease may have covered this exact review interval.
	for _, expires := range []time.Time{f.after.CreatedAt, time.Now().Add(time.Minute)} {
		if _, err := f.db.AcquireLease(t.Context(), id, "worker", earlier, expires); err != nil {
			t.Fatal(err)
		}
		f.diff("new", 409)
	}
	inv := domain.Invocation{ID: "inv_interrupted", Status: domain.InvRunning, Dispatched: true}
	if err := f.db.SaveBatch(t.Context(), storage.StoredBatch{ID: "batch_foreign", RunID: id, Invocations: []domain.Invocation{inv}}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.ReleaseLease(t.Context(), id, "worker"); err != nil {
		t.Fatal(err)
	}
	f.diff("new", 409)
	// Its eventual completion overlapped the reviewed write even though
	// cancellation and lease release are both historical by review time.
	inv.Status = domain.InvSucceeded
	if err := f.db.CommitInvocation(t.Context(), id, inv, 1, storage.Event{Type: "invocation.SUCCEEDED", Persistent: true, CreatedAt: f.after.CreatedAt}); err != nil {
		t.Fatal(err)
	}
	f.diff("new", 409)
	f.restore("new", true, 409)
}

func TestRunChangesFromToolDispatch(t *testing.T) {
	m, h := newServer(t)
	db := m.Store.(*sqlite.DB)
	m.Workspace, m.SkipPlanner = t.TempDir(), true
	m.Broker = tools.NewBroker(db, db)
	if err := tools.RegisterLocal(m.Broker, m.Workspace); err != nil {
		t.Fatal(err)
	}
	m.Grants = []domain.Grant{{Tool: "write_file", Op: "call", ResourceScope: m.Workspace, State: "active", Subject: "test"}}
	m.Provider = &provider.Mock{Responses: []domain.ModelResponse{
		{Provider: "mock", Complete: true, Items: []domain.ResponseItem{{Kind: domain.ItemToolCall, ToolCall: &domain.ToolCall{ProviderCallID: "first-write", Name: "write_file", Arguments: json.RawMessage(`{"path":"earlier.txt","content":"earlier"}`)}}}},
		{Provider: "mock", Complete: true, Items: []domain.ResponseItem{{Kind: domain.ItemText, Text: "done"}}},
		{Provider: "mock", Complete: true, Items: []domain.ResponseItem{{Kind: domain.ItemToolCall, ToolCall: &domain.ToolCall{ProviderCallID: "write", Name: "write_file", Arguments: json.RawMessage(`{"path":"edit.txt","content":"captured"}`)}}}},
		{Provider: "mock", Complete: true, Items: []domain.ResponseItem{{Kind: domain.ItemText, Text: "done"}}},
	}}
	session, err := m.CreateSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Reproduce the persisted history after losing the first run's closing
	// snapshots. The next real write must still be reviewable and restorable.
	first, err := m.RunMessage(t.Context(), session.ID, "write the first file")
	if err != nil || first.Status != domain.RunCompleted {
		t.Fatalf("first dispatch: %+v, %v", first, err)
	}
	records, err := db.ListWorkspaceSnapshots(t.Context(), first.WorkspaceID)
	if err != nil || len(records) != 4 {
		t.Fatalf("first dispatch snapshots: %+v, %v", records, err)
	}
	for _, rec := range records {
		if strings.HasSuffix(rec.Label, "/post") {
			if err := db.DeleteWorkspaceSnapshot(t.Context(), rec.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	run, err := m.RunMessage(t.Context(), session.ID, "write the file")
	if err != nil || run.Status != domain.RunCompleted {
		t.Fatalf("dispatch: %+v, %v", run, err)
	}
	f := changesFixture{run: run, t: t, h: h}
	result := modelConfigRequest(t, h, "GET", f.base(), nil, 200, "")
	var list review
	if err := json.Unmarshal(result.Body.Bytes(), &list); err != nil || len(list.Batches) != 1 || len(list.Batches[0].Files) != 1 {
		t.Fatalf("unexpected captures: %s", result.Body)
	}
	b := list.Batches[0]
	if b.Files[0] != (reviewedFile{Path: "edit.txt", Status: "A"}) {
		t.Fatalf("changed files: %+v", b.Files)
	}
	f.before.ID, f.after.ID = b.Before, b.After
	if patch := f.diff("edit.txt", 200); !strings.Contains(patch, "+captured") {
		t.Fatalf("missing second run's write: %s", patch)
	}
	f.restore("edit.txt", true, 200)
	if _, err := os.Stat(filepath.Join(m.Workspace, "edit.txt")); !os.IsNotExist(err) {
		t.Fatal("restore did not remove tool-created file")
	}
	if contents, err := os.ReadFile(filepath.Join(m.Workspace, "earlier.txt")); err != nil || string(contents) != "earlier" {
		t.Fatalf("first run's write after restore: %q, %v", contents, err)
	}
}
