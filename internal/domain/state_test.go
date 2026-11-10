package domain

import "testing"

func TestRunTransitionTableMatchesArchitecture(t *testing.T) {
	want := []struct {
		from   RunStatus
		reason WaitReason
		ev     RunEvent
		to     RunStatus
		toR    WaitReason
	}{
		{RunRunning, WaitNone, EventAskUser, RunWaiting, WaitUser},
		{RunRunning, WaitNone, EventApproval, RunWaiting, WaitApproval},
		{RunRunning, WaitNone, EventWaitChild, RunWaiting, WaitChild},
		{RunRunning, WaitNone, EventBudgetExhausted, RunWaiting, WaitBudget},
		{RunRunning, WaitNone, EventNeedsReconcile, RunWaiting, WaitReconciliation},
		{RunWaiting, WaitUser, EventResume, RunRunning, WaitNone},
		{RunWaiting, WaitApproval, EventResume, RunRunning, WaitNone},
		{RunWaiting, WaitChild, EventResume, RunRunning, WaitNone},
		{RunWaiting, WaitBudget, EventResume, RunRunning, WaitNone},
		{RunWaiting, WaitReconciliation, EventResume, RunRunning, WaitNone},
		{RunRunning, WaitNone, EventFinalAnswer, RunCompleted, WaitNone},
		{RunRunning, WaitNone, EventFatal, RunFailed, WaitNone},
		{RunWaiting, WaitUser, EventFatal, RunFailed, WaitNone},
		{RunRunning, WaitNone, EventCancel, RunCancelled, WaitNone},
		{RunWaiting, WaitApproval, EventCancel, RunCancelled, WaitNone},
	}
	if len(RunTransitionTable) != 11 {
		t.Fatalf("table rows = %d, want 11", len(RunTransitionTable))
	}
	for _, tc := range want {
		in := RunState{RunID: "run_same", Status: tc.from, Reason: tc.reason}
		got, err := TransitionRun(in, tc.ev)
		if err != nil {
			t.Fatalf("%s + %s: %v", tc.from, tc.ev, err)
		}
		if got.Status != tc.to || got.Reason != tc.toR {
			t.Fatalf("%s + %s -> %+v, want %s/%s", tc.from, tc.ev, got, tc.to, tc.toR)
		}
		if got.RunID != "run_same" {
			t.Fatalf("transition changed run id to %s", got.RunID)
		}
	}
}

func TestRunIllegalTransitions(t *testing.T) {
	cases := []struct {
		s  RunState
		ev RunEvent
	}{
		{RunState{Status: RunRunning}, EventResume},
		{RunState{Status: RunWaiting, Reason: WaitUser}, EventFinalAnswer},
		{RunState{Status: RunCompleted}, EventCancel},
		{RunState{Status: RunFailed}, EventResume},
		{RunState{Status: RunCancelled}, EventFatal},
		{RunState{Status: RunWaiting}, EventResume},
		{RunState{Status: RunRunning, Reason: WaitUser}, EventCancel},
	}
	for _, tc := range cases {
		if _, err := TransitionRun(tc.s, tc.ev); err == nil {
			t.Fatalf("expected error for %+v + %s", tc.s, tc.ev)
		}
	}
}

func TestInvocationTableAndUnknownIsNotTerminal(t *testing.T) {
	if TerminalInvocation(InvUnknown) {
		t.Fatal("UNKNOWN must not be terminal")
	}
	for _, row := range InvocationTransitionTable {
		got, err := TransitionInvocation(row.From, row.Event)
		if err != nil {
			t.Fatal(err)
		}
		if got != row.To {
			t.Fatalf("%s + %s = %s, want %s", row.From, row.Event, got, row.To)
		}
	}
	if _, err := TransitionInvocation(InvUnknown, InvStart); err == nil {
		t.Fatal("UNKNOWN must not return to RUNNING via start")
	}
	if _, err := TransitionInvocation(InvSucceeded, InvStart); err == nil {
		t.Fatal("terminal invocation was re-executed")
	}
	events := []InvEvent{InvStart, InvRequestApproval, InvApprove, InvDeny, InvSucceed, InvFail, InvMarkUnknown, InvReconcileSuccess, InvReconcileFail, InvSkip, InvCancel, InvProtocolReject}
	for _, st := range []InvStatus{InvSucceeded, InvFailed, InvDenied, InvSkipped, InvCancelled} {
		for _, ev := range events {
			if _, err := TransitionInvocation(st, ev); err == nil {
				t.Fatalf("terminal %s accepted %s", st, ev)
			}
		}
	}
}

func TestBudgetChargeAndNestedCap(t *testing.T) {
	b := DefaultBudget()
	if b.MaxModelCalls != 100 {
		t.Fatalf("model cap %d", b.MaxModelCalls)
	}
	next, err := b.Charge(ChargeModel, 1)
	if err != nil || next.UsedModelCalls != 1 || b.UsedModelCalls != 0 {
		t.Fatalf("charge mutated or failed: %+v %+v %v", b, next, err)
	}
	tight := Budget{MaxModelCalls: 1, MaxToolCalls: 1, MaxScriptCalls: 1, MaxChildRuns: 1}
	if _, err := tight.Charge(ChargeModel, 2); !IsCode(err, CodeBudget) {
		t.Fatalf("got %v", err)
	}
	parent := Budget{MaxToolCalls: 2}
	child := Budget{MaxToolCalls: 5}
	p2, c2, err := ChargeNested(parent, child, ChargeTool, 2)
	if err != nil || p2.UsedToolCalls != 2 || c2.UsedToolCalls != 2 {
		t.Fatalf("nested %+v %+v %v", p2, c2, err)
	}
	if _, _, err := ChargeNested(p2, c2, ChargeTool, 1); !IsCode(err, CodeBudget) {
		t.Fatalf("parent cap bypassed: %v", err)
	}
	if p2.UsedToolCalls != 2 {
		t.Fatal("failed nested charge mutated parent")
	}
}

func FuzzInvocationTransition(f *testing.F) {
	f.Add(string(InvPending), string(InvStart))
	f.Add(string(InvUnknown), string(InvStart))
	f.Add(string(InvSucceeded), string(InvSucceed))
	f.Fuzz(func(t *testing.T, from string, ev string) {
		got, err := TransitionInvocation(InvStatus(from), InvEvent(ev))
		if err != nil {
			return
		}
		found := false
		for _, row := range InvocationTransitionTable {
			if string(row.From) == from && string(row.Event) == ev && row.To == got {
				found = true
			}
		}
		if !found {
			t.Fatalf("transition %s + %s -> %s is not in the table", from, ev, got)
		}
	})
}
