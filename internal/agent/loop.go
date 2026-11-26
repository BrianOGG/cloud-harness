package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/config"
	contextpkg "github.com/BrianOGG/cloud-harness/internal/context"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/hooks"
	"github.com/BrianOGG/cloud-harness/internal/imageinput"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/runtime"
	"github.com/BrianOGG/cloud-harness/internal/security"
	"github.com/BrianOGG/cloud-harness/internal/storage"
	"github.com/BrianOGG/cloud-harness/internal/tools"
)

type sessionKeyType struct{}

var sessionKey sessionKeyType

const maxPlanBytes = 32 * 1024

func executeUpdatePlan(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
	var body struct {
		Plan string `json:"plan"`
	}
	if err := json.Unmarshal(args, &body); err != nil {
		return nil, domain.E(domain.CodeInvalidArgument, "update_plan requires a string plan")
	}
	if strings.TrimSpace(body.Plan) == "" {
		return nil, domain.E(domain.CodeInvalidArgument, "plan must not be empty")
	}
	if len(body.Plan) > maxPlanBytes {
		return nil, domain.E(domain.CodeInvalidArgument, "plan exceeds 32KiB limit")
	}
	return json.Marshal(map[string]any{
		"accepted": true,
		"bytes":    len(body.Plan),
	})
}

