package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	contextpkg "github.com/cloud-harness/cloud-harness/internal/context"
	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/provider"
	"github.com/cloud-harness/cloud-harness/internal/storage"
)

func TestCrossTurnFactAndHistoryByID(t *testing.T) {
	ctx := context.Background()
	first := &provider.Mock{Responses: []domain.ModelResponse{{
		Provider: "mock", Complete: true,
		Items: []domain.ResponseItem{{Ordinal: 0, Kind: domain.ItemToolCall, ToolCall: &domain.ToolCall{
			ProviderCallID: "f1", Name: "finish_turn",
			Arguments: json.RawMessage(`{"final_answer":"use 8080","turn_summary":"chose port 8080","memory_delta":[{"op":"ADD","section":"facts","id":"port","text":"port is 8080","source":"turn"}],"source_refs":["e1"]}`),
		}}},
	}}}
	m, db, _ := testManager(t, first)
	ses, _ := m.CreateSession(ctx)
	run, err := m.RunMessage(ctx, ses.ID, "choose a port")
	if err != nil || run.Status != domain.RunCompleted || run.FinalAnswer != "use 8080" {
		t.Fatalf("%+v %v", run, err)
	}
	if run.Budget.UsedModelCalls != 1 {
		t.Fatalf("planner ran on a short turn: %+v", run.Budget)
	}
	doc, err := db.GetHistory(ctx, run.TurnID)
	if err != nil || doc.RawText != "choose a port" {
		t.Fatalf("history by id %+v %v", doc, err)
	}
	mem, _, err := db.LatestMemory(ctx, ses.ID)
	if err != nil || !strings.Contains(mem, "port is 8080") {
		t.Fatalf("memory %s %v", mem, err)
	}
	second := &provider.Mock{Responses: []domain.ModelResponse{textOnly("8080")}}
	m.Provider = second
	again, err := m.RunMessage(ctx, ses.ID, "what port did we choose?")
	if err != nil || again.FinalAnswer != "8080" || again.ID == run.ID {
		t.Fatalf("second %+v %v", again, err)
	}
	blob := messageText(second.Requests[0].Messages)
	if !strings.Contains(blob, "port is 8080") || !strings.Contains(blob, "what port did we choose?") {
		t.Fatalf("suffix missed the fact: %s", blob)
	}
	if contextpkg.PrefixHash(toContext(second.Requests[0].Prefix)) != contextpkg.PrefixHash(toContext(first.Requests[0].Prefix)) {
		t.Fatal("prefix changed between turns")
	}
}

func TestPosthocSummaryFailureDoesNotChangeAnswer(t *testing.T) {
	ctx := context.Background()
	m, db, _ := testManager(t, &provider.Mock{Responses: []domain.ModelResponse{textOnly("the port is 8080")}})
	m.Summarizer = failingProvider{}
	ses, _ := m.CreateSession(ctx)
	run, err := m.RunMessage(ctx, ses.ID, "remember the port")
	if err != nil || run.Status != domain.RunCompleted || run.FinalAnswer != "the port is 8080" {
		t.Fatalf("%+v %v", run, err)
	}
	if _, err := db.GetTurnSummary(ctx, run.TurnID); err == nil {
		t.Fatal("failed summary was stored")
	}
	m.Summarizer = summaryProvider{text: `{"turn_summary":"port 8080","memory_delta":[{"op":"ADD","fact":"port is 8080"}]}`}
	m.Provider = &provider.Mock{Responses: []domain.ModelResponse{textOnly("ok")}}
	if _, err := m.RunMessage(ctx, ses.ID, "next"); err != nil {
		t.Fatal(err)
	}
	summary, err := db.GetTurnSummary(ctx, run.TurnID)
	if err != nil || summary != "port 8080" {
		t.Fatalf("retry summary %q %v", summary, err)
	}
	same, _ := db.GetRun(ctx, run.ID)
	if same.FinalAnswer != "the port is 8080" {
		t.Fatalf("answer changed %q", same.FinalAnswer)
	}
}

