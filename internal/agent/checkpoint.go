package agent

import (
	"context"
	"encoding/json"

	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/provider"
	"github.com/cloud-harness/cloud-harness/internal/runtime"
	"github.com/cloud-harness/cloud-harness/internal/security"
	"github.com/cloud-harness/cloud-harness/internal/storage"
)

type snapshot struct {
	Messages           []provider.Message     `json:"messages"`
	Continuation       []domain.ResponseItem  `json:"continuation,omitempty"`
	ContProvider       string                 `json:"cont_provider,omitempty"`
	Observations       []runtime.Observation  `json:"observations,omitempty"`
	ToolRefs           map[string]string      `json:"tool_refs,omitempty"`
	SensitiveRead      bool                   `json:"sensitive_read,omitempty"`
	Reminded           bool                   `json:"reminded,omitempty"`
	PrefixN            int                    `json:"prefix_n,omitempty"`
	UserAt             int                    `json:"user_at,omitempty"`
	WorkspaceSnapshots workspaceSnapshotState `json:"workspace_snapshots,omitempty"`
	Policy             *security.Policy       `json:"policy,omitempty"`
}

func (m *Manager) saveCheckpoint(ctx context.Context, run storage.Run, messages []provider.Message, continuation []domain.ResponseItem, contProvider string, st *loopState) error {
	return m.writeCheckpoint(ctx, run, messages, continuation, contProvider, st, false)
}

func (m *Manager) writeCheckpoint(ctx context.Context, run storage.Run, messages []provider.Message, continuation []domain.ResponseItem, contProvider string, st *loopState, initial bool) error {
	snap := snapshot{Messages: messages, Continuation: continuation, ContProvider: contProvider}
	if st != nil {
		snap.Observations = st.obs
		if len(st.toolRefs) > 0 {
			snap.ToolRefs = make(map[string]string, len(st.toolRefs))
			for k, v := range st.toolRefs {
				snap.ToolRefs[k] = v
			}
		}
		snap.SensitiveRead = st.sensitive
		snap.Reminded = st.reminded
		snap.PrefixN = st.prefixN
		snap.UserAt = st.userAt
		snap.WorkspaceSnapshots = st.snapshots
		snap.Policy = st.policy
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	if initial {
		// Version zero keeps this fallback behind every existing checkpoint,
		// including when a caller uses a frozen clock for the whole run.
		return m.Store.SaveCheckpoint(ctx, run.ID, 0, raw, m.now())
	}
	current, err := m.Store.GetRun(ctx, run.ID)
	if err != nil {
		return err
	}
	return m.Store.SaveCheckpoint(ctx, run.ID, current.Version, raw, m.now())
}

func (m *Manager) loadCheckpoint(ctx context.Context, runID string) (snapshot, error) {
	raw, err := m.Store.LatestCheckpoint(ctx, runID)
	if err != nil {
		return snapshot{}, err
	}
	var snap snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return snapshot{}, err
	}
	return snap, nil
}
