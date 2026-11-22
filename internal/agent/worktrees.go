package agent

import (
	"context"
	"errors"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/storage"
	"github.com/BrianOGG/cloud-harness/internal/tools"
	"github.com/BrianOGG/cloud-harness/internal/trace"
	"github.com/BrianOGG/cloud-harness/internal/worktree"
)

func (m *Manager) prepareWorktree(ctx context.Context, run *storage.Run) (*Manager, error) {
	if !m.Worktree {
		return m, nil
	}
	info, err := worktree.Create(ctx, m.Workspace, run.ID)
	if err != nil {
		return nil, err
	}
	run.WorktreeBaseSHA, run.WorktreeBranch, run.WorktreePath = info.BaseSHA, info.Branch, info.Path
	return m.worktreeManager(*run)
}

// A run's persisted workspace binding also applies after an approval or a
// process restart. The manager and broker are scoped, while Store is shared.
// Copy configuration explicitly: Manager contains mutexes that must not copy.
func (m *Manager) worktreeManager(run storage.Run) (*Manager, error) {
	if run.WorktreePath == "" || run.WorktreePath == m.Workspace {
		return m, nil
	}
	scoped := &Manager{
		Store: m.Store, Provider: m.Provider, OwnerID: m.OwnerID, Now: m.Now,
		Notifier: m.Notifier, ApprovalBaseURL: m.ApprovalBaseURL,
		LeaseFor: m.LeaseFor, InitialBudget: m.InitialBudget, UseBudget: m.UseBudget,
		System: m.System, Model: m.Model, ToolDigest: m.ToolDigest,
		Workspace: run.WorktreePath, DisableRepoMap: m.DisableRepoMap, DisableSnapshots: m.DisableSnapshots,
		Grants: append([]domain.Grant(nil), m.Grants...), CrashOnTool: m.CrashOnTool,
		SkipPlanner: m.SkipPlanner, ForcePlanner: m.ForcePlanner, ContextTokenLimit: m.ContextTokenLimit,
		Summarizer: m.Summarizer, Depth: m.Depth, AllowedTools: append([]string(nil), m.AllowedTools...),
		definitionToolLimit: m.definitionToolLimit, failOnToolBudget: m.failOnToolBudget,
		cancelOwner: m,
	}
	if m.Broker != nil {
		broker, err := m.Broker.ForkWorkspace(run.WorktreePath, "retrieve_resource", "retrieve_history",
			domain.ToolPoll, domain.ToolExecuteProgram, domain.ToolDelegateTask, tools.LoadSkillName)
		if err != nil {
			return nil, err
		}
		scoped.Broker = broker
	}
	return scoped, nil
}

func (m *Manager) finishWorktreeExecution(ctx context.Context, run storage.Run, result *storage.Run, runErr *error) {
	if run.WorktreePath == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	current, err := m.Store.GetRun(ctx, run.ID)
	if err != nil {
		*runErr = errors.Join(*runErr, err)
		return
	}
	if *runErr != nil && current.Status == domain.RunRunning {
		event := domain.EventFatal
		if errors.Is(*runErr, context.Canceled) || errors.Is(*runErr, context.DeadlineExceeded) {
			event = domain.EventCancel
		}
		current, err = m.finish(ctx, current, event, "", asDomain(*runErr))
		if err != nil {
			*runErr = errors.Join(*runErr, err)
			return
		}
		*result = current
	}
	if domain.TerminalRun(current.Status) {
		*runErr = errors.Join(*runErr, m.summarizeWorktree(ctx, current))
	}
}

func (m *Manager) summarizeWorktree(ctx context.Context, run storage.Run) error {
	if run.WorktreePath == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	events, err := m.Store.ListEvents(ctx, run.ID, 0)
	if err != nil {
		return err
	}
	for _, event := range events {
		if event.Type == "worktree.summary" {
			return nil
		}
	}
	summary, diffErr := worktree.Summarize(ctx, worktree.Info{
		RunID: run.ID, BaseSHA: run.WorktreeBaseSHA, Branch: run.WorktreeBranch, Path: run.WorktreePath,
	})
	if diffErr != nil {
		summary.Error = diffErr.Error()
	}
	event, err := trace.Persistent(run.ID, "worktree.summary", summary, m.now())
	if err != nil {
		return err
	}
	// The stable event id also deduplicates a cancellation racing a resumed
	// execution, without a process-local lock or a second persistence surface.
	event.ID = "evt_worktree_summary_" + run.ID
	if _, err = m.Store.AppendEvent(ctx, event); err != nil {
		if events, readErr := m.Store.ListEvents(ctx, run.ID, 0); readErr == nil {
			for _, existing := range events {
				if existing.ID == event.ID {
					return nil
				}
			}
		}
	}
	return errors.Join(diffErr, err)
}