func (m *Manager) ensureTools() {
	if m.Broker == nil {
		return
	}
	if m.Workspace != "" {
		_ = tools.EnsureLocal(m.Broker, m.Workspace)
	} else if !m.Broker.Has("edit_file") || !m.Broker.Has("list_dir") || !m.Broker.Has("list_files") || !m.Broker.Has("grep") || !m.Broker.Has("git_status") || !m.Broker.Has("git_diff") || !m.Broker.Has("git_log") {
		_ = tools.EnsureLocal(m.Broker, ".")
	}
	if !m.Broker.Has(tools.ToolAskOracle) {
		m.Broker.Register(tools.AskOracleSpec())
	}
	if !m.Broker.Has("update_plan") {
		m.Broker.Register(tools.Spec{
			Name: "update_plan", Description: "Replace the current run plan (non-empty string, max 32 KiB).",
			Required: []string{"plan"}, Readonly: true, Idempotent: true, SideEffectClass: "none",
			Parameters: json.RawMessage(`{"type":"object","properties":{"plan":{"type":"string"}},"required":["plan"],"additionalProperties":false}`),
			Execute:    executeUpdatePlan,
		})
	}
	if !m.Broker.Has(tools.LoadSkillName) {
		m.Broker.Register(tools.LoadSkillSpec(m.Workspace))
	}
	if !m.Broker.Has("retrieve_resource") {
		m.Broker.Register(tools.Spec{
			Name: "retrieve_resource", Description: "Load one history row by id at summary, excerpt, or verbatim precision.",
			Required: []string{"id"}, Readonly: true, Idempotent: true, SideEffectClass: "none",
			Parameters: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"precision":{"type":"string"}},"required":["id"],"additionalProperties":false}`),
			Execute: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
				session, _ := ctx.Value(sessionKey).(string)
				var body struct {
					ID        string `json:"id"`
					Precision string `json:"precision"`
				}
				if err := json.Unmarshal(args, &body); err != nil || body.ID == "" {
					return nil, domain.E(domain.CodeInvalidArgument, "id is required")
				}
				doc, err := m.Store.GetHistory(ctx, body.ID)
				if err != nil {
					return nil, err
				}
				if doc.SessionID != session {
					return nil, domain.E(domain.CodeAuthDenied, "history row is outside this session")
				}
				return json.Marshal(map[string]string{
					"id": doc.SourceID, "precision": body.Precision, "text": contextpkg.Present(doc.RawText, body.Precision),
				})
			},
		})
	}
	if !m.Broker.Has("retrieve_history") {
		m.Broker.Register(tools.Spec{
			Name: "retrieve_history", Description: "Search this session's history index.",
			Required: []string{"query"}, Readonly: true, Idempotent: true, SideEffectClass: "none",
			Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`),
			Execute: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
				session, _ := ctx.Value(sessionKey).(string)
				if session == "" || m.Store == nil {
					return nil, domain.E(domain.CodeToolError, "history is unavailable")
				}
				var body struct {
					Query string `json:"query"`
				}
				if err := json.Unmarshal(args, &body); err != nil {
					return nil, domain.E(domain.CodeInvalidArgument, "query is required")
				}
				hits, err := m.Store.SearchHistory(ctx, session, body.Query, 10)
				if err != nil {
					return nil, err
				}
				return json.Marshal(hits)
			},
		})
	}
	if !m.Broker.Has(domain.ToolPoll) {
		m.Broker.Register(tools.Spec{
			Name: domain.ToolPoll, Description: "Poll a readonly tool until a JSON field matches or the attempt cap is hit.",
			Required: []string{"target_tool", "until", "max_attempts"}, Readonly: true, Idempotent: true, SideEffectClass: "none",
			Parameters: json.RawMessage(`{"type":"object","properties":{"target_tool":{"type":"string"},"target_args":{"type":"object"},"until":{"type":"object"},"interval_seconds":{"type":"number"},"max_attempts":{"type":"number"},"timeout_seconds":{"type":"number"}},"required":["target_tool","until","max_attempts"],"additionalProperties":false}`),
			Execute:    m.executePoll,
		})
	}
	if !m.Broker.Has(domain.ToolExecuteProgram) {
		m.Broker.Register(tools.Spec{
			Name: domain.ToolExecuteProgram, Description: "Run a pre-authorized Starlark script. The script cannot request new permissions.",
			Required: []string{"script", "allow"}, Readonly: true, Idempotent: false, SideEffectClass: "none",
			Parameters: json.RawMessage(`{"type":"object","properties":{"script":{"type":"string"},"allow":{"type":"array","items":{"type":"string"}},"confirm_side_effects":{"type":"array","items":{"type":"string"}}},"required":["script","allow"],"additionalProperties":false}`),
			Execute:    m.executeProgram,
		})
	}
	if !m.Broker.Has(domain.ToolDelegateTask) {
		m.Broker.Register(tools.Spec{
			Name: domain.ToolDelegateTask, Description: "Run a child task with its own trace, checkpoint, and budget. Optionally select an agent: built-in explorer or reviewer, or a workspace definition in .agents/agents/*.md.",
			Required: []string{"goal"}, Readonly: true, Idempotent: false, SideEffectClass: "none",
			Parameters: json.RawMessage(`{"type":"object","properties":{"goal":{"type":"string"},"agent":{"type":"string","description":"Optional child-agent definition name."},"relevant_context":{"type":"string"},"success_criteria":{"type":"string","description":"Natural-language conditions for the subtask to count as done."},"output_contract":{"type":"string","description":"Expected output shape or format."},"allowed_tools":{"type":"array","items":{"type":"string"}},"max_model_calls":{"type":"number"},"max_tool_calls":{"type":"number"}},"required":["goal"],"additionalProperties":false}`),
			Execute:    m.executeDelegate,
		})
	}
	if !m.Broker.Has(domain.ToolFinishTurn) {
		m.Broker.Register(tools.Spec{
			Name:            domain.ToolFinishTurn,
			Description:     "End the current turn with a final answer, turn summary, and structured memory updates. Must be the only tool call in the response.",
			Required:        []string{"final_answer"},
			Readonly:        true,
			Idempotent:      true,
			SideEffectClass: "none",
			Parameters:      json.RawMessage(`{"type":"object","properties":{"final_answer":{"type":"string","description":"User-visible final response for this turn."},"turn_summary":{"type":"string","description":"Concise summary of the turn."},"memory_delta":{"type":"array","description":"Structured session memory updates from this turn.","items":{"type":"object","properties":{"op":{"type":"string","description":"Memory operation: ADD, UPDATE, DELETE, or NOOP."},"section":{"type":"string","description":"Memory section: facts, decisions, pending, references, goal, or progress."},"id":{"type":"string","description":"Entry identifier for facts or decisions."},"text":{"type":"string","description":"Statement or value to store."},"fact":{"type":"string","description":"Shorthand text when adding a fact."},"reason":{"type":"string","description":"Rationale when recording a decision."},"source":{"type":"string","description":"Source turn, invocation, or document id."}},"required":["op"],"additionalProperties":false}},"source_refs":{"type":"array","description":"Source identifiers referenced in this turn.","items":{"type":"string"}}},"required":["final_answer"],"additionalProperties":false}`),
		})
	}
	if !m.Broker.Has(domain.ToolAskUser) {
		m.Broker.Register(tools.Spec{
			Name:            domain.ToolAskUser,
			Description:     "Pause the run and ask the user a question when a decision is needed from the user. Must be the only tool call in the response.",
			Required:        []string{"question"},
			Readonly:        true,
			Idempotent:      true,
			SideEffectClass: "none",
			Parameters:      json.RawMessage(`{"type":"object","properties":{"question":{"type":"string","description":"Question to ask the user."}},"required":["question"],"additionalProperties":false}`),
		})
	}
}

type loopState struct {
	modelID       string
	model         string
	fallbackModel string
	providerID    string
	approvalMode  string
	// summaryOff stops summary attempts for this stretch of the run after one
	// fails, so a broken summary model costs one model call, not one per step.
	summaryOff     bool
	sensitive      bool
	reminded       bool
	obs            []runtime.Observation
	versions       map[string]int
	toolRefs       map[string]string
	prefixN        int
	userAt         int
	snapshots      workspaceSnapshotState
	policy         *security.Policy
	policyRecorded map[string]bool
	policyDenials  map[string]string
	planTools      planToolRegistry
	policyErr      error
	hooks          *hooks.Engine
	hookPre        map[string]bool
	hookDenials    map[string]*domain.Error
	hookErr        error
}

func (st *loopState) setToolRef(providerCallID, invID string) {
	if providerCallID == "" || invID == "" {
		return
	}
	if st.toolRefs == nil {
		st.toolRefs = map[string]string{}
	}
	st.toolRefs[providerCallID] = invID
}

