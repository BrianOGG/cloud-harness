package transport

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/agent"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/storage"
	"github.com/BrianOGG/cloud-harness/internal/storage/sqlite"
)

// These black-box HTTP expectations come from the complete messages section of
// docs/api.md at 9eba93e7cc393c81bbd9fb34efb639b8f7a363e9, not the older docs on
// the A2 tree. Fixtures commit durable sources through SQLite; they never insert
// projection rows or implement a replacement messages handler. The A2-only
// baseline has no route, so its endpoint assertions must fail (normally 404).
const a4ContractTime = "2006-01-02T15:04:05.000000000Z"

const a4ContractToken = "a4-contract-local-test-token"

type a4ContractMessage struct {
	ID        string  `json:"id"`
	SessionID string  `json:"session_id"`
	RunID     *string `json:"run_id"`
	TurnID    *string `json:"turn_id"`
	Role      string  `json:"role"`
	Kind      string  `json:"kind"`
	Status    string  `json:"status"`
	Content   string  `json:"content"`
	Event     string  `json:"event"`
	CreatedAt string  `json:"created_at"`
}

type a4ContractPage struct {
	Messages   []a4ContractMessage `json:"messages"`
	NextCursor string              `json:"next_cursor"`
}

type a4ContractBoundary struct {
	V         int    `json:"v"`
	SessionID string `json:"session_id"`
	CreatedAt string `json:"created_at"`
	ID        string `json:"id"`
}

type a4ContractFixture struct {
	db   *sqlite.DB
	path string
}

func a4NewContractFixture(t *testing.T) *a4ContractFixture {
	t.Helper()
	f := &a4ContractFixture{path: filepath.Join(t.TempDir(), "a4-messages.db")}
	var err error
	f.db, err = sqlite.Open(context.Background(), f.path)
	a4ContractMust(t, err)
	t.Cleanup(func() { _ = f.db.Close() })
	return f
}

func a4ContractMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func a4ContractString(s string) *string { return &s }

func (f *a4ContractFixture) session(t *testing.T, id string) {
	t.Helper()
	at := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	createTestSession(t, f.db, id, at, at)
}

func (f *a4ContractFixture) turn(t *testing.T, session, id, text string, at time.Time) {
	t.Helper()
	a4ContractMust(t, f.db.CreateTurn(context.Background(), storage.Turn{
		ID: id, SessionID: session, UserInput: text, Status: "completed", CreatedAt: at,
	}))
}

func (f *a4ContractFixture) run(t *testing.T, session, id, turn, parent string, at time.Time) storage.Run {
	t.Helper()
	// WAITING avoids unrelated startup recovery of lease-less RUNNING fixtures.
	r := storage.Run{ID: id, SessionID: session, TurnID: turn, ParentRunID: parent,
		Status: domain.RunWaiting, WaitReason: domain.WaitUser, Budget: domain.DefaultBudget(),
		Version: 1, CreatedAt: at, UpdatedAt: at}
	a4ContractMust(t, f.db.CreateRun(context.Background(), r))
	return r
}

func (f *a4ContractFixture) response(t *testing.T, run, id string, at time.Time, complete bool, action domain.Action, items ...domain.VisibleItem) {
	t.Helper()
	a4ContractMust(t, f.db.SaveModelResponse(context.Background(), storage.StoredResponse{
		ID: id, RunID: run, Provider: "a4-fixture", Complete: complete, Action: action,
		CreatedAt: at, Items: items,
	}))
}

func (f *a4ContractFixture) terminal(t *testing.T, run storage.Run, status domain.RunStatus, answer, reason, event string, at time.Time) {
	t.Helper()
	next := run
	next.Status, next.WaitReason, next.UpdatedAt, next.FinalAnswer = status, "", at, answer
	next.Version++
	if reason != "" {
		next.Error = domain.E(domain.CodeProviderHTTP, reason)
	}
	a4ContractMust(t, f.db.CommitRunTransition(context.Background(), next, run.Version,
		storage.Event{Type: event, CreatedAt: at, Persistent: true}))
}

func (f *a4ContractFixture) handler(store storage.Store, token string) http.Handler {
	if store == nil {
		store = f.db
	}
	return (&Server{Manager: &agent.Manager{Store: store, OwnerID: "a4-contract"},
		LoopbackBind: true, Token: token}).Handler()
}

func a4ContractGET(h http.Handler, session, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+session+"/messages", nil)
	// Set RawQuery after constructing the request so malformed percent escapes
	// reach the real handler rather than being rejected by the test URL parser.
	req.URL.RawQuery = query
	req.Host = "localhost:19421"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func a4ContractDecodePage(t *testing.T, rec *httptest.ResponseRecorder, session string) a4ContractPage {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("messages HTTP status=%d, want 200; body=%s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type=%q, want application/json", rec.Header().Get("Content-Type"))
	}
	var raw map[string]json.RawMessage
	a4ContractMust(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	if len(raw) != 2 || raw["messages"] == nil || raw["next_cursor"] == nil {
		t.Fatalf("page must have exactly messages and next_cursor: %s", rec.Body)
	}
	if s := strings.TrimSpace(string(raw["messages"])); !strings.HasPrefix(s, "[") {
		t.Fatalf("messages must be an array, including on an empty page: %s", rec.Body)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(raw["next_cursor"])), "\"") {
		t.Fatalf("next_cursor must be a string: %s", rec.Body)
	}
	var records []map[string]json.RawMessage
	a4ContractMust(t, json.Unmarshal(raw["messages"], &records))
	keys := []string{"id", "session_id", "run_id", "turn_id", "role", "kind", "status", "content", "event", "created_at"}
	for i, record := range records {
		if len(record) != len(keys) {
			t.Errorf("message[%d] field count=%d, want %d: %s", i, len(record), len(keys), raw["messages"])
		}
		for _, key := range keys {
			value, ok := record[key]
			if !ok {
				t.Errorf("message[%d] missing %s", i, key)
				continue
			}
			if (key == "run_id" || key == "turn_id") && string(value) == "null" {
				continue
			}
			var valueType any
			a4ContractMust(t, json.Unmarshal(value, &valueType))
			if _, ok := valueType.(string); !ok {
				t.Errorf("message[%d].%s must be a string (or association null): %s", i, key, value)
			}
		}
	}
	var page a4ContractPage
	a4ContractMust(t, json.Unmarshal(rec.Body.Bytes(), &page))
	for i, message := range page.Messages {
		if message.ID == "" || !a4ContractASCII(message.ID) || message.SessionID != session {
			t.Errorf("message identity/session=%+v", message)
		}
		if message.RunID != nil && *message.RunID == "" || message.TurnID != nil && *message.TurnID == "" {
			t.Errorf("empty association must be explicit null: %+v", message)
		}
		at, err := time.Parse(a4ContractTime, message.CreatedAt)
		if err != nil || at.UTC().Format(a4ContractTime) != message.CreatedAt {
			t.Errorf("created_at is not fixed UTC with nine fractional digits: %q (%v)", message.CreatedAt, err)
		}
		roles := map[string]string{"input": "user", "response": "assistant", "question": "assistant", "answer": "user",
			"approval_requested": "system", "approval_decision": "system", "run_status": "system"}
		if role, ok := roles[message.Kind]; !ok || message.Role != role {
			t.Errorf("invalid role/kind mapping: %+v", message)
		}
		if message.Status != "recorded" && !(message.Kind == "run_status" && (message.Status == "failed" || message.Status == "cancelled")) {
			t.Errorf("invalid fact status: %+v", message)
		}
		if i > 0 {
			prev := page.Messages[i-1]
			if prev.CreatedAt > message.CreatedAt || prev.CreatedAt == message.CreatedAt && prev.ID >= message.ID {
				t.Errorf("page is not strictly ordered by (created_at, ASCII id): %+v then %+v", prev, message)
			}
		}
	}
	return page
}

