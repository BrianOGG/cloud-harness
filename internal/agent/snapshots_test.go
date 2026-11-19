package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/provider"
	workspacesnapshot "github.com/cloud-harness/cloud-harness/internal/snapshot"
	"github.com/cloud-harness/cloud-harness/internal/storage"
	"github.com/cloud-harness/cloud-harness/internal/storage/sqlite"
	"github.com/cloud-harness/cloud-harness/internal/tools"
)

func snapshotWrite(id, path, content string) domain.ToolCall {
	args, _ := json.Marshal(map[string]string{"path": path, "content": content})
	return domain.ToolCall{ProviderCallID: id, Name: "write_file", Arguments: args}
}

func allowSnapshotWrite(m *Manager, root string) {
	m.Grants = append(m.Grants, domain.Grant{Tool: "write_file", Op: "call", ResourceScope: root, State: "active", Subject: "test"})
}

func runSnapshotMessage(t *testing.T, m *Manager) storage.Run {
	t.Helper()
	ses, err := m.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessage(context.Background(), ses.ID, "perform the scripted calls")
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func workspaceRecords(t *testing.T, db *sqlite.DB, root string) (*workspacesnapshot.Snapshotter, []storage.WorkspaceSnapshot) {
	t.Helper()
	s, err := workspacesnapshot.Open(root, db)
	if err != nil {
		t.Fatal(err)
	}
	records, err := db.ListWorkspaceSnapshots(context.Background(), s.WorkspaceID())
	if err != nil {
		t.Fatal(err)
	}
	return s, records
}

func snapshotSkips(t *testing.T, db *sqlite.DB, runID string) []storage.Event {
	t.Helper()
	events, err := db.ListEvents(context.Background(), runID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var skips []storage.Event
	for _, ev := range events {
		if ev.Type == "snapshot_skipped" {
			skips = append(skips, ev)
		}
	}
	return skips
}

func assertSnapshotLabels(t *testing.T, records []storage.WorkspaceSnapshot, runID string, suffixes ...string) {
	t.Helper()
	if len(records) != len(suffixes) {
		t.Fatalf("snapshots: %+v, want %v", records, suffixes)
	}
	for i, suffix := range suffixes {
		if records[i].Label != runID+"/"+suffix {
			t.Fatalf("snapshot label %q, want %q", records[i].Label, runID+"/"+suffix)
		}
	}
}

func TestWorkspaceSnapshotsWriteAndRestore(t *testing.T) {
	m, db, root := testManager(t, &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(snapshotWrite("write", "note.txt", "after")), textOnly("done"),
	}})
	allowSnapshotWrite(m, root)
	path := filepath.Join(root, "note.txt")
	if err := os.WriteFile(path, []byte("before"), 0644); err != nil {
		t.Fatal(err)
	}
	run := runSnapshotMessage(t, m)
	if run.Status != domain.RunCompleted {
		t.Fatalf("run: %+v", run)
	}
	s, records := workspaceRecords(t, db, root)
	assertSnapshotLabels(t, records, run.ID, "batch-1/pre", "batch-1/post")
	events, err := db.ListEvents(context.Background(), run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantTypes := []string{"model.completed", "invocation." + string(domain.InvSucceeded)}
	var previous int64
	for i, rec := range records {
		if rec.EventSeq == nil || *rec.EventSeq <= previous {
			t.Fatalf("event sequence not increasing: %+v, previous %d", rec, previous)
		}
		var matched bool
		for _, ev := range events {
			if ev.Seq == *rec.EventSeq && ev.RunID == run.ID && ev.Persistent && ev.Type == wantTypes[i] {
				matched = true
			}
		}
		if !matched {
			t.Fatalf("snapshot %+v does not point to expected event %q: %+v", rec, wantTypes[i], events)
		}
		previous = *rec.EventSeq
	}
	if body, err := os.ReadFile(path); err != nil || string(body) != "after" {
		t.Fatalf("write: %q, %v", body, err)
	}
	if err := s.Restore(context.Background(), records[0]); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(path); err != nil || string(body) != "before" {
		t.Fatalf("restore pre: %q, %v", body, err)
	}
	if skips := snapshotSkips(t, db, run.ID); len(skips) != 0 {
		t.Fatalf("skips: %+v", skips)
	}
	if len(m.snapshotters) != 0 {
		t.Fatal("completed run retained snapshotter")
	}
}

func assertNoWorkspaceSnapshots(t *testing.T, m *Manager, db *sqlite.DB, root, runID string) {
	t.Helper()
	if len(m.snapshotters) != 0 {
		t.Fatal("snapshotter was opened")
	}
	if _, err := os.Stat(filepath.Join(root, ".cloud-harness", "snapshots")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shadow repository was initialized: %v", err)
	}
	_, records := workspaceRecords(t, db, root)
	if len(records) != 0 || len(snapshotSkips(t, db, runID)) != 0 {
		t.Fatalf("expected zero snapshots and diagnostics, records: %+v", records)
	}
}

func TestWorkspaceSnapshotsReadonlyDoesNotOpen(t *testing.T) {
	m, db, root := testManager(t, &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(
			domain.ToolCall{ProviderCallID: "read", Name: "read_file", Arguments: json.RawMessage(`{"path":"note.txt"}`)},
			domain.ToolCall{ProviderCallID: "grep", Name: "grep", Arguments: json.RawMessage(`{"pattern":"hello"}`)},
		), textOnly("done"),
	}})
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "")
	run := runSnapshotMessage(t, m)
	if run.Status != domain.RunCompleted {
		t.Fatalf("run: %+v", run)
	}
	batches, err := db.LoadBatches(context.Background(), run.ID)
	if err != nil || len(batches) != 1 {
		t.Fatalf("readonly batches: %+v, %v", batches, err)
	}
	for _, inv := range batches[0].Invocations {
		if inv.Status != domain.InvSucceeded {
			t.Fatalf("readonly invocation did not execute: %+v", inv)
		}
	}
	assertNoWorkspaceSnapshots(t, m, db, root, run.ID)
}