func (st *loopState) version(id string) int {
	if st.versions == nil {
		st.versions = map[string]int{}
	}
	if v, ok := st.versions[id]; ok && v > 0 {
		return v
	}
	return 1
}

func (st *loopState) setVersion(id string, v int) {
	if st.versions == nil {
		st.versions = map[string]int{}
	}
	st.versions[id] = v
}

func (m *Manager) loop(ctx context.Context, run storage.Run, leaseVersion int64, messages []provider.Message) (storage.Run, error) {
	return m.loopFrom(ctx, run, leaseVersion, messages, nil, "", &loopState{})
}

func (m *Manager) loopFrom(ctx context.Context, run storage.Run, leaseVersion int64, messages []provider.Message, continuation []domain.ResponseItem, contProvider string, st *loopState) (storage.Run, error) {
	if st == nil {
		st = &loopState{}
	}
	ctx, err := m.withModelRouting(ctx)
	if err != nil {
		return m.finish(ctx, run, domain.EventFatal, "", asDomain(err))
	}
	model := providerFromContext(ctx, m.Provider)
	var profiled *provider.ProfileProvider
	reloadExecutor := true
	// A consultation can switch the planner and executor's shared profile.
	ctx = m.withOracle(ctx, run.ID, leaseVersion, func() { reloadExecutor = true })
	// New runs start at version 1; every resume has already committed a state
	// transition and restores its frozen rules from the checkpoint instead.
	if run.Version == 1 {
		messages = m.injectProjectRules(ctx, run, messages, st)
		messages = m.injectSkillCatalog(messages, st)
	}
	if hasImages(messages) {
		if !provider.SupportsImages(providerFromContext(ctx, m.Provider)) {
			return m.finish(ctx, run, domain.EventFatal, "", domain.E(domain.CodeProviderConfig, "configured provider/model does not support image input"))
		}
		for i := range messages {
			for j := range messages[i].Images {
				if len(messages[i].Images[j].Data) > 0 {
					continue
				}
				images, err := imageinput.Reload([]provider.Image{messages[i].Images[j]})
				if err != nil {
					return m.finish(ctx, run, domain.EventFatal, "", asDomain(err))
				}
				messages[i].Images[j] = images[0]
			}
		}
	}
	stopHeartbeat := m.startLeaseHeartbeat(ctx, run.ID, leaseVersion)
	defer stopHeartbeat()
	corrections := 0
	for {
		if err := ctx.Err(); err != nil {
			current, gerr := m.Store.GetRun(context.WithoutCancel(ctx), run.ID)
			if gerr == nil && (current.Status == domain.RunCancelled || current.Status == domain.RunCompleted || current.Status == domain.RunFailed) {
				return current, nil
			}
			return m.finish(context.WithoutCancel(ctx), run, domain.EventCancel, "", domain.E(domain.CodeCancelled, "run cancelled"))
		}
		if err := m.Store.HeartbeatLease(ctx, run.ID, m.owner(), leaseVersion, m.now(), m.now().Add(m.leaseFor())); err != nil {
			return m.finish(ctx, run, domain.EventFatal, "", asDomain(err))
		}
		if reloadExecutor {
			profiled, err = m.profiledModel(ctx, run.ID, leaseVersion, config.RoleExecutor)
			if err != nil {
				if ctx.Err() != nil {
					return m.Cancel(context.WithoutCancel(ctx), run.ID)
				}
				return m.finish(ctx, run, domain.EventFatal, "", asDomain(err))
			}
			if profiled != nil {
				model = profiled
			}
			reloadExecutor = false
		}
		if runtime.NeedsRepeatReminder(st.obs) && !st.reminded {
			messages = append(messages, provider.Message{Role: "user", Content: runtime.RepeatReminder})
			st.reminded = true
		}
		beforePrefix := ""
		if st.prefixN > 0 && st.prefixN <= len(messages) {
			beforePrefix = contextpkg.PrefixHash(toContext(messages[:st.prefixN]))
		}
		// Compaction comes before this step's charge: a summary is a model
		// call of its own, and the step must not run on a budget it used up.
		compacted, err := m.compactContext(ctx, run, leaseVersion, messages, st)
		if err != nil {
			_ = m.saveCheckpoint(ctx, run, messages, continuation, contProvider, st)
			return m.finish(ctx, run, domain.EventBudgetExhausted, "", asDomain(err))
		}
		messages = compacted
		if err := m.Store.Charge(ctx, run.ID, domain.ChargeModel, 1); err != nil {
			_ = m.saveCheckpoint(ctx, run, messages, continuation, contProvider, st)
			return m.finish(ctx, run, domain.EventBudgetExhausted, "", asDomain(err))
		}
		if err := m.Store.RequireLease(ctx, run.ID, m.owner(), leaseVersion, m.now()); err != nil {
			return m.finish(ctx, run, domain.EventFatal, "", asDomain(err))
		}
		var prefix []provider.Message
		if st.prefixN > 0 && st.prefixN <= len(messages) {
			prefix = append([]provider.Message(nil), messages[:st.prefixN]...)
			if got := contextpkg.PrefixHash(toContext(prefix)); beforePrefix != "" && got != beforePrefix {
				continuation = nil
				contProvider = ""
			}
		}
		requestMessages := m.withRepoMap(ctx, run, messages, st.prefixN)
		started := time.Now()
		resp, err := model.Complete(ctx, provider.Request{
			Model:                m.Model,
			Messages:             requestMessages,
			Prefix:               prefix,
			Tools:                m.toolDefsFor(st.planTools),
			Continuation:         continuation,
			ContinuationProvider: contProvider,
			OnDelta:              m.deltaSink(run.ID),
		})
		m.endDeltas(run.ID, err == nil)
		usage := m.annotateUsage(model, m.Model, resp)
		// Tokens a failed call reported are spent too.
		m.chargeUsage(ctx, run.ID, usage)
		if usage.LatencyMS == 0 {
			usage.LatencyMS = time.Since(started).Milliseconds()
		}
		if err != nil {
			m.recordFailedUsage(ctx, run.ID, resp, usage)
			if profiled != nil {
				if ctx.Err() != nil {
					return m.Cancel(context.WithoutCancel(ctx), run.ID)
				}
				if domain.IsCode(err, domain.CodeBudget) {
					_ = m.saveCheckpoint(ctx, run, messages, continuation, contProvider, st)
					return m.finish(ctx, run, domain.EventBudgetExhausted, "", asDomain(err))
				}
			}
			return m.finish(ctx, run, domain.EventFatal, "", asDomain(err))
		}
		if err := m.Store.HeartbeatLease(ctx, run.ID, m.owner(), leaseVersion, m.now(), m.now().Add(m.leaseFor())); err != nil {
			return m.finish(ctx, run, domain.EventFatal, "", asDomain(err))
		}
		visible, blob, err := domain.SplitForStorage(resp)
		if err != nil {
			return m.finish(ctx, run, domain.EventFatal, "", asDomain(err))
		}
		batch, planErr := domain.PlanBatch(run.ID, resp, domain.NewID)
		responseID := domain.NewID("rsp_")
		if err := m.Store.SaveModelResponse(ctx, storage.StoredResponse{
			ID: responseID, RunID: run.ID, Provider: resp.Provider, ProviderResponseID: resp.ProviderResponseID,
			Complete: resp.Complete, Usage: usage, Items: visible, Opaque: blob, Action: batch.Action, CreatedAt: m.now(),
		}); err != nil {
			return storage.Run{}, err
		}
		if text := domain.IndexableText(resp.Items); text != "" && !domain.ContainsOpaqueBytes([]byte(text), resp.Items) {
			m.indexDoc(ctx, responseID, run.SessionID, text, "model")
		}
		if err := m.event(ctx, run.ID, "model.completed", map[string]any{
			"response_id": responseID, "provider": resp.Provider, "model": usage.Model, "action": batch.Action,
			"prefix_hash":  contextpkg.PrefixHash(toContext(prefix)),
			"input_tokens": usage.InputTokens, "cached_input_tokens": usage.CachedInputTokens,
			"output_tokens": usage.OutputTokens, "latency_ms": usage.LatencyMS,
			"cost_usd": usage.CostUSD, "cost_known": usage.CostKnown,
		}); err != nil {
			return storage.Run{}, err
		}
		stored := storage.StoredBatch{
			ID: batch.ID, ModelResponseID: responseID, RunID: run.ID,
			Status: batch.Status, Action: batch.Action, Invocations: batch.Invocations,
			ProtocolError: batch.ProtocolError,
		}
		if len(batch.Invocations) > 0 || batch.Status == domain.BatchProtocolError {
			if err := m.Store.SaveBatch(ctx, stored); err != nil {
				return storage.Run{}, err
			}
		}
		if planErr != nil || !resp.Complete {
			corrections++
			if corrections > 1 || !resp.Complete {
				cause := asDomain(planErr)
				if cause == nil {
					cause = domain.E(domain.CodeProtocol, "incomplete response was not dispatched")
				}
				return m.finish(ctx, run, domain.EventFatal, "", cause)
			}
			messages = append(messages, provider.Message{Role: "user", Content: "The previous tool call was rejected before execution: " + planErr.Error() + ". Choose a valid action. Do not repeat a mixed control call."})
			continuation = nil
			contProvider = ""
			continue
		}
		corrections = 0
		if batch.Action == domain.ActionAnswer && batch.Status == domain.BatchComplete {
			return m.commitAnswer(ctx, run, resp, batch)
		}
		if batch.Action == domain.ActionAskUser && len(batch.Invocations) == 1 {
			if err := batch.BeginUserWait(batch.Invocations[0].ID); err != nil {
				return m.finish(ctx, run, domain.EventFatal, "", asDomain(err))
			}
			stored.Status = batch.Status
			stored.Invocations = batch.Invocations
			if err := m.Store.UpdateBatch(ctx, stored); err != nil {
				return storage.Run{}, err
			}
			_ = m.saveCheckpoint(ctx, run, messages, resp.Items, resp.Provider, st)
			return m.finish(ctx, run, domain.EventAskUser, "", nil)
		}
		driveErr := m.driveBatch(ctx, run, &batch, st)
		persist := ctx
		if ctx.Err() != nil {
			persist = context.WithoutCancel(ctx)
		}
		if driveErr != nil && !errors.Is(driveErr, domain.ErrUnknownOutcome) && batch.Status != domain.BatchProtocolError && ctx.Err() == nil {
			return m.finish(ctx, run, domain.EventFatal, "", asDomain(driveErr))
		} else if errors.Is(driveErr, domain.ErrUnknownOutcome) {
			_ = m.saveCheckpoint(persist, run, messages, resp.Items, resp.Provider, st)
			current, gerr := m.Store.GetRun(persist, run.ID)
			if gerr != nil {
				return storage.Run{}, gerr
			}
			return current, nil
		}
		stored.Status = batch.Status
		stored.Action = batch.Action
		stored.Invocations = batch.Invocations
		if len(batch.Invocations) > 0 {
			if err := m.Store.UpdateBatch(persist, stored); err != nil {
				return storage.Run{}, err
			}
		}
		if ctx.Err() != nil {
			return m.finishCancelled(persist, run)
		}
		if err := m.batchEvent(ctx, run.ID, batch); err != nil {
			return storage.Run{}, err
		}
		switch batch.Status {
		case domain.BatchWaitingApproval:
			if err := m.recordPendingApproval(ctx, run, batch); err != nil {
				return storage.Run{}, err
			}
			_ = m.saveCheckpoint(ctx, run, messages, resp.Items, resp.Provider, st)
			return m.finish(ctx, run, domain.EventApproval, "", nil)
		case domain.BatchNeedsReconciliation:
			_ = m.saveCheckpoint(ctx, run, messages, resp.Items, resp.Provider, st)
			return m.finish(ctx, run, domain.EventNeedsReconcile, "", domain.E(domain.CodeConflict, "a tool result is unknown and was not replayed"))
		}
		messages = append(messages, assistantMessage(resp))
		messages = append(messages, toolResultMessages(batch)...)
		if m.failOnToolBudget {
			for _, inv := range batch.Invocations {
				if inv.Result != nil && inv.Result.Error != nil && inv.Result.Error.Code == domain.CodeBudget {
					_ = m.saveCheckpoint(ctx, run, messages, resp.Items, resp.Provider, st)
					return m.finish(ctx, run, domain.EventBudgetExhausted, "", inv.Result.Error)
				}
			}
		}
		continuation = resp.Items
		contProvider = resp.Provider
	}
}

