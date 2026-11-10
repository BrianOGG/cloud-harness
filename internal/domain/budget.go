package domain

// Budget is the hard limit ledger for one run. Nested work charges the
// parent with ChargeNested so a child, script, or poll cannot spend past
// the parent totals.
type Budget struct {
	MaxModelCalls   int `json:"max_model_calls"`
	UsedModelCalls  int `json:"used_model_calls"`
	MaxToolCalls    int `json:"max_tool_calls"`
	UsedToolCalls   int `json:"used_tool_calls"`
	MaxScriptCalls  int `json:"max_script_calls"`
	UsedScriptCalls int `json:"used_script_calls"`
	MaxChildRuns    int `json:"max_child_runs"`
	UsedChildRuns   int `json:"used_child_runs"`
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