func TestWorkspaceSnapshotsUnavailableOnceAcrossApprovalResume(t *testing.T) {
	m, db, root := testManager(t, &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(snapshotWrite("a", "a.txt", "a")),
		toolResp(snapshotWrite("b", "b.txt", "b")), textOnly("done"),
	}})
	t.Setenv("PATH", "")
	run := runSnapshotMessage(t, m)
	if run.Status != domain.RunWaiting || run.WaitReason != domain.WaitApproval {
		t.Fatalf("waiting run: %+v", run)
	}
	// A fresh manager must honor the persisted unavailable warning suppression.
	resumer := &Manager{Store: db, Provider: m.Provider, Broker: m.Broker, Workspace: root, OwnerID: "resume"}
	for i := 0; i < 2; i++ {
		var err error
		run, err = resumer.Resume(context.Background(), run.ID, ResumeInput{Decision: "approve"})
		if err != nil {
			t.Fatal(err)
		}
	}
	if run.Status != domain.RunCompleted {
		t.Fatalf("run: %+v", run)
	}
	skips := snapshotSkips(t, db, run.ID)
	if len(skips) != 1 || !strings.Contains(string(skips[0].Payload), `"unavailable":true`) || !strings.Contains(string(skips[0].Payload), "reason") {
		t.Fatalf("unavailable diagnostics: %+v", skips)
	}
	_, records := workspaceRecords(t, db, root)
	if len(records) != 0 {
		t.Fatalf("unavailable snapshots: %+v", records)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorkspaceSnapshotsDisabled(t *testing.T) {
	m, db, root := testManager(t, &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(snapshotWrite("write", "out.txt", "written")), textOnly("done"),
	}})
	m.DisableSnapshots = true
	allowSnapshotWrite(m, root)
	t.Setenv("PATH", "")
	run := runSnapshotMessage(t, m)
	if run.Status != domain.RunCompleted {
		t.Fatalf("run: %+v", run)
	}
	if body, err := os.ReadFile(filepath.Join(root, "out.txt")); err != nil || string(body) != "written" {
		t.Fatalf("write: %q, %v", body, err)
	}
	assertNoWorkspaceSnapshots(t, m, db, root, run.ID)
}