func a4ContractASCII(s string) bool {
	for i := range s {
		if s[i] > 127 {
			return false
		}
	}
	return true
}

func a4ContractCursor(session, at, id string) string {
	body, err := json.Marshal(a4ContractBoundary{1, session, at, id})
	if err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(body)
}

func a4ContractCheckCursor(t *testing.T, page a4ContractPage, session string, more bool) {
	t.Helper()
	if !more {
		if page.NextCursor != "" {
			t.Errorf("last page next_cursor=%q, want empty", page.NextCursor)
		}
		return
	}
	if len(page.Messages) == 0 {
		t.Fatal("cannot continue an empty page")
	}
	last := page.Messages[len(page.Messages)-1]
	want := a4ContractCursor(session, last.CreatedAt, last.ID)
	if page.NextCursor != want {
		t.Fatalf("cursor must encode the last returned row in compact v1 field order (not look-ahead): got %q want %q", page.NextCursor, want)
	}
}

func a4ContractError(t *testing.T, rec *httptest.ResponseRecorder, status int, code, message, ref string) {
	t.Helper()
	if rec.Code != status {
		t.Errorf("status=%d, want %d; body=%s", rec.Code, status, rec.Body)
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("error Content-Type=%q, want application/json", rec.Header().Get("Content-Type"))
	}
	inner := map[string]any{"code": code, "message": message}
	if ref != "" {
		inner["ref"] = ref
	}
	want := map[string]any{"error": inner}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("exact error envelope: got %s, want %+v (decode=%v)", rec.Body, want, err)
	}
	challenge := ""
	if status == http.StatusUnauthorized {
		challenge = `Bearer realm="cloud-harness"`
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != challenge {
		t.Errorf("WWW-Authenticate=%q, want %q", got, challenge)
	}
}

func a4ContractWant(t *testing.T, page a4ContractPage, want []a4ContractMessage) {
	t.Helper()
	byID := make(map[string]a4ContractMessage)
	for _, got := range page.Messages {
		if _, found := byID[got.ID]; found {
			t.Errorf("duplicate source identity %q", got.ID)
		}
		byID[got.ID] = got
	}
	if len(page.Messages) != len(want) {
		t.Errorf("message count=%d, want %d; got=%+v", len(page.Messages), len(want), page.Messages)
	}
	for _, expected := range want {
		got, ok := byID[expected.ID]
		if !ok {
			t.Errorf("missing durable source %q", expected.ID)
			continue
		}
		// Empty expected time means a fact committed by the store's wall clock;
		// its exact fixed-format timestamp is still checked by DecodePage.
		if expected.CreatedAt == "" {
			got.CreatedAt = ""
		}
		if !reflect.DeepEqual(got, expected) {
			t.Errorf("message %s: got %+v; want %+v", expected.ID, got, expected)
		}
	}
}

func TestTranscriptSyncResumesAtLastChange(t *testing.T) {
	f := a4NewContractFixture(t)
	f.session(t, "sync")
	h := f.handler(nil, "")
	now := time.Now().UTC()
	f.turn(t, "sync", "one", "first", now)
	read := func(token string) (string, []a4ContractMessage) {
		t.Helper()
		rec := a4ContractGET(h, "sync", "sync="+token+"&limit=200")
		var page struct {
			Sync     string
			Messages []a4ContractMessage
			More     bool
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &page) != nil || page.Sync == "" || page.Messages == nil || page.More {
			t.Fatalf("unexpected sync response: %d %s", rec.Code, rec.Body)
		}
		return page.Sync, page.Messages
	}
	token, rows := read("0.0")
	if len(rows) != 1 || rows[0].Content != "first" {
		t.Fatalf("initial sync: %+v", rows)
	}
	f.turn(t, "sync", "two", "backdated", now.Add(-time.Hour))
	token, rows = read(token)
	if len(rows) != 1 || rows[0].Content != "backdated" {
		t.Fatalf("increment: %+v", rows)
	}
	if same, rows := read(token); same != token || len(rows) != 0 {
		t.Fatalf("unchanged sync: %s %+v", same, rows)
	}
	for _, query := range []string{"sync=", "sync=x.1", "sync=0.-1", "sync=0.0&sync=0.1", "cursor=&sync=0.0"} {
		if rec := a4ContractGET(h, "sync", query); rec.Code != http.StatusBadRequest {
			t.Fatalf("query %q returned %d", query, rec.Code)
		}
	}
}

func TestA4MessagesContractEmptyAndNullableAssociations(t *testing.T) {
	for _, scenario := range []string{"no_runs", "run_without_facts", "standalone_turn", "run_without_turn"} {
		t.Run(scenario, func(t *testing.T) {
			f := a4NewContractFixture(t)
			f.session(t, "ses_empty")
			at := time.Date(2026, 11, 25, 17, 0, 0, 123456789, time.FixedZone("fixture", 8*3600))
			want := []a4ContractMessage{}
			switch scenario {
			case "run_without_facts":
				f.run(t, "ses_empty", "run_wait", "", "", at)
			case "standalone_turn":
				f.turn(t, "ses_empty", "turn_standalone", "原始输入\n<unrendered>", at)
				want = append(want, a4ContractMessage{"msg_user_turn_standalone", "ses_empty", nil, a4ContractString("turn_standalone"), "user", "input", "recorded", "原始输入\n<unrendered>", "", at.UTC().Format(a4ContractTime)})
			case "run_without_turn":
				run := f.run(t, "ses_empty", "run_failed", "", "", at)
				f.terminal(t, run, domain.RunFailed, "", "原始失败原因\nsecond line", "run.FAILED", at)
				want = append(want, a4ContractMessage{"msg_status_run_failed", "ses_empty", a4ContractString("run_failed"), nil, "system", "run_status", "failed", "原始失败原因\nsecond line", "run.FAILED", at.UTC().Format(a4ContractTime)})
			}
			page := a4ContractDecodePage(t, a4ContractGET(f.handler(nil, ""), "ses_empty", ""), "ses_empty")
			a4ContractWant(t, page, want)
			a4ContractCheckCursor(t, page, "ses_empty", false)
		})
	}
}

