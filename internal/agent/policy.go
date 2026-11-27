package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/security"
	"github.com/BrianOGG/cloud-harness/internal/storage"
	"github.com/BrianOGG/cloud-harness/internal/tools"
	"github.com/BrianOGG/cloud-harness/internal/trace"
)

type policyContextKey struct{}

type policyContext struct {
	run storage.Run
	st  *loopState
}

func (m *Manager) policyRequest(inv domain.Invocation) security.PolicyRequest {
	req := security.PolicyRequest{Tool: inv.Name}
	switch inv.Name {
	case "run_command", "shell", "bash", "exec_command":
		req.Command = jsonString(inv.Arguments, "command")
		if cwd := jsonString(inv.Arguments, "cwd"); cwd != "" {
			req.Paths = []string{cwd}
		}
		return req
	case domain.ToolExecuteProgram:
		req.Command = jsonString(inv.Arguments, "script")
		return req
	}
	for _, key := range []string{"path", "file_path", "target_path"} {
		if target := jsonString(inv.Arguments, key); target != "" {
			req.Paths = append(req.Paths, target)
		}
	}
	if inv.Name == "grep" {
		if glob := jsonString(inv.Arguments, "glob"); glob != "" {
			req.Paths = append(req.Paths, glob)
		}
	}
	if m.Broker != nil {
		if resources, err := m.Broker.ExtractResources(inv.Name, inv.Arguments); err == nil {
			req.Paths = append(req.Paths, resources...)
		}
		if fact, ok := m.Broker.Lookup(inv.Name); ok && len(req.Paths) > 0 {
			req.FileRead = fact.Readonly
			req.FileWrite = !fact.Readonly
		}
	}
	return req
}

func (m *Manager) decidePolicy(ctx context.Context, run storage.Run, st *loopState, inv domain.Invocation) (security.PolicyDecision, error) {
	if st.policy == nil {
		return security.PolicyDecision{Effect: security.EffectAsk}, nil
	}
	req := m.policyRequest(inv)
	d := security.ApplyApprovalMode(st.approvalMode, st.policy, req, st.policy.Evaluate(req))
	if st.policyRecorded == nil {
		st.policyRecorded = make(map[string]bool)
	}
	if st.policyRecorded[inv.ID] {
		return d, nil
	}
	ev, err := trace.Persistent(run.ID, "policy.decided", d, m.now())
	if err != nil {
		return d, err
	}
	ev.ID, ev.InvocationID = "evt_policy_"+inv.ID, inv.ID
	if _, err := m.Store.AppendEvent(ctx, ev); err != nil {
		// The same invocation may resume in a new manager. Its decision is a
		// single durable fact, shared by the preflight and execution checks.
		events, readErr := m.Store.ListEvents(ctx, run.ID, 0)
		found := false
		if readErr == nil {
			for _, existing := range events {
				if existing.ID == ev.ID && existing.Type == ev.Type {
					found = true
					break
				}
			}
		}
		if !found {
			return d, errors.Join(err, readErr)
		}
	}
	st.policyRecorded[inv.ID] = true
	return d, nil
}

