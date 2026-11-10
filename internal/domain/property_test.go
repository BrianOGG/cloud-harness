package domain

import (
	"encoding/json"
	"math/rand/v2"
	"testing"
)

func TestPropertyOneTerminalResultAndNoReplay(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 5))
	for n := 0; n < 40; n++ {
		count := 1 + rng.IntN(4)
		calls := make([]ToolCall, count)
		for i := range calls {
			calls[i] = ToolCall{
				ProviderCallID: "c" + itoa(i),
				Name:           "read_file",
				Arguments:      json.RawMessage(`{"path":"a"}`),
			}
		}
		resp := ModelResponse{Provider: "mock", Complete: true}
		for i := range calls {
			c := calls[i]
			resp.Items = append(resp.Items, ResponseItem{Ordinal: i, Kind: ItemToolCall, ToolCall: &c})
		}
		seq := 0
		batch, err := PlanBatch("run", resp, func(prefix string) string {
			seq++
			return prefix + itoa(n) + "_" + itoa(seq)
		})
		if err != nil {
			t.Fatal(err)
		}
		err = Drive(&batch, func(Invocation) GateDecision { return GateAllow }, func(inv Invocation) (json.RawMessage, error) {
			return json.RawMessage(`{"ok":true}`), nil
		})
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]int{}
		for _, inv := range batch.Invocations {
			seen[inv.ProviderCallID]++
			if seen[inv.ProviderCallID] != 1 {
				t.Fatalf("call %s repeated", inv.ProviderCallID)
			}
			if !TerminalInvocation(inv.Status) || inv.Result == nil {
				t.Fatalf("missing one terminal result %+v", inv)
			}
			if err := batch.RecordSuccess(inv.ID, json.RawMessage(`{"again":true}`)); err == nil {
				t.Fatal("finished invocation was executed again")
			}
		}
		if _, ok := batch.NextPending(); ok {
			t.Fatal("finished batch still has pending work")
		}
		if err := batch.Pairing(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPropertyBudgetCannotBeEvaded(t *testing.T) {
	rng := rand.New(rand.NewPCG(9, 1))
	for n := 0; n < 30; n++ {
		parent := Budget{MaxToolCalls: 3 + rng.IntN(5), MaxScriptCalls: 2, MaxModelCalls: 4, MaxChildRuns: 1}
		parent.UsedToolCalls = parent.MaxToolCalls - 1
		child := Budget{MaxToolCalls: 100, MaxScriptCalls: 100, MaxModelCalls: 100, MaxChildRuns: 0}
		if _, _, err := ChargeNested(parent, child, ChargeTool, 2); !IsCode(err, CodeBudget) {
			t.Fatalf("child evaded parent tool budget: %v", err)
		}
		gotP, gotC, err := ChargeNested(parent, child, ChargeTool, 1)
		if err != nil || gotP.UsedToolCalls != parent.MaxToolCalls || gotC.UsedToolCalls != 1 {
			t.Fatalf("single nested charge %+v %+v %v", gotP, gotC, err)
		}
		if _, _, err := ChargeNested(gotP, gotC, ChargeScript, 3); !IsCode(err, CodeBudget) {
			t.Fatalf("script evaded parent budget: %v", err)
		}
		if _, err := parent.Charge(ChargeChild, 2); !IsCode(err, CodeBudget) {
			t.Fatal("child count evaded the parent cap")
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
