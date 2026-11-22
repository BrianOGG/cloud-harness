package agent

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/config"
	contextpkg "github.com/BrianOGG/cloud-harness/internal/context"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

func withUsage(resp domain.ModelResponse, input, output, cached int) domain.ModelResponse {
	resp.Items = append(resp.Items, domain.ResponseItem{
		Ordinal: len(resp.Items), Kind: domain.ItemUsage,
		Usage: &domain.Usage{InputTokens: input, OutputTokens: output, CachedInputTokens: cached},
	})
	return resp
}

// conversation lists the user and assistant messages of a request as
// "role: content" lines.
func conversation(req provider.Request) []string {
	var out []string
	for _, msg := range req.Messages {
		if msg.Role == "user" || msg.Role == "assistant" {
			out = append(out, msg.Role+": "+msg.Content)
		}
	}
	return out
}

func eventPayloads(t *testing.T, db storage.Store, runID, eventType string) []map[string]any {
	t.Helper()
	events, err := db.ListEvents(context.Background(), runID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, ev := range events {
		if ev.Type != eventType {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, payload)
	}
	return out
}

func TestNewTurnCarriesRecentConversationHistory(t *testing.T) {
	ctx := context.Background()
	mock := &provider.Mock{}
	m, db, _ := testManager(t, mock)
	m.SkipPlanner = true
	ses, _ := m.CreateSession(ctx)
	for _, message := range []string{"the deploy key is ALPHA-7", "the region is eu-west"} {
		if run, err := m.RunMessage(ctx, ses.ID, message); err != nil || run.Status != domain.RunCompleted {
			t.Fatalf("%+v %v", run, err)
		}
	}
	first := conversation(mock.Requests[0])
	if len(first) != 1 || first[0] != "user: the deploy key is ALPHA-7" {
		t.Fatalf("the first turn of a session carried history: %q", first)
	}
	before := len(mock.Requests)
	run, err := m.RunMessage(ctx, ses.ID, "which key and region?")
	if err != nil || run.Status != domain.RunCompleted {
		t.Fatalf("%+v %v", run, err)
	}
	got := conversation(mock.Requests[before])
	want := []string{
		"user: the deploy key is ALPHA-7",
		"assistant: Mock answer: the deploy key is ALPHA-7",
		"user: the region is eu-west",
		"assistant: Mock answer: the region is eu-west",
		"user: which key and region?",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("third turn conversation:\n%s", strings.Join(got, "\n"))
	}
	history := eventPayloads(t, db, run.ID, "context.history")
	if len(history) != 1 || history[0]["turns"] != float64(2) || history[0]["messages"] != float64(4) {
		t.Fatalf("context.history events=%v", history)
	}
}

func TestHistoryKeepsTheNewestTurnsWithinItsLimits(t *testing.T) {
	ctx := context.Background()
	mock := &provider.Mock{}
	m, _, _ := testManager(t, mock)
	m.SkipPlanner = true
	ses, _ := m.CreateSession(ctx)
	for _, message := range []string{"t1", "t2", "t3", "t4", "t5", "t6", "t7", "t8"} {
		if _, err := m.RunMessage(ctx, ses.ID, "question "+message); err != nil {
			t.Fatal(err)
		}
	}
	last := strings.Join(conversation(mock.Requests[len(mock.Requests)-1]), "\n")
	if strings.Contains(last, "question t1") || !strings.Contains(last, "user: question t2\nassistant: Mock answer: question t2") || strings.Count(last, "user: ") != historyTurns+1 {
		t.Fatalf("history is not the last %d turns:\n%s", historyTurns, last)
	}

	// A long message keeps its start and end and names where the rest is.
	// Older turns that no longer fit the history budget are left out.
	m.ContextTokenLimit = 900
	long := "HEAD-MARK " + strings.Repeat("filler ", 400) + "TAIL-MARK"
	if _, err := m.RunMessage(ctx, ses.ID, long); err != nil {
		t.Fatal(err)
	}
	before := len(mock.Requests)
	if _, err := m.RunMessage(ctx, ses.ID, "after the long one"); err != nil {
		t.Fatal(err)
	}
	lines := conversation(mock.Requests[before])
	if len(lines) != 3 || lines[2] != "user: after the long one" {
		t.Fatalf("history over its budget kept older turns: %d messages", len(lines))
	}
	for _, line := range lines[:2] {
		if len(line) >= len(long) || !strings.Contains(line, "HEAD-MARK") || !strings.Contains(line, "TAIL-MARK") || !strings.Contains(line, "[retrieve_resource id ") {
			t.Fatalf("long history message was not clipped around its middle: %d bytes", len(line))
		}
	}
}

func TestHistoryLeavesOutChildRuns(t *testing.T) {
	ctx := context.Background()
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "d1", Name: "delegate_task", Arguments: json.RawMessage(`{"goal":"CHILD-GOAL","allowed_tools":["read_file"],"max_model_calls":4,"max_tool_calls":4}`)}),
		textOnly("child-done"),
		textOnly("parent-done"),
	}}
	m, _, _ := testManager(t, mock)
	m.SkipPlanner = true
	ses, _ := m.CreateSession(ctx)
	if run, err := m.RunMessage(ctx, ses.ID, "do the child task"); err != nil || run.FinalAnswer != "parent-done" {
		t.Fatalf("%+v %v", run, err)
	}
	if _, err := m.RunMessage(ctx, ses.ID, "next"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(conversation(mock.Requests[len(mock.Requests)-1]), "\n")
	if got != "user: do the child task\nassistant: parent-done\nuser: next" {
		t.Fatalf("history after a delegated turn:\n%s", got)
	}
}