func hasImages(messages []provider.Message) bool {
	for _, message := range messages {
		if len(message.Images) > 0 {
			return true
		}
	}
	return false
}

func (m *Manager) commitAnswer(ctx context.Context, run storage.Run, resp domain.ModelResponse, batch domain.Batch) (storage.Run, error) {
	answer := resp.Text()
	summary := ""
	refs := "[]"
	var deltas []contextpkg.Delta
	syncSummary := false
	if len(batch.Invocations) == 1 && batch.Invocations[0].Name == domain.ToolFinishTurn {
		syncSummary = true
		answer = jsonString(batch.Invocations[0].Arguments, "final_answer")
		summary = jsonString(batch.Invocations[0].Arguments, "turn_summary")
		if raw := jsonField(batch.Invocations[0].Arguments, "source_refs"); len(raw) > 0 {
			refs = string(raw)
		}
		if raw := jsonField(batch.Invocations[0].Arguments, "memory_delta"); len(raw) > 0 {
			parsed, err := contextpkg.ParseDeltas(raw)
			if err != nil {
				return m.finish(ctx, run, domain.EventFatal, "", asDomain(err))
			}
			deltas = parsed
		}
	}
	if text := domain.IndexableText(resp.Items); text != "" && !domain.ContainsOpaqueBytes([]byte(text), resp.Items) {
		m.indexDoc(ctx, "ans_"+run.TurnID, run.SessionID, text, "answer")
	}
	if answer != "" && !strings.Contains(domain.IndexableText(resp.Items), answer) {
		m.indexDoc(ctx, "ans_"+run.TurnID, run.SessionID, answer, "answer")
	}
	if syncSummary {
		run.FinalAnswer = answer
		if strings.TrimSpace(summary) == "" {
			summary = generatedSummary(answer)
		}
		if err := m.persistSummary(ctx, run, summary, refs, deltas); err != nil {
			return storage.Run{}, err
		}
		return m.finish(ctx, run, domain.EventFinalAnswer, answer, nil)
	}
	finished, err := m.finish(ctx, run, domain.EventFinalAnswer, answer, nil)
	if err != nil {
		return storage.Run{}, err
	}
	now := m.now()
	job := storage.SummaryJob{
		ID: domain.NewID("sumjob_"), SessionID: finished.SessionID, TurnID: finished.TurnID, RunID: finished.ID,
		Status: "pending", CreatedAt: now, UpdatedAt: now,
	}
	if err := m.Store.SaveSummaryJob(ctx, job); err != nil {
		return finished, nil
	}
	if err := m.runModelSummary(ctx, finished, job, answer); err != nil {
		return finished, nil
	}
	return m.Store.GetRun(ctx, finished.ID)
}

