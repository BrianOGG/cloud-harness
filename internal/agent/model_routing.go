package agent

import (
	"context"
	"encoding/json"

	"github.com/BrianOGG/cloud-harness/internal/config"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

type modelRoutingKey struct{}

func (m *Manager) withModelRouting(ctx context.Context) (context.Context, error) {
	if _, ok := ctx.Value(modelRoutingKey{}).(config.ModelRouting); ok {
		return ctx, nil
	}
	// A worktree manager gets its routing from the manager that owns the run.
	for source := m; source != nil; source = source.cancelOwner {
		if source.ModelRouting != nil {
			return context.WithValue(ctx, modelRoutingKey{}, *source.ModelRouting), nil
		}
	}
	routing, err := config.LoadModelRouting()
	if err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, modelRoutingKey{}, routing), nil
}

func (m *Manager) profiledModel(ctx context.Context, runID string, leaseVersion int64, role config.ModelRole) (*provider.ProfileProvider, error) {
	routing, _ := ctx.Value(modelRoutingKey{}).(config.ModelRouting)
	name, profile, ok := routing.Profile(role)
	if !ok {
		return nil, nil
	}
	inner := m.Provider
	if role == config.RoleFast {
		inner = m.Summarizer
	}
	if role == config.RoleExecutor && m.childModelOverride {
		profile = config.ModelProfile{Model: m.Model}
	}
	if profile.FallbackModel != "" {
		// The durable switch event also restores the selected model after a wait
		// or restart. Roles sharing a named profile share its switch in this run.
		events, err := m.Store.ListEvents(ctx, runID, 0)
		if err != nil {
			return nil, err
		}
		for _, event := range events {
			if event.Type != provider.FallbackEventType {
				continue
			}
			var ev provider.FallbackEvent
			if json.Unmarshal(event.Payload, &ev) == nil {
				if switched, _, _ := routing.Profile(config.ModelRole(ev.Role)); switched == name && ev.FromModel == profile.Model && ev.ToModel == profile.FallbackModel {
					profile = config.ModelProfile{Model: ev.ToModel}
					break
				}
			}
		}
	}
	requireLease := func(ctx context.Context) error {
		if leaseVersion == 0 {
			return nil // Summary jobs run after the answer has been committed.
		}
		return m.Store.RequireLease(ctx, runID, m.owner(), leaseVersion, m.now())
	}
	return &provider.ProfileProvider{
		Provider: inner, Role: string(role), Model: profile.Model, FallbackModel: profile.FallbackModel,
		BeforeAttempt: func(ctx context.Context, additional bool) error {
			// Executor and planner already charge for their first call. Summary
			// jobs using fast charge here instead of after a successful response.
			if additional || role == config.RolePlanner {
				if err := requireLease(ctx); err != nil {
					return err
				}
			}
			if additional || role == config.RoleFast {
				return m.Store.Charge(ctx, runID, domain.ChargeModel, 1)
			}
			return nil
		},
		OnFallback: func(ctx context.Context, ev provider.FallbackEvent) error {
			if err := requireLease(ctx); err != nil {
				return err
			}
			return m.event(ctx, runID, provider.FallbackEventType, ev)
		},
	}, nil
}

func (m *Manager) injectModelPlan(ctx context.Context, run storage.Run, leaseVersion int64, messages []provider.Message, text string) ([]provider.Message, error) {
	planner, err := m.profiledModel(ctx, run.ID, leaseVersion, config.RolePlanner)
	if err != nil {
		return nil, err
	}
	if planner == nil {
		return m.injectPlan(ctx, run, messages, text), nil
	}
	stopHeartbeat := m.startLeaseHeartbeat(ctx, run.ID, leaseVersion)
	defer stopHeartbeat()
	// Bind a provider for this call without changing Manager.Provider or the
	// planner's existing prompt, history lookup, and event code.
	scoped := &Manager{Store: m.Store, Provider: planner, Now: m.Now}
	messages = scoped.injectPlan(ctx, run, messages, text)
	return messages, ctx.Err()
}

func (m *Manager) retryModelSummaries(ctx context.Context, sessionID string) {
	routing, _ := ctx.Value(modelRoutingKey{}).(config.ModelRouting)
	if _, _, ok := routing.Profile(config.RoleFast); m.Summarizer == nil || !ok {
		m.retrySummaries(ctx, sessionID)
		return
	}
	jobs, err := m.Store.ListOpenSummaryJobs(ctx, sessionID)
	if err != nil {
		return
	}
	for _, job := range jobs {
		if run, err := m.Store.GetRun(ctx, job.RunID); err == nil {
			_ = m.runModelSummary(ctx, run, job, run.FinalAnswer)
		}
	}
}

func (m *Manager) runModelSummary(ctx context.Context, run storage.Run, job storage.SummaryJob, answer string) error {
	if m.Summarizer == nil {
		return m.runSummaryJob(ctx, run, job, answer)
	}
	fast, err := m.profiledModel(ctx, run.ID, 0, config.RoleFast)
	if err != nil {
		return err
	}
	if fast == nil {
		return m.runSummaryJob(ctx, run, job, answer)
	}
	scoped := &Manager{Store: prepaidSummaryStore{Store: m.Store, runID: run.ID}, Summarizer: fast, Now: m.Now}
	return scoped.runSummaryJob(ctx, run, job, answer)
}

// The summary helper normally charges on success. A profiled summary has
// already charged before each attempt, so skip only that legacy debit.
type prepaidSummaryStore struct {
	storage.Store
	runID string
}

func (s prepaidSummaryStore) Charge(ctx context.Context, runID string, kind domain.ChargeKind, n int) error {
	if runID == s.runID && kind == domain.ChargeModel && n == 1 {
		return nil
	}
	return s.Store.Charge(ctx, runID, kind, n)
}
