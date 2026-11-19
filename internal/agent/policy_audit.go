package agent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/security"
	"github.com/cloud-harness/cloud-harness/internal/storage"
)

const (
	DefaultPolicyDecisionLimit = 50
	MaxPolicyDecisionLimit     = 500
)

// PolicyAuditStore is the read-only subset of the existing store needed for
// audit queries. Listing runs per session avoids ListRuns("")'s 200-run cap.
type PolicyAuditStore interface {
	ListSessions(context.Context) ([]storage.SessionSummary, error)
	ListRuns(context.Context, string) ([]storage.Run, error)
	ListEvents(context.Context, string, int64) ([]storage.Event, error)
}

type PolicyDecisionQuery struct {
	RunID  string
	Tool   string
	Effect security.Effect
	Since  time.Time
	Limit  int
}

func (q PolicyDecisionQuery) Validate() error {
	if q.Effect != "" && q.Effect != security.EffectAllow && q.Effect != security.EffectAsk && q.Effect != security.EffectDeny {
		return errors.New("effect must be allow, ask, or deny")
	}
	if q.Limit < 0 {
		return errors.New("limit must be positive")
	}
	return nil
}

// PolicyDecisionRecord deliberately projects decision fields, never invocation
// arguments, tool results, model messages, or the original event payload.
type PolicyDecisionRecord struct {
	Time         time.Time       `json:"time"`
	RunID        string          `json:"run"`
	InvocationID string          `json:"invocation_id,omitempty"`
	Tool         string          `json:"tool"`
	Effect       security.Effect `json:"effect"`
	Source       string          `json:"source"`
	Rule         string          `json:"rule"`
	RuleIndex    int             `json:"rule_index,omitempty"`

	seq int64
}

// ListPolicyDecisions applies all filters before sorting and limiting. Since
// is inclusive. A zero limit selects the default; larger limits clamp to 500.
func ListPolicyDecisions(ctx context.Context, store PolicyAuditStore, q PolicyDecisionQuery) ([]PolicyDecisionRecord, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	limit := q.Limit
	if limit == 0 {
		limit = DefaultPolicyDecisionLimit
	}
	if limit > MaxPolicyDecisionLimit {
		limit = MaxPolicyDecisionLimit
	}
	records := make([]PolicyDecisionRecord, 0)
	err := visitPolicyDecisions(ctx, store, q.RunID, func(record PolicyDecisionRecord) {
		if q.Tool != "" && record.Tool != q.Tool || q.Effect != "" && record.Effect != q.Effect || record.Time.Before(q.Since) {
			return
		}
		records = append(records, record)
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(records, func(i, j int) bool {
		a, b := records[i], records[j]
		if !a.Time.Equal(b.Time) {
			return a.Time.After(b.Time)
		}
		if a.RunID != b.RunID {
			return a.RunID < b.RunID
		}
		return a.seq > b.seq
	})
	if len(records) > limit {
		records = records[:limit]
	}
	return records, nil
}

func visitPolicyDecisions(ctx context.Context, store PolicyAuditStore, runID string, visit func(PolicyDecisionRecord)) error {
	runIDs := []string{runID}
	if runID == "" {
		runIDs = nil
		sessions, err := store.ListSessions(ctx)
		if err != nil {
			return err
		}
		for _, session := range sessions {
			runs, err := store.ListRuns(ctx, session.ID)
			if err != nil {
				return err
			}
			for _, run := range runs {
				runIDs = append(runIDs, run.ID)
			}
		}
	}
	for _, id := range runIDs {
		if err := ctx.Err(); err != nil {
			return err
		}
		events, err := store.ListEvents(ctx, id, 0)
		if err != nil {
			return err
		}
		for _, event := range events {
			if event.Type != "policy.decided" {
				continue
			}
			var d security.PolicyDecision
			if err := json.Unmarshal(event.Payload, &d); err != nil {
				return errors.New("invalid policy.decided event JSON")
			}
			if d.Effect != security.EffectAllow && d.Effect != security.EffectAsk && d.Effect != security.EffectDeny {
				return errors.New("invalid policy.decided effect")
			}
			switch d.Source {
			case "workspace", "global", "builtin", "default":
			default:
				return errors.New("invalid policy.decided source")
			}
			visit(PolicyDecisionRecord{
				Time: event.CreatedAt, RunID: event.RunID, InvocationID: event.InvocationID,
				Tool: d.Tool, Effect: d.Effect, Source: d.Source, Rule: d.Rule, RuleIndex: d.RuleIndex,
				seq: event.Seq,
			})
		}
	}
	return nil
}

type PolicyLayerSummary struct {
	Source string `json:"source"`
	File   string `json:"file"`
	Loaded bool   `json:"loaded"`
	Rules  int    `json:"rules"`
}

type PolicyDecisionCounts struct {
	Allow int `json:"allow"`
	Ask   int `json:"ask"`
	Deny  int `json:"deny"`
}

type PolicySummary struct {
	Layers    []PolicyLayerSummary `json:"layers"`
	Decisions PolicyDecisionCounts `json:"decisions"`
}

// SummarizePolicy reads the existing loader's result and counts every retained
// decision, without the listing limit. A nil store represents a missing DB.
func SummarizePolicy(ctx context.Context, workspace string, store PolicyAuditStore) (PolicySummary, error) {
	p, err := security.LoadPolicy(workspace)
	if err != nil {
		// Loader diagnostics can quote invalid configuration values. Doctor
		// reports failure without echoing policy selectors or file contents.
		return PolicySummary{}, errors.New("policy files could not be loaded; check workspace/global policy.json")
	}
	configDir, err := security.ConfigDirectory()
	if err != nil {
		return PolicySummary{}, errors.New("policy configuration directory unavailable")
	}
	summary := PolicySummary{Layers: []PolicyLayerSummary{
		{Source: "workspace", File: filepath.Join(p.Workspace, ".cloud-harness", "policy.json"), Loaded: p.WorkspaceRules != nil, Rules: len(p.WorkspaceRules)},
		{Source: "global", File: filepath.Join(configDir, "policy.json"), Loaded: p.GlobalRules != nil, Rules: len(p.GlobalRules)},
		// The existing evaluator has two embedded baseline rules: .env reads
		// and writes outside the workspace. Neither has a rule file.
		{Source: "builtin", File: "embedded", Loaded: true, Rules: 2},
	}}
	if store == nil {
		return summary, nil
	}
	err = visitPolicyDecisions(ctx, store, "", func(record PolicyDecisionRecord) {
		switch record.Effect {
		case security.EffectAllow:
			summary.Decisions.Allow++
		case security.EffectAsk:
			summary.Decisions.Ask++
		case security.EffectDeny:
			summary.Decisions.Deny++
		}
	})
	return summary, err
}
