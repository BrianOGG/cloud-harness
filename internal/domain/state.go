package domain

// RunStatus is the top-level state of one run.
type RunStatus string

const (
	RunRunning   RunStatus = "RUNNING"
	RunWaiting   RunStatus = "WAITING"
	RunCompleted RunStatus = "COMPLETED"
	RunFailed    RunStatus = "FAILED"
	RunCancelled RunStatus = "CANCELLED"
)

// WaitReason qualifies WAITING. It is empty for every other status.
type WaitReason string

const (
	WaitNone           WaitReason = ""
	WaitUser           WaitReason = "USER"
	WaitApproval       WaitReason = "APPROVAL"
	WaitChild          WaitReason = "CHILD"
	WaitBudget         WaitReason = "BUDGET"
	WaitReconciliation WaitReason = "RECONCILIATION"
)

// RunEvent is a trigger in the run transition table.
type RunEvent string

const (
	EventAskUser         RunEvent = "ask_user"
	EventApproval        RunEvent = "approval"
	EventWaitChild       RunEvent = "wait_child"
	EventBudgetExhausted RunEvent = "budget_exhausted"
	EventNeedsReconcile  RunEvent = "needs_reconciliation"
	EventResume          RunEvent = "resume"
	EventFinalAnswer     RunEvent = "final_answer"
	EventFatal           RunEvent = "fatal"
	EventCancel          RunEvent = "cancel"
)

// RunState is the transition input. RunID is preserved across every legal move
// so a resume continues the same run.
type RunState struct {
	RunID  string
	Status RunStatus
	Reason WaitReason
}

// RunTransition is one row of the authoritative run table.
type RunTransition struct {
	From      RunStatus
	Reason    WaitReason
	AnyReason bool
	Event     RunEvent
	To        RunStatus
	ToReason  WaitReason
}

// RunTransitionTable is the architecture table (architecture §8.1, plan §7.2)
// plus the requirement that a resume keeps the same run.
var RunTransitionTable = []RunTransition{
	{From: RunRunning, Event: EventAskUser, To: RunWaiting, ToReason: WaitUser},
	{From: RunRunning, Event: EventApproval, To: RunWaiting, ToReason: WaitApproval},
	{From: RunRunning, Event: EventWaitChild, To: RunWaiting, ToReason: WaitChild},
	{From: RunRunning, Event: EventBudgetExhausted, To: RunWaiting, ToReason: WaitBudget},
	{From: RunRunning, Event: EventNeedsReconcile, To: RunWaiting, ToReason: WaitReconciliation},
	{From: RunWaiting, AnyReason: true, Event: EventResume, To: RunRunning, ToReason: WaitNone},
	{From: RunRunning, Event: EventFinalAnswer, To: RunCompleted, ToReason: WaitNone},
	{From: RunRunning, Event: EventFatal, To: RunFailed, ToReason: WaitNone},
	{From: RunWaiting, AnyReason: true, Event: EventFatal, To: RunFailed, ToReason: WaitNone},
	{From: RunRunning, Event: EventCancel, To: RunCancelled, ToReason: WaitNone},
	{From: RunWaiting, AnyReason: true, Event: EventCancel, To: RunCancelled, ToReason: WaitNone},
}

// TerminalRun reports whether the run will not move again.
func TerminalRun(s RunStatus) bool {
	switch s {
	case RunCompleted, RunFailed, RunCancelled:
		return true
	default:
		return false
	}
}

func validateRunShape(s RunState) error {
	switch s.Status {
	case RunRunning, RunCompleted, RunFailed, RunCancelled:
		if s.Reason != WaitNone {
			return E(CodeCorrupt, "non-waiting run must not carry a wait reason")
		}
	case RunWaiting:
		switch s.Reason {
		case WaitUser, WaitApproval, WaitChild, WaitBudget, WaitReconciliation:
		default:
			return E(CodeCorrupt, "waiting run requires a wait reason")
		}
	default:
		return E(CodeCorrupt, "unknown run status "+string(s.Status))
	}
	return nil
}

