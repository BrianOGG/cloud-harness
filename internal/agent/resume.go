package agent

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/provider"
	"github.com/cloud-harness/cloud-harness/internal/storage"
	"github.com/cloud-harness/cloud-harness/internal/tools"
)

// ResumeInput is how a caller continues a waiting run. It never creates a run.
type ResumeInput struct {
	Decision        string
	Answer          string
	ExtraModelCalls int
}

// Resume continues the same waiting run after a lease is acquired.
// A live lease held by another owner is rejected. An expired lease can be taken.
func (m *Manager) Resume(ctx context.Context, runID string, in ResumeInput) (storage.Run, error) {
	run, err := m.Store.GetRun(ctx, runID)
	if err != nil {
		return storage.Run{}, err
	}
	if run.Status != domain.RunWaiting {
		return run, domain.E(domain.CodeNotWaiting, "run is not waiting").WithRef(runID)
	}
	now := m.now()
	lease, err := m.Store.AcquireLease(ctx, run.ID, m.owner(), now, now.Add(m.leaseFor()))
	if err != nil {
		return run, err
	}
	defer m.Store.ReleaseLease(ctx, run.ID, m.owner())
	ctx, cancel := context.WithCancel(ctx)
	m.trackCancel(run.ID, cancel)
	defer func() {
		cancel()
		m.untrackCancel(run.ID)
	}()
	m.ensureTools()
	switch run.WaitReason {
	case domain.WaitBudget:
		if in.ExtraModelCalls <= 0 {
			return run, domain.E(domain.CodeBudget, "budget was not extended").WithRef(run.ID)
		}
		if err := m.Store.ExtendModelBudget(ctx, run.ID, in.ExtraModelCalls); err != nil {
			return storage.Run{}, err
		}
	case domain.WaitApproval:
		if in.Decision != "approve" && in.Decision != "deny" {
			return run, domain.E(domain.CodeInvalidArgument, "approval resume requires decision approve or deny").WithRef(run.ID)
		}
	case domain.WaitUser:
		if in.Answer == "" {
			return run, domain.E(domain.CodeInvalidArgument, "ask_user resume requires an answer").WithRef(run.ID)
		}
	case domain.WaitReconciliation:
		if in.Decision != "reconcile_success" && in.Decision != "reconcile_fail" {
			return run, domain.E(domain.CodeInvalidArgument, "reconciliation requires reconcile_success or reconcile_fail").WithRef(run.ID)
		}
	case domain.WaitChild:
		if in.Decision != "child_done" {
			return run, domain.E(domain.CodeInvalidArgument, "child wait resumes with decision child_done").WithRef(run.ID)
		}
	default:
		return run, domain.E(domain.CodeConflict, "unsupported wait reason "+string(run.WaitReason)).WithRef(run.ID)
	}
	running, err := m.markRunning(ctx, run)
	if err != nil {
		return storage.Run{}, err
	}
	snap, err := m.loadCheckpoint(ctx, run.ID)
	if err != nil {
		return storage.Run{}, err
	}
	st := &loopState{sensitive: snap.SensitiveRead, reminded: snap.Reminded, obs: snap.Observations}
	ctx = context.WithValue(ctx, sessionKey, running.SessionID)
	switch run.WaitReason {
	case domain.WaitApproval:
		return m.resumeApproval(ctx, running, lease.LeaseVersion, snap, st, in.Decision)
	case domain.WaitUser:
		messages := append(snap.Messages, provider.Message{Role: "user", Content: "User reply: " + in.Answer})
		return m.loopFrom(ctx, running, lease.LeaseVersion, messages, snap.Continuation, snap.ContProvider, st)
	case domain.WaitBudget, domain.WaitChild:
		return m.loopFrom(ctx, running, lease.LeaseVersion, snap.Messages, snap.Continuation, snap.ContProvider, st)
	case domain.WaitReconciliation:
		return m.resumeReconcile(ctx, running, lease.LeaseVersion, snap, st, in.Decision == "reconcile_success")
	default:
		return running, domain.E(domain.CodeConflict, "resume was not applied")
	}
}

func (m *Manager) markRunning(ctx context.Context, run storage.Run) (storage.Run, error) {
	current, err := m.Store.GetRun(ctx, run.ID)
	if err != nil {
		return storage.Run{}, err
	}
	next, err := domain.TransitionRun(domain.RunState{RunID: current.ID, Status: current.Status, Reason: current.WaitReason}, domain.EventResume)
	if err != nil {
		return storage.Run{}, err
	}
	current.Status = next.Status
	current.WaitReason = next.Reason
	current.UpdatedAt = m.now()
	ev := storage.Event{Type: "run.resumed", Payload: []byte(`{"status":"RUNNING"}`), Persistent: true, CreatedAt: m.now()}
	if err := m.Store.CommitRunTransition(ctx, current, current.Version, ev); err != nil {
		return storage.Run{}, err
	}
	return m.Store.GetRun(ctx, run.ID)
}

