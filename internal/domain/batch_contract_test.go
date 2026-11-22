package domain

import (
	"encoding/json"
	"fmt"
	"testing"
)

func toolItem(ordinal int, id, name, args string) ResponseItem {
	return ResponseItem{
		Ordinal: ordinal,
		Kind:    ItemToolCall,
		ToolCall: &ToolCall{
			ProviderCallID: id,
			Name:           name,
			Arguments:      json.RawMessage(args),
			Ordinal:        ordinal,
		},
	}
}

func textItem(ordinal int, text string) ResponseItem {
	return ResponseItem{Ordinal: ordinal, Kind: ItemText, Text: text}
}

func seqID() func(string) string {
	n := 0
	return func(prefix string) string {
		n++
		return fmt.Sprintf("%s%d", prefix, n)
	}
}

func TestBatchThreeToolsExecuteInOrderAndPair(t *testing.T) {
	resp := ModelResponse{
		Provider: "mock",
		Complete: true,
		Items: []ResponseItem{
			textItem(0, "checking"),
			toolItem(1, "c1", "read_file", `{"path":"a"}`),
			toolItem(2, "c2", "list_dir", `{"path":"."}`),
			toolItem(3, "c3", "grep", `{"pattern":"err"}`),
		},
	}
	// Rebase ordinals to be contiguous. The helper above uses the call's own
	// ordinal field; ValidateItems requires item ordinals 0..n-1, which they are.
	batch, err := PlanBatch("run_1", resp, seqID())
	if err != nil {
		t.Fatal(err)
	}
	if batch.Action != ActionExecute {
		t.Fatalf("action %s", batch.Action)
	}
	var order []string
	err = Drive(&batch, func(Invocation) GateDecision { return GateAllow }, func(inv Invocation) (json.RawMessage, error) {
		order = append(order, inv.ProviderCallID)
		return json.RawMessage(fmt.Sprintf(`{"n":%d}`, len(order))), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := join(order); got != "c1,c2,c3" {
		t.Fatalf("order %s", got)
	}
	if batch.Status != BatchComplete {
		t.Fatalf("status %s", batch.Status)
	}
	if err := batch.Pairing(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"c1", "c2", "c3"} {
		res, ok := batch.ResultsByCallID()[id]
		if !ok || !res.OK {
			t.Fatalf("missing result for %s", id)
		}
	}
	for _, inv := range batch.Invocations {
		if !inv.Dispatched {
			t.Fatalf("%s was not dispatched", inv.ProviderCallID)
		}
	}
}

func TestBatchControlToolsMixedDoNotDispatch(t *testing.T) {
	cases := []ModelResponse{
		{
			Provider: "mock", Complete: true,
			Items: []ResponseItem{
				toolItem(0, "f", ToolFinishTurn, `{"final_answer":"done"}`),
				toolItem(1, "r", "read_file", `{"path":"a"}`),
			},
		},
		{
			Provider: "mock", Complete: true,
			Items: []ResponseItem{
				toolItem(0, "a", ToolAskUser, `{"question":"which?"}`),
				toolItem(1, "r", "read_file", `{"path":"a"}`),
			},
		},
	}
	for _, resp := range cases {
		calls := 0
		batch, err := PlanBatch("run_1", resp, seqID())
		if !IsCode(err, CodeProtocol) {
			t.Fatalf("got %v", err)
		}
		if batch.Status != BatchProtocolError {
			t.Fatalf("status %s", batch.Status)
		}
		if driveErr := Drive(&batch, nil, func(Invocation) (json.RawMessage, error) {
			calls++
			return json.RawMessage(`{}`), nil
		}); !IsCode(driveErr, CodeProtocol) {
			t.Fatalf("drive %v", driveErr)
		}
		if calls != 0 {
			t.Fatalf("executor called %d times", calls)
		}
		if len(batch.Invocations) != 2 {
			t.Fatalf("invocations %d", len(batch.Invocations))
		}
		for _, inv := range batch.Invocations {
			if inv.Dispatched || inv.Result == nil || inv.Result.Error == nil || inv.Result.Error.Code != CodeProtocol {
				t.Fatalf("call %s was executed or unpaired: %+v", inv.ProviderCallID, inv)
			}
		}
	}
}

func TestFinishTurnAloneConfirmsWithoutDispatch(t *testing.T) {
	resp := ModelResponse{
		Provider: "mock", Complete: true,
		Items: []ResponseItem{toolItem(0, "f1", ToolFinishTurn, `{"final_answer":"ok","memory_delta":[],"turn_summary":"s","source_refs":["e1"]}`)},
	}
	batch, err := PlanBatch("run_1", resp, seqID())
	if err != nil {
		t.Fatal(err)
	}
	if batch.Action != ActionAnswer || batch.Status != BatchComplete {
		t.Fatalf("action %s status %s", batch.Action, batch.Status)
	}
	if batch.Invocations[0].Dispatched || !batch.Invocations[0].Result.OK {
		t.Fatalf("finish_turn dispatched: %+v", batch.Invocations[0])
	}
	if err := batch.Pairing(); err != nil {
		t.Fatal(err)
	}
}

func TestAskUserAloneDoesNotDispatch(t *testing.T) {
	resp := ModelResponse{
		Provider: "mock", Complete: true,
		Items: []ResponseItem{toolItem(0, "q1", ToolAskUser, `{"question":"proceed?"}`)},
	}
	batch, err := PlanBatch("run_1", resp, seqID())
	if err != nil {
		t.Fatal(err)
	}
	if batch.Action != ActionAskUser {
		t.Fatalf("action %s", batch.Action)
	}
	if err := batch.BeginUserWait(batch.Invocations[0].ID); err != nil {
		t.Fatal(err)
	}
	if batch.Status != BatchWaitingUser || batch.Invocations[0].Dispatched {
		t.Fatalf("ask_user state %+v", batch)
	}
}

func TestApprovalPausesThenDenySkipsRest(t *testing.T) {
	resp := ModelResponse{
		Provider: "mock", Complete: true,
		Items: []ResponseItem{
			toolItem(0, "c1", "read_file", `{"path":"a"}`),
			toolItem(1, "c2", "write_file", `{"path":"b"}`),
			toolItem(2, "c3", "read_file", `{"path":"c"}`),
		},
	}
	batch, err := PlanBatch("run_1", resp, seqID())
	if err != nil {
		t.Fatal(err)
	}
	var ran []string
	err = Drive(&batch, func(inv Invocation) GateDecision {
		if inv.ProviderCallID == "c2" {
			return GateNeedsApproval
		}
		return GateAllow
	}, func(inv Invocation) (json.RawMessage, error) {
		ran = append(ran, inv.ProviderCallID)
		return json.RawMessage(`{"ok":true}`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if join(ran) != "c1" {
		t.Fatalf("ran %v", ran)
	}
	if batch.Status != BatchWaitingApproval {
		t.Fatalf("status %s", batch.Status)
	}
	if batch.Invocations[0].Status != InvSucceeded || batch.Invocations[1].Status != InvWaitingApproval || batch.Invocations[2].Status != InvPending {
		t.Fatalf("statuses %s %s %s", batch.Invocations[0].Status, batch.Invocations[1].Status, batch.Invocations[2].Status)
	}
	if err := batch.Deny(batch.Invocations[1].ID); err != nil {
		t.Fatal(err)
	}
	if batch.Invocations[1].Status != InvDenied || batch.Invocations[2].Status != InvSkipped {
		t.Fatalf("after deny %s %s", batch.Invocations[1].Status, batch.Invocations[2].Status)
	}
	if err := batch.Pairing(); err != nil {
		t.Fatal(err)
	}
	if join(ran) != "c1" {
		t.Fatalf("deny executed more tools: %v", ran)
	}
	for _, id := range []string{"c1", "c2", "c3"} {
		if _, ok := batch.ResultsByCallID()[id]; !ok {
			t.Fatalf("unpaired %s", id)
		}
	}
}

func TestApprovalContinueDoesNotReplayEarlierSuccess(t *testing.T) {
	resp := ModelResponse{
		Provider: "mock", Complete: true,
		Items: []ResponseItem{
			toolItem(0, "c1", "read_file", `{"path":"a"}`),
			toolItem(1, "c2", "read_file", `{"path":"b"}`),
		},
	}
	batch, err := PlanBatch("run_1", resp, seqID())
	if err != nil {
		t.Fatal(err)
	}
	_ = Drive(&batch, func(inv Invocation) GateDecision {
		if inv.ProviderCallID == "c2" {
			return GateNeedsApproval
		}
		return GateAllow
	}, func(Invocation) (json.RawMessage, error) {
		return json.RawMessage(`{"ok":true}`), nil
	})
	if err := batch.Approve(batch.Invocations[1].ID); err != nil {
		t.Fatal(err)
	}
	var ran []string
	if err := Drive(&batch, func(Invocation) GateDecision { return GateAllow }, func(inv Invocation) (json.RawMessage, error) {
		ran = append(ran, inv.ProviderCallID)
		return json.RawMessage(`{"ok":true}`), nil
	}); err != nil {
		t.Fatal(err)
	}
	if join(ran) != "c2" {
		t.Fatalf("replayed %v", ran)
	}
}

func TestCrashRecoveryDoesNotReplaySucceededOrUnknown(t *testing.T) {
	resp := ModelResponse{
		Provider: "mock", Complete: true,
		Items: []ResponseItem{
			toolItem(0, "c1", "read_file", `{"path":"a"}`),
			toolItem(1, "c2", "write_file", `{"path":"b"}`),
			toolItem(2, "c3", "read_file", `{"path":"c"}`),
		},
	}
	batch, err := PlanBatch("run_1", resp, seqID())
	if err != nil {
		t.Fatal(err)
	}
	if err := batch.MarkDispatched(batch.Invocations[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := batch.RecordSuccess(batch.Invocations[0].ID, json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := batch.MarkDispatched(batch.Invocations[1].ID); err != nil {
		t.Fatal(err)
	}
	// Crash while the second call is in flight.
	if err := batch.Recover(); err != nil {
		t.Fatal(err)
	}
	if batch.Invocations[0].Status != InvSucceeded {
		t.Fatalf("first status %s", batch.Invocations[0].Status)
	}
	if batch.Invocations[1].Status != InvUnknown || batch.Status != BatchNeedsReconciliation {
		t.Fatalf("second %s batch %s", batch.Invocations[1].Status, batch.Status)
	}
	if batch.Invocations[2].Status != InvPending {
		t.Fatalf("third was started: %s", batch.Invocations[2].Status)
	}
	ran := 0
	if err := Drive(&batch, func(Invocation) GateDecision { return GateAllow }, func(Invocation) (json.RawMessage, error) {
		ran++
		return json.RawMessage(`{}`), nil
	}); err != nil {
		t.Fatal(err)
	}
	if ran != 0 {
		t.Fatalf("recovery dispatched %d calls", ran)
	}
	if _, err := TransitionInvocation(InvUnknown, InvStart); err == nil {
		t.Fatal("unknown can be restarted")
	}
}

func TestUncertainOutcomeMarksUnknownAndStopsBatch(t *testing.T) {
	resp := ModelResponse{
		Provider: "mock", Complete: true,
		Items: []ResponseItem{
			toolItem(0, "c1", "write_file", `{"path":"a"}`),
			toolItem(1, "c2", "read_file", `{"path":"b"}`),
		},
	}
	batch, err := PlanBatch("run_1", resp, seqID())
	if err != nil {
		t.Fatal(err)
	}
	ran := 0
	if err := Drive(&batch, func(Invocation) GateDecision { return GateAllow }, func(Invocation) (json.RawMessage, error) {
		ran++
		return nil, ErrUncertainOutcome
	}); err != nil {
		t.Fatal(err)
	}
	if ran != 1 {
		t.Fatalf("executed %d calls, want 1", ran)
	}
	if batch.Invocations[0].Status != InvUnknown || batch.Status != BatchNeedsReconciliation {
		t.Fatalf("first %s batch %s", batch.Invocations[0].Status, batch.Status)
	}
	if batch.Invocations[1].Status != InvPending {
		t.Fatalf("second was started: %s", batch.Invocations[1].Status)
	}
}

func TestIncompleteAndBadArgumentsDoNotDispatch(t *testing.T) {
	cases := []ModelResponse{
		{Provider: "mock", Complete: false, Items: []ResponseItem{toolItem(0, "c1", "read_file", `{"path":"a"}`)}},
		{Provider: "mock", Complete: true, Items: nil},
		{Provider: "mock", Complete: true, Items: []ResponseItem{toolItem(0, "c1", "read_file", `[]`)}},
		{Provider: "mock", Complete: true, Items: []ResponseItem{toolItem(0, "c1", "read_file", `not-json`)}},
		{Provider: "mock", Complete: true, Items: []ResponseItem{toolItem(0, "c1", ToolFinishTurn, `{"turn_summary":"x"}`)}},
	}
	for i, resp := range cases {
		calls := 0
		batch, err := PlanBatch("run_1", resp, seqID())
		if !IsCode(err, CodeProtocol) {
			t.Fatalf("case %d: %v", i, err)
		}
		_ = Drive(&batch, nil, func(Invocation) (json.RawMessage, error) {
			calls++
			return nil, nil
		})
		if calls != 0 {
			t.Fatalf("case %d dispatched", i)
		}
		for _, inv := range batch.Invocations {
			if inv.Dispatched {
				t.Fatalf("case %d dispatched %s", i, inv.Name)
			}
		}
	}
}

func TestTextPlusToolsIsNotAnswer(t *testing.T) {
	resp := ModelResponse{
		Provider: "mock", Complete: true,
		Items: []ResponseItem{
			textItem(0, "I will read the file"),
			toolItem(1, "c1", "read_file", `{"path":"a"}`),
		},
	}
	action, err := Classify(resp)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionExecute {
		t.Fatalf("action %s", action)
	}
}

func join(s []string) string {
	out := ""
	for i, p := range s {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}
