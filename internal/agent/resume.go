package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/provider"
	"github.com/cloud-harness/cloud-harness/internal/storage"
	"github.com/cloud-harness/cloud-harness/internal/tools"
)

// PendingInvocation summarizes a tool call waiting for approval.
type PendingInvocation struct {
	InvocationID  string          `json:"invocation_id"`
	Tool          string          `json:"tool"`
	ArgsSummary   string          `json:"args_summary"`
	Arguments     json.RawMessage `json:"arguments,omitempty"`
	Resources     []string        `json:"resources,omitempty"`
	ResourceScope string          `json:"resource_scope,omitempty"`
}

// WaitInfo describes the pending wait state when a run is in WAITING.
type WaitInfo struct {
	Kind              domain.WaitReason  `json:"kind"`
	Question          string             `json:"question,omitempty"`
	PendingInvocation *PendingInvocation `json:"pending_invocation,omitempty"`
}

// WaitInfo returns the wait kind and pending question or invocation summary for a waiting run.
func (m *Manager) WaitInfo(ctx context.Context, run storage.Run) (WaitInfo, error) {
	if run.Status != domain.RunWaiting {
		return WaitInfo{}, nil
	}
	info := WaitInfo{Kind: run.WaitReason}
	if m == nil || m.Store == nil {
		return info, nil
	}
	batches, err := m.Store.LoadBatches(ctx, run.ID)
	if err != nil {
		return info, err
	}
	switch run.WaitReason {
	case domain.WaitUser:
		for i := len(batches) - 1; i >= 0; i-- {
			for _, inv := range batches[i].Invocations {
				if inv.Name == domain.ToolAskUser || batches[i].Action == domain.ActionAskUser {
					if q := jsonString(inv.Arguments, "question"); q != "" {
						info.Question = q
						return info, nil
					}
				}
			}
		}
	case domain.WaitApproval:
		var waiting *domain.Invocation
		for i := len(batches) - 1; i >= 0 && waiting == nil; i-- {
			for j := range batches[i].Invocations {
				if batches[i].Invocations[j].Status == domain.InvWaitingApproval {
					cp := batches[i].Invocations[j]
					waiting = &cp
					break
				}
			}
		}
		approvals, _ := m.Store.ListApprovals(ctx, run.ID)
		if waiting != nil {
			m.ensureTools()
			resources, _ := m.approvalResources(*waiting)
			scope := strings.Join(resources, ",")
			for _, a := range approvals {
				if a.InvocationID == waiting.ID || a.State == "pending" {
					if scope == "" && a.ResourceScope != "" {
						scope = a.ResourceScope
					}
					if len(resources) == 0 && a.ResourceScope != "" {
						resources = strings.Split(a.ResourceScope, ",")
					}
				}
			}
			if len(resources) == 0 {
				if p := jsonString(waiting.Arguments, "path"); p != "" {
					resources = []string{p}
					scope = p
				} else if cmd := jsonString(waiting.Arguments, "command"); cmd != "" {
					resources = []string{cmd}
					scope = cmd
				}
			}
			info.PendingInvocation = &PendingInvocation{
				InvocationID:  waiting.ID,
				Tool:          waiting.Name,
				ArgsSummary:   SummarizeArgs(waiting.Name, waiting.Arguments),
				Arguments:     waiting.Arguments,
				Resources:     resources,
				ResourceScope: scope,
			}
			return info, nil
		}
		for i := len(approvals) - 1; i >= 0; i-- {
			a := approvals[i]
			if a.State == "pending" {
				var res []string
				if a.ResourceScope != "" {
					res = strings.Split(a.ResourceScope, ",")
				}
				info.PendingInvocation = &PendingInvocation{
					InvocationID:  a.InvocationID,
					Tool:          a.Tool,
					ArgsSummary:   "{}",
					Resources:     res,
					ResourceScope: a.ResourceScope,
				}
				return info, nil
			}
		}
	}
	return info, nil
}

