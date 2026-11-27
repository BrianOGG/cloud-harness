package domain

import (
	"encoding/json"
	"math"
	"testing"
)

func TestBudgetAddUsageTracksBreakdownAndReportedFields(t *testing.T) {
	b := Budget{}.AddUsage(UsageCharge{
		InputTokens: 100, OutputTokens: 40, CachedInputTokens: 30,
		CacheCreationInputTokens: 12, ReasoningTokens: 9, CacheReported: true,
	})
	b = b.AddUsage(UsageCharge{InputTokens: 5, OutputTokens: 2, ReasoningReported: true})
	if b.UsedInputTokens != 105 || b.UsedOutputTokens != 42 || b.UsedCachedInputTokens != 30 ||
		b.UsedCacheCreationInputTokens != 12 || b.UsedReasoningTokens != 9 || !b.CacheReported || !b.ReasoningReported {
		t.Fatalf("budget usage breakdown=%+v", b)
	}
}

func TestBudgetReadsLegacyJSONWithoutUsageBreakdown(t *testing.T) {
	var b Budget
	if err := json.Unmarshal([]byte(`{"max_tokens":100,"used_input_tokens":12,"used_output_tokens":4,"used_cached_input_tokens":3}`), &b); err != nil {
		t.Fatal(err)
	}
	if b.UsedInputTokens != 12 || b.UsedOutputTokens != 4 || b.UsedCachedInputTokens != 3 ||
		b.UsedCacheCreationInputTokens != 0 || b.UsedReasoningTokens != 0 || b.CacheReported || b.ReasoningReported {
		t.Fatalf("legacy budget decoded incorrectly: %+v", b)
	}
}

func TestUsageLegacyJSONAndReportedZeroRoundTrip(t *testing.T) {
	var usage Usage
	if err := json.Unmarshal([]byte(`{"input_tokens":12,"output_tokens":4,"cached_input_tokens":3}`), &usage); err != nil {
		t.Fatal(err)
	}
	if usage.CacheReported || usage.ReasoningReported || usage.CacheCreationInputTokens != 0 || usage.ReasoningTokens != 0 {
		t.Fatalf("legacy usage=%+v", usage)
	}
	zero := Usage{CacheReported: true, ReasoningReported: true}
	raw, err := json.Marshal(zero)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip Usage
	if err := json.Unmarshal(raw, &roundTrip); err != nil || roundTrip != zero {
		t.Fatalf("zero usage=%+v err=%v", roundTrip, err)
	}
}

func TestTokenAndCostLimitsRefuseTheNextModelCall(t *testing.T) {
	b := Budget{MaxModelCalls: 10, MaxToolCalls: 10, MaxTokens: 100, MaxCostUSD: 0.5}
	b, err := b.Charge(ChargeModel, 1)
	if err != nil {
		t.Fatal(err)
	}
	// A call that is already paid for is recorded even when it crosses the limit.
	b = b.AddUsage(UsageCharge{InputTokens: 90, OutputTokens: 30, CachedInputTokens: 40, CostUSD: 0.1})
	if b.UsedTokens() != 120 || b.UsedInputTokens != 90 || b.UsedOutputTokens != 30 || b.UsedCachedInputTokens != 40 || b.UsedCostUSD != 0.1 {
		t.Fatalf("usage not recorded: %+v", b)
	}
	if _, err := b.Charge(ChargeModel, 1); !IsCode(err, CodeBudget) || err.Error() == "" {
		t.Fatalf("model call past the token limit: %v", err)
	}
	if _, err := b.Charge(ChargeTool, 1); err != nil {
		t.Fatalf("token limit refused a tool call: %v", err)
	}

	cost := Budget{MaxModelCalls: 10, MaxCostUSD: 0.5}.AddUsage(UsageCharge{InputTokens: 1, CostUSD: 0.5})
	if _, err := cost.Charge(ChargeModel, 1); !IsCode(err, CodeBudget) {
		t.Fatalf("model call past the cost limit: %v", err)
	}
	unlimited := Budget{MaxModelCalls: 10}.AddUsage(UsageCharge{InputTokens: 1 << 40, CostUSD: 1e9})
	if _, err := unlimited.Charge(ChargeModel, 1); err != nil {
		t.Fatalf("zero limits are unlimited: %v", err)
	}
	bad := Budget{}.AddUsage(UsageCharge{InputTokens: -5, OutputTokens: -5, CostUSD: math.NaN()})
	if bad.UsedTokens() != 0 || bad.UsedCostUSD != 0 {
		t.Fatalf("invalid usage was recorded: %+v", bad)
	}
}

func TestBudgetExtendRaisesOnlyExistingLimits(t *testing.T) {
	b := Budget{MaxModelCalls: 1, MaxTokens: 100, MaxCostUSD: 1}
	got, err := b.Extend(BudgetExtension{ModelCalls: 2, Tokens: 50, CostUSD: 0.25})
	if err != nil || got.MaxModelCalls != 3 || got.MaxTokens != 150 || got.MaxCostUSD != 1.25 {
		t.Fatalf("extended=%+v err=%v", got, err)
	}
	got, err = Budget{MaxModelCalls: 1}.Extend(BudgetExtension{Tokens: 50, CostUSD: 1})
	if err != nil || got.MaxTokens != 0 || got.MaxCostUSD != 0 {
		t.Fatalf("an unlimited limit became limited: %+v err=%v", got, err)
	}
	for _, x := range []BudgetExtension{{}, {ModelCalls: -1}, {Tokens: -1}, {CostUSD: -1}, {CostUSD: math.Inf(1)}, {CostUSD: math.NaN()}} {
		if _, err := b.Extend(x); !IsCode(err, CodeInvalidArgument) {
			t.Fatalf("extension %+v accepted: %v", x, err)
		}
	}
}