func (m *Manager) gate(ctx context.Context, run storage.Run, st *loopState) func(domain.Invocation) domain.GateDecision {
	return func(inv domain.Invocation) domain.GateDecision {
		if st.planTools.unknown(inv.Name) != nil {
			return domain.GateDeny
		}
		if excludedByChildToolLimit(ctx, inv.Name) || m.Depth > 0 && inv.Name == tools.ToolAskOracle {
			return domain.GateDeny
		}
		policy, err := m.decidePolicy(ctx, run, st, inv)
		if err != nil {
			st.policyErr = err
			return domain.GateDeny
		}
		if policy.Effect == security.EffectDeny {
			return st.denyPolicy(inv, policy.Reason())
		}
		if inv.Name == "run_command" && !tools.ValidCwd(m.Workspace, jsonString(inv.Arguments, "cwd")) {
			return domain.GateDeny
		}
		if policy.Effect == security.EffectAllow {
			if m.Broker == nil || st.sensitive && isEgressTool(m.Broker, inv.Name) {
				return domain.GateDeny
			}
			allowed, err := m.allowPolicyInvocation(ctx, run, st, inv, policy)
			if err != nil {
				st.policyErr = err
				return domain.GateDeny
			}
			if allowed {
				return domain.GateAllow
			}
		}
		if inv.Name == "run_command" && tools.Dangerous(jsonString(inv.Arguments, "command"), m.Workspace) {
			return domain.GateNeedsApproval
		}
		if m.Broker == nil || st.sensitive && isEgressTool(m.Broker, inv.Name) {
			return domain.GateDeny
		}
		res, err := m.Broker.Authorize(ctx, tools.Request{
			RunID: run.ID, SessionID: run.SessionID, InvocationID: inv.ID, Tool: inv.Name, Args: inv.Arguments,
			DefaultAllow:     m.oracleDefaultAllow(inv.Name),
			ApprovalPossible: policyApprovalPossible(inv.Name),
			SensitiveSeen:    st != nil && st.sensitive,
			Egress:           isEgressTool(m.Broker, inv.Name),
			Now:              m.now(),
		})
		if err != nil || res.Error != nil && !res.NeedsApproval && !res.OK {
			if res.NeedsApproval {
				return domain.GateNeedsApproval
			}
			if res.Error != nil && res.Error.Code == domain.CodeInvalidArgument {
				return domain.GateAllow
			}
			return domain.GateDeny
		}
		if res.NeedsApproval {
			return domain.GateNeedsApproval
		}
		return domain.GateAllow
	}
}