func TestCompactionKeepsPairingAndOpaque(t *testing.T) {
	ctx := context.Background()
	sig := []byte("SIG-OPAQUE-XYZ")
	payload := []byte("SECRET-PAYLOAD")
	long := strings.Repeat("line of log ", 40)
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		{
			Provider: "mock", Complete: true,
			Items: []domain.ResponseItem{
				{Ordinal: 0, Kind: domain.ItemToolCall, ToolCall: &domain.ToolCall{ProviderCallID: "c1", Name: "read_file", Arguments: json.RawMessage(`{"path":"log.txt"}`)}},
			},
		},
		{
			Provider: "mock", Complete: true,
			Items: []domain.ResponseItem{
				{Ordinal: 0, Kind: domain.ItemOpaque, Opaque: &domain.OpaqueItem{Provider: "mock", Kind: "reasoning", Payload: append([]byte(nil), payload...), Signature: append([]byte(nil), sig...)}},
				{Ordinal: 1, Kind: domain.ItemToolCall, ToolCall: &domain.ToolCall{ProviderCallID: "c2", Name: "read_file", Arguments: json.RawMessage(`{"path":"note.txt"}`)}},
			},
		},
		textOnly("done"),
	}}
	m, db, root := testManager(t, mock)
	m.ContextTokenLimit = 80
	if err := os.WriteFile(filepath.Join(root, "log.txt"), []byte(long), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	ses, _ := m.CreateSession(ctx)
	run, err := m.RunMessage(ctx, ses.ID, "read the log")
	if err != nil || run.Status != domain.RunCompleted {
		t.Fatalf("%+v %v", run, err)
	}
	if len(mock.Requests) < 3 {
		t.Fatal("third call missing")
	}
	third := mock.Requests[2]
	if len(third.Continuation) == 0 || third.Continuation[0].Opaque == nil || !bytesEqual(third.Continuation[0].Opaque.Signature, sig) || !bytesEqual(third.Continuation[0].Opaque.Payload, payload) {
		t.Fatalf("opaque changed %#v", third.Continuation)
	}
	if contextpkg.PrefixHash(toContext(mock.Requests[0].Prefix)) != contextpkg.PrefixHash(toContext(third.Prefix)) {
		t.Fatal("compaction changed the prefix")
	}
	body := messageText(third.Messages)
	if !strings.Contains(body, "c1") || !strings.Contains(body, "c2") || !strings.Contains(body, "compacted") || !strings.Contains(body, "ok") {
		t.Fatalf("pair not preserved: %s", body)
	}
	if strings.Contains(body, string(payload)) || strings.Contains(body, string(sig)) {
		t.Fatal("compacted messages contain opaque bytes")
	}
	hits, err := db.SearchHistory(ctx, ses.ID, "SECRET-PAYLOAD", 10)
	if err != nil || len(hits) != 0 {
		t.Fatalf("opaque entered history %#v %v", hits, err)
	}
}

