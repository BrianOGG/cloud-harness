package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/program"
	"github.com/cloud-harness/cloud-harness/internal/tools"
)

type runKeyType struct{}

var runKey runKeyType

func (m *Manager) executePoll(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	var body struct {
		TargetTool      string          `json:"target_tool"`
		TargetArgs      json.RawMessage `json:"target_args"`
		Until           tools.Until     `json:"until"`
		IntervalSeconds float64         `json:"interval_seconds"`
		MaxAttempts     int             `json:"max_attempts"`
		TimeoutSeconds  float64         `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(args, &body); err != nil {
		return nil, domain.E(domain.CodeInvalidArgument, "poll arguments are not JSON")
	}
	runID, _ := ctx.Value(runKey).(string)
	sessionID, _ := ctx.Value(sessionKey).(string)
	if runID == "" {
		return nil, domain.E(domain.CodeToolError, "poll has no run")
	}
	if excludedByChildToolLimit(ctx, body.TargetTool) {
		return nil, domain.E(domain.CodeAuthDenied, "poll target is outside the child tool allowlist")
	}
	if err := m.checkNestedPolicy(ctx, body.TargetTool, body.TargetArgs); err != nil {
		return nil, err
	}
	res, err := m.pollHooked(ctx, tools.PollRequest{
		RunID: runID, SessionID: sessionID, TargetTool: body.TargetTool, TargetArgs: body.TargetArgs,
		Until: body.Until, Interval: time.Duration(body.IntervalSeconds * float64(time.Second)),
		MaxAttempts: body.MaxAttempts, Timeout: time.Duration(body.TimeoutSeconds * float64(time.Second)),
		Now: m.now(),
	})
	if err != nil && res.Attempts == 0 && res.Stopped == "" {
		return nil, err
	}
	raw, merr := json.Marshal(res)
	if merr != nil {
		return nil, merr
	}
	if err != nil && res.Stopped != "cancelled" {
		return raw, err
	}
	return raw, nil
}

func (m *Manager) executeProgram(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	var body struct {
		Script             string   `json:"script"`
		Allow              []string `json:"allow"`
		ConfirmSideEffects []string `json:"confirm_side_effects"`
	}
	if err := json.Unmarshal(args, &body); err != nil {
		return nil, domain.E(domain.CodeInvalidArgument, "program arguments are not JSON")
	}
	runID, _ := ctx.Value(runKey).(string)
	sessionID, _ := ctx.Value(sessionKey).(string)
	if runID == "" {
		return nil, domain.E(domain.CodeToolError, "program has no run")
	}
	facts, err := m.authorizeScript(ctx, runID, sessionID, body.Allow, body.ConfirmSideEffects)
	if err != nil {
		return nil, err
	}
	allow := map[string]bool{}
	for _, name := range body.Allow {
		allow[name] = true
	}
	confirmed := map[string]bool{}
	for _, name := range body.ConfirmSideEffects {
		confirmed[name] = true
	}
	invoke := func(callCtx context.Context, tool string, toolArgs json.RawMessage) (bool, json.RawMessage, *domain.Error) {
		if !allow[tool] {
			return false, nil, domain.E(domain.CodeAuthDenied, "script cannot request a tool that was not pre-authorized")
		}
		fact, ok := m.Broker.Lookup(tool)
		if !ok {
			return false, nil, domain.E(domain.CodeToolError, "unknown tool "+tool)
		}
		if sideEffectTool(fact) && !confirmed[tool] {
			return false, nil, domain.E(domain.CodeScriptPolicy, "side-effect tool requires confirmation before the script starts")
		}
		if err := m.checkNestedPolicy(callCtx, tool, toolArgs); err != nil {
			return false, nil, asDomain(err)
		}
		if err := m.Store.Charge(callCtx, runID, domain.ChargeScript, 1); err != nil {
			return false, nil, asDomain(err)
		}
		res, callErr := m.invokeNestedHooked(callCtx, tools.Request{
			RunID: runID, SessionID: sessionID, Tool: tool, Args: toolArgs, Now: m.now(),
		})
		m.recordSnapshotDispatch(callCtx, domain.Invocation{Name: tool, Arguments: toolArgs}, res)
		if callErr != nil {
			return false, nil, asDomain(callErr)
		}
		if res.NeedsApproval {
			return false, nil, domain.E(domain.CodeScriptPolicy, "a running script cannot request new permissions")
		}
		if res.Error != nil {
			return false, res.Data, res.Error
		}
		return res.OK, res.Data, nil
	}
	res, err := program.Replay(program.SafeReplay(facts), func() (program.Result, error) {
		return program.Execute(ctx, program.Request{Script: body.Script}, invoke)
	})
	out := map[string]any{"ok": res.OK, "hash": res.Hash, "replay_safe": program.SafeReplay(facts)}
	if len(res.Data) > 0 {
		out["data"] = json.RawMessage(res.Data)
	}
	if res.Error != nil {
		out["error"] = res.Error
	}
	if err != nil {
		out["error"] = asDomain(err)
		out["ok"] = false
	}
	raw, merr := json.Marshal(out)
	if merr != nil {
		return nil, merr
	}
	if err != nil {
		return raw, err
	}
	if res.Error != nil && !res.OK {
		return raw, res.Error
	}
	return raw, nil
}

func (m *Manager) authorizeScript(ctx context.Context, runID, sessionID string, allow, confirmed []string) ([]program.ToolFact, error) {
	if len(allow) == 0 {
		return nil, domain.E(domain.CodeScriptPolicy, "script allowlist is empty")
	}
	okConfirm := map[string]bool{}
	for _, name := range confirmed {
		okConfirm[name] = true
	}
	var facts []program.ToolFact
	for _, name := range allow {
		if excludedByChildToolLimit(ctx, name) {
			return nil, domain.E(domain.CodeScriptPolicy, "tool "+name+" is outside the child tool allowlist")
		}
		fact, ok := m.Broker.Lookup(name)
		if !ok {
			return nil, domain.E(domain.CodeScriptPolicy, "allowlist names an unknown tool "+name)
		}
		if sideEffectTool(fact) && !okConfirm[name] {
			return nil, domain.E(domain.CodeScriptPolicy, "side-effect tool "+name+" requires confirmation and was not started")
		}
		grants, err := m.Store.ListGrants(ctx, runID, sessionID, m.now())
		if err != nil {
			return nil, err
		}
		if !grantCovers(grants, name) {
			return nil, domain.E(domain.CodeScriptPolicy, "script cannot start without pre-authorization for "+name)
		}
		facts = append(facts, program.ToolFact{
			Name: fact.Name, Readonly: fact.Readonly, Idempotent: fact.Idempotent, SideEffect: fact.SideEffect,
		})
	}
	return facts, nil
}

func sideEffectTool(fact tools.Fact) bool {
	return !fact.Readonly || (fact.SideEffect != "" && fact.SideEffect != "none")
}

func grantCovers(grants []domain.Grant, tool string) bool {
	for _, g := range grants {
		if g.Tool == tool && g.State == "active" && g.InvocationID == "" {
			return true
		}
	}
	return false
}