func TestA4MessagesContractDurableSourcesAndSessionIsolation(t *testing.T) {
	f := a4NewContractFixture(t)
	ctx := context.Background()
	f.session(t, "ses_sources")
	f.session(t, "ses_foreign")
	at := time.Date(2026, 11, 25, 9, 0, 0, 23, time.UTC)
	f.turn(t, "ses_sources", "turn_root", "same original input\n原文", at)
	root := f.run(t, "ses_sources", "run_root", "turn_root", "", at)
	f.run(t, "ses_sources", "run_retry", "turn_root", "", at.Add(time.Second))
	child := f.run(t, "ses_sources", "run_child", "turn_root", root.ID, at)
	foreign := f.run(t, "ses_foreign", "run_foreign_child", "", root.ID, at)
	f.response(t, root.ID, "rsp_visible", at.Add(time.Second), true, domain.ActionAnswer,
		domain.VisibleItem{Ordinal: 2, Kind: domain.ItemText, Text: "C"},
		domain.VisibleItem{Ordinal: 0, Kind: domain.ItemText, Text: "A\n"},
		domain.VisibleItem{Ordinal: 1, Kind: domain.ItemText, Text: "B"})
	f.response(t, child.ID, "rsp_child", at.Add(time.Second), true, domain.ActionAnswer, domain.VisibleItem{Ordinal: 0, Kind: domain.ItemText, Text: "child text"})
	f.response(t, foreign.ID, "rsp_foreign", at, true, domain.ActionAnswer, domain.VisibleItem{Ordinal: 0, Kind: domain.ItemText, Text: "foreign child must not leak"})
	// Equal text with distinct persisted response IDs must remain distinct.
	f.response(t, root.ID, "rsp_equal", at.Add(2*time.Second), true, domain.ActionAnswer, domain.VisibleItem{Ordinal: 0, Kind: domain.ItemText, Text: "A\nBC"})
	for _, excluded := range []struct {
		id       string
		complete bool
		action   domain.Action
	}{
		{"rsp_partial", false, domain.ActionAnswer},
		{"rsp_compact", true, domain.ActionCompact},
		{"rsp_plan", true, domain.ActionPlan},
		{"rsp_oracle", true, domain.Action("ORACLE")},
	} {
		f.response(t, root.ID, excluded.id, at, excluded.complete, excluded.action, domain.VisibleItem{Ordinal: 0, Kind: domain.ItemText, Text: "excluded: " + excluded.id})
	}
	a4ContractMust(t, f.db.SaveModelResponse(ctx, storage.StoredResponse{ID: "rsp_opaque", RunID: root.ID, Provider: "a4-fixture", Complete: true, Action: domain.ActionAnswer, CreatedAt: at, Opaque: []byte("provider continuation must not leak")}))
	f.response(t, root.ID, "rsp_no_text", at, true, domain.ActionExecute)
	f.response(t, root.ID, "rsp_question", at.Add(3*time.Second), true, domain.ActionAskUser)
	batch := storage.StoredBatch{ID: "batch_question", ModelResponseID: "rsp_question", RunID: root.ID, Action: domain.ActionAskUser, Status: domain.BatchWaitingUser,
		Invocations: []domain.Invocation{{ID: "inv_question", Ordinal: 0, ProviderCallID: "question_call", Name: domain.ToolAskUser, Class: domain.ActionAskUser, Status: domain.InvPending, Arguments: json.RawMessage(`{"question":"Which region?\n原文"}`)}}}
	a4ContractMust(t, f.db.SaveBatch(ctx, batch))
	batch.Status = domain.BatchComplete
	batch.Invocations[0].Status = domain.InvSucceeded
	batch.Invocations[0].Result = &domain.CallResult{OK: true, Data: json.RawMessage(`{"answer":"上海\naccepted"}`)}
	acceptedBefore := time.Now().UTC()
	a4ContractMust(t, f.db.UpdateBatch(ctx, batch))
	acceptedAfter := time.Now().UTC()
	a4ContractMust(t, f.db.UpdateBatch(ctx, batch)) // Replay cannot duplicate/re-time.
	f.response(t, root.ID, "rsp_write", at.Add(4*time.Second), true, domain.ActionExecute)
	a4ContractMust(t, f.db.SaveBatch(ctx, storage.StoredBatch{ID: "batch_write", ModelResponseID: "rsp_write", RunID: root.ID, Action: domain.ActionExecute, Status: domain.BatchWaitingApproval,
		Invocations: []domain.Invocation{{ID: "inv_write", Ordinal: 0, ProviderCallID: "call_write", Name: "write_file", Class: domain.ActionExecute, Status: domain.InvPending, Arguments: json.RawMessage("{\"path\":\"artifact.txt\",\"content\":\"raw private tool argument\"}")}}}))
	approval := domain.Approval{ID: "apr_write", RunID: root.ID, InvocationID: "inv_write", Tool: "write_file", State: "pending", CreatedAt: at.Add(4 * time.Second)}
	a4ContractMust(t, f.db.SaveApproval(ctx, approval))
	a4ContractMust(t, f.db.UpdateApprovalState(ctx, root.ID, "inv_write", "denied"))
	a4ContractMust(t, f.db.UpdateApprovalState(ctx, root.ID, "inv_write", "denied"))
	f.response(t, child.ID, "rsp_pending", at.Add(5*time.Second), true, domain.ActionExecute)
	a4ContractMust(t, f.db.SaveBatch(ctx, storage.StoredBatch{ID: "batch_pending", ModelResponseID: "rsp_pending", RunID: child.ID, Action: domain.ActionExecute, Status: domain.BatchWaitingApproval,
		Invocations: []domain.Invocation{{ID: "inv_pending", Ordinal: 0, ProviderCallID: "call_pending", Name: "write_file", Class: domain.ActionExecute, Status: domain.InvPending, Arguments: json.RawMessage("{}")}}}))
	pending := domain.Approval{ID: "apr_pending", RunID: child.ID, InvocationID: "inv_pending", Tool: "write_file", State: "pending", CreatedAt: at.Add(5 * time.Second)}
	a4ContractMust(t, f.db.SaveApproval(ctx, pending))
	// Raw/indexed/summary/SSE data are retained sources but not messages.
	a4ContractMust(t, f.db.SaveTurnSummary(ctx, "turn_root", "private compaction summary", "[]"))
	a4ContractMust(t, f.db.SaveMemory(ctx, "ses_sources", `{"private":"session memory"}`, "[]"))
	a4ContractMust(t, f.db.IndexHistory(ctx, storage.HistoryDoc{SourceID: "index_only", SessionID: "ses_sources", RawText: "index-only secret", CreatedAt: at}))
	_, err := f.db.AppendEvent(ctx, storage.Event{RunID: root.ID, Type: "stream.delta", Payload: json.RawMessage(`{"text":"uncommitted stream text"}`), Persistent: false, CreatedAt: at})
	if !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("ephemeral fixture must not enter durable storage: %v", err)
	}
	_, err = f.db.AppendEvent(ctx, storage.Event{RunID: root.ID, Type: "model.completed", Payload: json.RawMessage(`{"text":"raw event is not another response"}`), Persistent: true, CreatedAt: at})
	a4ContractMust(t, err)
	f.terminal(t, root, domain.RunFailed, "", "provider stopped\noriginal reason", "run.FAILED", at.Add(8*time.Second))
	f.terminal(t, child, domain.RunCancelled, "", "", "run.cancelled", at.Add(9*time.Second))

	h := f.handler(nil, "")
	page := a4ContractDecodePage(t, a4ContractGET(h, "ses_sources", "limit=200"), "ses_sources")
	r, tr := a4ContractString(root.ID), a4ContractString("turn_root")
	want := []a4ContractMessage{
		{"msg_user_turn_root", "ses_sources", r, tr, "user", "input", "recorded", "same original input\n原文", "", at.Format(a4ContractTime)},
		{"msg_response_rsp_visible", "ses_sources", r, tr, "assistant", "response", "recorded", "A\nBC", "", at.Add(time.Second).Format(a4ContractTime)},
		{"msg_response_rsp_equal", "ses_sources", r, tr, "assistant", "response", "recorded", "A\nBC", "", at.Add(2 * time.Second).Format(a4ContractTime)},
		{"msg_response_rsp_child", "ses_sources", a4ContractString(child.ID), tr, "assistant", "response", "recorded", "child text", "", at.Add(time.Second).Format(a4ContractTime)},
		{"msg_question_inv_question", "ses_sources", r, tr, "assistant", "question", "recorded", "Which region?\n原文", "", at.Add(3 * time.Second).Format(a4ContractTime)},
		{"msg_answer_inv_question", "ses_sources", r, tr, "user", "answer", "recorded", "上海\naccepted", "", ""},
		{"msg_approval_apr_write_requested", "ses_sources", r, tr, "system", "approval_requested", "recorded", "", "", at.Add(4 * time.Second).Format(a4ContractTime)},
		{"msg_approval_apr_write_decision", "ses_sources", r, tr, "system", "approval_decision", "recorded", "deny", "", ""},
		{"msg_approval_apr_pending_requested", "ses_sources", a4ContractString(child.ID), tr, "system", "approval_requested", "recorded", "", "", at.Add(5 * time.Second).Format(a4ContractTime)},
		{"msg_status_run_root", "ses_sources", r, tr, "system", "run_status", "failed", "provider stopped\noriginal reason", "run.FAILED", at.Add(8 * time.Second).Format(a4ContractTime)},
		{"msg_status_run_child", "ses_sources", a4ContractString(child.ID), tr, "system", "run_status", "cancelled", "CANCELLED", "run.cancelled", at.Add(9 * time.Second).Format(a4ContractTime)},
	}
	// A1 requires a user-visible approval summary, not an English prefix.
	// For these ordinary tools, check the stored tool name and exclude raw
	// arguments; compare all other source fields exactly below. The separate
	// run_command test still requires the original command text verbatim.
	approvalMetadata := page
	approvalMetadata.Messages = append([]a4ContractMessage(nil), page.Messages...)
	for i, msg := range approvalMetadata.Messages {
		if msg.ID != "msg_approval_apr_write_requested" && msg.ID != "msg_approval_apr_pending_requested" {
			continue
		}
		if strings.TrimSpace(msg.Content) == "" || !strings.Contains(msg.Content, "write_file") {
			t.Errorf("approval %s must have a nonempty visible summary naming the stored tool: %q", msg.ID, msg.Content)
		}
		if json.Valid([]byte(msg.Content)) || strings.Contains(msg.Content, "\"path\"") || strings.Contains(msg.Content, "\"content\"") || strings.Contains(msg.Content, "raw private tool argument") {
			t.Errorf("approval %s exposes raw tool arguments instead of a visible summary: %q", msg.ID, msg.Content)
		}
		// Only these two summary contents use semantic assertions. Keep the
		// original page intact for the immutable repeated-GET comparison.
		approvalMetadata.Messages[i].Content = ""
	}
	a4ContractWant(t, approvalMetadata, want)
	a4ContractCheckCursor(t, page, "ses_sources", false)
	for _, msg := range page.Messages {
		if msg.ID == "msg_answer_inv_question" {
			accepted, err := time.Parse(a4ContractTime, msg.CreatedAt)
			if err != nil || accepted.Before(acceptedBefore) || accepted.After(acceptedAfter) {
				t.Errorf("accepted-answer time must be its first acceptance, not replay or run update: %s not in [%s, %s]", msg.CreatedAt, acceptedBefore, acceptedAfter)
			}
		}
	}
	again := a4ContractDecodePage(t, a4ContractGET(h, "ses_sources", "limit=200"), "ses_sources")
	if !reflect.DeepEqual(again, page) {
		t.Errorf("read-only repeated GET changed immutable facts: first=%+v again=%+v", page, again)
	}
	other := a4ContractDecodePage(t, a4ContractGET(h, "ses_foreign", ""), "ses_foreign")
	a4ContractWant(t, other, []a4ContractMessage{{"msg_response_rsp_foreign", "ses_foreign", a4ContractString(foreign.ID), nil, "assistant", "response", "recorded", "foreign child must not leak", "", at.Format(a4ContractTime)}})
}

