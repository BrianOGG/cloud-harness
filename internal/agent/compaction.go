package agent

import (
	"context"
	"strings"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/config"
	contextpkg "github.com/BrianOGG/cloud-harness/internal/context"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

// compactionModel picks the model that writes summaries: the fast profile
// when one is routed, else the summarizer, else the run's own provider.
// prepaid is true when the returned model charges its own model call.
func (m *Manager) compactionModel(ctx context.Context, runID string, leaseVersion int64) (model provider.Provider, prepaid bool, err error) {
	ctx, err = m.withModelRouting(ctx)
	if err != nil {
		return nil, false, err
	}
	profiled, err := m.profiledModel(ctx, runID, leaseVersion, config.RoleFast)
	if err != nil {
		return nil, false, err
	}
	if profiled != nil {
		if profiled.Provider == nil {
			copy := *profiled
			copy.Provider = m.Provider
			profiled = &copy
		}
		return profiled, true, nil
	}
	if m.Summarizer != nil {
		return m.Summarizer, false, nil
	}
	if m.Provider == nil {
		return nil, false, domain.E(domain.CodeProviderConfig, "no model is configured for compaction")
	}
	return m.Provider, false, nil
}

// compactContext brings a context over the token limit back under it before
// a model call. The turn's older work is replaced by a summary a model
// writes; the prefix, the current user message, and the latest exchanges stay
// as they were. The structural fallback runs when there is nothing a summary
// can replace, when the summary call fails, or when the context is still
// over the limit afterwards. After a failed summary call the run does not
// try another until it is resumed. A summary the run budget cannot pay for is
// returned as the budget error with the context unchanged, so the run waits
// for more budget instead of losing detail to the fallback.
func (m *Manager) compactContext(ctx context.Context, run storage.Run, leaseVersion int64, messages []provider.Message, st *loopState) ([]provider.Message, error) {
	limit := m.tokenLimit()
	view := toContext(messages)
	before := contextpkg.Estimate(view)
	if before <= limit {
		return messages, nil
	}
	opt := contextpkg.CompactOptions{PrefixCount: st.prefixN, KeepIndex: st.userAt, TokenLimit: limit, Refs: st.toolRefs}
	if start, end, ok := contextpkg.SummaryRange(view, opt, max(limit/8, 64)); ok && !st.summaryOff {
		summary, err := m.summarizeWork(ctx, run, leaseVersion, view, start, end, st)
		if domain.IsCode(err, domain.CodeBudget) {
			return messages, err
		}
		if err != nil {
			st.summaryOff = true
			_ = m.event(ctx, run.ID, "context.summary_failed", map[string]any{"error": asDomain(err).Message})
		} else {
			compacted := make([]provider.Message, 0, len(messages)-(end-start)+1)
			compacted = append(compacted, messages[:start]...)
			compacted = append(compacted, provider.Message{Role: "user", Content: contextpkg.WorkSummaryPrefix + summary})
			messages = append(compacted, messages[end:]...)
			_ = m.event(ctx, run.ID, "context.summarized", map[string]any{
				"replaced_messages": end - start, "summary_bytes": len(summary),
				"before_tokens": before, "after_tokens": contextpkg.Estimate(toContext(messages)),
			})
		}
	}
	return maybeCompact(messages, st.prefixN, st.userAt, limit, st.toolRefs), nil
}

// summarizeWork asks the compaction model for a structured summary of
// view[start:end]. The call is a model call of the run: it is charged to the
// run budget and its tokens are counted.
func (m *Manager) summarizeWork(ctx context.Context, run storage.Run, leaseVersion int64, view []contextpkg.Message, start, end int, st *loopState) (string, error) {
	model, prepaid, err := m.compactionModel(ctx, run.ID, leaseVersion)
	if err != nil {
		return "", err
	}
	if !prepaid {
		if err := m.Store.Charge(ctx, run.ID, domain.ChargeModel, 1); err != nil {
			return "", err
		}
	}
	span := view[start:end]
	// The transcript stays under about three quarters of the context limit.
	perMessage := contextpkg.SummaryMessageCap(span, m.tokenLimit()*3)
	request := "The user's request this work is for:\n" + contextpkg.ClipMiddle(view[st.userAt].Content, 2000) +
		"\n\nEarlier work to compress:\n" + contextpkg.SummaryTranscript(span, st.toolRefs, perMessage)
	started := time.Now()
	resp, err := model.Complete(ctx, provider.Request{Messages: []provider.Message{
		{Role: "system", Content: contextpkg.SummaryPrompt},
		{Role: "user", Content: request},
	}})
	m.recordMemoryCall(ctx, run.ID, model, resp, err, started)
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(resp.Text())
	if text == "" {
		return "", domain.E(domain.CodeProtocol, "compaction returned an empty summary")
	}
	return contextpkg.NormalizeCompactionSummary(text), nil
}
