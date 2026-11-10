package domain

import "encoding/json"

// Action is the harness meaning of one model response.
type Action string

const (
	ActionAnswer   Action = "ANSWER"
	ActionRetrieve Action = "RETRIEVE"
	ActionExecute  Action = "EXECUTE"
	ActionPlan     Action = "PLAN"
	ActionAskUser  Action = "ASK_USER"
	ActionProtocol Action = "PROTOCOL_ERROR"
)

const (
	ToolFinishTurn       = "finish_turn"
	ToolAskUser          = "ask_user"
	ToolUpdatePlan       = "update_plan"
	ToolRetrieveHistory  = "retrieve_history"
	ToolRetrieveResource = "retrieve_resource"
	ToolExecuteProgram   = "execute_program"
	ToolDelegateTask     = "delegate_task"
	ToolPoll             = "poll_tool"
)

// IsControl reports ask_user and finish_turn, which must occupy a response alone.
func IsControl(name string) bool {
	return name == ToolAskUser || name == ToolFinishTurn
}

// IsRetrieve reports a built-in history read.
func IsRetrieve(name string) bool {
	return name == ToolRetrieveHistory || name == ToolRetrieveResource
}

// Classify decides the action for a complete response.
// A non-nil error means the response is a protocol failure and must not be executed.
// Text alongside tool calls is non-terminal and does not change the action.
func Classify(resp ModelResponse) (Action, error) {
	if !resp.Complete {
		return ActionProtocol, E(CodeProtocol, "incomplete response is not dispatched")
	}
	if len(resp.Items) == 0 {
		return ActionProtocol, E(CodeProtocol, "empty response")
	}
	if err := ValidateItems(resp.Items); err != nil {
		return ActionProtocol, err
	}
	calls := resp.ToolCalls()
	if len(calls) == 0 {
		if resp.Text() == "" {
			return ActionProtocol, E(CodeProtocol, "response has neither text nor tool calls")
		}
		return ActionAnswer, nil
	}
	seen := map[string]struct{}{}
	control := 0
	for _, c := range calls {
		if _, ok := seen[c.ProviderCallID]; ok {
			return ActionProtocol, E(CodeProtocol, "duplicate provider_call_id "+c.ProviderCallID)
		}
		seen[c.ProviderCallID] = struct{}{}
		if err := requireObjectArgs(c.Arguments); err != nil {
			return ActionProtocol, err
		}
		if IsControl(c.Name) {
			control++
		}
	}
	if control > 0 && len(calls) != 1 {
		return ActionProtocol, E(CodeProtocol, "ask_user and finish_turn must be the only tool call")
	}
	if len(calls) == 1 {
		switch calls[0].Name {
		case ToolFinishTurn:
			if err := validateFinishTurn(calls[0].Arguments); err != nil {
				return ActionProtocol, err
			}
			return ActionAnswer, nil
		case ToolAskUser:
			if err := requireStringField(calls[0].Arguments, "question"); err != nil {
				return ActionProtocol, err
			}
			return ActionAskUser, nil
		case ToolUpdatePlan:
			return ActionPlan, nil
		}
		if IsRetrieve(calls[0].Name) {
			return ActionRetrieve, nil
		}
		return ActionExecute, nil
	}
	allRetrieve := true
	for _, c := range calls {
		if !IsRetrieve(c.Name) {
			allRetrieve = false
			break
		}
	}
	if allRetrieve {
		return ActionRetrieve, nil
	}
	return ActionExecute, nil
}

func requireObjectArgs(raw json.RawMessage) error {
	if len(raw) == 0 {
		return E(CodeProtocol, "tool arguments are missing")
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return E(CodeProtocol, "tool arguments are not valid JSON")
	}
	if _, ok := v.(map[string]any); !ok {
		return E(CodeProtocol, "tool arguments must be a JSON object")
	}
	return nil
}

func requireStringField(raw json.RawMessage, field string) error {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return E(CodeProtocol, "tool arguments are not valid JSON")
	}
	s, ok := m[field].(string)
	if !ok || s == "" {
		return E(CodeProtocol, "missing string field "+field)
	}
	return nil
}

func validateFinishTurn(raw json.RawMessage) error {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return E(CodeProtocol, "finish_turn arguments are not valid JSON")
	}
	if _, ok := m["final_answer"].(string); !ok {
		return E(CodeProtocol, "finish_turn requires string final_answer")
	}
	if v, ok := m["memory_delta"]; ok {
		if _, ok := v.([]any); !ok {
			return E(CodeProtocol, "finish_turn memory_delta must be an array")
		}
	}
	if v, ok := m["turn_summary"]; ok {
		if _, ok := v.(string); !ok {
			return E(CodeProtocol, "finish_turn turn_summary must be a string")
		}
	}
	if v, ok := m["source_refs"]; ok {
		if _, ok := v.([]any); !ok {
			return E(CodeProtocol, "finish_turn source_refs must be an array")
		}
	}
	return nil
}

// CallClass is the per-call action inside a batch.
func CallClass(name string) Action {
	switch name {
	case ToolFinishTurn:
		return ActionAnswer
	case ToolAskUser:
		return ActionAskUser
	case ToolUpdatePlan:
		return ActionPlan
	}
	if IsRetrieve(name) {
		return ActionRetrieve
	}
	return ActionExecute
}