func TestA4MessagesContractLinkedResponseAndFinalAnswerFallback(t *testing.T) {
	for _, linked := range []bool{true, false} {
		t.Run(fmt.Sprintf("linked_%t", linked), func(t *testing.T) {
			f := a4NewContractFixture(t)
			f.session(t, "ses_fallback")
			at := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
			run := f.run(t, "ses_fallback", "run_fallback", "", "", at)
			want := []a4ContractMessage{}
			if linked {
				f.response(t, run.ID, "rsp_linked", at, true, domain.ActionAnswer, domain.VisibleItem{Ordinal: 0, Kind: domain.ItemText, Text: "same final text"})
				want = append(want, a4ContractMessage{"msg_response_rsp_linked", "ses_fallback", a4ContractString(run.ID), nil, "assistant", "response", "recorded", "same final text", "", at.Format(a4ContractTime)})
				// An internal response written later does not erase a retained visible
				// response or make another copy of that answer eligible.
				f.response(t, run.ID, "rsp_later_internal", at.Add(time.Second), true, domain.ActionPlan, domain.VisibleItem{Ordinal: 0, Kind: domain.ItemText, Text: "hidden plan"})
			} else {
				want = append(want, a4ContractMessage{"msg_answer_run_fallback", "ses_fallback", a4ContractString(run.ID), nil, "assistant", "response", "recorded", "same final text", "", at.Add(2 * time.Second).Format(a4ContractTime)})
			}
			f.terminal(t, run, domain.RunCompleted, "same final text", "", "run.completed", at.Add(2*time.Second))
			want = append(want, a4ContractMessage{"msg_status_run_fallback", "ses_fallback", a4ContractString(run.ID), nil, "system", "run_status", "recorded", "COMPLETED", "run.completed", at.Add(2 * time.Second).Format(a4ContractTime)})
			// This witness reports actual A2 projection rows before the HTTP check.
			// It is diagnostic evidence, never the oracle or an HTTP pass.
			witness, err := f.db.ListMessages(context.Background(), "ses_fallback", nil, 200)
			a4ContractMust(t, err)
			for _, msg := range witness.Messages {
				t.Logf("A2 source witness only: id=%s kind=%s content=%q event=%q", msg.ID, msg.Kind, msg.Content, msg.Event)
			}
			page := a4ContractDecodePage(t, a4ContractGET(f.handler(nil, ""), "ses_fallback", ""), "ses_fallback")
			a4ContractWant(t, page, want)
		})
	}
}

