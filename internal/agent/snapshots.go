package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	workspacesnapshot "github.com/BrianOGG/cloud-harness/internal/snapshot"
	"github.com/BrianOGG/cloud-harness/internal/storage"
	"github.com/BrianOGG/cloud-harness/internal/tools"
)

// This state survives WAITING and a new manager loading the checkpoint. The
// pre marker records the attempt as well as success, so resume never retries it.
type workspaceSnapshotState struct {
	BatchNumber  int    `json:"batch_number,omitempty"`
	BatchID      string `json:"batch_id,omitempty"`
	PreAttempted bool   `json:"pre_attempted,omitempty"`
	Unavailable  bool   `json:"unavailable,omitempty"`
}

type runSnapshotter struct {
	once   sync.Once
	handle *workspacesnapshot.Snapshotter
	err    error
}

// Bind the existing nullable event_seq field at record creation, without
// changing Snapshot's API or ever guessing a sequence from a run version.
type runSnapshotStore struct {
	workspacesnapshot.RecordStore
	events storage.Store
	runID  string
}

func (s runSnapshotStore) CreateWorkspaceSnapshot(ctx context.Context, rec storage.WorkspaceSnapshot) (storage.WorkspaceSnapshot, error) {
	// During Execute the invocation is RUNNING. These review points capture
	// code only and cannot serve as chat rewind boundaries.
	if strings.Contains(rec.Label, "/exec_") {
		return s.RecordStore.CreateWorkspaceSnapshot(ctx, rec)
	}
	if events, err := s.events.ListEvents(ctx, s.runID, 0); err == nil {
		for _, ev := range events {
			if ev.Persistent && ev.Seq > 0 && (rec.EventSeq == nil || ev.Seq > *rec.EventSeq) {
				seq := ev.Seq
				rec.EventSeq = &seq
			}
		}
	}
	return s.RecordStore.CreateWorkspaceSnapshot(ctx, rec)
}

func (m *Manager) runSnapshotter(runID string) *runSnapshotter {
	m.snapshotMu.Lock()
	defer m.snapshotMu.Unlock()
	if m.snapshotters == nil {
		m.snapshotters = make(map[string]*runSnapshotter)
	}
	s := m.snapshotters[runID]
	if s == nil {
		s = &runSnapshotter{}
		m.snapshotters[runID] = s
	}
	s.once.Do(func() {
		records, ok := m.Store.(workspacesnapshot.RecordStore)
		if !ok {
			s.err = &workspacesnapshot.UnavailableError{Reason: "store does not support workspace snapshots"}
			return
		}
		s.handle, s.err = workspacesnapshot.Open(m.Workspace, runSnapshotStore{
			RecordStore: records, events: m.Store, runID: runID,
		})
	})
	return s
}

func (m *Manager) releaseRunSnapshotter(ctx context.Context, runID string) {
	m.snapshotMu.Lock()
	defer m.snapshotMu.Unlock()
	if m.snapshotters[runID] == nil {
		return
	}
	run, err := m.Store.GetRun(context.WithoutCancel(ctx), runID)
	if err == nil && (run.Status == domain.RunCompleted || run.Status == domain.RunFailed || run.Status == domain.RunCancelled) {
		delete(m.snapshotters, runID)
	}
}

func (m *Manager) takeWorkspaceSnapshot(ctx context.Context, run storage.Run, st *loopState, phase string) {
	if st.snapshots.Unavailable {
		return
	}
	label := fmt.Sprintf("%s/batch-%d/%s", run.ID, st.snapshots.BatchNumber, phase)
	s := m.runSnapshotter(run.ID)
	err := s.err
	if err == nil {
		_, err = s.handle.Snapshot(ctx, label)
	}
	if err == nil {
		return
	}
	unavailable := errors.Is(err, workspacesnapshot.ErrUnavailable)
	st.snapshots.Unavailable = unavailable
	reason := strings.Join(strings.Fields(err.Error()), " ")
	if len(reason) > 240 {
		reason = reason[:240]
	}
	// Snapshot diagnostics are best effort, just like the capture itself.
	_ = m.event(context.WithoutCancel(ctx), run.ID, "snapshot_skipped", map[string]any{
		"batch_id": st.snapshots.BatchID, "label": label, "reason": reason, "unavailable": unavailable,
	})
}

func (m *Manager) toolHasSideEffects(name string) bool {
	if m.Broker == nil {
		return false
	}
	fact, ok := m.Broker.Lookup(name)
	return ok && sideEffectTool(fact)
}

func (m *Manager) invocationHasSideEffects(inv domain.Invocation) bool {
	if m.toolHasSideEffects(inv.Name) {
		return true
	}
	// Local run_command keeps its existing readonly/none fact for permission
	// decisions, while Dangerous identifies commands that require approval.
	// Treat those conditional commands as snapshot-worthy without changing the
	// broker's authorization or classification behavior.
	return inv.Name == "run_command" && tools.Dangerous(jsonString(inv.Arguments, "command"), m.Workspace)
}

func (m *Manager) batchHasSideEffects(batch *domain.Batch) bool {
	for _, inv := range batch.Invocations {
		if m.invocationHasSideEffects(inv) {
			return true
		}
		// execute_program is itself readonly; its pre-authorized tools carry
		// the same broker facts used by script authorization.
		if inv.Name == domain.ToolExecuteProgram {
			var args struct {
				Allow []string `json:"allow"`
			}
			if json.Unmarshal(inv.Arguments, &args) == nil {
				for _, name := range args.Allow {
					if m.toolHasSideEffects(name) {
						return true
					}
				}
			}
		}
	}
	return false
}

// Both initial dispatch and approval resume pass here. Only actual broker
// execution triggers post: denials, validation errors and cached results do not.
func (m *Manager) driveBatch(ctx context.Context, run storage.Run, batch *domain.Batch, st *loopState) error {
	if st.planTools != nil {
		err := m.drivePolicyBatch(ctx, run, batch, st)
		st.planTools.markUnknownTools(batch)
		return err
	}
	if m.DisableSnapshots || m.Workspace == "" || !batch.Accepted() {
		return m.drivePolicyBatch(ctx, run, batch, st)
	}
	if st.snapshots.BatchID != batch.ID {
		st.snapshots.BatchID = batch.ID
		st.snapshots.BatchNumber++
		st.snapshots.PreAttempted = false
	}
	if m.batchHasSideEffects(batch) && !st.snapshots.PreAttempted {
		st.snapshots.PreAttempted = true
		m.takeWorkspaceSnapshot(ctx, run, st, "pre")
	}
	executed := false
	// Keep batch checkpoints for CLI rewind. Review needs a fresh baseline
	// for each actual mutation, including nested script calls and retries
	// that reuse an invocation ID after a wait or process restart.
	ctx = tools.WithDispatchObserver(ctx, func(callCtx context.Context, req tools.Request) func() {
		if req.RunID != run.ID || !m.invocationHasSideEffects(domain.Invocation{Name: req.Tool, Arguments: req.Args}) {
			return nil
		}
		executed = true
		attempt := domain.NewID("exec_")
		m.takeWorkspaceSnapshot(callCtx, run, st, attempt+"/pre")
		return func() { m.takeWorkspaceSnapshot(context.WithoutCancel(callCtx), run, st, attempt+"/post") }
	})
	defer func() {
		if executed {
			m.takeWorkspaceSnapshot(context.WithoutCancel(ctx), run, st, "post")
		}
	}()
	return m.drivePolicyBatch(ctx, run, batch, st)
}