func TestSessionHistoryExactChineseAndPath(t *testing.T) {
	ctx := context.Background()
	m, db, _ := testManager(t, &provider.Mock{})
	ses, _ := m.CreateSession(ctx)
	if _, err := m.RunMessage(ctx, ses.ID, "会议记录讨论了检索的索引策略"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RunMessage(ctx, ses.ID, "see internal/server.go event evt_9f3a2c"); err != nil {
		t.Fatal(err)
	}
	if err := db.IndexHistory(ctx, storage.HistoryDoc{
		SourceID: "note_words", SessionID: ses.ID, RawText: "the server and go modules were discussed", CreatedAt: m.now(),
	}); err != nil {
		t.Fatal(err)
	}
	hits, err := db.SearchHistory(ctx, ses.ID, "检索", 10)
	if err != nil || len(hits) == 0 {
		t.Fatalf("chinese %#v %v", hits, err)
	}
	cloud, err := db.SearchHistory(ctx, ses.ID, "云计算", 10)
	if err != nil || len(cloud) != 0 {
		t.Fatalf("云计算 hit %#v %v", cloud, err)
	}
	paths, err := db.SearchHistory(ctx, ses.ID, "server.go", 10)
	if err != nil || len(paths) == 0 {
		t.Fatalf("path %#v %v", paths, err)
	}
	for _, hit := range paths {
		if hit.SourceID == "note_words" || !strings.Contains(hit.RawText, "evt_9f3a2c") {
			t.Fatalf("path hit %#v", hit)
		}
	}
	idHits, err := db.SearchHistory(ctx, ses.ID, "evt_9f3a2c", 10)
	if err != nil || len(idHits) == 0 {
		t.Fatalf("id %#v %v", idHits, err)
	}
	for _, hit := range idHits {
		if hit.SourceID == "note_words" {
			t.Fatalf("id hit the words row %#v", hit)
		}
	}
}

type failingProvider struct{}

func (failingProvider) Name() string { return "fail" }

func (failingProvider) Complete(context.Context, provider.Request) (domain.ModelResponse, error) {
	return domain.ModelResponse{}, domain.E(domain.CodeToolError, "summarizer unavailable")
}

type summaryProvider struct{ text string }

func (summaryProvider) Name() string { return "summary" }

func (s summaryProvider) Complete(context.Context, provider.Request) (domain.ModelResponse, error) {
	return textOnly(s.text), nil
}

func textOnly(text string) domain.ModelResponse {
	return domain.ModelResponse{Provider: "mock", Complete: true, Items: []domain.ResponseItem{{Ordinal: 0, Kind: domain.ItemText, Text: text}}}
}

func messageText(msgs []provider.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Content)
		b.WriteByte('\n')
		for _, c := range m.ToolCalls {
			b.WriteString(c.ProviderCallID)
			b.WriteByte('\n')
		}
		b.WriteString(m.ToolCallID)
		b.WriteByte('\n')
	}
	return b.String()
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPlannerReceivesHistoryCatalog(t *testing.T) {
	ctx := context.Background()
	seedMock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{
			ProviderCallID: "f1",
			Name:           domain.ToolFinishTurn,
			Arguments:      json.RawMessage(`{"final_answer":"seeded","turn_summary":"earlier turn configured redis cache"}`),
		}),
	}}
	m, db, _ := testManager(t, seedMock)
	ses, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seedRun, err := m.RunMessage(ctx, ses.ID, "configure cache")
	if err != nil || seedRun.Status != domain.RunCompleted {
		t.Fatalf("seed run: %+v %v", seedRun, err)
	}

	knownDocID := "inv_known_catalog_doc_42"
	knownText := "production redis endpoint is redis.internal:6379 with auth enabled"
	meta, _ := json.Marshal(map[string]string{"kind": "tool"})
	if err := db.IndexHistory(ctx, storage.HistoryDoc{
		SourceID:  knownDocID,
		SessionID: ses.ID,
		RawText:   knownText,
		Metadata:  meta,
		CreatedAt: m.now(),
	}); err != nil {
		t.Fatal(err)
	}

	var plannerReq provider.Request
	var mainReq provider.Request
	callCount := 0
	m.ForcePlanner = true
	m.Provider = providerFunc(func(_ context.Context, req provider.Request) (domain.ModelResponse, error) {
		callCount++
		if callCount == 1 {
			plannerReq = req
			return textOnly(`{"resources":[{"id":"` + knownDocID + `","precision":"verbatim"}]}`), nil
		}
		mainReq = req
		return textOnly("redis.internal:6379"), nil
	})

	run, err := m.RunMessage(ctx, ses.ID, "What was the redis endpoint from earlier?")
	if err != nil || run.Status != domain.RunCompleted || run.FinalAnswer != "redis.internal:6379" {
		t.Fatalf("planned run: %+v %v", run, err)
	}
	if callCount != 2 {
		t.Fatalf("expected 2 model calls (planner + main), got %d", callCount)
	}
	plannerInput := messageText(plannerReq.Messages)
	if !strings.Contains(plannerInput, knownDocID) {
		t.Fatalf("planner request input missing known doc ID %q:\n%s", knownDocID, plannerInput)
	}
	if !strings.Contains(plannerInput, "What was the redis endpoint from earlier?") {
		t.Fatalf("planner request input missing user text:\n%s", plannerInput)
	}
	if !strings.Contains(plannerInput, "earlier turn configured redis cache") {
		t.Fatalf("planner request input missing turn summary catalog entry:\n%s", plannerInput)
	}

	mainInput := messageText(mainReq.Messages)
	if !strings.Contains(mainInput, knownText) {
		t.Fatalf("main request missing retrieved doc content:\n%s", mainInput)
	}
	if !strings.Contains(mainInput, "earlier turn configured redis cache") {
		t.Fatalf("main request missing recent turn summary from Assemble:\n%s", mainInput)
	}
}