func TestA4MessagesContractPaginationAndRestart(t *testing.T) {
	f := a4NewContractFixture(t)
	f.session(t, "ses_pages")
	at := time.Date(2026, 11, 25, 17, 0, 0, 7, time.FixedZone("fixture", 8*3600))
	want := make([]a4ContractMessage, 0, 233)
	// Commit in reverse order and share timestamps across groups: insertion
	// sequence, run grouping, and timestamp-only cursors cannot pass this.
	for i := 232; i >= 0; i-- {
		id := fmt.Sprintf("turn_%03d", i)
		when := at.Add(time.Duration(i/7) * time.Nanosecond)
		f.turn(t, "ses_pages", id, "same text", when)
		want = append(want, a4ContractMessage{"msg_user_" + id, "ses_pages", nil, a4ContractString(id), "user", "input", "recorded", "same text", "", when.UTC().Format(a4ContractTime)})
	}
	sort.Slice(want, func(i, j int) bool {
		if want[i].CreatedAt != want[j].CreatedAt {
			return want[i].CreatedAt < want[j].CreatedAt
		}
		return want[i].ID < want[j].ID
	})
	h := f.handler(nil, "")
	for _, tc := range []struct {
		query string
		limit int
	}{
		{"", 50}, {"cursor=", 50}, {"limit=1", 1}, {"limit=200", 200}, {"limit=%35%30", 50},
		{"limit=2&run_id=ignored&turn_id=ignored&order=desc&other=arbitrary", 2},
	} {
		t.Run("first_page_"+tc.query, func(t *testing.T) {
			page := a4ContractDecodePage(t, a4ContractGET(h, "ses_pages", tc.query), "ses_pages")
			if !reflect.DeepEqual(page.Messages, want[:tc.limit]) {
				t.Errorf("first page differs: got %d messages, want ordered first %d", len(page.Messages), tc.limit)
			}
			a4ContractCheckCursor(t, page, "ses_pages", true)
		})
	}
	t.Run("changing_limits_and_restart", func(t *testing.T) {
		var got []a4ContractMessage
		cursor := ""
		seenCursors := map[string]bool{}
		for pageIndex := 0; pageIndex < 10; pageIndex++ {
			limit := []int{1, 2, 17, 200}[pageIndex%4]
			page := a4ContractDecodePage(t, a4ContractGET(h, "ses_pages", fmt.Sprintf("limit=%d&cursor=%s", limit, url.QueryEscape(cursor))), "ses_pages")
			got = append(got, page.Messages...)
			a4ContractCheckCursor(t, page, "ses_pages", len(got) < len(want))
			if page.NextCursor == "" {
				break
			}
			if seenCursors[page.NextCursor] {
				t.Fatal("cursor cycle")
			}
			seenCursors[page.NextCursor] = true
			cursor = page.NextCursor
			if pageIndex == 0 {
				a4ContractMust(t, f.db.Close())
				var err error
				f.db, err = sqlite.Open(context.Background(), f.path)
				a4ContractMust(t, err)
				h = f.handler(nil, "")
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("complete traversal after reopen: got %d, want %d, ordered identities/content/times must match", len(got), len(want))
		}
	})
	for _, remaining := range []int{0, 1, 33} {
		t.Run(fmt.Sprintf("terminal_page_remaining_%d", remaining), func(t *testing.T) {
			boundary := want[len(want)-remaining-1]
			cursor := a4ContractCursor("ses_pages", boundary.CreatedAt, boundary.ID)
			limit := remaining
			if limit == 0 {
				limit = 1
			}
			page := a4ContractDecodePage(t, a4ContractGET(h, "ses_pages", fmt.Sprintf("limit=%d&cursor=%s", limit, cursor)), "ses_pages")
			a4ContractWant(t, page, want[len(want)-remaining:])
			a4ContractCheckCursor(t, page, "ses_pages", false)
		})
	}
}

func TestA4MessagesContractGoldenCursorAndNoncanonicalInput(t *testing.T) {
	f := a4NewContractFixture(t)
	f.session(t, "ses_demo")
	at := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	f.turn(t, "ses_demo", "trn_demo", "input", at)
	run := f.run(t, "ses_demo", "run_demo", "trn_demo", "", at)
	f.response(t, run.ID, "rsp_demo", at.Add(time.Second), true, domain.ActionAnswer, domain.VisibleItem{Ordinal: 0, Kind: domain.ItemText, Text: "reply"})
	f.turn(t, "ses_demo", "trn_tail", "tail", at.Add(2*time.Second))
	h := f.handler(nil, "")
	const golden = "eyJ2IjoxLCJzZXNzaW9uX2lkIjoic2VzX2RlbW8iLCJjcmVhdGVkX2F0IjoiMjAyNi0xMS0yNVQwOTowMDowMS4wMDAwMDAwMDBaIiwiaWQiOiJtc2dfcmVzcG9uc2VfcnNwX2RlbW8ifQ"
	t.Run("encoder_matches_frozen_example", func(t *testing.T) {
		page := a4ContractDecodePage(t, a4ContractGET(h, "ses_demo", "limit=2"), "ses_demo")
		if page.NextCursor != golden {
			t.Errorf("cursor=%q, want frozen example %q", page.NextCursor, golden)
		}
	})
	for name, cursor := range map[string]string{
		"frozen_example":              golden,
		"member_order_and_whitespace": base64.RawURLEncoding.EncodeToString([]byte(" \n{ \"id\":\"msg_response_rsp_demo\", \"created_at\":\"2026-11-25T09:00:01.000000000Z\", \"session_id\":\"ses_demo\", \"v\": 1 }\t")),
	} {
		t.Run(name, func(t *testing.T) {
			page := a4ContractDecodePage(t, a4ContractGET(h, "ses_demo", "limit=1&cursor="+url.QueryEscape(cursor)), "ses_demo")
			a4ContractWant(t, page, []a4ContractMessage{{"msg_user_trn_tail", "ses_demo", nil, a4ContractString("trn_tail"), "user", "input", "recorded", "tail", "", at.Add(2 * time.Second).Format(a4ContractTime)}})
			a4ContractCheckCursor(t, page, "ses_demo", false)
		})
	}
}

// Observe the persistence boundary without replacing valid reads. Injections
// exercise only failures which are otherwise nondeterministic to provoke.
// Handler startup recovery is excluded from the endpoint call trace.
type a4ContractObservedStore struct {
	storage.Store
	calls      []string
	sessionErr error
	pageErr    error
}

func (s *a4ContractObservedStore) GetSession(ctx context.Context, id string) (storage.Session, error) {
	s.calls = append(s.calls, "session")
	if s.sessionErr != nil {
		return storage.Session{}, s.sessionErr
	}
	return s.Store.GetSession(ctx, id)
}

func (s *a4ContractObservedStore) ListRuns(ctx context.Context, id string) ([]storage.Run, error) {
	s.calls = append(s.calls, "runs")
	return s.Store.ListRuns(ctx, id)
}

func (s *a4ContractObservedStore) ListMessages(ctx context.Context, id string, after *storage.MessageCursor, limit int) (storage.MessagePage, error) {
	s.calls = append(s.calls, "messages")
	if s.pageErr != nil {
		return storage.MessagePage{Messages: []storage.Message{{ID: "must-not-leak-partial-page"}}}, s.pageErr
	}
	return s.Store.ListMessages(ctx, id, after, limit)
}

func TestA4MessagesContractLimitValidation(t *testing.T) {
	f := a4NewContractFixture(t)
	f.session(t, "ses_query")
	observed := &a4ContractObservedStore{Store: f.db}
	h := f.handler(observed, "")
	for _, value := range []string{"", "0", "00", "01", "201", "-1", "+1", " 1", "1 ", "1.0", "1e2", "0x10", "1\n", "１", "١", "999999999999999999999999999999999999999999999999999999"} {
		t.Run("value_"+value, func(t *testing.T) {
			observed.calls = nil
			rec := a4ContractGET(h, "ses_query", "limit="+url.QueryEscape(value))
			a4ContractError(t, rec, 400, "invalid_argument", "invalid message limit", "")
			if len(observed.calls) != 0 {
				t.Errorf("invalid limit reached storage: %v", observed.calls)
			}
		})
	}
	for _, query := range []string{"limit", "limit=1&limit=1", "limit=1&limit=2", "limit=2&%6cimit=2", "limit=%2531"} {
		t.Run(query, func(t *testing.T) {
			observed.calls = nil
			a4ContractError(t, a4ContractGET(h, "ses_query", query), 400, "invalid_argument", "invalid message limit", "")
			if len(observed.calls) != 0 {
				t.Errorf("invalid limit reached storage: %v", observed.calls)
			}
		})
	}
	for _, query := range []string{"limit=1", "limit=200", "limit=50", "limit=%32%30%30", "cursor=&other=ignored"} {
		t.Run("valid_"+query, func(t *testing.T) {
			page := a4ContractDecodePage(t, a4ContractGET(h, "ses_query", query), "ses_query")
			a4ContractWant(t, page, nil)
			a4ContractCheckCursor(t, page, "ses_query", false)
		})
	}
}

func TestA4MessagesContractStrictCursorV1(t *testing.T) {
	f := a4NewContractFixture(t)
	f.session(t, "ses_cursor")
	observed := &a4ContractObservedStore{Store: f.db}
	h := f.handler(observed, "")
	const body = `{"v":1,"session_id":"ses_cursor","created_at":"2026-11-25T09:00:00.000000000Z","id":"anchor"}`
	encode := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	valid := encode(body)
	badJSON := map[string]string{
		"not_json": "garbage", "null_object": "null", "array": "[]", "trailing_object": body + "{}", "trailing_text": body + "x",
		"extra_member":          strings.TrimSuffix(body, "}") + `,"extra":0}`,
		"version_zero":          strings.Replace(body, `"v":1`, `"v":0`, 1),
		"version_future":        strings.Replace(body, `"v":1`, `"v":2`, 1),
		"version_decimal":       strings.Replace(body, `"v":1`, `"v":1.0`, 1),
		"version_exponent":      strings.Replace(body, `"v":1`, `"v":1e0`, 1),
		"version_string":        strings.Replace(body, `"v":1`, `"v":"1"`, 1),
		"version_bool":          strings.Replace(body, `"v":1`, `"v":true`, 1),
		"session_empty":         strings.Replace(body, `"ses_cursor"`, `""`, 1),
		"session_number":        strings.Replace(body, `"ses_cursor"`, "12", 1),
		"session_case_mismatch": strings.Replace(body, "ses_cursor", "SES_cursor", 1),
		"session_other":         strings.Replace(body, "ses_cursor", "ses_other", 1),
		"id_empty":              strings.Replace(body, `"anchor"`, `""`, 1),
		"id_number":             strings.Replace(body, `"anchor"`, "42", 1),
		"id_non_ascii":          strings.Replace(body, "anchor", "锚点", 1),
		"id_unpaired_surrogate": strings.Replace(body, "anchor", `\ud800`, 1),
		"invalid_utf8":          strings.Replace(body, "anchor", string([]byte{0xff}), 1),
	}
	for _, member := range []struct{ key, value string }{{"v", "1"}, {"session_id", `"ses_cursor"`}, {"created_at", `"2026-11-25T09:00:00.000000000Z"`}, {"id", `"anchor"`}} {
		pair := fmt.Sprintf("%q:%s", member.key, member.value)
		badJSON["missing_"+member.key] = strings.Replace(strings.Replace(body, pair+",", "", 1), ","+pair, "", 1)
		badJSON["null_"+member.key] = strings.Replace(body, pair, fmt.Sprintf("%q:null", member.key), 1)
		badJSON["duplicate_"+member.key] = strings.TrimSuffix(body, "}") + "," + pair + "}"
		badJSON["wrong_key_case_"+member.key] = strings.Replace(body, fmt.Sprintf("%q:", member.key), fmt.Sprintf("%q:", strings.ToUpper(member.key)), 1)
	}
	badJSON["escaped_duplicate_key"] = strings.TrimSuffix(body, "}") + `,"\u0076":1}`
	for _, timestamp := range []string{
		"2026-11-25T09:00:00Z", "2026-11-25T09:00:00.000Z", "2026-11-25T09:00:00.0000000000Z",
		"2026-11-25T09:00:00.000000000+00:00", "2026-11-25T17:00:00.000000000+08:00",
		"2026-11-25T09:00:00.000000000z", "2026-02-29T09:00:00.000000000Z", "2026-11-31T09:00:00.000000000Z",
		"2026-13-01T09:00:00.000000000Z", "2026-11-25T24:00:00.000000000Z", "2026-11-25T09:00:60.000000000Z", "not-a-time",
	} {
		badJSON["date_"+timestamp] = strings.Replace(body, "2026-11-25T09:00:00.000000000Z", timestamp, 1)
	}
	badJSON["date_number"] = strings.Replace(body, `"2026-11-25T09:00:00.000000000Z"`, "5", 1)
	badWire := map[string]string{
		"padding": valid + "=", "space": " " + valid, "newline": valid + "\n", "whitespace_only": " \t",
		"plus": "+" + valid[1:], "slash": "/" + valid[1:], "non_alphabet": "!", "impossible_length": "A",
		"double_url_escape": "%" + valid,
	}
	// Alter only the unused low bits of a valid base64url final sextet.
	// Verify the test vector itself: permissive decoding preserves bytes while
	// strict decoding refuses it. This does not duplicate the cursor parser.
	trailingBody := body
	for len(trailingBody)%3 == 0 {
		trailingBody += " "
	}
	canonical := encode(trailingBody)
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alphabet, canonical[len(canonical)-1])
	noncanonical := canonical[:len(canonical)-1] + string(alphabet[last|1])
	decoded, err := base64.RawURLEncoding.DecodeString(noncanonical)
	if err != nil || string(decoded) != trailingBody {
		t.Fatalf("bad trailing-bits fixture is not byte-equivalent: %v", err)
	}
	if _, err := base64.RawURLEncoding.Strict().DecodeString(noncanonical); err == nil {
		t.Fatal("trailing-bits fixture must fail strict decoding")
	}
	badWire["invalid_trailing_bits"] = noncanonical
	for name, raw := range badJSON {
		badWire[name] = encode(raw)
	}
	for name, cursor := range badWire {
		t.Run(name, func(t *testing.T) {
			observed.calls = nil
			a4ContractError(t, a4ContractGET(h, "ses_cursor", "cursor="+url.QueryEscape(cursor)), 400, "invalid_argument", "invalid message cursor", "")
			if len(observed.calls) != 0 {
				t.Errorf("invalid cursor reached storage before rejection: %v", observed.calls)
			}
		})
	}
	for _, query := range []string{"cursor=&cursor=", "cursor=" + valid + "&cursor=" + valid, "cursor=&%63ursor=", "cursor=" + url.QueryEscape(url.QueryEscape("+"+valid))} {
		t.Run("query_"+query, func(t *testing.T) {
			observed.calls = nil
			a4ContractError(t, a4ContractGET(h, "ses_cursor", query), 400, "invalid_argument", "invalid message cursor", "")
			if len(observed.calls) != 0 {
				t.Errorf("invalid cursor reached storage: %v", observed.calls)
			}
		})
	}
}