// Policy replaces the approval decision by minting the same exact invocation
// authority that approval uses. Broker still validates, authorizes, charges,
// and executes, and the command executor still requires an exact command grant.
func (m *Manager) allowPolicyInvocation(ctx context.Context, run storage.Run, st *loopState, inv domain.Invocation, policy security.PolicyDecision) (bool, error) {
	legacy, err := m.Broker.Authorize(ctx, tools.Request{
		RunID: run.ID, SessionID: run.SessionID, InvocationID: inv.ID, Tool: inv.Name, Args: inv.Arguments,
		DefaultAllow: m.oracleDefaultAllow(inv.Name), ApprovalMode: st.approvalMode,
		SensitiveSeen: st.sensitive, Egress: isEgressTool(m.Broker, inv.Name), Now: m.now(),
	})
	if err != nil {
		return false, err
	}
	// Leave malformed arguments to the broker's normal structured error path.
	if legacy.Error != nil && legacy.Error.Code == domain.CodeInvalidArgument {
		return true, nil
	}
	if legacy.Error != nil && legacy.Error.Code != domain.CodeAuthDenied {
		// Unknown tools retain the existing pre-dispatch rejection path.
		return false, nil
	}
	fact, ok := m.Broker.Lookup(inv.Name)
	if !ok {
		return false, domain.E(domain.CodeToolError, "unknown tool "+inv.Name)
	}
	resources, resourceErr := m.Broker.ExtractRequestResources(tools.Request{Tool: inv.Name, Args: inv.Arguments, ApprovalMode: st.approvalMode})
	grants, err := m.Store.ListGrants(ctx, run.ID, run.SessionID, m.now())
	if err != nil {
		return false, err
	}
	decision := security.Decide(grants, security.Request{
		RunID: run.ID, SessionID: run.SessionID, InvocationID: inv.ID, Tool: inv.Name,
		Readonly: fact.Readonly, SideEffectClass: fact.SideEffect, Resources: resources,
		UnresolvedResources: resourceErr != nil, SensitiveSeen: st.sensitive,
		Egress: isEgressTool(m.Broker, inv.Name), Now: m.now(), Policy: &policy,
		RestrictPolicyAllow: m.Depth > 0,
	})
	if !decision.Allow {
		return false, nil
	}
	exactCommand := inv.Name == "run_command" && len(resources) == 1 && resources[0] == strings.TrimSpace(jsonString(inv.Arguments, "command"))
	if legacy.OK && !exactCommand {
		return true, nil
	}
	scope := ""
	if len(resources) == 1 {
		scope = resources[0]
	} else if len(resources) > 1 {
		raw, err := json.Marshal(resources)
		if err != nil {
			return false, err
		}
		scope = string(raw)
	}
	g := domain.Grant{
		ID: "grd_policy_" + inv.ID, Subject: "policy", RunID: run.ID, SessionID: run.SessionID,
		InvocationID: inv.ID, Tool: inv.Name, Op: "call", ResourceScope: scope, State: "active",
	}
	if err := m.Store.SaveGrant(ctx, g, m.now()); err != nil {
		return false, err
	}
	return true, nil
}

func (st *loopState) denyPolicy(inv domain.Invocation, reason string) domain.GateDecision {
	if st.policyDenials == nil {
		st.policyDenials = make(map[string]string)
	}
	st.policyDenials[inv.ID] = reason
	return domain.GateDeny
}

// Domain's generic GateDeny result says "approval denied". Supply the actual
// policy reason before persistence and before results are sent to the model.
func (m *Manager) drivePolicyBatch(ctx context.Context, run storage.Run, batch *domain.Batch, st *loopState) error {
	ctx = context.WithValue(ctx, policyContextKey{}, policyContext{run: run, st: st})
	err := m.driveHooked(ctx, run, batch, st)
	for i := range batch.Invocations {
		inv := &batch.Invocations[i]
		if reason := st.policyDenials[inv.ID]; reason != "" && inv.Status == domain.InvDenied {
			inv.Result = &domain.CallResult{Error: domain.E(domain.CodeAuthDenied, reason).WithRef(inv.ProviderCallID)}
		}
	}
	return errors.Join(err, st.policyErr)
}

// Nested calls keep poll/script pre-authorization and child limits. They also
// consult the same run snapshot, so wrapping read_file cannot bypass a deny.
func (m *Manager) checkNestedPolicy(ctx context.Context, name string, args json.RawMessage) error {
	p, ok := ctx.Value(policyContextKey{}).(policyContext)
	if !ok || p.st.policy == nil {
		return nil
	}
	inv := domain.Invocation{ID: domain.NewID("inv_"), Name: name, Arguments: args}
	d, err := m.decidePolicy(ctx, p.run, p.st, inv)
	if err != nil {
		return err
	}
	if d.Effect == security.EffectDeny {
		return domain.E(domain.CodeAuthDenied, d.Reason())
	}
	return nil
}

func policyApprovalPossible(name string) bool {
	return name == "write_file" || name == "edit_file" || name == "run_command" || strings.HasPrefix(name, "mcp.")
}
