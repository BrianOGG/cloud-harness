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
	s, err := f.snap.Snapshot(f.t.Context(), f.run.ID+"/batch-1/"+phase)
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
		{Provider: "mock", Complete: true, Items: []domain.ResponseItem{{Kind: domain.ItemToolCall, ToolCall: &domain.ToolCall{ProviderCallID: "write", Name: "write_file", Arguments: json.RawMessage(`{"path":"edit.txt","content":"captured"}`)}}}},
		{Provider: "mock", Complete: true, Items: []domain.ResponseItem{{Kind: domain.ItemText, Text: "done"}}},
	}}
	session, err := m.CreateSession(t.Context())
	if err != nil {
		t.Fatal(err)
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
	f.restore("edit.txt", true, 200)
	if _, err := os.Stat(filepath.Join(m.Workspace, "edit.txt")); !os.IsNotExist(err) {
		t.Fatal("restore did not remove tool-created file")
	}
}
