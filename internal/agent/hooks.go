package agent

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/hooks"
	"github.com/BrianOGG/cloud-harness/internal/storage"
	"github.com/BrianOGG/cloud-harness/internal/tools"
)

type hookContextKey struct{}
type hookContext struct {
	runID  string
	engine *hooks.Engine
}

func withHooks(ctx context.Context, run storage.Run, engine *hooks.Engine) context.Context {
	return context.WithValue(ctx, hookContextKey{}, hookContext{runID: run.ID, engine: engine})
}

func (m *Manager) hookRecorder(run storage.Run) hooks.Recorder {
	return func(ctx context.Context, event hooks.Event) error {
		// A timeout still needs a durable explanation. Do not let the expired
		// action context cancel its own audit event.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		typ := "hook.completed"
		if event.Warning {
			typ = "hook.warning"
		}
		ev, err := json.Marshal(event)
		if err != nil {
			return err
		}
		_, err = m.Store.AppendEvent(ctx, storage.Event{
			ID: domain.NewID("evt_"), RunID: run.ID, Type: typ, InvocationID: event.InvocationID,
			Payload: ev, Persistent: true, CreatedAt: m.now(),
		})
		return err
	}
}

func (m *Manager) lifecycleHook(ctx context.Context, run storage.Run, engine *hooks.Engine, input hooks.Input) error {
	input.RunID, input.SessionID = run.ID, run.SessionID
	out, err := engine.Execute(ctx, input, m.hookRecorder(run))
	if err != nil {
		return err
	}
	if out.Blocked {
		return domain.E("hook_blocked", out.Message)
	}
	return nil
}

func (m *Manager) startHooks(ctx context.Context, run storage.Run, leaseVersion int64, engine *hooks.Engine, prompt string) error {
	if len(engine.Config.Hooks[hooks.SessionStart])+len(engine.Config.Hooks[hooks.UserPromptSubmit]) > 0 {
		stopHeartbeat := m.startLeaseHeartbeat(ctx, run.ID, leaseVersion)
		defer stopHeartbeat()
	}
	if err := m.lifecycleHook(ctx, run, engine, hooks.Input{Point: hooks.SessionStart}); err != nil {
		return err
	}
	return m.lifecycleHook(ctx, run, engine, hooks.Input{Point: hooks.UserPromptSubmit, Prompt: prompt})
}

func (m *Manager) replyHook(ctx context.Context, run storage.Run, leaseVersion int64, engine *hooks.Engine, prompt string) error {
	if len(engine.Config.Hooks[hooks.UserPromptSubmit]) > 0 {
		stopHeartbeat := m.startLeaseHeartbeat(ctx, run.ID, leaseVersion)
		defer stopHeartbeat()
	}
	return m.lifecycleHook(ctx, run, engine, hooks.Input{Point: hooks.UserPromptSubmit, Prompt: prompt})
}

// Waiting is a pause within the same run. Stop runs immediately before a
// terminal transition. A blocked successful completion becomes a failed run;
// failure and cancellation keep their original causes.
func (m *Manager) stopHooks(ctx context.Context, run storage.Run, event domain.RunEvent, answer string, cause *domain.Error) (domain.RunEvent, string, *domain.Error, error) {
	h, ok := ctx.Value(hookContextKey{}).(hookContext)
	if !ok || h.runID != run.ID {
		return event, answer, cause, nil
	}
	next, err := domain.TransitionRun(domain.RunState{RunID: run.ID, Status: run.Status, Reason: run.WaitReason}, event)
	if err != nil || !domain.TerminalRun(next.Status) {
		return event, answer, cause, err
	}
	err = m.lifecycleHook(ctx, run, h.engine, hooks.Input{Point: hooks.Stop, Answer: answer})
	if err == nil {
		return event, answer, cause, nil
	}
	if !domain.IsCode(err, "hook_blocked") {
		return event, answer, cause, err
	}
	if next.Status == domain.RunCompleted {
		return domain.EventFatal, "", asDomain(err), nil
	}
	return event, answer, cause, nil
}

// Cancellation may come from another request or a fresh manager while a run
// is waiting. Restore its frozen hooks, just as Resume does.
func (m *Manager) cancelHooks(ctx context.Context, run storage.Run) error {
	if domain.TerminalRun(run.Status) {
		return nil
	}
	h, ok := ctx.Value(hookContextKey{}).(hookContext)
	if !ok || h.runID != run.ID {
		snap, _ := m.loadCheckpoint(ctx, run.ID)
		cfg := snap.Hooks
		if legacyHooks(cfg) {
			workspace := m.Workspace
			if run.WorktreePath != "" {
				workspace = run.WorktreePath
			}
			var err error
			cfg, err = hooks.Load(workspace)
			if err != nil {
				return err
			}
		}
		ctx = withHooks(ctx, run, hooks.New(cfg))
	}
	_, _, _, err := m.stopHooks(context.WithoutCancel(ctx), run, domain.EventCancel, "", nil)
	return err
}