// summaryCompactionSetup prepares a turn whose third step is over the context
// limit: two file reads, then the answer.
func summaryCompactionSetup(t *testing.T, summarizer provider.Provider) (*Manager, *provider.Mock, storage.Store, string, string) {
	t.Helper()
	ctx := context.Background()
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "c1", Name: "read_file", Arguments: json.RawMessage(`{"path":"log.txt"}`)}),
		toolResp(domain.ToolCall{ProviderCallID: "c2", Name: "read_file", Arguments: json.RawMessage(`{"path":"note.txt"}`)}),
		textOnly("done"),
	}}
	m, db, root := testManager(t, mock)
	m.SkipPlanner = true
	m.ContextTokenLimit = 300
	m.Summarizer = summarizer
	// The fact sits past character 160 of the log, where the old compaction
	// had already cut the message off.
	log := strings.Repeat("line of log ", 20) + "FACT-PAST-160 port=8443 " + strings.Repeat("line of log ", 20)
	if err := os.WriteFile(filepath.Join(root, "log.txt"), []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("note-ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	ses, _ := m.CreateSession(ctx)
	return m, mock, db, ses.ID, log
}

func summaryCompactionRun(t *testing.T, summarizer provider.Provider) (*Manager, *provider.Mock, storage.Store, storage.Run, string) {
	t.Helper()
	m, mock, db, sessionID, log := summaryCompactionSetup(t, summarizer)
	run, err := m.RunMessage(context.Background(), sessionID, "read the log then the note")
	if err != nil || run.Status != domain.RunCompleted || run.FinalAnswer != "done" || len(mock.Requests) != 3 {
		t.Fatalf("%+v %v requests=%d", run, err, len(mock.Requests))
	}
	return m, mock, db, run, log
}

func TestCompactionReplacesOlderWorkWithAModelSummary(t *testing.T) {
	var summaryRequests []provider.Request
	summarizer := providerFunc(func(_ context.Context, req provider.Request) (domain.ModelResponse, error) {
		// The same model also writes the turn summary after the run.
		if req.Messages[0].Content != contextpkg.SummaryPrompt {
			return textOnly("turn summary"), nil
		}
		summaryRequests = append(summaryRequests, req)
		return withUsage(textOnly(`{"confirmed_facts":["log.txt says FACT-PAST-160 port=8443"],"in_progress":[],"pending":["read note.txt"],"key_files_and_decisions":["log.txt"]}`), 70, 9, 0), nil
	})
	_, mock, db, run, log := summaryCompactionRun(t, summarizer)

	if len(summaryRequests) != 1 || len(summaryRequests[0].Messages) != 2 || summaryRequests[0].Messages[0].Content != contextpkg.SummaryPrompt {
		t.Fatalf("summary requests=%d", len(summaryRequests))
	}
	asked := summaryRequests[0].Messages[1].Content
	for _, want := range []string{"read the log then the note", "tool call c1: read_file", log} {
		if !strings.Contains(asked, want) {
			t.Fatalf("summary request is missing %q:\n%s", want, asked)
		}
	}
	if len(summaryRequests[0].Tools) != 0 {
		t.Fatal("the summary call was offered tools")
	}

	third := mock.Requests[2]
	if contextpkg.PrefixHash(toContext(mock.Requests[0].Prefix)) != contextpkg.PrefixHash(toContext(third.Prefix)) {
		t.Fatal("compaction changed the prefix")
	}
	var summary string
	for _, msg := range third.Messages {
		if strings.HasPrefix(msg.Content, contextpkg.WorkSummaryPrefix) {
			summary = msg.Content
		}
	}
	if !strings.Contains(summary, "FACT-PAST-160 port=8443") || !strings.Contains(summary, "read note.txt") {
		t.Fatalf("third request has no model summary: %q", summary)
	}
	body := messageText(third.Messages)
	if strings.Contains(body, "c1") || strings.Contains(body, "line of log") || strings.Contains(body, `"compacted":true`) {
		t.Fatalf("summarised work is still in the context:\n%s", body)
	}
	if !strings.Contains(body, "read the log then the note") || !strings.Contains(body, "c2") || !strings.Contains(body, "note-ok") {
		t.Fatalf("the user message or the newest exchange changed:\n%s", body)
	}
	for i, msg := range third.Messages {
		if msg.Role == "tool" && (i == 0 || len(third.Messages[i-1].ToolCalls) == 0) {
			t.Fatalf("tool result %d lost its call", i)
		}
	}

	summarized := eventPayloads(t, db, run.ID, "context.summarized")
	if len(summarized) != 1 || summarized[0]["replaced_messages"] != float64(2) || summarized[0]["after_tokens"].(float64) >= summarized[0]["before_tokens"].(float64) {
		t.Fatalf("context.summarized events=%v", summarized)
	}
	// The summary is a model call of the run: three steps and one summary.
	if run.Budget.UsedModelCalls != 4 || run.Budget.UsedInputTokens != 70 || run.Budget.UsedOutputTokens != 9 {
		t.Fatalf("summary call was not charged: %+v", run.Budget)
	}
}

func TestCompactionFallsBackOnceWhenTheSummaryFails(t *testing.T) {
	calls := 0
	summarizer := providerFunc(func(_ context.Context, req provider.Request) (domain.ModelResponse, error) {
		if req.Messages[0].Content != contextpkg.SummaryPrompt {
			return textOnly("turn summary"), nil
		}
		calls++
		return domain.ModelResponse{}, domain.E(domain.CodeProviderConfig, "summary model is down")
	})
	_, mock, db, run, _ := summaryCompactionRun(t, summarizer)
	body := messageText(mock.Requests[2].Messages)
	if strings.Contains(body, contextpkg.WorkSummaryPrefix) || !strings.Contains(body, "c1") || !strings.Contains(body, `"compacted":true`) || !strings.Contains(body, "note-ok") {
		t.Fatalf("fallback did not keep the paired, compacted tool result:\n%s", body)
	}
	failed := eventPayloads(t, db, run.ID, "context.summary_failed")
	if calls != 1 || len(failed) != 1 || !strings.Contains(failed[0]["error"].(string), "summary model is down") {
		t.Fatalf("summary calls=%d failed events=%v", calls, failed)
	}
	if len(eventPayloads(t, db, run.ID, "context.summarized")) != 0 {
		t.Fatal("a failed summary was reported as done")
	}
}

func TestStepWaitsWhenItsSummaryUsesUpTheTokenBudget(t *testing.T) {
	ctx := context.Background()
	summaries := 0
	summarizer := providerFunc(func(_ context.Context, req provider.Request) (domain.ModelResponse, error) {
		if req.Messages[0].Content != contextpkg.SummaryPrompt {
			return textOnly("turn summary"), nil
		}
		summaries++
		return withUsage(textOnly(`{"confirmed_facts":["log.txt says FACT-PAST-160 port=8443"],"in_progress":[],"pending":[],"key_files_and_decisions":[]}`), 70, 9, 0), nil
	})
	m, mock, db, sessionID, _ := summaryCompactionSetup(t, summarizer)
	m.MaxRunTokens = 60
	run, err := m.RunMessage(ctx, sessionID, "read the log then the note")
	if err != nil || run.Status != domain.RunWaiting || run.WaitReason != domain.WaitBudget {
		t.Fatalf("run whose summary crossed the token limit: %+v err=%v", run, err)
	}
	// The summary spent the budget, so the step it prepared was not sent.
	if len(mock.Requests) != 2 || summaries != 1 || run.Budget.UsedModelCalls != 3 || run.Budget.UsedTokens() != 79 {
		t.Fatalf("requests=%d summaries=%d budget=%+v", len(mock.Requests), summaries, run.Budget)
	}
	run, err = m.Resume(ctx, run.ID, ResumeInput{ExtraTokens: 1000})
	if err != nil || run.Status != domain.RunCompleted || run.FinalAnswer != "done" || len(mock.Requests) != 3 {
		t.Fatalf("resume: %+v %v requests=%d", run, err, len(mock.Requests))
	}
	// The resumed step runs on the summary already paid for.
	if body := messageText(mock.Requests[2].Messages); summaries != 1 || !strings.Contains(body, contextpkg.WorkSummaryPrefix) || !strings.Contains(body, "FACT-PAST-160 port=8443") {
		t.Fatalf("summaries=%d resumed context:\n%s", summaries, body)
	}
	if len(eventPayloads(t, db, run.ID, "context.summary_failed")) != 0 {
		t.Fatal("a budget wait was reported as a failed summary")
	}
}

func TestSummaryWithoutBudgetWaitsInsteadOfClipping(t *testing.T) {
	ctx := context.Background()
	summaries := 0
	summarizer := providerFunc(func(_ context.Context, req provider.Request) (domain.ModelResponse, error) {
		if req.Messages[0].Content == contextpkg.SummaryPrompt {
			summaries++
		}
		return textOnly(`{"confirmed_facts":["summary"],"in_progress":[],"pending":[],"key_files_and_decisions":[]}`), nil
	})
	m, mock, db, sessionID, _ := summaryCompactionSetup(t, summarizer)
	m.UseBudget, m.InitialBudget = true, domain.DefaultBudget()
	m.InitialBudget.MaxModelCalls = 2
	run, err := m.RunMessage(ctx, sessionID, "read the log then the note")
	if err != nil || run.Status != domain.RunWaiting || run.WaitReason != domain.WaitBudget || len(mock.Requests) != 2 || summaries != 0 {
		t.Fatalf("%+v err=%v requests=%d summaries=%d", run, err, len(mock.Requests), summaries)
	}
	run, err = m.Resume(ctx, run.ID, ResumeInput{ExtraModelCalls: 5})
	if err != nil || run.Status != domain.RunCompleted || len(mock.Requests) != 3 || summaries != 1 {
		t.Fatalf("resume: %+v %v requests=%d summaries=%d", run, err, len(mock.Requests), summaries)
	}
	// The wait kept the context whole, so the summary was written from it.
	if body := messageText(mock.Requests[2].Messages); !strings.Contains(body, contextpkg.WorkSummaryPrefix) || strings.Contains(body, `"compacted":true`) {
		t.Fatalf("resumed context:\n%s", body)
	}
	if len(eventPayloads(t, db, run.ID, "context.summary_failed")) != 0 {
		t.Fatal("a budget wait was reported as a failed summary")
	}
}

func TestRunWaitsAtTheTokenLimitAndResumesWithMoreTokens(t *testing.T) {
	ctx := context.Background()
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		withUsage(toolResp(domain.ToolCall{ProviderCallID: "c1", Name: "read_file", Arguments: json.RawMessage(`{"path":"note.txt"}`)}), 80, 30, 0),
		withUsage(textOnly("done"), 100, 5, 0),
	}}
	m, db, root := testManager(t, mock)
	m.SkipPlanner = true
	m.MaxRunTokens = 100
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("note"), 0o644); err != nil {
		t.Fatal(err)
	}
	ses, _ := m.CreateSession(ctx)
	run, err := m.RunMessage(ctx, ses.ID, "read the note")
	if err != nil || run.Status != domain.RunWaiting || run.WaitReason != domain.WaitBudget || len(mock.Requests) != 1 {
		t.Fatalf("run past its token limit: %+v err=%v requests=%d", run, err, len(mock.Requests))
	}
	if run.Budget.MaxTokens != 100 || run.Budget.UsedTokens() != 110 || run.Budget.UsedToolCalls != 1 {
		t.Fatalf("budget=%+v", run.Budget)
	}
	// More model calls do not help a run that is out of tokens.
	if _, err := m.Resume(ctx, run.ID, ResumeInput{ExtraModelCalls: 5}); !domain.IsCode(err, domain.CodeBudget) {
		t.Fatalf("resume without tokens: %v", err)
	}
	if got, _ := db.GetRun(ctx, run.ID); got.Status != domain.RunWaiting || got.Budget.MaxModelCalls != run.Budget.MaxModelCalls || len(mock.Requests) != 1 {
		t.Fatalf("a refused resume changed the run: %+v", got)
	}
	run, err = m.Resume(ctx, run.ID, ResumeInput{ExtraTokens: 200})
	if err != nil || run.Status != domain.RunCompleted || run.FinalAnswer != "done" {
		t.Fatalf("resume with tokens: %+v %v", run, err)
	}
	if run.Budget.MaxTokens != 300 || run.Budget.UsedInputTokens != 180 || run.Budget.UsedOutputTokens != 35 || run.Budget.UsedModelCalls != 2 {
		t.Fatalf("budget after resume=%+v", run.Budget)
	}
}