func TestWorkspaceSnapshotsApprovalSegmentsReusePre(t *testing.T) {
	m, db, root := testManager(t, &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(snapshotWrite("a", "a.txt", "a"), snapshotWrite("b", "b.txt", "b"), snapshotWrite("c", "c.txt", "c")),
		textOnly("done"),
	}})
	allowSnapshotWrite(m, filepath.Join(root, "a.txt"))
	run := runSnapshotMessage(t, m)
	if run.Status != domain.RunWaiting {
		t.Fatalf("run: %+v", run)
	}
	_, records := workspaceRecords(t, db, root)
	assertSnapshotLabels(t, records, run.ID, "batch-1/pre", "batch-1/post")
	handle := m.snapshotters[run.ID]
	if handle == nil {
		t.Fatal("waiting run lost snapshotter")
	}
	for i := 0; i < 2; i++ {
		var err error
		run, err = m.Resume(context.Background(), run.ID, ResumeInput{Decision: "approve"})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 && (run.Status != domain.RunWaiting || m.snapshotters[run.ID] != handle) {
			t.Fatalf("resume reopened snapshotter or lost wait: %+v", run)
		}
	}
	if run.Status != domain.RunCompleted {
		t.Fatalf("run: %+v", run)
	}
	_, records = workspaceRecords(t, db, root)
	assertSnapshotLabels(t, records, run.ID, "batch-1/pre", "batch-1/post", "batch-1/post", "batch-1/post")
	if len(m.snapshotters) != 0 {
		t.Fatal("resumed completed run retained snapshotter")
	}
	for i, rec := range records {
		if rec.EventSeq == nil || i > 0 && *rec.EventSeq <= *records[i-1].EventSeq {
			t.Fatalf("event sequences not increasing: %+v", records)
		}
	}
}

func TestWorkspaceSnapshotsResumeNewManagerAndDeny(t *testing.T) {
	for _, decision := range []string{"approve", "deny"} {
		t.Run(decision, func(t *testing.T) {
			m, db, root := testManager(t, &provider.Mock{Responses: []domain.ModelResponse{
				toolResp(snapshotWrite("write", "out.txt", "written")), textOnly("done"),
			}})
			run := runSnapshotMessage(t, m)
			if run.Status != domain.RunWaiting {
				t.Fatalf("run: %+v", run)
			}
			_, records := workspaceRecords(t, db, root)
			assertSnapshotLabels(t, records, run.ID, "batch-1/pre")
			preID := records[0].ID
			resumer := &Manager{Store: db, Provider: m.Provider, Broker: m.Broker, Workspace: root, OwnerID: "resumer"}
			run, err := resumer.Resume(context.Background(), run.ID, ResumeInput{Decision: decision})
			if err != nil || run.Status != domain.RunCompleted {
				t.Fatalf("resume: %+v, %v", run, err)
			}
			_, records = workspaceRecords(t, db, root)
			want := []string{"batch-1/pre"}
			if decision == "approve" {
				want = append(want, "batch-1/post")
			}
			assertSnapshotLabels(t, records, run.ID, want...)
			if records[0].ID != preID {
				t.Fatal("resume replaced pre snapshot")
			}
		})
	}
}

