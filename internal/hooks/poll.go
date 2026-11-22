package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/tools"
)

// Poll preserves tools.Poll's validation, caps and result protocol while
// allowing the agent's hooks to surround each actual broker invocation.
// tools.Poll itself exposes no invocation interception API.
func Poll(ctx context.Context, broker *tools.Broker, req tools.PollRequest, invoke func(context.Context, tools.Request) (tools.Result, error)) (tools.PollResult, error) {
	if broker == nil {
		return tools.PollResult{}, domain.E(domain.CodeToolError, "no tool broker")
	}
	if req.MaxAttempts < 1 {
		return tools.PollResult{}, domain.E(domain.CodeInvalidArgument, "max_attempts must be at least 1")
	}
	limit := min(req.MaxAttempts, 20)
	fact, ok := broker.Lookup(req.TargetTool)
	if !ok {
		return tools.PollResult{}, domain.E(domain.CodeToolError, "unknown poll target "+req.TargetTool)
	}
	if !fact.Pollable || !fact.Readonly || fact.SideEffect != "none" {
		return tools.PollResult{}, domain.E(domain.CodeScriptPolicy, "poll target must be a pollable readonly tool")
	}
	if req.TargetTool == domain.ToolPoll || req.TargetTool == domain.ToolExecuteProgram || req.TargetTool == domain.ToolDelegateTask {
		return tools.PollResult{}, domain.E(domain.CodeScriptPolicy, "poll target is not a safe repeated tool")
	}
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	var last json.RawMessage
	for attempt := 1; attempt <= limit; attempt++ {
		if err := ctx.Err(); err != nil {
			return tools.PollResult{Stopped: "cancelled", Attempts: attempt - 1, Last: last}, err
		}
		res, err := invoke(ctx, tools.Request{
			RunID: req.RunID, SessionID: req.SessionID, Tool: req.TargetTool, Args: req.TargetArgs, Now: req.Now,
		})
		if err != nil {
			return tools.PollResult{Stopped: "error", Attempts: attempt, Last: last}, err
		}
		if res.Error != nil || !res.OK {
			raw, _ := json.Marshal(res.Error)
			return tools.PollResult{Stopped: "error", Attempts: attempt, Last: raw}, res.Error
		}
		last = res.Data
		if hookPollMatches(last, req.Until) {
			return tools.PollResult{Stopped: "condition", Attempts: attempt, Last: last}, nil
		}
		if attempt == limit || req.Interval <= 0 {
			continue
		}
		timer := time.NewTimer(req.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return tools.PollResult{Stopped: "cancelled", Attempts: attempt, Last: last}, ctx.Err()
		case <-timer.C:
		}
	}
	return tools.PollResult{Stopped: "cap", Attempts: limit, Last: last}, nil
}

func hookPollMatches(raw json.RawMessage, until tools.Until) bool {
	var fields map[string]any
	if until.Field == "" || json.Unmarshal(raw, &fields) != nil {
		return false
	}
	value, exists := fields[until.Field]
	if !exists {
		return false
	}
	text := fmt.Sprint(value)
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