func TestRunWaitsAtTheCostLimitAndReportsCallCost(t *testing.T) {
	ctx := context.Background()
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		withUsage(toolResp(domain.ToolCall{ProviderCallID: "c1", Name: "read_file", Arguments: json.RawMessage(`{"path":"note.txt"}`)}), 1000, 100, 400),
		withUsage(textOnly("done"), 1000, 100, 0),
	}}
	m, db, root := testManager(t, mock)
	m.SkipPlanner = true
	m.MaxRunCostUSD = 0.002
	m.ModelPrices = map[string]config.ModelPrice{"mock": {Input: 2, Output: 10, Cached: 0.5}}
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("note"), 0o644); err != nil {
		t.Fatal(err)
	}
	ses, _ := m.CreateSession(ctx)
	run, err := m.RunMessage(ctx, ses.ID, "read the note")
	if err != nil || run.WaitReason != domain.WaitBudget || len(mock.Requests) != 1 {
		t.Fatalf("run past its cost limit: %+v err=%v requests=%d", run, err, len(mock.Requests))
	}
	// 600 uncached input at $2, 400 cached at $0.50 and 100 output at $10 per million.
	const firstCall = 0.0024
	if math.Abs(run.Budget.UsedCostUSD-firstCall) > 1e-12 || run.Budget.UsedCachedInputTokens != 400 {
		t.Fatalf("budget=%+v", run.Budget)
	}
	completed := eventPayloads(t, db, run.ID, "model.completed")
	if len(completed) != 1 || math.Abs(completed[0]["cost_usd"].(float64)-firstCall) > 1e-12 || completed[0]["input_tokens"] != float64(1000) || completed[0]["cached_input_tokens"] != float64(400) {
		t.Fatalf("model.completed=%v", completed)
	}
	run, err = m.Resume(ctx, run.ID, ResumeInput{ExtraCostUSD: 0.01})
	if err != nil || run.Status != domain.RunCompleted {
		t.Fatalf("resume with more cost: %+v %v", run, err)
	}
	if math.Abs(run.Budget.MaxCostUSD-0.012) > 1e-12 || math.Abs(run.Budget.UsedCostUSD-(firstCall+0.003)) > 1e-12 {
		t.Fatalf("budget after resume=%+v", run.Budget)
	}
}