func TestWorkspaceSnapshotsPostOnlyForActualDispatch(t *testing.T) {
	for _, mode := range []string{"failed", "invalid", "budget", "cached"} {
		t.Run(mode, func(t *testing.T) {
			call := snapshotWrite("write", "out.txt", "written")
			if mode == "invalid" {
				call.Arguments = json.RawMessage(`{"path":"out.txt"}`)
			}
			if mode == "cached" {
				call.Arguments = json.RawMessage(`{"path":"out.txt","content":"written","idempotency_key":"same"}`)
			}
			responses := []domain.ModelResponse{toolResp(call), textOnly("done")}
			if mode == "cached" {
				second := call
				second.ProviderCallID = "cached"
				responses = []domain.ModelResponse{toolResp(call), toolResp(second), textOnly("done")}
			}
			m, db, root := testManager(t, &provider.Mock{Responses: responses})
			allowSnapshotWrite(m, root)
			if mode == "failed" {
				// Writing a directory passes authorization but fails in Execute.
				if err := os.Mkdir(filepath.Join(root, "out.txt"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "budget" {
				m.UseBudget = true
				m.InitialBudget = domain.DefaultBudget()
				m.InitialBudget.MaxToolCalls = 0
			}
			run := runSnapshotMessage(t, m)
			if run.Status != domain.RunCompleted {
				t.Fatalf("run: %+v", run)
			}
			_, records := workspaceRecords(t, db, root)
			want := []string{"batch-1/pre"}
			if mode == "failed" || mode == "cached" {
				want = append(want, "batch-1/post")
			}
			if mode == "cached" {
				want = append(want, "batch-2/pre")
			}
			assertSnapshotLabels(t, records, run.ID, want...)
		})
	}
}

func TestWorkspaceSnapshotsShadowErrorDoesNotFailRun(t *testing.T) {
	m, db, root := testManager(t, &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(snapshotWrite("a", "a.txt", "a")), toolResp(snapshotWrite("b", "b.txt", "b")), textOnly("done"),
	}})
	allowSnapshotWrite(m, root)
	if err := os.WriteFile(filepath.Join(root, ".cloud-harness"), []byte("blocked"), 0644); err != nil {
		t.Fatal(err)
	}
	run := runSnapshotMessage(t, m)
	if run.Status != domain.RunCompleted || run.Budget.UsedToolCalls != 2 {
		t.Fatalf("run: %+v", run)
	}
	skips := snapshotSkips(t, db, run.ID)
	if len(skips) != 4 {
		t.Fatalf("want a diagnostic for each ordinary pre/post failure: %+v", skips)
	}
	for _, ev := range skips {
		if !strings.Contains(string(ev.Payload), `"unavailable":false`) {
			t.Fatalf("ordinary error treated as unavailable: %s", ev.Payload)
		}
	}
}

// This wrapper observes record creation and can independently fail the event
// lookup or one snapshot write without affecting run event persistence.
type snapshotTestStore struct {
	*sqlite.DB
	recordAttempts int
	sequenceError  bool
	failFirst      bool
}

func (s *snapshotTestStore) ListEvents(ctx context.Context, runID string, after int64) ([]storage.Event, error) {
	if s.sequenceError {
		return nil, errors.New("event read unavailable")
	}
	return s.DB.ListEvents(ctx, runID, after)
}

func (s *snapshotTestStore) CreateWorkspaceSnapshot(ctx context.Context, rec storage.WorkspaceSnapshot) (storage.WorkspaceSnapshot, error) {
	s.recordAttempts++
	if s.failFirst && s.recordAttempts == 1 {
		return storage.WorkspaceSnapshot{}, errors.New("temporary snapshot record failure")
	}
	return s.DB.CreateWorkspaceSnapshot(ctx, rec)
}

func TestWorkspaceSnapshotsSequenceUnavailableStaysNull(t *testing.T) {
	m, db, root := testManager(t, &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(snapshotWrite("write", "out.txt", "written")), textOnly("done"),
	}})
	allowSnapshotWrite(m, root)
	m.Store = &snapshotTestStore{DB: db, sequenceError: true}
	run := runSnapshotMessage(t, m)
	if run.Status != domain.RunCompleted {
		t.Fatalf("run: %+v", run)
	}
	_, records := workspaceRecords(t, db, root)
	assertSnapshotLabels(t, records, run.ID, "batch-1/pre", "batch-1/post")
	for _, rec := range records {
		if rec.EventSeq != nil {
			t.Fatalf("fabricated event sequence: %+v", rec)
		}
	}
}

