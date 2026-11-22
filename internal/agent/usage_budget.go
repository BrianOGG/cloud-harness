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

// usageCost is the estimated cost of one call. A model without a configured
// price costs zero, as it does in stats.
func usageCost(prices map[string]config.ModelPrice, model string, usage domain.Usage) float64 {
	price, ok := prices[model]
	if !ok {
		return 0
	}
	uncached := max(usage.InputTokens-usage.CachedInputTokens, 0)
	return (float64(uncached)*price.Input + float64(usage.OutputTokens)*price.Output + float64(usage.CachedInputTokens)*price.Cached) / 1_000_000
}

// chargeUsage adds a finished model call's tokens and cost to the run budget
// and returns the cost. The call is already paid for, so a failed write does
// not fail the run: the caller's next model charge still sees every committed
// call.
func (m *Manager) chargeUsage(ctx context.Context, runID, model string, usage domain.Usage) float64 {
	cost := usageCost(m.prices(), model, usage)
	if m.Store == nil || usage.InputTokens <= 0 && usage.OutputTokens <= 0 && usage.CachedInputTokens <= 0 {
		return cost
	}
	_ = m.Store.ChargeUsage(context.WithoutCancel(ctx), runID, domain.UsageCharge{
		InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
		CachedInputTokens: usage.CachedInputTokens, CostUSD: cost,
	})
	return cost
}

// recordMemoryCall accounts for a finished summary or memory call: its usage
// goes to the run budget and, when the provider answered, to a stored
// response row, so stats and exports count the calls the budget counts. The
// row keeps no items: the summary lives in the context or memory it was
// written for, not in the run's transcript.
func (m *Manager) recordMemoryCall(ctx context.Context, runID string, model provider.Provider, resp domain.ModelResponse, callErr error, started time.Time) {
	usage := usageOf(resp)
	m.chargeUsage(ctx, runID, m.pricedModel(model, ""), usage)
	if callErr != nil || m.Store == nil {
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