func TestA4MessagesContractValidationPriorityAndExactErrors(t *testing.T) {
	f := a4NewContractFixture(t)
	f.session(t, "ses_priority")
	observed := &a4ContractObservedStore{Store: f.db}
	h := f.handler(observed, "")
	for _, tc := range []struct {
		name, session, query, message string
		status                        int
	}{
		{"escaping_before_both", "ses_missing", "cursor=!&limit=0&other=%GG", "invalid message query", 400},
		{"escaping_in_key", "ses_missing", "%GG=x&limit=0", "invalid message query", 400},
		{"bare_percent", "ses_missing", "other=%", "invalid message query", 400},
		{"semicolon", "ses_missing", "other=a;b&limit=0", "invalid message query", 400},
		{"limit_before_cursor", "ses_missing", "limit=0&cursor=!", "invalid message limit", 400},
		{"query_order_does_not_change_priority", "ses_missing", "cursor=!&limit=0", "invalid message limit", 400},
		{"cursor_before_lookup", "ses_missing", "limit=1&cursor=!", "invalid message cursor", 400},
		{"binding_before_lookup", "ses_missing", "cursor=" + a4ContractCursor("ses_priority", "2026-11-25T09:00:00.000000000Z", "unknown"), "invalid message cursor", 400},
		{"unknown_session", "ses_missing", "", "record not found", 404},
		{"beyond_end_cannot_hide_missing_session", "ses_missing", "cursor=" + a4ContractCursor("ses_missing", "2099-01-01T00:00:00.000000000Z", "unknown"), "record not found", 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observed.calls = nil
			code, ref := "invalid_argument", ""
			if tc.status == 404 {
				code, ref = "not_found", tc.session
			}
			a4ContractError(t, a4ContractGET(h, tc.session, tc.query), tc.status, code, tc.message, ref)
			if tc.status == 400 && len(observed.calls) != 0 {
				t.Errorf("validation must precede session lookup/page: %v", observed.calls)
			}
			if tc.status == 404 && !reflect.DeepEqual(observed.calls, []string{"session"}) {
				t.Errorf("unknown session must be looked up before page read: %v", observed.calls)
			}
		})
	}
	for _, fault := range []string{"session_lookup", "page_read"} {
		t.Run("storage_failure_"+fault, func(t *testing.T) {
			observed.calls = nil
			if fault == "session_lookup" {
				observed.sessionErr = errors.New("private lookup diagnostic")
			} else {
				observed.pageErr = errors.New("private SQL page diagnostic")
			}
			t.Cleanup(func() { observed.sessionErr, observed.pageErr = nil, nil })
			a4ContractError(t, a4ContractGET(h, "ses_priority", ""), 500, "internal", "request failed", "")
			wantCalls := []string{"session"}
			if fault == "page_read" {
				wantCalls = append(wantCalls, "messages")
			}
			if !reflect.DeepEqual(observed.calls, wantCalls) {
				t.Errorf("storage failure call order=%v, want %v", observed.calls, wantCalls)
			}
		})
	}
}

