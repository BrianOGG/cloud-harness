package domain

import (
	"encoding/json"
	"errors"
)

// BatchStatus is the lifecycle of one ordered tool batch.
type BatchStatus string

const (
	BatchOpen                BatchStatus = "open"
	BatchWaitingApproval     BatchStatus = "waiting_approval"
	BatchWaitingUser         BatchStatus = "waiting_user"
	BatchComplete            BatchStatus = "complete"
	BatchProtocolError       BatchStatus = "protocol_error"
	BatchNeedsReconciliation BatchStatus = "needs_reconciliation"
)

// CallResult is the paired result for one provider call id.
type CallResult struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error *Error          `json:"error,omitempty"`
}

// Invocation is one call inside a batch. Dispatched is true only after an
// external executor was entered (status passed through RUNNING for that reason).
type Invocation struct {
	ID             string          `json:"id"`
	Ordinal        int             `json:"ordinal"`
	ProviderCallID string          `json:"provider_call_id"`
	Name           string          `json:"name"`
	Arguments      json.RawMessage `json:"arguments"`
	Class          Action          `json:"class"`
	Status         InvStatus       `json:"status"`
	Dispatched     bool            `json:"dispatched"`
	Result         *CallResult     `json:"result,omitempty"`
	Version        int             `json:"version,omitempty"`
}

// Batch is the serial execution record for one model response.
type Batch struct {
	ID            string       `json:"id"`
	RunID         string       `json:"run_id"`
	Action        Action       `json:"action"`
	Status        BatchStatus  `json:"status"`
	Invocations   []Invocation `json:"invocations"`
	ProtocolError *Error       `json:"protocol_error,omitempty"`
}

// Accepted reports whether the batch may dispatch external tools.
func (b Batch) Accepted() bool {
	return b.Status != BatchProtocolError && b.ProtocolError == nil
}

// PlanBatch freezes a model response into a batch.
// A protocol failure still returns a batch whose every call has a paired
// error result and Dispatched=false, together with the protocol error.
// Exclusive finish_turn is confirmed in place and is not an external dispatch.
func PlanBatch(runID string, resp ModelResponse, newID func(prefix string) string) (Batch, error) {
	if newID == nil {
		newID = NewID
	}
	action, classErr := Classify(resp)
	calls := resp.ToolCalls()
	b := Batch{
		ID:     newID("bat_"),
		RunID:  runID,
		Action: action,
		Status: BatchOpen,
	}
	if classErr != nil {
		b.Action = ActionProtocol
		b.Status = BatchProtocolError
		b.ProtocolError = asDomain(classErr)
		for i, c := range calls {
			inv := newInvocation(newID, i, c)
			if err := (&b).reject(inv); err != nil {
				return Batch{}, err
			}
		}
		if len(calls) == 0 {
			// No call id exists. The protocol error itself is the result.
			return b, b.ProtocolError
		}
		return b, b.ProtocolError
	}
	for i, c := range calls {
		inv := newInvocation(newID, i, c)
		b.Invocations = append(b.Invocations, inv)
	}
	if action == ActionAnswer && len(calls) == 1 && calls[0].Name == ToolFinishTurn {
		if err := b.confirmFinish(0); err != nil {
			return Batch{}, err
		}
	}
	if len(calls) == 0 {
		b.Status = BatchComplete
	}
	if err := b.CheckInvariants(); err != nil {
		return Batch{}, err
	}
	return b, nil
}

func newInvocation(newID func(prefix string) string, ordinal int, c ToolCall) Invocation {
	return Invocation{
		ID:             newID("inv_"),
		Ordinal:        ordinal,
		ProviderCallID: c.ProviderCallID,
		Name:           c.Name,
		Arguments:      append(json.RawMessage(nil), c.Arguments...),
		Class:          CallClass(c.Name),
		Status:         InvPending,
	}
}

func (b *Batch) reject(inv Invocation) error {
	next, err := TransitionInvocation(InvPending, InvProtocolReject)
	if err != nil {
		return err
	}
	inv.Status = next
	inv.Dispatched = false
	inv.Result = &CallResult{OK: false, Error: E(CodeProtocol, "batch refused before execution").WithRef(inv.ProviderCallID)}
	b.Invocations = append(b.Invocations, inv)
	return nil
}

func (b *Batch) confirmFinish(index int) error {
	// Pass through RUNNING only in memory so the table is honored, then succeed.
	// Nothing is dispatched, and callers persist the terminal SUCCEEDED row.
	if err := b.step(index, InvStart); err != nil {
		return err
	}
	b.Invocations[index].Dispatched = false
	if err := b.step(index, InvSucceed); err != nil {
		return err
	}
	b.Invocations[index].Result = &CallResult{
		OK:   true,
		Data: json.RawMessage(`{"status":"accepted"}`),
	}
	b.Status = BatchComplete
	return nil
}

