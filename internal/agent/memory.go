package agent

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	contextpkg "github.com/cloud-harness/cloud-harness/internal/context"
	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/provider"
	"github.com/cloud-harness/cloud-harness/internal/storage"
)

const (
	defaultContextTokens = 12000
	plannerCatalogLimit  = 20
)

func (m *Manager) tokenLimit() int {
	if m.ContextTokenLimit > 0 {
		return m.ContextTokenLimit
	}
	return defaultContextTokens
}

func (m *Manager) indexDoc(ctx context.Context, sourceID, sessionID, text, kind string) {
	text = strings.TrimSpace(text)
	if text == "" || m.Store == nil || sourceID == "" {
		return
	}
	meta, _ := json.Marshal(map[string]string{"kind": kind})
	_ = m.Store.IndexHistory(ctx, storage.HistoryDoc{
		SourceID: sourceID, SessionID: sessionID, RawText: text, Metadata: meta, CreatedAt: m.now(),
	})
}

func (m *Manager) retrySummaries(ctx context.Context, sessionID string) {
	jobs, err := m.Store.ListOpenSummaryJobs(ctx, sessionID)
	if err != nil {
		return
	}
	for _, job := range jobs {
		run, err := m.Store.GetRun(ctx, job.RunID)
		if err != nil {
			continue
		}
		_ = m.runSummaryJob(ctx, run, job, run.FinalAnswer)
	}
}

func (m *Manager) persistSummary(ctx context.Context, run storage.Run, summary, refs string, deltas []contextpkg.Delta) error {
	if strings.TrimSpace(summary) == "" {
		summary = generatedSummary(run.FinalAnswer)
	}
	if refs == "" {
		refs = "[]"
	}
	raw, _, err := m.Store.LatestMemory(ctx, run.SessionID)
	if err != nil {
		return err
	}
	mem, err := contextpkg.ParseMemory(raw)
	if err != nil {
		return err
	}
	if len(deltas) == 0 {
		deltas = []contextpkg.Delta{{Op: "ADD", Section: "facts", Text: summary, Source: run.TurnID}}
	}
	mem, err = contextpkg.ApplyDelta(mem, deltas)
	if err != nil {
		return err
	}
	if err := m.Store.SaveTurnSummary(ctx, run.TurnID, summary, refs); err != nil {
		return err
	}
	if err := m.Store.SaveMemory(ctx, run.SessionID, mem.JSON(), refs); err != nil {
		return err
	}
	m.indexDoc(ctx, "sum_"+run.TurnID, run.SessionID, summary, "summary")
	if err := m.event(ctx, run.ID, "turn.summary", map[string]string{"turn_id": run.TurnID}); err != nil {
		return err
	}
	return nil
}

func (m *Manager) runSummaryJob(ctx context.Context, run storage.Run, job storage.SummaryJob, answer string) error {
	job.Attempts++
	job.UpdatedAt = m.now()
	summary, refs, deltas, err := m.summarize(ctx, run, answer)
	if err != nil {
		job.Status = "failed"
		job.LastError = err.Error()
		_ = m.Store.UpdateSummaryJob(ctx, job)
		return err
	}
	run.FinalAnswer = answer
	if err := m.persistSummary(ctx, run, summary, refs, deltas); err != nil {
		job.Status = "failed"
		job.LastError = err.Error()
		_ = m.Store.UpdateSummaryJob(ctx, job)
		return err
	}
	job.Status = "done"
	job.LastError = ""
	return m.Store.UpdateSummaryJob(ctx, job)
}

func (m *Manager) summarize(ctx context.Context, run storage.Run, answer string) (string, string, []contextpkg.Delta, error) {
	if m.Summarizer == nil {
		summary := generatedSummary(answer)
		return summary, `["posthoc:local"]`, []contextpkg.Delta{{
			Op: "ADD", Section: "facts", Text: summary, Source: run.TurnID,
		}}, nil
	}
	resp, err := m.Summarizer.Complete(ctx, provider.Request{Messages: []provider.Message{
		{Role: "system", Content: "Return one JSON object with turn_summary and memory_delta. Do not change the user-visible answer."},
		{Role: "user", Content: answer},
	}})
	if err != nil {
		return "", "", nil, err
	}
	_ = m.Store.Charge(ctx, run.ID, domain.ChargeModel, 1)
	return parseSummaryJSON(resp.Text(), run.TurnID)
}

func parseSummaryJSON(text, turnID string) (string, string, []contextpkg.Delta, error) {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return "", "", nil, domain.E(domain.CodeProtocol, "summary response has no JSON object")
	}
	var body struct {
		TurnSummary string          `json:"turn_summary"`
		MemoryDelta json.RawMessage `json:"memory_delta"`
		SourceRefs  json.RawMessage `json:"source_refs"`
	}
	if err := json.Unmarshal([]byte(text[start:end+1]), &body); err != nil {
		return "", "", nil, err
	}
	if strings.TrimSpace(body.TurnSummary) == "" {
		return "", "", nil, domain.E(domain.CodeProtocol, "summary response missing turn_summary")
	}
	deltas, err := contextpkg.ParseDeltas(body.MemoryDelta)
	if err != nil {
		return "", "", nil, err
	}
	refs := `["posthoc:model"]`
	if len(body.SourceRefs) > 0 && string(body.SourceRefs) != "null" {
		refs = string(body.SourceRefs)
	}
	if len(deltas) == 0 {
		deltas = []contextpkg.Delta{{Op: "ADD", Section: "facts", Text: body.TurnSummary, Source: turnID}}
	}
	return body.TurnSummary, refs, deltas, nil
}