func (m *Manager) preToolHooks(ctx context.Context, run storage.Run, st *loopState, inv domain.Invocation) error {
	if st == nil || st.hooks == nil {
		return nil
	}
	if st.hookErr != nil {
		return st.hookErr
	}
	if st.hookPre == nil {
		st.hookPre = make(map[string]bool)
		st.hookDenials = make(map[string]*domain.Error)
	}
	if st.hookPre[inv.ID] {
		if denial := st.hookDenials[inv.ID]; denial != nil {
			return denial
		}
		return nil
	}
	err := m.lifecycleHook(ctx, run, st.hooks, hooks.Input{
		Point: hooks.PreToolUse, Tool: inv.Name, InvocationID: inv.ID, Arguments: inv.Arguments,
	})
	if err != nil {
		if domain.IsCode(err, "hook_blocked") {
			st.hookDenials[inv.ID] = asDomain(err)
		} else {
			st.hookErr = err
		}
	}
	st.hookPre[inv.ID] = true
	return err
}

// The policy/permission gate runs first. Approval resumes enter exec directly,
// so they perform the same pre hook there before persisting a dispatch.
func (m *Manager) driveHooked(ctx context.Context, run storage.Run, batch *domain.Batch, st *loopState) error {
	gate, execute := m.gate(ctx, run, st), m.execute(ctx, run, st)
	err := domain.Drive(batch, func(inv domain.Invocation) domain.GateDecision {
		decision := gate(inv)
		if decision == domain.GateAllow && m.preToolHooks(ctx, run, st, inv) != nil {
			return domain.GateDeny
		}
		return decision
	}, execute)
	for i := range batch.Invocations {
		inv := &batch.Invocations[i]
		if denial := st.hookDenials[inv.ID]; denial != nil {
			inv.Status, inv.Dispatched = domain.InvDenied, false
			inv.Result = &domain.CallResult{Error: denial.WithRef(inv.ProviderCallID)}
		}
	}
	return errors.Join(err, st.hookErr)
}

// This surrounds Broker.Invoke, retaining its argument, grant, budget and hard
// boundary checks. Even a failed executor can have changed a Go file.
func (m *Manager) invokeHooked(ctx context.Context, run storage.Run, st *loopState, inv domain.Invocation, request tools.Request) (tools.Result, error) {
	if st == nil || st.hooks == nil {
		return m.Broker.Invoke(ctx, request)
	}
	before, scanErr := st.hooks.SnapshotGo(ctx)
	res, callErr := m.Broker.Invoke(ctx, request)
	input := hooks.Input{Point: hooks.PostToolUse, RunID: run.ID, SessionID: run.SessionID, Tool: inv.Name, InvocationID: inv.ID, Arguments: inv.Arguments}
	if res.Dispatched {
		check, err := st.hooks.CheckGo(ctx, input, before, scanErr, m.hookRecorder(run))
		if err != nil {
			st.hookErr = err
			return res, err
		}
		if check.Message != "" {
			res.Data = appendHookDiagnostic(res.Data, check.Message)
			if res.Error != nil {
				copy := *res.Error
				copy.Message += "\n" + check.Message
				res.Error = &copy
			} else if check.Blocked {
				res.OK, res.Error = false, domain.E("hook_blocked", check.Message)
			}
		}
	}
	input.Result, _ = json.Marshal(domain.CallResult{OK: res.OK, Data: res.Data, Error: res.Error})
	if err := m.lifecycleHook(ctx, run, st.hooks, input); err != nil {
		if !domain.IsCode(err, "hook_blocked") {
			st.hookErr = err
			return res, err
		}
		if res.Error != nil {
			err = domain.E("hook_blocked", res.Error.Message+"\n"+err.Error())
		}
		res.OK, res.Error = false, asDomain(err)
	}
	return res, callErr
}

func appendHookDiagnostic(data json.RawMessage, diagnostic string) json.RawMessage {
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil || object == nil {
		object = make(map[string]json.RawMessage)
		if json.Valid(data) {
			object["tool_result"] = data
		}
	}
	object["hook_diagnostics"], _ = json.Marshal(diagnostic)
	result, _ := json.Marshal(object)
	return result
}

func (m *Manager) invokeNestedHooked(ctx context.Context, request tools.Request) (tools.Result, error) {
	p, ok := ctx.Value(policyContextKey{}).(policyContext)
	if !ok || p.st.hooks == nil {
		return m.Broker.Invoke(ctx, request)
	}
	inv := domain.Invocation{ID: domain.NewID("inv_"), Name: request.Tool, Arguments: request.Args}
	if err := m.preToolHooks(ctx, p.run, p.st, inv); err != nil {
		return tools.Result{Error: asDomain(err)}, nil
	}
	return m.invokeHooked(ctx, p.run, p.st, inv, request)
}

func (m *Manager) pollHooked(ctx context.Context, request tools.PollRequest) (tools.PollResult, error) {
	p, ok := ctx.Value(policyContextKey{}).(policyContext)
	if !ok || p.st.hooks == nil {
		return tools.Poll(ctx, m.Broker, request)
	}
	return hooks.Poll(ctx, m.Broker, request, m.invokeNestedHooked)
}
