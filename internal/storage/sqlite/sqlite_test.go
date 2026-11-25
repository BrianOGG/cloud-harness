package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestMigrationWALAndForeignKeys(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "h.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	ver, err := db.SchemaVersion(ctx)
	if err != nil || ver != 9 {
		t.Fatalf("version %d %v", ver, err)
	}
	mode, err := db.JournalMode(ctx)
	if err != nil || !strings.EqualFold(mode, "wal") {
		t.Fatalf("journal %q %v", mode, err)
	}
	fk, err := db.ForeignKeys(ctx)
	if err != nil || !fk {
		t.Fatalf("fk %v %v", fk, err)
	}
	if _, err := db.sql.ExecContext(ctx, `INSERT INTO turns(id, session_id, user_input, status, created_at) VALUES('t', 'missing', 'x', 'open', '2026-01-01T00:00:00.000000000Z')`); err == nil {
		t.Fatal("foreign key did not reject a turn without a session")
	}
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	if err := db.CreateSession(ctx, storage.Session{ID: "ses_1", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ver, err = db.SchemaVersion(ctx)
	if err != nil || ver != 9 {
		t.Fatalf("reopen version %d %v", ver, err)
	}
	got, err := db.GetSession(ctx, "ses_1")
	if err != nil || got.ID != "ses_1" {
		t.Fatalf("session after reopen %+v %v", got, err)
	}
}

func TestHistoryChinesePathCodeAndTransaction(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Date(2026, 11, 10, 1, 0, 0, 0, time.UTC)
	docs := []storage.HistoryDoc{
		{SourceID: "doc_cn", SessionID: "ses", RawText: "云端执行框架负责检索历史记录", CreatedAt: now},
		{SourceID: "doc_path", SessionID: "ses", RawText: "log file /var/log/app/server.go failed", CreatedAt: now},
		{SourceID: "doc_words", SessionID: "ses", RawText: "the server will go now", CreatedAt: now},
		{SourceID: "doc_code", SessionID: "ses", RawText: "call parseToolCallID after the failure", CreatedAt: now},
		{SourceID: "doc_id", SessionID: "ses", RawText: "failure event evt_9f3a2c recorded", CreatedAt: now},
		{SourceID: "doc_num", SessionID: "ses", RawText: "ticket 10086 opened", CreatedAt: now},
		{SourceID: "doc_other", SessionID: "other", RawText: "云端执行框架负责检索历史记录", CreatedAt: now},
	}
	for _, doc := range docs {
		if err := db.IndexHistory(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	must := func(query, id, kind string) {
		t.Helper()
		hits, err := db.SearchHistory(ctx, "ses", query, 10)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		if len(hits) != 1 || hits[0].SourceID != id || hits[0].MatchKind != kind {
			t.Fatalf("query %q hits %#v, want %s/%s", query, hits, id, kind)
		}
	}
	mustNot := func(query string) {
		t.Helper()
		hits, err := db.SearchHistory(ctx, "ses", query, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 0 {
			t.Fatalf("query %q unexpected %#v", query, hits)
		}
	}
	must("检索", "doc_cn", "fts")
	must("历史", "doc_cn", "fts")
	must("框架", "doc_cn", "fts")
	mustNot("云计算")
	must("/var/log/app/server.go", "doc_path", "exact")
	must("server.go", "doc_path", "exact")
	hits, err := db.SearchHistory(ctx, "ses", "server.go", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].SourceID != "doc_path" {
		t.Fatalf("server.go hits %#v", hits)
	}
	must("parseToolCallID", "doc_code", "exact")
	must("ToolCall", "doc_code", "fts")
	must("evt_9f3a2c", "doc_id", "exact")
	must("10086", "doc_num", "exact")
	mustNot("100")
	mustNot("evt_000000")
	other, err := db.SearchHistory(ctx, "other", "检索", 10)
	if err != nil || len(other) != 1 || other[0].SourceID != "doc_other" {
		t.Fatalf("session isolation %#v %v", other, err)
	}
	if err := db.RebuildHistory(ctx, "ses"); err != nil {
		t.Fatal(err)
	}
	must("检索", "doc_cn", "fts")
	must("server.go", "doc_path", "exact")
	must("ToolCall", "doc_code", "fts")

	if err := db.indexHistory(ctx, storage.HistoryDoc{SourceID: "doc_rollback", SessionID: "ses", RawText: "不应提交的检索", CreatedAt: now}, false); !errors.Is(err, errForcedRollback) {
		t.Fatalf("rollback err %v", err)
	}
	var n int
	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(1) FROM history_docs WHERE source_id = 'doc_rollback'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rolled back doc count %d %v", n, err)
	}
	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(1) FROM history_fts WHERE source_id = 'doc_rollback'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rolled back fts count %d %v", n, err)
	}
}

func TestInvocationEventAtomicityAndLease(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC)
	if err := db.CreateSession(ctx, storage.Session{ID: "ses", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	run := storage.Run{
		ID: "run_1", SessionID: "ses", Status: domain.RunRunning, Budget: domain.DefaultBudget(),
		Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	inv := domain.Invocation{ID: "inv_1", Ordinal: 0, ProviderCallID: "c1", Name: "read_file", Arguments: []byte(`{"path":"a"}`), Class: domain.ActionExecute, Status: domain.InvPending}
	batch := storage.StoredBatch{ID: "bat_1", RunID: "run_1", Status: domain.BatchOpen, Action: domain.ActionExecute, Invocations: []domain.Invocation{inv}}
	if err := db.SaveBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendEvent(ctx, storage.Event{ID: "evt_seed", RunID: "run_1", Type: "seed", Payload: []byte(`{}`), Persistent: true, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	inv.Status = domain.InvSucceeded
	inv.Dispatched = true
	inv.Result = &domain.CallResult{OK: true, Data: []byte(`{"ok":true}`)}
	// The event primary key collides, so the invocation update must roll back with it.
	err := db.CommitInvocation(ctx, "run_1", inv, 1, storage.Event{
		ID: "evt_seed", Type: "tool.succeeded", Payload: []byte(`{}`), Persistent: true, CreatedAt: now,
	})
	if err == nil {
		t.Fatal("event insert failure still committed the invocation")
	}
	loaded, err := db.LoadBatches(ctx, "run_1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded[0].Invocations[0].Status != domain.InvPending || loaded[0].Invocations[0].Dispatched {
		t.Fatalf("invocation changed after rollback: %+v", loaded[0].Invocations[0])
	}
	if err := db.CommitInvocation(ctx, "run_1", inv, 1, storage.Event{
		ID: "evt_ok", Type: "tool.succeeded", Payload: []byte(`{"ok":true}`), Persistent: true, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	loaded, err = db.LoadBatches(ctx, "run_1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded[0].Invocations[0].Status != domain.InvSucceeded {
		t.Fatalf("status %s", loaded[0].Invocations[0].Status)
	}
	if _, err := db.AppendEvent(ctx, storage.Event{RunID: "run_1", Type: "stream.delta", Persistent: false, CreatedAt: now}); err == nil {
		t.Fatal("ephemeral event was stored")
	}

	lease, err := db.AcquireLease(ctx, "run_1", "owner_a", now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireLease(ctx, "run_1", "owner_b", now, now.Add(time.Hour)); !domain.IsCode(err, domain.CodeLease) {
		t.Fatalf("second owner acquired: %v", err)
	}
	if err := db.RequireLease(ctx, "run_1", "owner_a", lease.LeaseVersion, now); err != nil {
		t.Fatal(err)
	}
	later := now.Add(2 * time.Hour)
	if err := db.RequireLease(ctx, "run_1", "owner_a", lease.LeaseVersion, later); !domain.IsCode(err, domain.CodeLease) {
		t.Fatalf("expired owner still valid: %v", err)
	}
	if _, err := db.AcquireLease(ctx, "run_1", "owner_b", later, later.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := db.HeartbeatLease(ctx, "run_1", "owner_a", lease.LeaseVersion, later, later.Add(time.Minute)); !domain.IsCode(err, domain.CodeLease) {
		t.Fatalf("stale owner heartbeat: %v", err)
	}
}

func TestOpaqueBlobIsNotInVisibleItems(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Now().UTC()
	if err := db.CreateSession(ctx, storage.Session{ID: "ses", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateRun(ctx, storage.Run{ID: "run", SessionID: "ses", Status: domain.RunRunning, Budget: domain.DefaultBudget(), Version: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	resp := domain.ModelResponse{
		Provider: "anthropic", Complete: true,
		Items: []domain.ResponseItem{
			{Ordinal: 0, Kind: domain.ItemText, Text: "visible"},
			{Ordinal: 1, Kind: domain.ItemOpaque, Opaque: &domain.OpaqueItem{Provider: "anthropic", Kind: "thinking", Payload: []byte("SECRET-PAYLOAD-7f3a"), Signature: []byte("SIG-OPAQUE-XYZ")}},
		},
	}
	visible, blob, err := domain.SplitForStorage(resp)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveModelResponse(ctx, storage.StoredResponse{
		ID: "rsp_1", RunID: "run", Provider: "anthropic", Complete: true, Items: visible, Opaque: blob, Action: domain.ActionAnswer, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := db.ListVisibleResponses(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	raw := string(mustJSON(t, got))
	if strings.Contains(raw, "SECRET-PAYLOAD") || strings.Contains(raw, "SIG-OPAQUE") {
		t.Fatalf("visible response leaked opaque: %s", raw)
	}
	blob, err = db.LoadContinuation(ctx, "rsp_1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), "SECRET-PAYLOAD-7f3a") && !containsDecoded(blob) {
		// JSON stores []byte as base64. Decode via merge.
		merged, err := domain.MergeFromStorage("anthropic", got[0].Items, blob)
		if err != nil {
			t.Fatal(err)
		}
		if string(merged[1].Opaque.Payload) != "SECRET-PAYLOAD-7f3a" || string(merged[1].Opaque.Signature) != "SIG-OPAQUE-XYZ" {
			t.Fatalf("continuation lost bytes: %#v", merged[1].Opaque)
		}
	}
}

func containsDecoded(blob []byte) bool { return strings.Contains(string(blob), "SECRET-PAYLOAD-7f3a") }

func TestPropertyLeaseIsUniqueAndResumeKeepsTheRun(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	if err := db.CreateSession(ctx, storage.Session{ID: "ses", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	run := storage.Run{
		ID: "run_same", SessionID: "ses", Status: domain.RunWaiting, WaitReason: domain.WaitApproval,
		Budget: domain.DefaultBudget(), Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireLease(ctx, run.ID, "owner-a", now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireLease(ctx, run.ID, "owner-b", now, now.Add(time.Hour)); !domain.IsCode(err, domain.CodeLease) {
		t.Fatalf("second owner took a live lease: %v", err)
	}
	later := now.Add(2 * time.Hour)
	if _, err := db.AcquireLease(ctx, run.ID, "owner-b", later, later.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetRun(ctx, run.ID)
	if err != nil || got.ID != run.ID || got.Status != domain.RunWaiting {
		t.Fatalf("lease takeover replaced the run %+v %v", got, err)
	}
}

func TestPruneHistoryKeepsNewerRows(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if err := db.IndexHistory(ctx, storage.HistoryDoc{SourceID: "old", SessionID: "ses", RawText: "obsolete note", CreatedAt: old}); err != nil {
		t.Fatal(err)
	}
	if err := db.IndexHistory(ctx, storage.HistoryDoc{SourceID: "new", SessionID: "ses", RawText: "current note", CreatedAt: newer}); err != nil {
		t.Fatal(err)
	}
	n, err := db.PruneHistory(ctx, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || n != 1 {
		t.Fatalf("pruned %d %v", n, err)
	}
	if _, err := db.GetHistory(ctx, "old"); err == nil {
		t.Fatal("old history remained")
	}
	if doc, err := db.GetHistory(ctx, "new"); err != nil || doc.RawText != "current note" {
		t.Fatalf("new history %+v %v", doc, err)
	}
}

func TestHistoryIdempotentUpsertAndDelete(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	t1 := time.Date(2026, 11, 18, 1, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 11, 18, 2, 0, 0, 0, time.UTC)
	if err := db.IndexHistory(ctx, storage.HistoryDoc{
		SourceID: "note_search", SessionID: "ses_1", RawText: "会议记录讨论了检索的索引策略，标识 note_search。", CreatedAt: t1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.IndexHistory(ctx, storage.HistoryDoc{
		SourceID: "note_search", SessionID: "ses_2", RawText: "会议记录讨论了检索的索引策略，标识 note_search。", CreatedAt: t2,
	}); err != nil {
		t.Fatalf("second IndexHistory on same source_id failed: %v", err)
	}
	doc, err := db.GetHistory(ctx, "note_search")
	if err != nil || doc.SessionID != "ses_2" {
		t.Fatalf("expected session ses_2 after upsert, got %+v %v", doc, err)
	}
	oldHits, err := db.SearchHistory(ctx, "ses_1", "检索", 10)
	if err != nil || len(oldHits) != 0 {
		t.Fatalf("expected 0 hits in old session ses_1, got %#v %v", oldHits, err)
	}
	newHits, err := db.SearchHistory(ctx, "ses_2", "检索", 10)
	if err != nil || len(newHits) != 1 || newHits[0].SourceID != "note_search" {
		t.Fatalf("expected 1 hit in new session ses_2, got %#v %v", newHits, err)
	}
	if err := db.DeleteHistory(ctx, "note_search"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetHistory(ctx, "note_search"); err == nil {
		t.Fatal("expected note_search to be deleted")
	}
}

func TestLoadBatchesDeterministicOrder(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	if err := db.CreateSession(ctx, storage.Session{ID: "ses", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateRun(ctx, storage.Run{
		ID: "run_ord", SessionID: "ses", Status: domain.RunRunning, Budget: domain.DefaultBudget(),
		Version: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	ids := []string{"bat_zzz", "bat_aaa", "bat_mmm", "bat_bbb"}
	for i, id := range ids {
		inv := domain.Invocation{
			ID: "inv_" + id, Ordinal: 0, ProviderCallID: id, Name: "read_file",
			Arguments: json.RawMessage(`{"path":"a"}`), Class: domain.ActionExecute, Status: domain.InvSucceeded,
			Result: &domain.CallResult{OK: true, Data: json.RawMessage(`{}`)},
		}
		if err := db.SaveBatch(ctx, storage.StoredBatch{
			ID: id, RunID: "run_ord", Status: domain.BatchComplete, Action: domain.ActionExecute,
			NextOrdinal: i, Invocations: []domain.Invocation{inv},
		}); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := db.LoadBatches(ctx, "run_ord")
	if err != nil || len(loaded) != len(ids) {
		t.Fatalf("LoadBatches %+v %v", loaded, err)
	}
	for i, want := range ids {
		if loaded[i].ID != want {
			t.Fatalf("batch[%d]=%s, want %s", i, loaded[i].ID, want)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPruneRunsTerminalAndChildren(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	now := time.Now().UTC()
	tOld := now.AddDate(0, 0, -30)
	tNew := now.AddDate(0, 0, -2)

	if err := db.CreateSession(ctx, storage.Session{ID: "ses_all", CreatedAt: tOld, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}

	// 1. Overdue COMPLETED run
	// 2. Overdue FAILED run
	// 3. Overdue CANCELLED run
	// 4. Overdue WAITING run
	// 5. Recent COMPLETED run
	// 6. Active RUNNING run
	runs := []struct {
		id     string
		status domain.RunStatus
		reason domain.WaitReason
		t      time.Time
	}{
		{id: "run_done_old", status: domain.RunCompleted, reason: domain.WaitNone, t: tOld},
		{id: "run_failed_old", status: domain.RunFailed, reason: domain.WaitNone, t: tOld},
		{id: "run_cancelled_old", status: domain.RunCancelled, reason: domain.WaitNone, t: tOld},
		{id: "run_waiting_old", status: domain.RunWaiting, reason: domain.WaitUser, t: tOld},
		{id: "run_done_new", status: domain.RunCompleted, reason: domain.WaitNone, t: tNew},
		{id: "run_active", status: domain.RunRunning, reason: domain.WaitNone, t: now},
	}

	for _, r := range runs {
		if err := db.CreateRun(ctx, storage.Run{
			ID: r.id, SessionID: "ses_all", Status: r.status, WaitReason: r.reason,
			Budget: domain.DefaultBudget(), Version: 1, CreatedAt: r.t, UpdatedAt: r.t,
		}); err != nil {
			t.Fatal(err)
		}

		// checkpoints
		if err := db.SaveCheckpoint(ctx, r.id, 1, []byte(`{"snapshot":"ok"}`), r.t); err != nil {
			t.Fatal(err)
		}
		// events
		if _, err := db.AppendEvent(ctx, storage.Event{
			ID: "ev_" + r.id, RunID: r.id, Seq: 1, Type: "step",
			Payload: json.RawMessage(`{}`), CreatedAt: r.t, Persistent: true,
		}); err != nil {
			t.Fatal(err)
		}
		// model responses + continuations + items + steps
		if err := db.SaveModelResponse(ctx, storage.StoredResponse{
			ID: "rsp_" + r.id, RunID: r.id, Provider: "mock", Complete: true,
			Opaque: []byte("cont_" + r.id), CreatedAt: r.t,
			Items: []domain.VisibleItem{{Ordinal: 0, Kind: "text", Text: "txt"}},
		}); err != nil {
			t.Fatal(err)
		}
		// tool batches + invocations
		if err := db.SaveBatch(ctx, storage.StoredBatch{
			ID: "bat_" + r.id, RunID: r.id, Status: domain.BatchComplete, Action: domain.ActionExecute,
			NextOrdinal: 1, Invocations: []domain.Invocation{{
				ID: "inv_" + r.id, Ordinal: 0, ProviderCallID: "c_" + r.id, Name: "read_file",
				Arguments: json.RawMessage(`{}`), Class: domain.ActionExecute, Status: domain.InvSucceeded,
			}},
		}); err != nil {
			t.Fatal(err)
		}
		// run_leases
		if _, err := db.AcquireLease(ctx, r.id, "worker", r.t, r.t.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		// approvals
		if err := db.SaveApproval(ctx, domain.Approval{
			ID: "app_" + r.id, RunID: r.id, Tool: "run_command", State: "approved", CreatedAt: r.t,
		}); err != nil {
			t.Fatal(err)
		}
		// scoped_grants (run-scoped)
		if err := db.SaveGrant(ctx, domain.Grant{
			ID: "grt_" + r.id, RunID: r.id, Subject: "s", Tool: "read_file", Op: "read", State: "active",
		}, r.t); err != nil {
			t.Fatal(err)
		}
		// idempotency_keys
		if err := db.SaveIdempotency(ctx, r.id, "tool", "key_"+r.id, json.RawMessage(`{}`), r.t); err != nil {
			t.Fatal(err)
		}
		// summary_jobs
		if err := db.SaveSummaryJob(ctx, storage.SummaryJob{
			ID: "job_" + r.id, SessionID: "ses_all", TurnID: "trn_1", RunID: r.id, Status: "done", CreatedAt: r.t, UpdatedAt: r.t,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Also add a session-scoped grant (run_id = '') that must NEVER be deleted
	if err := db.SaveGrant(ctx, domain.Grant{
		ID: "grt_session_wide", RunID: "", SessionID: "ses_all", Subject: "s", Tool: "read_file", Op: "read", State: "active",
	}, tOld); err != nil {
		t.Fatal(err)
	}

	// Cutoff is 7 days ago
	cut := now.AddDate(0, 0, -7)
	res, err := db.PruneRuns(ctx, cut)
	if err != nil {
		t.Fatalf("PruneRuns: %v", err)
	}

	// 3 overdue terminal runs should be deleted (run_done_old, run_failed_old, run_cancelled_old)
	if res.RunsDeleted != 3 {
		t.Fatalf("expected 3 runs deleted, got %d", res.RunsDeleted)
	}
	if res.Tables["runs"] != 3 {
		t.Fatalf("expected 3 in tables[runs], got %d", res.Tables["runs"])
	}
	for tbl, want := range map[string]int{
		"checkpoints":            3,
		"messages":               9,
		"events":                 3,
		"provider_continuations": 3,
		"model_response_items":   3,
		"model_responses":        3,
		"tool_invocations":       3,
		"tool_batches":           3,
		"steps":                  3,
		"run_leases":             3,
		"approvals":              3,
		"scoped_grants":          3,
		"idempotency_keys":       3,
		"summary_jobs":           3,
	} {
		if got := res.Tables[tbl]; got != want {
			t.Fatalf("expected %s deleted count %d, got %d", tbl, want, got)
		}
	}

	// Check FK violations
	violations, err := db.ForeignKeyCheck(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("foreign key violations after prune: %v", violations)
	}

	// Verify session-scoped grant is still present
	grants, err := db.ListGrants(ctx, "", "ses_all", now)
	if err != nil || len(grants) < 1 {
		t.Fatalf("expected session grant to remain: %v %v", grants, err)
	}
	foundSessionGrant := false
	for _, g := range grants {
		if g.ID == "grt_session_wide" {
			foundSessionGrant = true
		}
	}
	if !foundSessionGrant {
		t.Fatal("grt_session_wide was incorrectly deleted")
	}

	// Verify the 3 surviving runs
	for _, id := range []string{"run_waiting_old", "run_done_new", "run_active"} {
		if _, err := db.GetRun(ctx, id); err != nil {
			t.Fatalf("surviving run %s should exist, got %v", id, err)
		}
		if _, err := db.LoadContinuation(ctx, "rsp_"+id); err != nil {
			t.Fatalf("continuation for %s should exist, got %v", id, err)
		}
	}
}

func TestPruneRunsZeroTime(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	res, err := db.PruneRuns(ctx, time.Time{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RunsDeleted != 0 {
		t.Fatalf("expected 0 runs deleted on zero time, got %d", res.RunsDeleted)
	}
}

func TestPruneRunsRollbackOnFKViolation(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	now := time.Now().UTC()
	tOld := now.AddDate(0, 0, -30)

	if err := db.CreateSession(ctx, storage.Session{ID: "ses_fk", CreatedAt: tOld, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateRun(ctx, storage.Run{
		ID: "run_fk_old", SessionID: "ses_fk", Status: domain.RunCompleted,
		Budget: domain.DefaultBudget(), Version: 1, CreatedAt: tOld, UpdatedAt: tOld,
	}); err != nil {
		t.Fatal(err)
	}

	// Disable foreign keys temporarily to inject an orphan row that violates foreign keys
	if _, err := db.sql.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.ExecContext(ctx, `INSERT INTO checkpoints(id, run_id, version, snapshot_json, created_at) VALUES('ckp_orphan', 'non_existent_run', 1, '{}', ?)`, formatTime(tOld)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		t.Fatal(err)
	}

	// PruneRuns should catch the foreign key violation, rollback, and return an error
	_, err := db.PruneRuns(ctx, now.AddDate(0, 0, -7))
	if err == nil || !strings.Contains(err.Error(), "foreign key check failed") {
		t.Fatalf("expected foreign key check failure, got: %v", err)
	}

	// Because of rollback, run_fk_old must still exist!
	if _, err := db.GetRun(ctx, "run_fk_old"); err != nil {
		t.Fatalf("run_fk_old should remain intact due to rollback, got: %v", err)
	}
}