func (b *Batch) step(index int, ev InvEvent) error {
	next, err := TransitionInvocation(b.Invocations[index].Status, ev)
	if err != nil {
		return err
	}
	b.Invocations[index].Status = next
	return nil
}

func (b *Batch) index(id string) (int, error) {
	for i, inv := range b.Invocations {
		if inv.ID == id {
			return i, nil
		}
	}
	return 0, E(CodeNotFound, "invocation not in batch").WithRef(id)
}

// MarkDispatched moves the next PENDING invocation to RUNNING.
// It refuses any invocation that is not the earliest unfinished call.
func (b *Batch) MarkDispatched(id string) error {
	if !b.Accepted() {
		return E(CodeProtocol, "protocol-error batch cannot dispatch")
	}
	idx, err := b.index(id)
	if err != nil {
		return err
	}
	if err := b.onlyNext(idx); err != nil {
		return err
	}
	if b.Invocations[idx].Status != InvPending {
		return E(CodeIllegalTransition, "only a pending invocation can be dispatched")
	}
	if err := b.step(idx, InvStart); err != nil {
		return err
	}
	b.Invocations[idx].Dispatched = true
	b.Status = BatchOpen
	return nil
}

func (b *Batch) onlyNext(idx int) error {
	for i := 0; i < idx; i++ {
		st := b.Invocations[i].Status
		if st == InvUnknown || st == InvRunning {
			return E(CodeConflict, "earlier invocation is unresolved")
		}
		if !TerminalInvocation(st) {
			return E(CodeConflict, "earlier invocation is not finished")
		}
	}
	return nil
}

// RequestApproval pauses the next pending call. Later calls stay PENDING.
func (b *Batch) RequestApproval(id string) error {
	idx, err := b.index(id)
	if err != nil {
		return err
	}
	if err := b.onlyNext(idx); err != nil {
		return err
	}
	if err := b.step(idx, InvRequestApproval); err != nil {
		return err
	}
	b.Status = BatchWaitingApproval
	return nil
}

// BeginUserWait records an exclusive ask_user pause. The invocation is not dispatched.
func (b *Batch) BeginUserWait(id string) error {
	idx, err := b.index(id)
	if err != nil {
		return err
	}
	if b.Action != ActionAskUser || len(b.Invocations) != 1 {
		return E(CodeProtocol, "user wait requires an exclusive ask_user batch")
	}
	if b.Invocations[idx].Dispatched {
		return E(CodeConflict, "ask_user was dispatched")
	}
	b.Status = BatchWaitingUser
	return nil
}

// CompleteUserWait resolves an exclusive ask_user pause with the user's reply.
// The invocation passes through RUNNING in memory to honor the transition table,
// then becomes SUCCEEDED with Dispatched=false.
func (b *Batch) CompleteUserWait(id string, data json.RawMessage) error {
	idx, err := b.index(id)
	if err != nil {
		return err
	}
	if b.Action != ActionAskUser || len(b.Invocations) != 1 {
		return E(CodeProtocol, "user wait requires an exclusive ask_user batch")
	}
	if b.Invocations[idx].Dispatched {
		return E(CodeConflict, "ask_user was dispatched")
	}
	if b.Invocations[idx].Status == InvPending {
		if err := b.step(idx, InvStart); err != nil {
			return err
		}
		b.Invocations[idx].Dispatched = false
	}
	if err := b.step(idx, InvSucceed); err != nil {
		return err
	}
	b.Invocations[idx].Result = &CallResult{
		OK:   true,
		Data: append(json.RawMessage(nil), data...),
	}
	b.Status = BatchComplete
	return b.Pairing()
}

// Approve continues a waiting call by moving it to RUNNING and marking it dispatched
// before the executor is entered.
func (b *Batch) Approve(id string) error {
	idx, err := b.index(id)
	if err != nil {
		return err
	}
	if err := b.step(idx, InvApprove); err != nil {
		return err
	}
	b.Invocations[idx].Dispatched = true
	b.Status = BatchOpen
	return nil
}