func TestCompactedRefResolvesViaRetrieve(t *testing.T) {
	ctx := context.Background()
	longContent := "line-1: alpha-secret-token-99\nline-2:\t\"quoted-value\"\\backslash\r\n" + strings.Repeat("verbose log entry line\n", 30)
	var capturedRef string
	step := 0

	m, db, root := testManager(t, &provider.Mock{})
	m.ContextTokenLimit = 80
	m.SkipPlanner = true
	if err := os.WriteFile(filepath.Join(root, "big.log"), []byte(longContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "small.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	m.Provider = providerFunc(func(_ context.Context, req provider.Request) (domain.ModelResponse, error) {
		step++
		switch step {
		case 1:
			return toolResp(domain.ToolCall{
				ProviderCallID: "provider_call_1",
				Name:           "read_file",
				Arguments:      json.RawMessage(`{"path":"big.log"}`),
			}), nil
		case 2:
			return toolResp(domain.ToolCall{
				ProviderCallID: "provider_call_2",
				Name:           "read_file",
				Arguments:      json.RawMessage(`{"path":"small.txt"}`),
			}), nil
		case 3:
			// Find the compacted tool message for provider_call_1 and extract its ref.
			for _, msg := range req.Messages {
				if msg.Role != "tool" || msg.ToolCallID != "provider_call_1" {
					continue
				}
				var placeholder struct {
					Compacted  bool   `json:"compacted"`
					ToolCallID string `json:"tool_call_id"`
					Excerpt    string `json:"excerpt"`
					Ref        string `json:"ref"`
				}
				if err := json.Unmarshal([]byte(msg.Content), &placeholder); err != nil {
					t.Fatalf("compacted placeholder is invalid JSON: %v (%s)", err, msg.Content)
				}
				if !placeholder.Compacted {
					t.Fatalf("expected provider_call_1 to be compacted, got: %s", msg.Content)
				}
				capturedRef = placeholder.Ref
			}
			if capturedRef == "" {
				t.Fatalf("did not find compacted ref in request messages: %+v", req.Messages)
			}
			if capturedRef == "provider_call_1" {
				t.Fatalf("compacted ref %q used provider_call_id instead of history_docs invocation ID", capturedRef)
			}
			return toolResp(domain.ToolCall{
				ProviderCallID: "provider_call_3",
				Name:           "retrieve_resource",
				Arguments:      json.RawMessage(`{"id":"` + capturedRef + `","precision":"verbatim"}`),
			}), nil
		default:
			for _, msg := range req.Messages {
				if msg.Role == "tool" && msg.ToolCallID == "provider_call_3" {
					if !strings.Contains(msg.Content, "alpha-secret-token-99") {
						t.Fatalf("retrieve_resource output missing original tool content: %s", msg.Content)
					}
					return textOnly("retrieved:" + capturedRef), nil
				}
			}
			t.Fatalf("missing retrieve_resource result in step 4: %+v", req.Messages)
			return domain.ModelResponse{}, nil
		}
	})

	ses, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessage(ctx, ses.ID, "read and retrieve compacted ref")
	if err != nil || run.Status != domain.RunCompleted {
		t.Fatalf("run failed: %+v %v", run, err)
	}
	if !strings.HasPrefix(capturedRef, "inv_") || run.FinalAnswer != "retrieved:"+capturedRef {
		t.Fatalf("unexpected capturedRef=%q finalAnswer=%q", capturedRef, run.FinalAnswer)
	}
	doc, err := db.GetHistory(ctx, capturedRef)
	if err != nil || !strings.Contains(doc.RawText, "alpha-secret-token-99") {
		t.Fatalf("GetHistory(%q): %+v %v", capturedRef, doc, err)
	}
}