// TransitionRun applies one event. The returned state keeps RunID.
// Illegal pairs return CodeIllegalTransition and do not invent a next state.
func TransitionRun(s RunState, ev RunEvent) (RunState, error) {
	if err := validateRunShape(s); err != nil {
		return RunState{}, err
	}
	for _, row := range RunTransitionTable {
		if row.Event != ev || row.From != s.Status {
			continue
		}
		if !row.AnyReason && row.Reason != s.Reason {
			continue
		}
		return RunState{RunID: s.RunID, Status: row.To, Reason: row.ToReason}, nil
	}
	return RunState{}, E(CodeIllegalTransition, "no run transition for "+string(s.Status)+"/"+string(s.Reason)+" + "+string(ev))
}

// InvStatus is the state of one tool invocation.
type InvStatus string

const (
	InvPending         InvStatus = "PENDING"
	InvWaitingApproval InvStatus = "WAITING_APPROVAL"
	InvRunning         InvStatus = "RUNNING"
	InvUnknown         InvStatus = "UNKNOWN"
	InvSucceeded       InvStatus = "SUCCEEDED"
	InvFailed          InvStatus = "FAILED"
	InvDenied          InvStatus = "DENIED"
	InvSkipped         InvStatus = "SKIPPED"
	InvCancelled       InvStatus = "CANCELLED"
)

// InvEvent is a trigger in the invocation transition table.
type InvEvent string

const (
	InvStart            InvEvent = "start"
	InvRequestApproval  InvEvent = "request_approval"
	InvApprove          InvEvent = "approve"
	InvDeny             InvEvent = "deny"
	InvSucceed          InvEvent = "succeed"
	InvFail             InvEvent = "fail"
	InvMarkUnknown      InvEvent = "mark_unknown"
	InvReconcileSuccess InvEvent = "reconcile_success"
	InvReconcileFail    InvEvent = "reconcile_fail"
	InvSkip             InvEvent = "skip"
	InvCancel           InvEvent = "cancel"
	// InvProtocolReject terminates a call that was never dispatched.
	// The architecture requires a paired protocol error and forbids side effects.
	// FAILED via RUNNING would claim execution had started, so this edge stays
	// out of RUNNING. Dispatched must remain false.
	InvProtocolReject InvEvent = "protocol_reject"
)

// InvTransition is one row of the invocation table.
type InvTransition struct {
	From  InvStatus
	Event InvEvent
	To    InvStatus
}

// InvocationTransitionTable is the plan §7.2 invocation lifecycle.
// UNKNOWN is intentionally not terminal and has no edge back to RUNNING.
var InvocationTransitionTable = []InvTransition{
	{From: InvPending, Event: InvStart, To: InvRunning},
	{From: InvPending, Event: InvRequestApproval, To: InvWaitingApproval},
	{From: InvPending, Event: InvDeny, To: InvDenied},
	{From: InvPending, Event: InvSkip, To: InvSkipped},
	{From: InvPending, Event: InvCancel, To: InvCancelled},
	{From: InvPending, Event: InvProtocolReject, To: InvFailed},
	{From: InvWaitingApproval, Event: InvApprove, To: InvRunning},
	{From: InvWaitingApproval, Event: InvDeny, To: InvDenied},
	{From: InvWaitingApproval, Event: InvCancel, To: InvCancelled},
	{From: InvRunning, Event: InvSucceed, To: InvSucceeded},
	{From: InvRunning, Event: InvFail, To: InvFailed},
	{From: InvRunning, Event: InvMarkUnknown, To: InvUnknown},
	{From: InvRunning, Event: InvCancel, To: InvCancelled},
	{From: InvUnknown, Event: InvReconcileSuccess, To: InvSucceeded},
	{From: InvUnknown, Event: InvReconcileFail, To: InvFailed},
	{From: InvUnknown, Event: InvCancel, To: InvCancelled},
}

// TerminalInvocation reports whether the invocation must not be executed again.
// UNKNOWN is not terminal.
func TerminalInvocation(s InvStatus) bool {
	switch s {
	case InvSucceeded, InvFailed, InvDenied, InvSkipped, InvCancelled:
		return true
	default:
		return false
	}
}

// TransitionInvocation applies one event.
func TransitionInvocation(from InvStatus, ev InvEvent) (InvStatus, error) {
	if from == "" {
		return "", E(CodeCorrupt, "empty invocation status")
	}
	for _, row := range InvocationTransitionTable {
		if row.From == from && row.Event == ev {
			return row.To, nil
		}
	}
	return "", E(CodeIllegalTransition, "no invocation transition for "+string(from)+" + "+string(ev))
}
