package agent

import (
	"context"
	"encoding/json"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/hooks"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/runtime"
	"github.com/BrianOGG/cloud-harness/internal/security"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

type snapshot struct {
	ReasoningEffort    string                 `json:"reasoning_effort,omitempty"`
	ModelID            string                 `json:"model_id,omitempty"`
	Model              string                 `json:"model,omitempty"`
	FallbackModel      string                 `json:"fallback_model,omitempty"`
	ProviderID         string                 `json:"provider_id,omitempty"`
	ApprovalMode       string                 `json:"approval_mode,omitempty"`
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
	MCPSelected        []string               `json:"mcp_selected,omitempty"`
	PlanMode           bool                   `json:"plan_mode,omitempty"`
	Hooks              *hooks.Config          `json:"hooks,omitempty"`
}

func (m *Manager) saveCheckpoint(ctx context.Context, run storage.Run, messages []provider.Message, continuation []domain.ResponseItem, contProvider string, st *loopState) error {
	return m.writeCheckpoint(ctx, run, messages, continuation, contProvider, st, false)
}

func (m *Manager) writeCheckpoint(ctx context.Context, run storage.Run, messages []provider.Message, continuation []domain.ResponseItem, contProvider string, st *loopState, initial bool) error {
	snap := snapshot{Messages: messages, Continuation: continuation, ContProvider: contProvider}
	if st != nil {
		snap.ModelID = st.modelID
		snap.Model = st.model
		snap.FallbackModel = st.fallbackModel
		snap.ProviderID = st.providerID
		snap.ApprovalMode = st.approvalMode
		snap.ReasoningEffort = st.reasoningEffort
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
		snap.MCPSelected = st.mcpSelected
		snap.PlanMode = st.planTools != nil
		if st.hooks != nil {
			snap.Hooks = st.hooks.Config
		}
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