// Deny terminates the call and marks every later pending call SKIPPED.
func (b *Batch) Deny(id string) error {
	idx, err := b.index(id)
	if err != nil {
		return err
	}
	if err := b.step(idx, InvDeny); err != nil {
		return err
	}
	b.Invocations[idx].Result = &CallResult{OK: false, Error: E(CodeAuthDenied, "approval denied").WithRef(b.Invocations[idx].ProviderCallID)}
	for i := idx + 1; i < len(b.Invocations); i++ {
		if b.Invocations[i].Status != InvPending {
			continue
		}
		if err := b.step(i, InvSkip); err != nil {
			return err
		}
		b.Invocations[i].Result = &CallResult{OK: false, Error: E(CodeSkipped, "skipped after denial").WithRef(b.Invocations[i].ProviderCallID)}
	}
	b.Status = BatchComplete
	return b.Pairing()
}

// RecordSuccess stores a terminal success for a RUNNING invocation.
func (b *Batch) RecordSuccess(id string, data json.RawMessage) error {
	idx, err := b.index(id)
	if err != nil {
		return err
	}
	if !b.Invocations[idx].Dispatched {
		return E(CodeConflict, "success recorded without dispatch")
	}
	if err := b.step(idx, InvSucceed); err != nil {
		return err
	}
	b.Invocations[idx].Result = &CallResult{OK: true, Data: append(json.RawMessage(nil), data...)}
	b.refresh()
	return nil
}

// RecordFailure stores a terminal tool failure for a RUNNING invocation.
func (b *Batch) RecordFailure(id string, cause *Error) error {
	idx, err := b.index(id)
	if err != nil {
		return err
	}
	if !b.Invocations[idx].Dispatched {
		return E(CodeConflict, "failure recorded without dispatch")
	}
	if err := b.step(idx, InvFail); err != nil {
		return err
	}
	if cause == nil {
		cause = E(CodeToolError, "tool failed")
	}
	b.Invocations[idx].Result = &CallResult{OK: false, Error: cause.WithRef(b.Invocations[idx].ProviderCallID)}
	b.refresh()
	return nil
}

// MarkUnknown records that a dispatched call has no confirmed outcome.
func (b *Batch) MarkUnknown(id string) error {
	idx, err := b.index(id)
	if err != nil {
		return err
	}
	if !b.Invocations[idx].Dispatched {
		return E(CodeConflict, "unknown requires a dispatch")
	}
	if err := b.step(idx, InvMarkUnknown); err != nil {
		return err
	}
	b.Status = BatchNeedsReconciliation
	return nil
}

// Recover inspects a batch after a crash. RUNNING means the outcome was not
// observed, so it becomes UNKNOWN. SUCCEEDED calls are left untouched and are
// not eligible for dispatch. PENDING calls that never started stay PENDING.
func (b *Batch) Recover() error {
	for i := range b.Invocations {
		if b.Invocations[i].Status == InvRunning {
			if err := b.step(i, InvMarkUnknown); err != nil {
				return err
			}
		}
	}
	b.refresh()
	return nil
}

func (b *Batch) refresh() {
	unknown := false
	approval := false
	allTerminal := len(b.Invocations) > 0
	for _, inv := range b.Invocations {
		switch inv.Status {
		case InvUnknown:
			unknown = true
			allTerminal = false
		case InvWaitingApproval:
			approval = true
			allTerminal = false
		case InvRunning, InvPending:
			allTerminal = false
		}
	}
	switch {
	case unknown:
		b.Status = BatchNeedsReconciliation
	case approval:
		b.Status = BatchWaitingApproval
	case allTerminal:
		b.Status = BatchComplete
	default:
		if b.Status != BatchWaitingUser {
			b.Status = BatchOpen
		}
	}
}

// NextPending is the earliest PENDING invocation that may be dispatched.
// It returns false when the batch is blocked, refused, or finished.
// A SUCCEEDED invocation is never returned.
func (b *Batch) NextPending() (*Invocation, bool) {
	if b.Status == BatchProtocolError || b.Status == BatchNeedsReconciliation || b.Status == BatchWaitingApproval || b.Status == BatchWaitingUser || b.Status == BatchComplete {
		return nil, false
	}
	for i := range b.Invocations {
		st := b.Invocations[i].Status
		if st == InvSucceeded || st == InvFailed || st == InvDenied || st == InvSkipped || st == InvCancelled {
			continue
		}
		if st == InvPending {
			cp := b.Invocations[i]
			return &cp, true
		}
		return nil, false
	}
	return nil, false
}

// RunningUnresolved is a dispatched call that still needs an executor result.
func (b *Batch) RunningUnresolved() (*Invocation, bool) {
	for i := range b.Invocations {
		if b.Invocations[i].Status == InvRunning && b.Invocations[i].Result == nil {
			cp := b.Invocations[i]
			return &cp, true
		}
	}
	return nil, false
}

// GateDecision is the broker's pre-dispatch decision for one pending call.
type GateDecision string