// SummarizeArgs formats a concise single-line summary of tool arguments.
func SummarizeArgs(_ string, args json.RawMessage) string {
	if len(args) == 0 {
		return "{}"
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, args); err != nil {
		s := strings.TrimSpace(string(args))
		if len(s) > 160 {
			return s[:160] + "..."
		}
		return s
	}
	s := buf.String()
	if len(s) <= 160 {
		return s
	}
	if p := jsonString(args, "path"); p != "" {
		return "path=" + p + " " + s[:120] + "..."
	}
	if cmd := jsonString(args, "command"); cmd != "" {
		if len(cmd) > 80 {
			cmd = cmd[:80] + "..."
		}
		return "command=" + cmd + " " + s[:120] + "..."
	}
	return s[:160] + "..."
}

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
	st := &loopState{
		sensitive: snap.SensitiveRead,
		reminded:  snap.Reminded,
		obs:       snap.Observations,
		toolRefs:  snap.ToolRefs,
		prefixN:   snap.PrefixN,
		userAt:    snap.UserAt,
	}
	ctx = context.WithValue(ctx, sessionKey, running.SessionID)
	ctx = context.WithValue(ctx, runKey, running.ID)
	switch run.WaitReason {
	case domain.WaitApproval:
		return m.resumeApproval(ctx, running, lease.LeaseVersion, snap, st, in.Decision)
	case domain.WaitUser:
		return m.resumeUser(ctx, running, lease.LeaseVersion, snap, st, in.Answer)
	case domain.WaitBudget, domain.WaitChild:
		return m.loopFrom(ctx, running, lease.LeaseVersion, snap.Messages, snap.Continuation, snap.ContProvider, st)
	case domain.WaitReconciliation:
		return m.resumeReconcile(ctx, running, lease.LeaseVersion, snap, st, in.Decision == "reconcile_success")
	default:
		return running, domain.E(domain.CodeConflict, "resume was not applied")
	}
}

func (m *Manager) resumeUser(ctx context.Context, run storage.Run, leaseVersion int64, snap snapshot, st *loopState, answer string) (storage.Run, error) {
	batches, err := m.Store.LoadBatches(ctx, run.ID)
	if err != nil {
		return storage.Run{}, err
	}
	if len(batches) == 0 {
		return storage.Run{}, domain.E(domain.CodeConflict, "ask_user resume has no batch").WithRef(run.ID)
	}
	stored := batches[len(batches)-1]
	batch := domain.Batch{ID: stored.ID, RunID: stored.RunID, Action: stored.Action, Status: stored.Status, Invocations: stored.Invocations}
	if len(batch.Invocations) == 0 {
		return storage.Run{}, domain.E(domain.CodeConflict, "ask_user batch has no invocation").WithRef(run.ID)
	}
	for _, inv := range batch.Invocations {
		st.setVersion(inv.ID, inv.Version)
	}
	replyData, err := json.Marshal(map[string]string{"answer": answer})
	if err != nil {
		return storage.Run{}, err
	}
	if err := batch.CompleteUserWait(batch.Invocations[0].ID, replyData); err != nil {
		return storage.Run{}, err
	}
	if err := m.Store.UpdateBatch(ctx, storage.StoredBatch{
		ID: batch.ID, ModelResponseID: stored.ModelResponseID, RunID: run.ID,
		Status: batch.Status, Action: batch.Action, Invocations: batch.Invocations,
	}); err != nil {
		return storage.Run{}, err
	}
	messages := append([]provider.Message(nil), snap.Messages...)
	messages = append(messages, assistantFromBatch(batch))
	messages = append(messages, toolResultMessages(batch)...)
	messages = append(messages, provider.Message{Role: "user", Content: "User reply: " + answer})
	return m.loopFrom(ctx, run, leaseVersion, messages, snap.Continuation, snap.ContProvider, st)
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
	var waiting *domain.Invocation
	for i := range batch.Invocations {
		inv := batch.Invocations[i]
		st.setVersion(inv.ID, inv.Version)
		if inv.Status == domain.InvWaitingApproval && waiting == nil {
			cp := inv
			waiting = &cp
		}
	}
	if waiting == nil {
		return storage.Run{}, domain.E(domain.CodeConflict, "no invocation is waiting for approval").WithRef(run.ID)
	}
	if decision == "deny" {
		if err := batch.Deny(waiting.ID); err != nil {
			return storage.Run{}, err
		}
	} else {
		if err := m.grantApprovedInvocation(ctx, run, *waiting); err != nil {
			return storage.Run{}, err
		}
		if err := batch.Approve(waiting.ID); err != nil {
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
			if err := m.recordPendingApproval(ctx, run, batch); err != nil {
				return storage.Run{}, err
			}
			return m.finish(ctx, run, domain.EventApproval, "", nil)
		}
		return m.finish(ctx, run, domain.EventNeedsReconcile, "", domain.E(domain.CodeConflict, "a tool result is unknown and was not replayed"))
	}
	return m.loopFrom(ctx, run, leaseVersion, messages, snap.Continuation, snap.ContProvider, st)
}