func TestA4MessagesContractGuardsBeforeQueryAndLookup(t *testing.T) {
	for _, token := range []string{"", a4ContractToken} {
		t.Run(fmt.Sprintf("protected_%t", token != ""), func(t *testing.T) {
			f := a4NewContractFixture(t)
			f.session(t, "ses_guard")
			observed := &a4ContractObservedStore{Store: f.db}
			h := f.handler(observed, token)
			cases := []struct {
				name, host, origin, authorization string
				status                            int
				message                           string
			}{
				{"host_before_origin_auth_and_query", "foreign.invalid", "null", "", 403, "Host header is not a loopback address or an allowed host"},
				{"origin_before_auth_and_query", "localhost:19421", "https://foreign.invalid", "", 403, "cross-origin request refused"},
				{"null_origin", "localhost:19421", "null", "", 403, "cross-origin request refused"},
			}
			if token != "" {
				cases = append(cases,
					struct {
						name, host, origin, authorization string
						status                            int
						message                           string
					}{"missing_bearer_before_query", "localhost:19421", "http://localhost:19421", "", 401, "missing or invalid bearer token"},
					struct {
						name, host, origin, authorization string
						status                            int
						message                           string
					}{"bad_bearer_before_query", "localhost:19421", "", "Bearer invalid", 401, "missing or invalid bearer token"},
				)
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					observed.calls = nil
					req := httptest.NewRequest(http.MethodGet, "/v1/sessions/ses_missing/messages", nil)
					req.URL.RawQuery = "limit=0&cursor=!&other=%GG"
					req.Host = tc.host
					if tc.origin != "" {
						req.Header.Set("Origin", tc.origin)
					}
					if tc.authorization != "" {
						req.Header.Set("Authorization", tc.authorization)
					}
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, req)
					a4ContractError(t, rec, tc.status, "auth_denied", tc.message, "")
					if len(observed.calls) != 0 {
						t.Errorf("refused request reached lookup/query: %v", observed.calls)
					}
				})
			}
			// A matching-origin, valid-bearer control must reach the endpoint.
			t.Run("authorized_control", func(t *testing.T) {
				observed.calls = nil
				req := httptest.NewRequest(http.MethodGet, "/v1/sessions/ses_guard/messages", nil)
				req.Host = "localhost:19421"
				req.Header.Set("Origin", "https://localhost:19421")
				if token != "" {
					req.Header.Set("Authorization", "Bearer "+token)
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				page := a4ContractDecodePage(t, rec, "ses_guard")
				a4ContractCheckCursor(t, page, "ses_guard", false)
				if !reflect.DeepEqual(observed.calls, []string{"session", "messages"}) {
					t.Errorf("authorized request call order=%v", observed.calls)
				}
			})
		})
	}
}

func TestA4MessagesContractLogicalBoundariesAndLiveInserts(t *testing.T) {
	f := a4NewContractFixture(t)
	f.session(t, "ses_live")
	at := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	for _, id := range []string{"turn_m", "turn_z"} {
		f.turn(t, "ses_live", id, id, at)
	}
	h := f.handler(nil, "")
	for _, tc := range []struct {
		name, stamp, id string
		count           int
	}{
		{"absent_anchor_same_timestamp", at.Format(a4ContractTime), "msg_user_turn_n", 1},
		{"old_cursor_has_no_expiry", "2000-01-01T00:00:00.000000000Z", "old_unknown_anchor", 2},
		{"beyond_end", "2099-01-01T00:00:00.000000000Z", "unknown_anchor", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cursor := a4ContractCursor("ses_live", tc.stamp, tc.id)
			page := a4ContractDecodePage(t, a4ContractGET(h, "ses_live", "cursor="+cursor), "ses_live")
			if len(page.Messages) != tc.count {
				t.Errorf("logical boundary returned %d messages, want %d", len(page.Messages), tc.count)
			}
			if tc.name == "absent_anchor_same_timestamp" && len(page.Messages) == 1 && page.Messages[0].ID != "msg_user_turn_z" {
				t.Errorf("wrong result after absent anchor: %+v", page.Messages)
			}
			a4ContractCheckCursor(t, page, "ses_live", false)
		})
	}
	t.Run("insert_before_and_after_boundary", func(t *testing.T) {
		first := a4ContractDecodePage(t, a4ContractGET(h, "ses_live", "limit=1"), "ses_live")
		a4ContractCheckCursor(t, first, "ses_live", true)
		f.turn(t, "ses_live", "turn_a", "backdated insert", at)
		f.turn(t, "ses_live", "turn_n", "later insert", at)
		next := a4ContractDecodePage(t, a4ContractGET(h, "ses_live", "limit=200&cursor="+first.NextCursor), "ses_live")
		a4ContractWant(t, next, []a4ContractMessage{
			{"msg_user_turn_n", "ses_live", nil, a4ContractString("turn_n"), "user", "input", "recorded", "later insert", "", at.Format(a4ContractTime)},
			{"msg_user_turn_z", "ses_live", nil, a4ContractString("turn_z"), "user", "input", "recorded", "turn_z", "", at.Format(a4ContractTime)},
		})
		a4ContractCheckCursor(t, next, "ses_live", false)
		refresh := a4ContractDecodePage(t, a4ContractGET(h, "ses_live", ""), "ses_live")
		if len(refresh.Messages) != 4 || refresh.Messages[0].ID != "msg_user_turn_a" {
			t.Errorf("new traversal must include backdated insert: %+v", refresh.Messages)
		}
	})
}