func (m *Manager) resumeApproval(ctx context.Context, run storage.Run, leaseVersion int64, snap snapshot, st *loopState, decision string) (storage.Run, error) {
	batches, err := m.Store.LoadBatches(ctx, run.ID)
	if err != nil {
		return storage.Run{}, err
	}
	if len(batches) == 0 {
		return storage.Run{}, domain.E(domain.CodeConflict, "approval resume has no batch").WithRef(run.ID)
	}
	stored := batches[len(batches)-1]
	batch := domain.Batch{ID: stored.ID, RunID: stored.RunID, Action: stored.Action, Status: stored.Status, Invocations: stored.Invocations}
	var waiting string
	for _, inv := range batch.Invocations {
		if inv.Status == domain.InvWaitingApproval {
			waiting = inv.ID
			st.setVersion(inv.ID, inv.Version)
		}
	}
	if waiting == "" {
		return storage.Run{}, domain.E(domain.CodeConflict, "no invocation is waiting for approval").WithRef(run.ID)
	}
	if decision == "deny" {
		if err := batch.Deny(waiting); err != nil {
			return storage.Run{}, err
		}
	} else {
		if err := batch.Approve(waiting); err != nil {
			return storage.Run{}, err
		}
		if err := domain.Drive(&batch, m.gate(ctx, run, st), m.execute(ctx, run, st)); err != nil && !errors.Is(err, domain.ErrUnknownOutcome) {
			return storage.Run{}, err
		}
	}
	if err := m.Store.UpdateBatch(ctx, storage.StoredBatch{
		ID: batch.ID, ModelResponseID: stored.ModelResponseID, RunID: run.ID,
		Status: batch.Status, Action: batch.Action, Invocations: batch.Invocations,
	}); err != nil {
		return storage.Run{}, err
	}
	messages := append(snap.Messages, assistantFromBatch(batch))
	messages = append(messages, toolResultMessages(batch)...)
	if batch.Status == domain.BatchWaitingApproval || batch.Status == domain.BatchNeedsReconciliation {
		_ = m.saveCheckpoint(ctx, run, snap.Messages, snap.Continuation, snap.ContProvider, st)
		if batch.Status == domain.BatchWaitingApproval {
			return m.finish(ctx, run, domain.EventApproval, "", nil)
		}
		return m.finish(ctx, run, domain.EventNeedsReconcile, "", domain.E(domain.CodeConflict, "a tool result is unknown and was not replayed"))
	}
	return m.loopFrom(ctx, run, leaseVersion, messages, snap.Continuation, snap.ContProvider, st)
}

func (m *Manager) resumeReconcile(ctx context.Context, run storage.Run, leaseVersion int64, snap snapshot, st *loopState, success bool) (storage.Run, error) {
	batches, err := m.Store.LoadBatches(ctx, run.ID)
	if err != nil {
		return storage.Run{}, err
	}
	if len(batches) == 0 {
		return storage.Run{}, domain.E(domain.CodeConflict, "nothing to reconcile").WithRef(run.ID)
	}
	stored := batches[len(batches)-1]
	batch := domain.Batch{ID: stored.ID, RunID: run.ID, Action: stored.Action, Status: stored.Status, Invocations: stored.Invocations}
	for i := range batch.Invocations {
		if batch.Invocations[i].Status != domain.InvUnknown {
			continue
		}
		ev := domain.InvReconcileFail
		status := domain.InvFailed
		result := &domain.CallResult{OK: false, Error: domain.E(domain.CodeConflict, "reconciled as failed without replay").WithRef(batch.Invocations[i].ProviderCallID)}
		if success {
			ev = domain.InvReconcileSuccess
			status = domain.InvSucceeded
			result = &domain.CallResult{OK: true, Data: json.RawMessage(`{"reconciled":true}`)}
		}
		next, err := domain.TransitionInvocation(domain.InvUnknown, ev)
		if err != nil {
			return storage.Run{}, err
		}
		batch.Invocations[i].Status = next
		batch.Invocations[i].Result = result
		if next != status {
			return storage.Run{}, domain.E(domain.CodeConflict, "reconcile transition mismatch")
		}
	}
	batch.Recover()
	if err := m.Store.UpdateBatch(ctx, storage.StoredBatch{
		ID: batch.ID, ModelResponseID: stored.ModelResponseID, RunID: run.ID,
		Status: batch.Status, Action: batch.Action, Invocations: batch.Invocations,
	}); err != nil {
		return storage.Run{}, err
	}
	if batch.Status == domain.BatchNeedsReconciliation {
		return m.finish(ctx, run, domain.EventNeedsReconcile, "", domain.E(domain.CodeConflict, "still unknown"))
	}
	messages := append(snap.Messages, assistantFromBatch(batch))
	messages = append(messages, toolResultMessages(batch)...)
	return m.loopFrom(ctx, run, leaseVersion, messages, snap.Continuation, snap.ContProvider, st)
}

