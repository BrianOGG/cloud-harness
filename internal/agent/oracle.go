package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/config"
	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/provider"
	"github.com/cloud-harness/cloud-harness/internal/storage"
	"github.com/cloud-harness/cloud-harness/internal/tools"
)

const (
	maxOracleConsultations = 3
	oracleTimeout          = 2 * time.Minute
	oracleSlotNamespace    = tools.ToolAskOracle + ":slots"
)

func (m *Manager) oracleDefaultAllow(name string) []string {
	if m.Depth == 0 && name == tools.ToolAskOracle {
		return []string{tools.ToolAskOracle}
	}
	return nil
}

func (m *Manager) withOracle(ctx context.Context, runID string, leaseVersion int64, onSwitch func()) context.Context {
	if m.Depth > 0 {
		return ctx
	}
	return tools.WithOracle(ctx, runID, func(ctx context.Context, input tools.OraclePrompt) (string, error) {
		return m.askOracle(ctx, runID, leaseVersion, input, onSwitch)
	})
}

func (m *Manager) askOracle(ctx context.Context, runID string, leaseVersion int64, input tools.OraclePrompt, onSwitch func()) (string, error) {
	cfg, err := config.Load("", nil)
	if err != nil {
		return "", m.oracleFailure(ctx, runID, err)
	}
	routed, err := m.withModelRouting(ctx)
	if err != nil {
		return "", m.oracleFailure(ctx, runID, err)
	}
	model, err := m.profiledModel(routed, runID, leaseVersion, config.RolePlanner)
	if err != nil {
		return "", m.oracleFailure(ctx, runID, err)
	}
	if model == nil {
		// Keep the default model when no planner profile is mapped. The profile
		// wrapper still meters each attempt and owns retries at the HTTP boundary.
		model = &provider.ProfileProvider{Provider: m.Provider, Model: m.Model}
	}
	before := model.BeforeAttempt
	lastModel := ""
	model.BeforeAttempt = func(ctx context.Context, additional bool) error {
		if before != nil {
			if err := before(ctx, additional); err != nil {
				return err
			}
		} else if err := m.Store.RequireLease(ctx, runID, m.owner(), leaseVersion, m.now()); err != nil {
			return err
		}
		if !additional {
			if err := m.admitOracle(ctx, runID, cfg.OracleMaxCostUSD, cfg.ModelPrices); err != nil {
				return err
			}
		} else if before == nil {
			if err := m.Store.Charge(ctx, runID, domain.ChargeModel, 1); err != nil {
				return err
			}
		}
		lastModel = provider.ModelName(m.Provider, model.Model)
		return nil
	}
	callCtx, cancel := context.WithTimeout(routed, oracleTimeout)
	defer cancel()
	started := time.Now()
	primary := model.Model
	resp, err := model.Complete(callCtx, provider.OracleRequest(input.Text))
	if model.Model != primary && onSwitch != nil {
		onSwitch()
	}
	if callCtx.Err() == context.DeadlineExceeded && !domain.IsCode(err, domain.CodeBudget) {
		err = callCtx.Err()
	}
	persist := context.WithoutCancel(ctx)
	if lastModel != "" {
		usage := usageOf(resp)
		elapsed := time.Since(started).Milliseconds()
		if eventErr := m.event(persist, runID, "oracle.asked", map[string]any{
			"question_length": input.QuestionLength, "code_bytes": input.CodeBytes, "model": lastModel,
			"input_tokens": usage.InputTokens, "output_tokens": usage.OutputTokens,
			"cached_input_tokens": usage.CachedInputTokens, "latency_ms": elapsed,
		}); eventErr != nil {
			return "", eventErr
		}
		if err == nil || usage != (domain.Usage{}) {
			if usage.LatencyMS == 0 {
				usage.LatencyMS = elapsed
			}
			// The usual usage record feeds stats. The advice goes back through
			// the tool result, independent of the main model's continuation.
			if recordErr := m.Store.SaveModelResponse(persist, storage.StoredResponse{
				ID: domain.NewID("rsp_"), RunID: runID, Provider: resp.Provider, ProviderResponseID: resp.ProviderResponseID,
				Complete: resp.Complete, Usage: usage, Action: "ORACLE", CreatedAt: m.now(),
			}); recordErr != nil {
				return "", recordErr
			}
		}
	}
	answer := resp.Text()
	if err == nil && (!resp.Complete || strings.TrimSpace(answer) == "") {
		err = domain.E(domain.CodeProtocol, "oracle response did not contain complete advice")
	}
	if err != nil {
		return "", m.oracleFailure(persist, runID, err)
	}
	return answer, nil
}