func TestEstimateCountsToolCallArgs(t *testing.T) {
	small := []contextpkg.Message{{
		Role:      "assistant",
		ToolCalls: []contextpkg.ToolRef{{ID: "c1", Name: "write_file"}},
	}}
	large := []contextpkg.Message{{
		Role: "assistant",
		ToolCalls: []contextpkg.ToolRef{{
			ID:        "c1",
			Name:      "write_file",
			Arguments: json.RawMessage(`{"path":"a.txt","content":"` + strings.Repeat("x", 400) + `"}`),
		}},
	}}
	if contextpkg.Estimate(large) <= contextpkg.Estimate(small)+80 {
		t.Fatalf("Estimate did not include ToolCalls argument bytes: small=%d large=%d", contextpkg.Estimate(small), contextpkg.Estimate(large))
	}
}

func TestMemoryDeltaIDsUniqueAcrossTurns(t *testing.T) {
	ctx := context.Background()
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{
			ProviderCallID: "f1",
			Name:           domain.ToolFinishTurn,
			Arguments:      json.RawMessage(`{"final_answer":"turn1","turn_summary":"s1","memory_delta":[{"op":"ADD","section":"facts","text":"fact one"}]}`),
		}),
		toolResp(domain.ToolCall{
			ProviderCallID: "f2",
			Name:           domain.ToolFinishTurn,
			Arguments:      json.RawMessage(`{"final_answer":"turn2","turn_summary":"s2","memory_delta":[{"op":"ADD","section":"facts","text":"fact two"}]}`),
		}),
	}}
	m, db, _ := testManager(t, mock)
	ses, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.RunMessage(ctx, ses.ID, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RunMessage(ctx, ses.ID, "second"); err != nil {
		t.Fatal(err)
	}

	raw, _, err := db.LatestMemory(ctx, ses.ID)
	if err != nil {
		t.Fatal(err)
	}
	mem, err := contextpkg.ParseMemory(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(mem.Facts) != 2 {
		t.Fatalf("expected 2 facts, got %+v", mem.Facts)
	}
	id1, id2 := mem.Facts[0].ID, mem.Facts[1].ID
	if id1 == "" || id2 == "" || id1 == id2 {
		t.Fatalf("expected distinct IDs across turns, got %q and %q", id1, id2)
	}

	m.Provider = &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{
			ProviderCallID: "f3",
			Name:           domain.ToolFinishTurn,
			Arguments:      json.RawMessage(`{"final_answer":"turn3","turn_summary":"s3","memory_delta":[{"op":"UPDATE","section":"facts","id":"` + id1 + `","text":"fact one updated"}]}`),
		}),
	}}
	if _, err := m.RunMessage(ctx, ses.ID, "third"); err != nil {
		t.Fatal(err)
	}
	raw, _, err = db.LatestMemory(ctx, ses.ID)
	if err != nil {
		t.Fatal(err)
	}
	mem, err = contextpkg.ParseMemory(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(mem.Facts) != 2 {
		t.Fatalf("expected 2 facts after update, got %+v", mem.Facts)
	}
	if mem.Facts[0].ID != id1 || mem.Facts[0].Text != "fact one updated" {
		t.Fatalf("first fact was not updated: %+v", mem.Facts[0])
	}
	if mem.Facts[1].ID != id2 || mem.Facts[1].Text != "fact two" {
		t.Fatalf("second fact was touched by update to first fact: %+v", mem.Facts[1])
	}
}

type providerFunc func(context.Context, provider.Request) (domain.ModelResponse, error)

func (f providerFunc) Name() string { return "func" }

func (f providerFunc) Complete(ctx context.Context, req provider.Request) (domain.ModelResponse, error) {
	return f(ctx, req)
}
