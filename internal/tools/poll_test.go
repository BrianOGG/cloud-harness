package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cloud-harness/cloud-harness/internal/domain"
)

func TestPollStopsOnConditionOrCap(t *testing.T) {
	charge := &memCharge{b: domain.Budget{MaxToolCalls: 10}}
	grants := &memGrants{grants: []domain.Grant{{
		ID: "g", Tool: "status", Op: "call", State: "active", RunID: "run",
	}}}
	b := NewBroker(charge, grants)
	n := 0
	b.Register(Spec{
		Name: "status", Description: "status", Readonly: true, Idempotent: true, Pollable: true, SideEffectClass: "none",
		Execute: func(context.Context, json.RawMessage) (json.RawMessage, error) {
			n++
			state := "down"
			if n >= 2 {
				state = "ready"
			}
			return json.Marshal(map[string]string{"content": state})
		},
	})
	got, err := Poll(context.Background(), b, PollRequest{
		RunID: "run", TargetTool: "status", TargetArgs: json.RawMessage(`{}`),
		Until: Until{Field: "content", Operator: "eq", Value: "ready"}, MaxAttempts: 5,
	})
	if err != nil || got.Stopped != "condition" || got.Attempts != 2 {
		t.Fatalf("condition %+v %v", got, err)
	}
	n = 0
	charge.b.UsedToolCalls = 0
	b.reg["status"] = Spec{
		Name: "status", Readonly: true, Idempotent: true, Pollable: true, SideEffectClass: "none",
		Execute: func(context.Context, json.RawMessage) (json.RawMessage, error) {
			n++
			return json.Marshal(map[string]string{"content": "down"})
		},
	}
	got, err = Poll(context.Background(), b, PollRequest{
		RunID: "run", TargetTool: "status", TargetArgs: json.RawMessage(`{}`),
		Until: Until{Field: "content", Operator: "eq", Value: "ready"}, MaxAttempts: 3,
	})
	if err != nil || got.Stopped != "cap" || got.Attempts != 3 || n != 3 {
		t.Fatalf("cap %+v n=%d %v", got, n, err)
	}
}