func TestSummaryIsPricedAsTheModelThatWroteIt(t *testing.T) {
	// A child's steps name its own model. Its summary call names none, so the
	// provider answers with its default model and is billed at that price.
	m, mock, _, sessionID, _ := summaryCompactionSetup(t, nil)
	mock.Responses = []domain.ModelResponse{
		withUsage(mock.Responses[0], 10, 0, 0),
		mock.Responses[1],
		withUsage(textOnly(`{"confirmed_facts":["summary"],"in_progress":[],"pending":[],"key_files_and_decisions":[]}`), 1000, 100, 0),
		mock.Responses[2],
	}
	m.Model = "step-model"
	m.ModelPrices = map[string]config.ModelPrice{"mock": {Input: 1, Output: 1}, "step-model": {Input: 100, Output: 100}}
	run, err := m.RunMessage(context.Background(), sessionID, "read the log then the note")
	if err != nil || run.Status != domain.RunCompleted || len(mock.Requests) != 4 {
		t.Fatalf("%+v %v requests=%d", run, err, len(mock.Requests))
	}
	if mock.Requests[0].Model != "step-model" || mock.Requests[2].Model != "" || mock.Requests[2].Messages[0].Content != contextpkg.SummaryPrompt {
		t.Fatalf("step model=%q summary model=%q", mock.Requests[0].Model, mock.Requests[2].Model)
	}
	// 10 tokens at $100 per million for the step, 1100 at $1 for the summary.
	if want := 0.001 + 0.0011; math.Abs(run.Budget.UsedCostUSD-want) > 1e-12 {
		t.Fatalf("cost=%v want %v", run.Budget.UsedCostUSD, want)
	}
}