func (m *Manager) execute(ctx context.Context, run storage.Run, st *loopState) domain.ExecFunc {
	return func(inv domain.Invocation) (json.RawMessage, error) {
		if err := st.planTools.unknown(inv.Name); err != nil {
			return nil, err
		}
		policy, err := m.decidePolicy(ctx, run, st, inv)
		if err != nil {
			return nil, err
		}
		if policy.Effect == security.EffectDeny {
			return nil, domain.E(domain.CodeAuthDenied, policy.Reason())
		}
		if err := m.preToolHooks(ctx, run, st, inv); err != nil {
			return nil, err
		}
		fp := runtime.Fingerprint(inv.Name, inv.Arguments)
		if st != nil && (runtime.Blocked(st.obs, fp) || (runtime.AlternatingNoProgress(st.obs) && fingerprintInTail(st.obs, fp))) {
			return nil, domain.E("no_progress", "repeated tool calls made no progress; replan and do not repeat this call")
		}
		if m.Broker == nil {
			return nil, domain.E(domain.CodeToolError, "no tool broker is configured")
		}
		running := inv
		running.Status = domain.InvRunning
		running.Dispatched = true
		running.Result = nil
		nextVer, err := m.commitInv(ctx, run.ID, running, st.version(inv.ID))
		if err != nil {
			return nil, err
		}
		st.setVersion(inv.ID, nextVer)
		if m.CrashOnTool != "" && m.CrashOnTool == inv.Name {
			return nil, domain.ErrUnknownOutcome
		}
		res, err := m.invokeHooked(ctx, run, st, inv, tools.Request{
			RunID: run.ID, SessionID: run.SessionID, InvocationID: inv.ID, Tool: inv.Name, Args: inv.Arguments,
			DefaultAllow:     m.oracleDefaultAllow(inv.Name),
			IdempotencyKey:   jsonString(inv.Arguments, "idempotency_key"),
			ApprovalPossible: false,
			SensitiveSeen:    st.sensitive,
			Egress:           isEgressTool(m.Broker, inv.Name),
			Now:              m.now(),
		})
		m.recordSnapshotDispatch(ctx, inv, res)
		if err != nil {
			return nil, err
		}
		if res.NeedsApproval {
			return nil, domain.E(domain.CodeAuthDenied, "approval required")
		}
		if res.Error != nil {
			if res.UncertainOutcome {
				return nil, domain.ErrUncertainOutcome
			}
			if inv.Name == tools.ToolAskOracle && !res.Dispatched && res.Error.Code == domain.CodeBudget {
				return nil, m.oracleFailure(ctx, run.ID, res.Error)
			}
			return nil, res.Error
		}
		data := res.Data
		if len(data) == 0 {
			data = json.RawMessage(`{}`)
		}
		if inv.Name == "read_file" && tools.SensitivePath(jsonString(inv.Arguments, "path")) {
			st.sensitive = true
		}
		if isOrdinaryTool(inv.Name) {
			st.obs = append(st.obs, runtime.Observation{Fingerprint: fp, ResultHash: runtime.HashResult(data)})
		}
		done := inv
		done.Status = domain.InvSucceeded
		done.Dispatched = true
		done.Result = &domain.CallResult{OK: true, Data: data}
		nextVer, err = m.commitInv(ctx, run.ID, done, st.version(inv.ID))
		if err != nil {
			return nil, err
		}
		st.setVersion(inv.ID, nextVer)
		if text := string(data); !domain.ContainsOpaqueBytes([]byte(text), nil) {
			m.indexDoc(ctx, inv.ID, run.SessionID, text, "tool")
			if st != nil {
				st.setToolRef(inv.ProviderCallID, inv.ID)
			}
		}
		return data, nil
	}
}