func TestWorkspaceSnapshotsTransientFailureRetriesNextCapture(t *testing.T) {
	m, db, root := testManager(t, &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(snapshotWrite("write", "out.txt", "written")), textOnly("done"),
	}})
	allowSnapshotWrite(m, root)
	store := &snapshotTestStore{DB: db, failFirst: true}
	m.Store = store
	run := runSnapshotMessage(t, m)
	if run.Status != domain.RunCompleted || store.recordAttempts != 2 {
		t.Fatalf("run: %+v, attempts: %d", run, store.recordAttempts)
	}
	_, records := workspaceRecords(t, db, root)
	assertSnapshotLabels(t, records, run.ID, "batch-1/post")
	if skips := snapshotSkips(t, db, run.ID); len(skips) != 1 {
		t.Fatalf("skips: %+v", skips)
	}
}

func TestWorkspaceSnapshotsUseBrokerSideEffectFacts(t *testing.T) {
	m, db, root := testManager(t, &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "test", Name: "go_test", Arguments: json.RawMessage(`{}`)}), textOnly("done"),
	}})
	// A readonly annotation alone cannot suppress a non-none side effect class.
	m.Broker.Register(tools.Spec{
		Name: "go_test", Readonly: true, SideEffectClass: "reversible",
		Execute: func(context.Context, json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"ok":true}`), nil
		},
	})
	m.Grants = append(m.Grants, domain.Grant{Tool: "go_test", Op: "call", State: "active"})
	run := runSnapshotMessage(t, m)
	if run.Status != domain.RunCompleted {
		t.Fatalf("run: %+v", run)
	}
	_, records := workspaceRecords(t, db, root)
	assertSnapshotLabels(t, records, run.ID, "batch-1/pre", "batch-1/post")
}

func TestWorkspaceSnapshotsScriptWrites(t *testing.T) {
	m, db, root := testManager(t, &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "script", Name: domain.ToolExecuteProgram, Arguments: json.RawMessage(`{"script":"def main():\n  return broker.invoke(\"write_file\", {\"path\": \"script.txt\", \"content\": \"written\"})\n","allow":["write_file"],"confirm_side_effects":["write_file"]}`)}),
		textOnly("done"),
	}})
	allowSnapshotWrite(m, root)
	run := runSnapshotMessage(t, m)
	if run.Status != domain.RunCompleted {
		t.Fatalf("run: %+v", run)
	}
	if body, err := os.ReadFile(filepath.Join(root, "script.txt")); err != nil || string(body) != "written" {
		t.Fatalf("script write: %q, %v", body, err)
	}
	_, records := workspaceRecords(t, db, root)
	assertSnapshotLabels(t, records, run.ID, "batch-1/pre", "batch-1/post")
}

func TestWorkspaceSnapshotsConcurrentRuns(t *testing.T) {
	for _, sharedManager := range []bool{false, true} {
		t.Run(fmt.Sprintf("shared-manager-%t", sharedManager), func(t *testing.T) {
			m, db, root := testManager(t, &provider.Mock{})
			const rounds = 3
			var calls atomic.Int64
			m.Broker.Register(tools.Spec{
				Name: "write_file", SideEffectClass: "reversible", Required: []string{"path", "content"},
				ExtractResources: func(json.RawMessage) ([]string, error) { return []string{root}, nil },
				Execute: func(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
					// Fresh file names keep concurrently written paths stable for git add.
					name := fmt.Sprintf("write-%d.txt", calls.Add(1))
					if err := os.WriteFile(filepath.Join(root, name), []byte("written"), 0644); err != nil {
						return nil, err
					}
					return json.RawMessage(`{}`), nil
				},
			})
			allowSnapshotWrite(m, root)
			var responses []domain.ModelResponse
			for i := 0; i < 2*rounds; i++ {
				responses = append(responses, toolResp(snapshotWrite(fmt.Sprintf("w%d", i), "out.txt", "written")))
			}
			// With a shared manager either run may consume the next response;
			// both only answer after all scripted write batches were consumed.
			m.Provider = &provider.Mock{Responses: responses}
			other := m
			if !sharedManager {
				m.Provider = &provider.Mock{Responses: append(append([]domain.ModelResponse{}, responses[:rounds]...), textOnly("done"))}
				other = &Manager{
					Store: db, Broker: m.Broker, Workspace: root, OwnerID: "other",
					Provider: &provider.Mock{Responses: append(append([]domain.ModelResponse{}, responses[rounds:]...), textOnly("done"))},
				}
				allowSnapshotWrite(other, root)
			}
			type result struct {
				run storage.Run
				err error
			}
			start := make(chan struct{})
			results := make(chan result, 2)
			for _, mgr := range []*Manager{m, other} {
				ses, err := mgr.CreateSession(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				go func() {
					<-start
					run, err := mgr.RunMessage(context.Background(), ses.ID, "write concurrently")
					results <- result{run: run, err: err}
				}()
			}
			close(start)
			var runs []storage.Run
			for i := 0; i < 2; i++ {
				got := <-results
				if got.err != nil || got.run.Status != domain.RunCompleted {
					t.Fatalf("concurrent run: %+v, %v", got.run, got.err)
				}
				runs = append(runs, got.run)
			}
			s, records := workspaceRecords(t, db, root)
			if calls.Load() != 2*rounds || len(records) != 4*rounds {
				t.Fatalf("writes: %d, snapshots: %+v", calls.Load(), records)
			}
			for _, run := range runs {
				if skips := snapshotSkips(t, db, run.ID); len(skips) != 0 {
					t.Fatalf("concurrent snapshot failures: %+v", skips)
				}
			}
			cmd := exec.Command("git", "--git-dir="+filepath.Join(root, ".cloud-harness", "snapshots", s.WorkspaceID()), "fsck", "--full")
			cmd.Dir = root
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("shadow git fsck: %v\n%s", err, out)
			}
		})
	}
}

func TestWorkspaceSnapshotsPostAfterCancellation(t *testing.T) {
	m, db, root := testManager(t, &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "write", Name: "write_file", Arguments: json.RawMessage(`{"path":"out.txt","content":"written"}`)}),
	}})
	started := make(chan struct{})
	m.Broker.Register(tools.Spec{
		Name: "write_file", SideEffectClass: "reversible",
		ExtractResources: func(json.RawMessage) ([]string, error) { return []string{root}, nil },
		Execute: func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
			if err := os.WriteFile(filepath.Join(root, "out.txt"), []byte("written"), 0644); err != nil {
				return nil, err
			}
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	allowSnapshotWrite(m, root)
	ses, err := m.CreateSession(context.Background())
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
		run, err := m.RunMessage(ctx, ses.ID, "write then cancel")
		done <- result{run: run, err: err}
	}()
	select {
	case <-started:
		cancel()
	case <-ctx.Done():
		t.Fatal("write was not dispatched")
	}
	select {
	case got := <-done:
		if got.err != nil || got.run.Status != domain.RunCancelled {
			t.Fatalf("cancelled run: %+v, %v", got.run, got.err)
		}
		_, records := workspaceRecords(t, db, root)
		assertSnapshotLabels(t, records, got.run.ID, "batch-1/pre", "batch-1/post")
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled run did not finish")
	}
}

func TestRunCommandApprovalWaitsThenSnapshotsActualExecution(t *testing.T) {
	m, db, _ := testManager(t, &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "cmd", Name: "run_command", Arguments: json.RawMessage(`{"command":"echo snapshot-command"}`)}),
		textOnly("done"),
	}})
	run := runSnapshotMessage(t, m)
	if run.Status != domain.RunWaiting || run.WaitReason != domain.WaitApproval {
		t.Fatalf("run: %+v", run)
	}
	_, records := workspaceRecords(t, db, m.Workspace)
	if len(records) != 1 || !strings.HasSuffix(records[0].Label, "/batch-1/pre") {
		t.Fatalf("dangerous command must capture pre before approval: %+v", records)
	}
	run, err := m.Resume(context.Background(), run.ID, ResumeInput{Decision: "approve"})
	if err != nil || run.Status != domain.RunCompleted {
		t.Fatalf("resume: %+v, %v", run, err)
	}
	_, records = workspaceRecords(t, db, m.Workspace)
	if len(records) != 2 || !strings.HasSuffix(records[1].Label, "/batch-1/post") {
		t.Fatalf("approved command did not capture post: %+v", records)
	}
}