// Recover marks in-flight invocations UNKNOWN and does not execute them again.
// Succeeded calls stay succeeded. The run id does not change.
func (m *Manager) Recover(ctx context.Context, runID string) (storage.Run, error) {
	run, err := m.Store.GetRun(ctx, runID)
	if err != nil {
		return storage.Run{}, err
	}
	now := m.now()
	if _, err := m.Store.AcquireLease(ctx, run.ID, m.owner(), now, now.Add(m.leaseFor())); err != nil {
		return run, err
	}
	defer m.Store.ReleaseLease(ctx, run.ID, m.owner())
	batches, err := m.Store.LoadBatches(ctx, run.ID)
	if err != nil {
		return storage.Run{}, err
	}
	for _, stored := range batches {
		batch := domain.Batch{ID: stored.ID, RunID: run.ID, Action: stored.Action, Status: stored.Status, Invocations: stored.Invocations}
		before := map[string]domain.InvStatus{}
		for _, inv := range batch.Invocations {
			before[inv.ID] = inv.Status
		}
		if err := confirmProbedWrites(ctx, m, run, &batch); err != nil {
			return storage.Run{}, err
		}
		if err := batch.Recover(); err != nil {
			return storage.Run{}, err
		}
		for _, inv := range batch.Invocations {
			if before[inv.ID] == domain.InvSucceeded && inv.Status != domain.InvSucceeded {
				return storage.Run{}, domain.E(domain.CodeConflict, "recovery changed a succeeded invocation")
			}
			if inv.Status == domain.InvUnknown && before[inv.ID] == domain.InvRunning {
				if sideEffect(inv.Name) {
					// Side-effecting UNKNOWN stays unknown. Do not call the tool.
					continue
				}
			}
		}
		if err := m.Store.UpdateBatch(ctx, storage.StoredBatch{
			ID: batch.ID, ModelResponseID: stored.ModelResponseID, RunID: run.ID,
			Status: batch.Status, Action: batch.Action, Invocations: batch.Invocations,
		}); err != nil {
			return storage.Run{}, err
		}
		if batch.Status == domain.BatchNeedsReconciliation && run.Status == domain.RunRunning {
			return m.finish(ctx, run, domain.EventNeedsReconcile, "", domain.E(domain.CodeConflict, "in-flight tool is UNKNOWN and was not replayed"))
		}
	}
	return m.Store.GetRun(ctx, run.ID)
}

// WaitForChild records WAITING(CHILD) on this run. It does not start a sub-agent.
func (m *Manager) WaitForChild(ctx context.Context, runID string) (storage.Run, error) {
	run, err := m.Store.GetRun(ctx, runID)
	if err != nil {
		return storage.Run{}, err
	}
	if run.Status != domain.RunRunning {
		return run, domain.E(domain.CodeConflict, "only a running run can wait for a child").WithRef(runID)
	}
	snap, _ := m.loadCheckpoint(ctx, runID)
	if len(snap.Messages) == 0 {
		snap.Messages = []provider.Message{{Role: "user", Content: "waiting for child"}}
		_ = m.saveCheckpoint(ctx, run, snap.Messages, nil, "", nil)
	}
	return m.finish(ctx, run, domain.EventWaitChild, "", nil)
}

func assistantFromBatch(batch domain.Batch) provider.Message {
	return assistantFromInvocations(batch.Invocations)
}

func assistantFromInvocations(inv []domain.Invocation) provider.Message {
	msg := provider.Message{Role: "assistant"}
	for _, item := range inv {
		msg.ToolCalls = append(msg.ToolCalls, domain.ToolCall{
			ProviderCallID: item.ProviderCallID, Name: item.Name, Arguments: item.Arguments,
		})
	}
	return msg
}

func sideEffect(name string) bool {
	return name == "write_file" || name == "run_command"
}

// confirmProbedWrites records a side effect only when the tool's status probe
// proves the bytes are already present. It does not call Execute.
func confirmProbedWrites(ctx context.Context, m *Manager, run storage.Run, batch *domain.Batch) error {
	if m.Broker == nil {
		return nil
	}
	for i := range batch.Invocations {
		inv := batch.Invocations[i]
		if inv.Status != domain.InvRunning || !sideEffect(inv.Name) {
			continue
		}
		probed, err := m.Broker.Probe(ctx, tools.Request{
			RunID: run.ID, SessionID: run.SessionID, Tool: inv.Name, Args: inv.Arguments,
		})
		if err != nil {
			return err
		}
		if !probed.Known || !probed.OK {
			continue
		}
		if err := batch.MarkUnknown(inv.ID); err != nil {
			return err
		}
		next, err := domain.TransitionInvocation(domain.InvUnknown, domain.InvReconcileSuccess)
		if err != nil {
			return err
		}
		batch.Invocations[i].Status = next
		batch.Invocations[i].Result = &domain.CallResult{OK: true, Data: probed.Data}
		if err := m.event(ctx, run.ID, "tool.probed", map[string]any{
			"invocation_id": inv.ID, "tool": inv.Name, "provider_call_id": inv.ProviderCallID,
		}); err != nil {
			return err
		}
	}
	return nil
}