func (m *Manager) finishCancelled(ctx context.Context, run storage.Run) (storage.Run, error) {
	current, err := m.Store.GetRun(ctx, run.ID)
	if err == nil && (current.Status == domain.RunCancelled || current.Status == domain.RunCompleted || current.Status == domain.RunFailed) {
		return current, nil
	}
	return m.finish(ctx, run, domain.EventCancel, "", domain.E(domain.CodeCancelled, "run cancelled"))
}

func (m *Manager) commitInv(ctx context.Context, runID string, inv domain.Invocation, expected int) (int, error) {
	payload, _ := json.Marshal(map[string]any{"status": inv.Status, "tool": inv.Name, "provider_call_id": inv.ProviderCallID})
	ev := storage.Event{Type: "invocation." + string(inv.Status), Payload: payload, Persistent: true, CreatedAt: m.now()}
	if err := m.Store.CommitInvocation(ctx, runID, inv, int64(expected), ev); err != nil {
		return expected, err
	}
	return expected + 1, nil
}

func isOrdinaryTool(name string) bool {
	switch name {
	case domain.ToolFinishTurn, domain.ToolAskUser, domain.ToolUpdatePlan, domain.ToolPoll, domain.ToolExecuteProgram, domain.ToolDelegateTask, tools.ToolAskOracle:
		return false
	default:
		return true
	}
}

func fingerprintInTail(obs []runtime.Observation, fp string) bool {
	if !runtime.AlternatingNoProgress(obs) {
		return false
	}
	w := obs[len(obs)-domain.DefaultNoProgressLimit:]
	return fp == w[0].Fingerprint || fp == w[1].Fingerprint
}