func TestChildSpendsTheParentTokenAndCostBudget(t *testing.T) {
	ctx := context.Background()
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		withUsage(toolResp(domain.ToolCall{ProviderCallID: "d1", Name: "delegate_task", Arguments: json.RawMessage(`{"goal":"CHILD-GOAL","allowed_tools":["read_file"],"max_model_calls":4,"max_tool_calls":4}`)}), 100, 10, 0),
		withUsage(toolResp(domain.ToolCall{ProviderCallID: "c1", Name: "read_file", Arguments: json.RawMessage(`{"path":"note.txt"}`)}), 200, 20, 0),
		withUsage(textOnly("child-done"), 300, 30, 0),
		withUsage(textOnly("parent-done"), 400, 40, 0),
	}}
	m, db, root := testManager(t, mock)
	m.SkipPlanner = true
	m.MaxRunTokens = 10_000
	m.MaxRunCostUSD = 1
	m.ModelPrices = map[string]config.ModelPrice{"mock": {Input: 1, Output: 1}}
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("note"), 0o644); err != nil {
		t.Fatal(err)
	}
	ses, _ := m.CreateSession(ctx)
	run, err := m.RunMessage(ctx, ses.ID, "do the child task")
	if err != nil || run.Status != domain.RunCompleted || run.FinalAnswer != "parent-done" {
		t.Fatalf("%+v %v", run, err)
	}
	runs, _ := db.ListRuns(ctx, ses.ID)
	var child storage.Run
	for _, r := range runs {
		if r.ParentRunID == run.ID {
			child = r
		}
	}
	if child.Budget.UsedInputTokens != 500 || child.Budget.UsedOutputTokens != 50 || math.Abs(child.Budget.UsedCostUSD-0.00055) > 1e-12 {
		t.Fatalf("child budget=%+v", child.Budget)
	}
	// The child starts with what the parent had left after its first call.
	if child.Budget.MaxTokens != 10_000-110 || math.Abs(child.Budget.MaxCostUSD-(1-0.00011)) > 1e-12 {
		t.Fatalf("child limits=%+v", child.Budget)
	}
	if run.Budget.UsedInputTokens != 1000 || run.Budget.UsedOutputTokens != 100 || math.Abs(run.Budget.UsedCostUSD-0.0011) > 1e-12 {
		t.Fatalf("parent budget does not include the child: %+v", run.Budget)
	}
}
