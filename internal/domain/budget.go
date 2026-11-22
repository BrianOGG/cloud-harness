package domain

import "math"

// Budget is the hard limit ledger for one run. Nested work charges the
// parent with ChargeNested so a child, script, or poll cannot spend past
// the parent totals.
//
// Call counts are charged before the work runs. Tokens and cost are only
// known after a model call returns, so they are recorded with AddUsage and
// enforced on the next model charge: a run can pass MaxTokens or MaxCostUSD
// by at most one call. Zero for either maximum means no limit.
type Budget struct {
	MaxModelCalls   int `json:"max_model_calls"`
	UsedModelCalls  int `json:"used_model_calls"`
	MaxToolCalls    int `json:"max_tool_calls"`
	UsedToolCalls   int `json:"used_tool_calls"`
	MaxScriptCalls  int `json:"max_script_calls"`
	UsedScriptCalls int `json:"used_script_calls"`
	MaxChildRuns    int `json:"max_child_runs"`
	UsedChildRuns   int `json:"used_child_runs"`

	// MaxTokens limits input plus output tokens across every model call of
	// the run. Cached input tokens are part of the input count.
	MaxTokens             int64 `json:"max_tokens"`
	UsedInputTokens       int64 `json:"used_input_tokens"`
	UsedOutputTokens      int64 `json:"used_output_tokens"`
	UsedCachedInputTokens int64 `json:"used_cached_input_tokens"`
	// MaxCostUSD limits the estimated cost from configured model prices.
	// Calls to a model without a price add tokens but no cost.
	MaxCostUSD  float64 `json:"max_cost_usd"`
	UsedCostUSD float64 `json:"used_cost_usd"`
}

// UsageCharge is what one model call spent.
type UsageCharge struct {
	InputTokens       int
	OutputTokens      int
	CachedInputTokens int
	CostUSD           float64
}

// BudgetExtension raises limits on a run that waits for budget.
type BudgetExtension struct {
	ModelCalls int
	Tokens     int64
	CostUSD    float64
}

const (
	// DefaultMaxModelCalls is the confirmed per-run main-model cap.
	DefaultMaxModelCalls = 100
	// DefaultMaxToolCalls bounds ordinary broker dispatches.
	DefaultMaxToolCalls = 300
	// DefaultMaxScriptCalls bounds broker calls made from a program.
	DefaultMaxScriptCalls = 50
	// DefaultMaxChildRuns bounds delegated child runs.
	DefaultMaxChildRuns = 4
	// DefaultRepeatWarning is the confirmed identical-call reminder threshold.
	DefaultRepeatWarning = 3
	// DefaultNoProgressLimit is the placeholder sliding-window length.
	// A later eval may calibrate it. M2 blocks the same operation at this length
	// when the result hash does not change.
	DefaultNoProgressLimit = 4
)

// ChargeKind selects which counter a charge applies to.
type ChargeKind string

const (
	ChargeModel  ChargeKind = "model"
	ChargeTool   ChargeKind = "tool"
	ChargeScript ChargeKind = "script"
	ChargeChild  ChargeKind = "child"
)

// DefaultBudget returns the M0 limits. Every field is overridable in config later.
func DefaultBudget() Budget {
	return Budget{
		MaxModelCalls:  DefaultMaxModelCalls,
		MaxToolCalls:   DefaultMaxToolCalls,
		MaxScriptCalls: DefaultMaxScriptCalls,
		MaxChildRuns:   DefaultMaxChildRuns,
	}
}

// Charge returns an updated budget or CodeBudget without mutating the receiver.
func (b Budget) Charge(kind ChargeKind, n int) (Budget, error) {
	if n <= 0 {
		return b, E(CodeInvalidArgument, "charge must be positive")
	}
	next := b
	switch kind {
	case ChargeModel:
		if b.UsedModelCalls+n > b.MaxModelCalls {
			return b, E(CodeBudget, "model call budget exhausted")
		}
		if err := b.SpendErr(); err != nil {
			return b, err
		}
		next.UsedModelCalls += n
	case ChargeTool:
		if b.UsedToolCalls+n > b.MaxToolCalls {
			return b, E(CodeBudget, "tool call budget exhausted")
		}
		next.UsedToolCalls += n
	case ChargeScript:
		if b.UsedScriptCalls+n > b.MaxScriptCalls {
			return b, E(CodeBudget, "script call budget exhausted")
		}
		next.UsedScriptCalls += n
	case ChargeChild:
		if b.UsedChildRuns+n > b.MaxChildRuns {
			return b, E(CodeBudget, "child run budget exhausted")
		}
		next.UsedChildRuns += n
	default:
		return b, E(CodeInvalidArgument, "unknown charge kind "+string(kind))
	}
	return next, nil
}

// ChargeNested charges the parent and the child. If either would exceed its
// own cap, both returned budgets are the inputs.
func ChargeNested(parent, child Budget, kind ChargeKind, n int) (Budget, Budget, error) {
	np, err := parent.Charge(kind, n)
	if err != nil {
		return parent, child, err
	}
	nc, err := child.Charge(kind, n)
	if err != nil {
		return parent, child, err
	}
	return np, nc, nil
}

// UsedTokens is the count MaxTokens applies to.
func (b Budget) UsedTokens() int64 {
	return b.UsedInputTokens + b.UsedOutputTokens
}

// SpendErr reports a token or cost limit that has already been reached. A
// model call is refused while it is non-nil.
func (b Budget) SpendErr() error {
	if b.MaxTokens > 0 && b.UsedTokens() >= b.MaxTokens {
		return E(CodeBudget, "token budget exhausted")
	}
	if b.MaxCostUSD > 0 && b.UsedCostUSD >= b.MaxCostUSD {
		return E(CodeBudget, "cost budget exhausted")
	}
	return nil
}

// AddUsage records what a finished model call spent. It never fails: the
// tokens are already spent, and the next model charge is the one refused.
func (b Budget) AddUsage(u UsageCharge) Budget {
	b.UsedInputTokens += int64(max(u.InputTokens, 0))
	b.UsedOutputTokens += int64(max(u.OutputTokens, 0))
	b.UsedCachedInputTokens += int64(max(u.CachedInputTokens, 0))
	if u.CostUSD > 0 && !math.IsNaN(u.CostUSD) && !math.IsInf(u.CostUSD, 0) {
		b.UsedCostUSD += u.CostUSD
	}
	return b
}

// Extend raises the limits. A limit that is unlimited (zero) stays unlimited.
func (b Budget) Extend(x BudgetExtension) (Budget, error) {
	if x.ModelCalls < 0 || x.Tokens < 0 || x.CostUSD < 0 || math.IsNaN(x.CostUSD) || math.IsInf(x.CostUSD, 0) {
		return b, E(CodeInvalidArgument, "budget extension must not be negative")
	}
	if x.ModelCalls == 0 && x.Tokens == 0 && x.CostUSD == 0 {
		return b, E(CodeInvalidArgument, "budget extension must add model calls, tokens, or cost")
	}
	b.MaxModelCalls += x.ModelCalls
	if b.MaxTokens > 0 {
		b.MaxTokens += x.Tokens
	}
	if b.MaxCostUSD > 0 {
		b.MaxCostUSD += x.CostUSD
	}
	return b, nil
}
