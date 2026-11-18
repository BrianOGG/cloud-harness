package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/domain"
)

// Until is a deterministic stop condition on one JSON field of a tool result.
type Until struct {
	Field    string `json:"field"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
}

// PollRequest is one poll_tool call. Interval may be zero in tests.
type PollRequest struct {
	RunID       string
	SessionID   string
	TargetTool  string
	TargetArgs  json.RawMessage
	Until       Until
	Interval    time.Duration
	MaxAttempts int
	Timeout     time.Duration
	Now         time.Time
}

// PollResult reports why polling stopped. Attempts is how many broker calls ran.
type PollResult struct {
	Stopped  string          `json:"stopped"`
	Attempts int             `json:"attempts"`
	Last     json.RawMessage `json:"last,omitempty"`
}

// Poll calls a pollable readonly tool until the condition matches, the attempt
// cap is hit, or the context ends. Every attempt goes through Broker.
func Poll(ctx context.Context, b *Broker, req PollRequest) (PollResult, error) {
	if b == nil {
		return PollResult{}, domain.E(domain.CodeToolError, "no tool broker")
	}
	if req.MaxAttempts < 1 {
		return PollResult{}, domain.E(domain.CodeInvalidArgument, "max_attempts must be at least 1")
	}
	if req.MaxAttempts > 20 {
		req.MaxAttempts = 20
	}
	spec, ok := b.spec(req.TargetTool)
	if !ok {
		return PollResult{}, domain.E(domain.CodeToolError, "unknown poll target "+req.TargetTool)
	}
	if !spec.Pollable || !spec.Readonly || spec.SideEffectClass != "none" {
		return PollResult{}, domain.E(domain.CodeScriptPolicy, "poll target must be a pollable readonly tool")
	}
	if req.TargetTool == "poll_tool" || req.TargetTool == "execute_program" || req.TargetTool == "delegate_task" {
		return PollResult{}, domain.E(domain.CodeScriptPolicy, "poll target is not a safe repeated tool")
	}
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	var last json.RawMessage
	for attempt := 1; attempt <= req.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return PollResult{Stopped: "cancelled", Attempts: attempt - 1, Last: last}, err
		}
		res, err := b.Invoke(ctx, Request{
			RunID: req.RunID, SessionID: req.SessionID, Tool: req.TargetTool, Args: req.TargetArgs, Now: req.Now,
		})
		if err != nil {
			return PollResult{Stopped: "error", Attempts: attempt, Last: last}, err
		}
		if res.Error != nil || !res.OK {
			raw, _ := json.Marshal(res.Error)
			return PollResult{Stopped: "error", Attempts: attempt, Last: raw}, res.Error
		}
		last = res.Data
		if matchUntil(res.Data, req.Until) {
			return PollResult{Stopped: "condition", Attempts: attempt, Last: last}, nil
		}
		if attempt == req.MaxAttempts {
			break
		}
		if req.Interval > 0 {
			timer := time.NewTimer(req.Interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return PollResult{Stopped: "cancelled", Attempts: attempt, Last: last}, ctx.Err()
			case <-timer.C:
			}
		}
	}
	return PollResult{Stopped: "cap", Attempts: req.MaxAttempts, Last: last}, nil
}

func matchUntil(data json.RawMessage, until Until) bool {
	if until.Field == "" {
		return false
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return false
	}
	got, ok := m[until.Field]
	if !ok {
		return false
	}
	text := fmt.Sprint(got)
	switch until.Operator {
	case "", "eq":
		return text == until.Value
	case "ne":
		return text != until.Value
	case "contains":
		return strings.Contains(text, until.Value)
	default:
		return false
	}
}