func TestA4MessagesContractDeletedAnchorAndNoGhosts(t *testing.T) {
	f := a4NewContractFixture(t)
	f.session(t, "ses_delete")
	at := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	old := f.run(t, "ses_delete", "run_old", "", "", at)
	f.response(t, old.ID, "rsp_old", at, true, domain.ActionAnswer, domain.VisibleItem{Ordinal: 0, Kind: domain.ItemText, Text: "prunable"})
	f.terminal(t, old, domain.RunCompleted, "", "", "run.completed", at.Add(time.Second))
	keep := f.run(t, "ses_delete", "run_keep", "", "", at.Add(time.Hour))
	f.response(t, keep.ID, "rsp_keep", at.Add(time.Hour), true, domain.ActionAnswer, domain.VisibleItem{Ordinal: 0, Kind: domain.ItemText, Text: "retained"})
	h := f.handler(nil, "")
	first := a4ContractDecodePage(t, a4ContractGET(h, "ses_delete", "limit=1"), "ses_delete")
	if len(first.Messages) != 1 || first.Messages[0].ID != "msg_response_rsp_old" {
		t.Fatalf("first page anchor=%+v, want old response", first.Messages)
	}
	a4ContractCheckCursor(t, first, "ses_delete", true)
	pruned, err := f.db.PruneRuns(context.Background(), at.Add(time.Minute))
	a4ContractMust(t, err)
	if pruned.RunsDeleted != 1 {
		t.Fatalf("fixture pruning deleted %d runs, want 1", pruned.RunsDeleted)
	}
	for name, query := range map[string]string{"deleted_anchor": "cursor=" + first.NextCursor, "refresh_no_ghosts": ""} {
		t.Run(name, func(t *testing.T) {
			page := a4ContractDecodePage(t, a4ContractGET(h, "ses_delete", query), "ses_delete")
			a4ContractWant(t, page, []a4ContractMessage{{"msg_response_rsp_keep", "ses_delete", a4ContractString(keep.ID), nil, "assistant", "response", "recorded", "retained", "", at.Add(time.Hour).Format(a4ContractTime)}})
			a4ContractCheckCursor(t, page, "ses_delete", false)
		})
	}
}

func TestA4MessagesContractApprovalCommandSummaryAndDecision(t *testing.T) {
	for _, state := range []string{"approved", "denied", "pending"} {
		t.Run(state, func(t *testing.T) {
			f := a4NewContractFixture(t)
			f.session(t, "ses_command")
			ctx := context.Background()
			at := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
			run := f.run(t, "ses_command", "run_command", "", "", at)
			const command = "printf 'review this command\\n'"
			args, err := json.Marshal(map[string]string{"command": command})
			a4ContractMust(t, err)
			f.response(t, run.ID, "rsp_command", at, true, domain.ActionExecute)
			batch := storage.StoredBatch{ID: "batch_command", ModelResponseID: "rsp_command", RunID: run.ID, Action: domain.ActionExecute, Status: domain.BatchWaitingApproval,
				Invocations: []domain.Invocation{{ID: "inv_command", Ordinal: 0, ProviderCallID: "call_command", Name: "run_command", Arguments: args, Class: domain.ActionExecute, Status: domain.InvPending}}}
			a4ContractMust(t, f.db.SaveBatch(ctx, batch))
			a4ContractMust(t, f.db.SaveApproval(ctx, domain.Approval{ID: "apr_command", RunID: run.ID, InvocationID: "inv_command", Tool: "run_command", State: "pending", CreatedAt: at}))
			if state != "pending" {
				a4ContractMust(t, f.db.UpdateApprovalState(ctx, run.ID, "inv_command", state))
				a4ContractMust(t, f.db.UpdateApprovalState(ctx, run.ID, "inv_command", state))
			}
			want := []a4ContractMessage{{"msg_approval_apr_command_requested", "ses_command", a4ContractString(run.ID), nil, "system", "approval_requested", "recorded", command, "", at.Format(a4ContractTime)}}
			if state != "pending" {
				decision := "approve"
				if state == "denied" {
					decision = "deny"
				}
				want = append(want, a4ContractMessage{"msg_approval_apr_command_decision", "ses_command", a4ContractString(run.ID), nil, "system", "approval_decision", "recorded", decision, "", ""})
			}
			witness, err := f.db.ListMessages(ctx, "ses_command", nil, 200)
			a4ContractMust(t, err)
			for _, msg := range witness.Messages {
				t.Logf("A2 source witness only: id=%s kind=%s content=%q event=%q", msg.ID, msg.Kind, msg.Content, msg.Event)
			}
			page := a4ContractDecodePage(t, a4ContractGET(f.handler(nil, ""), "ses_command", ""), "ses_command")
			a4ContractWant(t, page, want)
		})
	}
}

func TestA4MessagesContractGETDoesNotWriteSQLite(t *testing.T) {
	f := a4NewContractFixture(t)
	f.session(t, "ses_readonly")
	at := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	f.turn(t, "ses_readonly", "turn_readonly", "retained original", at)
	h := f.handler(nil, "")
	// A separate read-only connection observes commits made by the handler's
	// connection. Establish the baseline after unrelated Handler startup work.
	monitor, err := sql.Open("sqlite", "file:"+f.path+"?mode=ro")
	a4ContractMust(t, err)
	defer func() { _ = monitor.Close() }()
	monitor.SetMaxOpenConns(1)
	var before, after int64
	a4ContractMust(t, monitor.QueryRow("PRAGMA data_version").Scan(&before))
	page := a4ContractDecodePage(t, a4ContractGET(h, "ses_readonly", ""), "ses_readonly")
	a4ContractWant(t, page, []a4ContractMessage{{"msg_user_turn_readonly", "ses_readonly", nil, a4ContractString("turn_readonly"), "user", "input", "recorded", "retained original", "", at.Format(a4ContractTime)}})
	a4ContractMust(t, monitor.QueryRow("PRAGMA data_version").Scan(&after))
	if before != after {
		t.Errorf("GET committed a SQLite write: data_version %d -> %d", before, after)
	}
}

func TestA4MessagesContractYearOneCursorBoundary(t *testing.T) {
	f := a4NewContractFixture(t)
	f.session(t, "ses_year_one")
	at := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	f.turn(t, "ses_year_one", "turn_year_one", "retained original", at)
	cursor := a4ContractCursor("ses_year_one", "0001-01-01T00:00:00.000000000Z", "msg_unknown")
	page := a4ContractDecodePage(t, a4ContractGET(f.handler(nil, ""), "ses_year_one", "cursor="+cursor), "ses_year_one")
	a4ContractWant(t, page, []a4ContractMessage{{"msg_user_turn_year_one", "ses_year_one", nil, a4ContractString("turn_year_one"), "user", "input", "recorded", "retained original", "", at.Format(a4ContractTime)}})
}