func (m *Manager) recordPendingApproval(ctx context.Context, run storage.Run, batch domain.Batch) error {
	if m.Store == nil {
		return nil
	}
	var waiting *domain.Invocation
	for i := range batch.Invocations {
		if batch.Invocations[i].Status == domain.InvWaitingApproval {
			waiting = &batch.Invocations[i]
			break
		}
	}
	if waiting == nil {
		return nil
	}
	resources, _ := m.approvalResources(*waiting)
	return m.Store.SaveApproval(ctx, domain.Approval{
		ID:            domain.NewID("apr_"),
		RunID:         run.ID,
		InvocationID:  waiting.ID,
		Tool:          waiting.Name,
		ResourceScope: strings.Join(resources, ","),
		State:         "pending",
		CreatedAt:     m.now(),
	})
}

func (m *Manager) grantApprovedInvocation(ctx context.Context, run storage.Run, inv domain.Invocation) error {
	if m.Store == nil {
		return nil
	}
	now := m.now()
	resources, err := m.approvalResources(inv)
	if err != nil {
		return err
	}
	scope := strings.Join(resources, ",")
	approvals, err := m.Store.ListApprovals(ctx, run.ID)
	if err != nil {
		return err
	}
	found := false
	for _, a := range approvals {
		if a.InvocationID == inv.ID {
			found = true
			break
		}
	}
	if found {
		if err := m.Store.UpdateApprovalState(ctx, run.ID, inv.ID, "approved"); err != nil {
			return err
		}
	} else {
		if err := m.Store.SaveApproval(ctx, domain.Approval{
			ID:            domain.NewID("apr_"),
			RunID:         run.ID,
			InvocationID:  inv.ID,
			Tool:          inv.Name,
			ResourceScope: scope,
			State:         "approved",
			CreatedAt:     now,
		}); err != nil {
			return err
		}
	}
	if len(resources) == 0 {
		resources = []string{""}
	}
	for _, r := range resources {
		g := domain.Grant{
			ID:            domain.NewID("grd_"),
			Subject:       "approval",
			RunID:         run.ID,
			SessionID:     run.SessionID,
			InvocationID:  inv.ID,
			Tool:          inv.Name,
			Op:            "call",
			ResourceScope: r,
			State:         "active",
		}
		if err := m.Store.SaveGrant(ctx, g, now); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) approvalResources(inv domain.Invocation) ([]string, error) {
	if m.Broker == nil {
		return nil, nil
	}
	return m.Broker.ExtractResources(inv.Name, inv.Arguments)
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

type expiredRunFinder interface {
	ListExpiredRunningRuns(ctx context.Context, now time.Time) ([]storage.Run, error)
}

// RecoverExpired scans for lease-expired RUNNING runs and recovers each one.
func (m *Manager) RecoverExpired(ctx context.Context) ([]storage.Run, error) {
	if m.Store == nil {
		return nil, nil
	}
	now := m.now()
	var runs []storage.Run
	var err error
	if finder, ok := m.Store.(expiredRunFinder); ok {
		runs, err = finder.ListExpiredRunningRuns(ctx, now)
	} else {
		all, lerr := m.Store.ListRuns(ctx, "")
		if lerr != nil {
			return nil, lerr
		}
		for _, r := range all {
			if r.Status == domain.RunRunning {
				runs = append(runs, r)
			}
		}
	}
	if err != nil {
		return nil, err
	}
	recovered := make([]storage.Run, 0, len(runs))
	for _, r := range runs {
		got, err := m.Recover(ctx, r.ID)
		if err != nil {
			if domain.IsCode(err, domain.CodeLease) {
				continue
			}
			return recovered, err
		}
		recovered = append(recovered, got)
	}
	return recovered, nil
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
	return name == "write_file" || name == "edit_file" || name == "run_command"
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
