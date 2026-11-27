package agent

import (
	"context"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/config"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

// prices returns the configured model prices. A manager built without them
// (a child, a worktree manager, an embedder) reads its owner's, then the
// process configuration once.
func (m *Manager) prices() map[string]config.ModelPrice {
	for source := m; source != nil; source = source.cancelOwner {
		if source.ModelPrices != nil {
			return source.ModelPrices
		}
	}
	m.priceOnce.Do(func() {
		m.loadedPrices = map[string]config.ModelPrice{}
		if cfg, err := config.Load("", nil); err == nil && cfg.ModelPrices != nil {
			m.loadedPrices = cfg.ModelPrices
		}
	})
	return m.loadedPrices
}

// pricedModel names the model a call on p is billed as. requested is the
// model named in the request: a call that names none runs on the provider's
// own model, whatever model the manager's steps ask for.
func (m *Manager) pricedModel(p provider.Provider, requested string) string {
	if profiled, ok := p.(*provider.ProfileProvider); ok && profiled != nil {
		return provider.ModelName(profiled.Provider, profiled.Model)
	}
	return provider.ModelName(p, requested)
}

// usageCostKnown estimates one call using the price snapshot available to the
// manager. Cache creation tokens are input-priced; reasoning is already part
// of output tokens. A missing price is distinct from a known zero-cost call.
func usageCostKnown(prices map[string]config.ModelPrice, model string, usage domain.Usage) (float64, bool) {
	price, ok := prices[model]
	if !ok {
		return 0, false
	}
	uncached := max(usage.InputTokens-usage.CachedInputTokens, 0)
	return (float64(uncached)*price.Input + float64(usage.OutputTokens)*price.Output + float64(usage.CachedInputTokens)*price.Cached) / 1_000_000, true
}

// annotateUsage attaches the model active for this completed call and freezes
// the price result in usage_json. ProfileProvider is inspected after Complete,
// so a fallback response is attributed to its backup model.
func (m *Manager) annotateUsage(model provider.Provider, requested string, resp domain.ModelResponse) domain.Usage {
	return m.usageForModel(m.pricedModel(model, requested), resp)
}

func (m *Manager) usageForModel(model string, resp domain.ModelResponse) domain.Usage {
	usage := usageOf(resp)
	usage.Model = model
	// A missing usage item is not evidence that a priced call was free.
	for _, item := range resp.Items {
		if item.Kind == domain.ItemUsage && item.Usage != nil {
			usage.CostUSD, usage.CostKnown = usageCostKnown(m.prices(), model, usage)
			break
		}
	}
	return usage
}

// chargeUsage adds a finished model call's tokens and cost to the run budget
// using the same captured estimate that is persisted for stats. The call is
// already paid for, so a failed write does not fail the run: the caller's next
// model charge still sees every committed call.
func (m *Manager) chargeUsage(ctx context.Context, runID string, usage domain.Usage) {
	if m.Store == nil || usage.InputTokens <= 0 && usage.OutputTokens <= 0 && usage.CachedInputTokens <= 0 &&
		usage.CacheCreationInputTokens <= 0 && usage.ReasoningTokens <= 0 && !usage.CacheReported && !usage.ReasoningReported {
		return
	}
	_ = m.Store.ChargeUsage(context.WithoutCancel(ctx), runID, domain.UsageCharge{
		InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
		CachedInputTokens: usage.CachedInputTokens, CacheCreationInputTokens: usage.CacheCreationInputTokens,
		ReasoningTokens: usage.ReasoningTokens, CacheReported: usage.CacheReported,
		ReasoningReported: usage.ReasoningReported, CostUSD: usage.CostUSD,
	})
}

// recordFailedUsage persists token and price metadata reported before an
// unsuccessful completion, without storing partial text or tool calls.
func (m *Manager) recordFailedUsage(ctx context.Context, runID string, resp domain.ModelResponse, usage domain.Usage) {
	if m.Store == nil || usage.InputTokens <= 0 && usage.OutputTokens <= 0 && usage.CachedInputTokens <= 0 &&
		usage.CacheCreationInputTokens <= 0 && usage.ReasoningTokens <= 0 && !usage.CacheReported && !usage.ReasoningReported {
		return
	}
	_ = m.Store.SaveModelResponse(context.WithoutCancel(ctx), storage.StoredResponse{
		ID: domain.NewID("rsp_"), RunID: runID, Provider: resp.Provider,
		Complete: false, Usage: usage, Action: domain.ActionProtocol, CreatedAt: m.now(),
	})
}

// recordMemoryCall accounts for a finished summary or memory call: its usage
// goes to the run budget and, when the provider answered, to a stored
// response row, so stats and exports count the calls the budget counts. The
// row keeps no items: the summary lives in the context or memory it was
// written for, not in the run's transcript.
func (m *Manager) recordMemoryCall(ctx context.Context, runID string, model provider.Provider, resp domain.ModelResponse, callErr error, started time.Time) {
	usage := m.annotateUsage(model, "", resp)
	m.chargeUsage(ctx, runID, usage)
	if m.Store == nil {
		return
	}
	if callErr != nil {
		m.recordFailedUsage(ctx, runID, resp, usage)
		return
	}
	if usage.LatencyMS == 0 {
		usage.LatencyMS = time.Since(started).Milliseconds()
	}
	_ = m.Store.SaveModelResponse(context.WithoutCancel(ctx), storage.StoredResponse{
		ID: domain.NewID("rsp_"), RunID: runID, Provider: resp.Provider, ProviderResponseID: resp.ProviderResponseID,
		Complete: resp.Complete, Usage: usage, Action: domain.ActionCompact, CreatedAt: m.now(),
	})
}