func (m *Manager) startLeaseHeartbeat(ctx context.Context, runID string, leaseVersion int64) func() {
	store := m.Store
	ttl := m.leaseFor()
	if store == nil || runID == "" || ttl <= 0 {
		return func() {}
	}
	interval := ttl / 3
	if interval <= 0 {
		interval = time.Millisecond
	}
	owner := m.owner()
	nowFn := m.Now
	now := func() time.Time {
		if nowFn != nil {
			return nowFn()
		}
		return time.Now().UTC()
	}
	hbCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				t := now()
				if err := store.HeartbeatLease(hbCtx, runID, owner, leaseVersion, t, t.Add(ttl)); err != nil {
					return
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func (m *Manager) installGrants(ctx context.Context, run storage.Run) error {
	var grants []domain.Grant
	if m.Depth > 0 {
		grants = m.childGrants(ctx, run)
	} else {
		grants = append(grants, m.Grants...)
		add := func(name, scope string) {
			grants = append(grants, domain.Grant{
				ID: domain.NewID("grd_"), Subject: "harness", RunID: run.ID, Tool: name, Op: "call", ResourceScope: scope, State: "active",
			})
		}
		if m.Workspace != "" {
			add("read_file", m.Workspace)
			add("list_dir", m.Workspace)
			add("list_files", m.Workspace)
			add("grep", m.Workspace)
			add("git_status", m.Workspace)
			add("git_diff", m.Workspace)
			add("git_log", m.Workspace)
			add("run_command", m.Workspace)
		}
		add("update_plan", "")
		add("retrieve_history", "")
		add("retrieve_resource", "")
		add(tools.LoadSkillName, "")
		add(domain.ToolPoll, "")
		add(domain.ToolExecuteProgram, "")
		add(domain.ToolDelegateTask, "")
	}
	for _, g := range grants {
		if g.ID == "" {
			g.ID = domain.NewID("grd_")
		}
		if g.RunID == "" {
			g.RunID = run.ID
		}
		if g.State == "" {
			g.State = "active"
		}
		if g.Op == "" {
			g.Op = "call"
		}
		if err := m.Store.SaveGrant(ctx, g, m.now()); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) parentAllowsWorkspaceReadTool(ctx context.Context, run storage.Run, tool string) bool {
	if m.Workspace == "" {
		return false
	}
	if m.parentHasWorkspaceToolGrant(ctx, run, tool) {
		return true
	}
	if m.Store == nil || run.ParentRunID == "" {
		return true
	}
	parentGrants, err := m.Store.ListGrants(ctx, run.ParentRunID, run.SessionID, m.now())
	if err != nil {
		return false
	}
	return len(parentGrants) == 0
}

func (m *Manager) parentHasWorkspaceWriteGrant(ctx context.Context, run storage.Run) bool {
	return m.parentHasWorkspaceToolGrant(ctx, run, "write_file")
}

func (m *Manager) parentHasWorkspaceToolGrant(ctx context.Context, run storage.Run, tool string) bool {
	if m.Workspace == "" {
		return false
	}
	for _, g := range m.Grants {
		if (g.State == "" || g.State == "active") && g.Tool == tool && (g.Op == "" || g.Op == "call") && g.InvocationID == "" && g.ResourceScope == m.Workspace {
			return true
		}
	}
	if m.Store == nil || run.ParentRunID == "" {
		return false
	}
	parentGrants, err := m.Store.ListGrants(ctx, run.ParentRunID, run.SessionID, m.now())
	if err != nil {
		return false
	}
	for _, g := range parentGrants {
		if (g.State == "" || g.State == "active") && g.Tool == tool && (g.Op == "" || g.Op == "call") && g.InvocationID == "" && g.ResourceScope == m.Workspace {
			return true
		}
	}
	return false
}

func (m *Manager) toolDefs() []provider.ToolDefinition {
	return m.toolDefsFor(nil)
}

func (m *Manager) toolDefsFor(planTools planToolRegistry) []provider.ToolDefinition {
	if m.Broker == nil {
		return nil
	}
	m.ensureTools()
	hasAllowed := m.Depth > 0 && (m.definitionToolLimit || len(m.AllowedTools) > 0)
	allowed := map[string]bool{}
	for _, name := range m.AllowedTools {
		allowed[name] = true
	}
	var out []provider.ToolDefinition
	for _, spec := range m.Broker.List() {
		if planTools.unknown(spec.Name) != nil {
			continue
		}
		if m.Depth > 0 && rootOnlyTool(spec.Name) {
			continue
		}
		if hasAllowed && spec.Name != domain.ToolFinishTurn && !allowed[spec.Name] {
			continue
		}
		params := tools.CleanSchema(spec.Parameters)
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, provider.ToolDefinition{Name: spec.Name, Description: spec.Description, Parameters: params})
	}
	return out
}

func assistantMessage(resp domain.ModelResponse) provider.Message {
	msg := provider.Message{Role: "assistant", Content: resp.Text()}
	msg.ToolCalls = append(msg.ToolCalls, resp.ToolCalls()...)
	return msg
}

func toolResultMessages(batch domain.Batch) []provider.Message {
	var out []provider.Message
	for _, inv := range batch.Invocations {
		body := map[string]any{"untrusted": true, "source": "tool", "provider_call_id": inv.ProviderCallID}
		if inv.Result != nil {
			body["ok"] = inv.Result.OK
			body["data"] = json.RawMessage(inv.Result.Data)
			if inv.Result.Error != nil {
				body["error"] = inv.Result.Error
			}
		}
		raw, err := json.Marshal(body)
		if err != nil {
			raw = []byte(`{"untrusted":true}`)
		}
		out = append(out, provider.Message{Role: "tool", ToolCallID: inv.ProviderCallID, Ref: inv.ID, Content: string(raw)})
	}
	return out
}

// batchEvent records the state of a batch. A denial, a skip, and a
// reconciliation change invocations without running them, so no
// invocation event reports those outcomes.
func (m *Manager) batchEvent(ctx context.Context, runID string, batch domain.Batch) error {
	return m.event(ctx, runID, "tool.batch", map[string]any{
		"batch_id": batch.ID, "status": batch.Status, "calls": callTrace(batch),
	})
}

func callTrace(batch domain.Batch) []map[string]any {
	out := make([]map[string]any, 0, len(batch.Invocations))
	for _, inv := range batch.Invocations {
		ok := false
		if inv.Result != nil {
			ok = inv.Result.OK
		}
		out = append(out, map[string]any{
			"ordinal": inv.Ordinal, "provider_call_id": inv.ProviderCallID,
			"name": inv.Name, "status": inv.Status, "ok": ok,
		})
	}
	return out
}

func generatedSummary(answer string) string {
	a := strings.TrimSpace(answer)
	if len(a) > 500 {
		a = a[:500]
	}
	if a == "" {
		a = "(empty answer)"
	}
	return "Turn summary: " + a
}

func jsonString(raw json.RawMessage, key string) string {
	field := jsonField(raw, key)
	if len(field) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(field, &s); err != nil {
		return ""
	}
	return s
}

func jsonField(raw json.RawMessage, key string) json.RawMessage {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m[key]
}

func isEgressTool(b *tools.Broker, name string) bool {
	if name == "run_command" || name == "http_fetch" || name == tools.ToolAskOracle {
		return true
	}
	if strings.HasPrefix(name, "mcp.") && b != nil {
		if fact, ok := b.Lookup(name); ok && !fact.Readonly {
			return true
		}
	}
	return false
}