func (m *Manager) injectPlan(ctx context.Context, run storage.Run, messages []provider.Message, userText string) []provider.Message {
	if err := m.Store.Charge(ctx, run.ID, domain.ChargeModel, 1); err != nil {
		return messages
	}
	var catalog []contextpkg.CatalogEntry
	if m.Store != nil {
		if entries, err := m.Store.ListHistoryCatalog(ctx, run.SessionID, plannerCatalogLimit); err == nil {
			for _, e := range entries {
				catalog = append(catalog, contextpkg.CatalogEntry{
					SourceID:  e.SourceID,
					Kind:      e.Kind,
					CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339),
					Summary:   e.Summary,
				})
			}
		}
	}
	started := time.Now()
	resp, err := m.Provider.Complete(ctx, provider.Request{Messages: []provider.Message{
		{Role: "system", Content: "You are the context planner. Return JSON {\"resources\":[{\"id\":\"...\",\"precision\":\"summary|excerpt|verbatim\"}]} and nothing else."},
		{Role: "user", Content: contextpkg.FormatPlannerPrompt(userText, catalog)},
	}})
	if err != nil {
		_ = m.event(ctx, run.ID, "planner.failed", map[string]string{"error": err.Error()})
		return messages
	}
	usage := usageOf(resp)
	if usage.LatencyMS == 0 {
		usage.LatencyMS = time.Since(started).Milliseconds()
	}
	if visible, blob, splitErr := domain.SplitForStorage(resp); splitErr == nil {
		_ = m.Store.SaveModelResponse(ctx, storage.StoredResponse{
			ID: domain.NewID("rsp_"), RunID: run.ID, Provider: resp.Provider, Complete: resp.Complete,
			Usage: usage, Items: visible, Opaque: blob, Action: domain.ActionPlan, CreatedAt: m.now(),
		})
	}
	resources, err := contextpkg.ParsePlan(resp.Text())
	if err != nil {
		_ = m.event(ctx, run.ID, "planner.failed", map[string]string{"error": err.Error()})
		return messages
	}
	ids := make([]string, 0, len(resources))
	for _, r := range resources {
		rawText, ok := m.resolvePlanResource(ctx, run.SessionID, r.ID)
		if !ok {
			continue
		}
		body := contextpkg.Present(rawText, r.Precision)
		messages = append(messages, provider.Message{
			Role:    "user",
			Content: "Retrieved " + r.ID + " (" + r.Precision + ", untrusted data):\n" + body,
		})
		ids = append(ids, r.ID)
	}
	_ = m.event(ctx, run.ID, "planner.completed", map[string]any{"resources": ids})
	return messages
}

func (m *Manager) resolvePlanResource(ctx context.Context, sessionID, id string) (string, bool) {
	if m.Store == nil || id == "" {
		return "", false
	}
	if doc, err := m.Store.GetHistory(ctx, id); err == nil && doc.SessionID == sessionID {
		return doc.RawText, true
	}
	if summaries, err := m.Store.RecentTurnSummaries(ctx, sessionID, plannerCatalogLimit); err == nil {
		for _, ts := range summaries {
			if ts.TurnID == id || "sum_"+ts.TurnID == id {
				return ts.Summary, true
			}
		}
	}
	return "", false
}

func maybeCompact(messages []provider.Message, prefixN, userAt, limit int, refs map[string]string) []provider.Message {
	view := toContext(messages)
	if contextpkg.Estimate(view) <= limit {
		return messages
	}
	return fromContext(contextpkg.Compact(view, contextpkg.CompactOptions{
		PrefixCount: prefixN, KeepIndex: userAt, TokenLimit: limit, Refs: refs,
	}))
}

func toContext(in []provider.Message) []contextpkg.Message {
	out := make([]contextpkg.Message, len(in))
	for i, m := range in {
		out[i] = contextpkg.Message{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID, Ref: m.Ref}
		for _, c := range m.ToolCalls {
			out[i].ToolCalls = append(out[i].ToolCalls, contextpkg.ToolRef{
				ID: c.ProviderCallID, Name: c.Name, Arguments: append(json.RawMessage(nil), c.Arguments...),
			})
		}
	}
	return out
}

func fromContext(in []contextpkg.Message) []provider.Message {
	out := make([]provider.Message, len(in))
	for i, m := range in {
		out[i] = provider.Message{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID, Ref: m.Ref}
		for _, c := range m.ToolCalls {
			out[i].ToolCalls = append(out[i].ToolCalls, domain.ToolCall{
				ProviderCallID: c.ID, Name: c.Name, Arguments: append(json.RawMessage(nil), c.Arguments...),
			})
		}
	}
	return out
}

func usageOf(resp domain.ModelResponse) domain.Usage {
	var u domain.Usage
	for _, it := range resp.Items {
		if it.Kind != domain.ItemUsage || it.Usage == nil {
			continue
		}
		u.InputTokens += it.Usage.InputTokens
		u.OutputTokens += it.Usage.OutputTokens
		u.CachedInputTokens += it.Usage.CachedInputTokens
		if it.Usage.LatencyMS > u.LatencyMS {
			u.LatencyMS = it.Usage.LatencyMS
		}
	}
	return u
}