func (m *Manager) admitOracle(ctx context.Context, runID string, maxCostUSD float64, prices map[string]config.ModelPrice) error {
	if maxCostUSD > 0 {
		cost, err := m.oracleCost(ctx, runID, prices)
		if err != nil {
			return err
		}
		if cost >= maxCostUSD {
			cause := domain.E(domain.CodeBudget, fmt.Sprintf(
				"ask_oracle cost limit reached: limit $%s per run; accumulated cost $%s",
				formatOracleUSD(maxCostUSD), formatOracleUSD(cost),
			))
			return cause
		}
	}
	for slot := 1; slot <= maxOracleConsultations; slot++ {
		key := strconv.Itoa(slot)
		_, occupied, err := m.Store.LookupIdempotency(ctx, runID, oracleSlotNamespace, key)
		if err != nil {
			return err
		}
		if occupied {
			continue
		}
		run, err := m.Store.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		if _, err := run.Budget.Charge(domain.ChargeModel, 1); err != nil {
			return err
		}
		// Chat rewind can delete events. Keep the reservation in a run-scoped
		// record that survives rewind and verify ownership after insert-if-absent.
		reservation := strconv.Quote(domain.NewID("oracle_"))
		if err := m.Store.SaveIdempotency(ctx, runID, oracleSlotNamespace, key, json.RawMessage(reservation), m.now()); err != nil {
			return err
		}
		stored, found, err := m.Store.LookupIdempotency(ctx, runID, oracleSlotNamespace, key)
		if err != nil {
			return err
		}
		if !found || string(stored) != reservation {
			return domain.E(domain.CodeConflict, "ask_oracle consultation slot could not be reserved")
		}
		return m.Store.Charge(ctx, runID, domain.ChargeModel, 1)
	}
	return domain.E(domain.CodeBudget, fmt.Sprintf("ask_oracle limit of %d consultations per run reached", maxOracleConsultations))
}

func (m *Manager) oracleCost(ctx context.Context, runID string, prices map[string]config.ModelPrice) (float64, error) {
	events, err := m.Store.ListEvents(ctx, runID, 0)
	if err != nil {
		return 0, err
	}
	var cost float64
	for _, event := range events {
		if event.Type != "oracle.asked" {
			continue
		}
		var asked struct {
			Model        string `json:"model"`
			InputTokens  int    `json:"input_tokens"`
			OutputTokens int    `json:"output_tokens"`
		}
		if err := json.Unmarshal(event.Payload, &asked); err != nil {
			return 0, domain.E(domain.CodeCorrupt, "cannot calculate ask_oracle cost from an invalid oracle.asked event")
		}
		if asked.InputTokens < 0 || asked.OutputTokens < 0 {
			return 0, domain.E(domain.CodeCorrupt, "cannot calculate ask_oracle cost from negative token usage")
		}
		price, ok := prices[asked.Model]
		if !ok {
			continue
		}
		cost += (float64(asked.InputTokens)*price.Input + float64(asked.OutputTokens)*price.Output) / 1_000_000
	}
	return cost, nil
}

func formatOracleUSD(amount float64) string {
	if math.IsInf(amount, 1) {
		return "+Inf"
	}
	return strconv.FormatFloat(amount, 'f', -1, 64)
}

func (m *Manager) oracleFailure(ctx context.Context, runID string, err error) error {
	reason := "upstream_error"
	cause := domain.E(domain.CodeToolError, "ask_oracle consultation failed")
	switch {
	case isOracleCostBudget(err):
		reason, cause = "budget", asDomain(err)
	case domain.IsCode(err, domain.CodeBudget):
		reason, cause = domain.CodeBudget, asDomain(err)
	case provider.IsTimeout(err):
		reason, cause = "timeout", domain.E(domain.CodeToolError, "ask_oracle consultation timed out")
	}
	if eventErr := m.event(context.WithoutCancel(ctx), runID, "oracle.failed", map[string]string{"reason": reason}); eventErr != nil {
		return eventErr
	}
	return cause
}

func isOracleCostBudget(err error) bool {
	return domain.IsCode(err, domain.CodeBudget) && strings.Contains(err.Error(), "ask_oracle cost limit reached:")
}