const (
	GateAllow         GateDecision = "allow"
	GateDeny          GateDecision = "deny"
	GateNeedsApproval GateDecision = "needs_approval"
)

// ErrUnknownOutcome means a dispatched call ended without a confirmed result.
// Drive leaves the invocation RUNNING so recovery can mark it UNKNOWN.
// It must not be recorded as an ordinary failure or retried automatically.
var ErrUnknownOutcome = errors.New("tool outcome unknown")

// ExecFunc performs one external tool call. It is not used for protocol errors.
type ExecFunc func(Invocation) (json.RawMessage, error)

// Drive runs the serial batch until it completes, waits, or must reconcile.
// exec is not called for a protocol-error batch, a denied call, or a call that
// already has a terminal result.
func Drive(b *Batch, gate func(Invocation) GateDecision, exec ExecFunc) error {
	if b.Status == BatchProtocolError {
		return b.ProtocolError
	}
	for {
		if inv, ok := b.RunningUnresolved(); ok {
			if exec == nil {
				return E(CodeConflict, "running invocation requires an executor")
			}
			data, err := exec(*inv)
			if errors.Is(err, ErrUnknownOutcome) {
				return err
			}
			if err != nil {
				cause := asDomain(err)
				if cause.Code == "" {
					cause = E(CodeToolError, err.Error())
				}
				if recErr := b.RecordFailure(inv.ID, cause); recErr != nil {
					return recErr
				}
			} else if recErr := b.RecordSuccess(inv.ID, data); recErr != nil {
				return recErr
			}
			continue
		}
		inv, ok := b.NextPending()
		if !ok {
			return nil
		}
		decision := GateAllow
		if gate != nil {
			decision = gate(*inv)
		}
		switch decision {
		case GateNeedsApproval:
			return b.RequestApproval(inv.ID)
		case GateDeny:
			return b.Deny(inv.ID)
		case GateAllow:
			if err := b.MarkDispatched(inv.ID); err != nil {
				return err
			}
		default:
			return E(CodeInvalidArgument, "unknown gate decision")
		}
	}
}

// Pairing checks that every invocation has exactly one result once the batch is complete,
// and that provider call ids are unique.
func (b Batch) Pairing() error {
	seen := map[string]int{}
	for _, inv := range b.Invocations {
		if inv.ProviderCallID == "" {
			return E(CodeProtocol, "invocation missing provider_call_id")
		}
		seen[inv.ProviderCallID]++
		if seen[inv.ProviderCallID] > 1 {
			return E(CodeProtocol, "duplicate provider_call_id "+inv.ProviderCallID)
		}
		if b.Status == BatchComplete || b.Status == BatchProtocolError {
			if inv.Result == nil || !TerminalInvocation(inv.Status) {
				return E(CodeProtocol, "complete batch has an unpaired call "+inv.ProviderCallID)
			}
		}
	}
	return nil
}

// CheckInvariants checks the batch rules that do not depend on persistence.
func (b Batch) CheckInvariants() error {
	if b.ID == "" {
		return E(CodeCorrupt, "batch id required")
	}
	ords := map[int]struct{}{}
	for i, inv := range b.Invocations {
		if inv.Ordinal != i {
			return E(CodeCorrupt, "batch ordinals must be contiguous")
		}
		if _, ok := ords[inv.Ordinal]; ok {
			return E(CodeCorrupt, "duplicate ordinal")
		}
		ords[inv.Ordinal] = struct{}{}
		if TerminalInvocation(inv.Status) && inv.Result == nil && b.Status != BatchOpen {
			return E(CodeCorrupt, "terminal invocation missing result")
		}
		if inv.Dispatched && inv.Status == InvPending {
			return E(CodeCorrupt, "pending invocation cannot be dispatched")
		}
		if inv.Status == InvFailed && !inv.Dispatched && inv.Result != nil && inv.Result.Error != nil && inv.Result.Error.Code == CodeProtocol {
			// Protocol rejection. Allowed.
			continue
		}
	}
	if b.Status != BatchProtocolError {
		if err := b.Pairing(); err != nil && b.Status == BatchComplete {
			return err
		}
	}
	return nil
}

// ResultsByCallID returns terminal results keyed by provider call id.
func (b Batch) ResultsByCallID() map[string]CallResult {
	out := map[string]CallResult{}
	for _, inv := range b.Invocations {
		if inv.Result != nil {
			out[inv.ProviderCallID] = *inv.Result
		}
	}
	return out
}

func asDomain(err error) *Error {
	if err == nil {
		return nil
	}
	var de *Error
	if errors.As(err, &de) {
		return de
	}
	return E(CodeToolError, err.Error())
}
