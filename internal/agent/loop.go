package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	contextpkg "github.com/cloud-harness/cloud-harness/internal/context"

	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/provider"
	"github.com/cloud-harness/cloud-harness/internal/runtime"
	"github.com/cloud-harness/cloud-harness/internal/storage"
	"github.com/cloud-harness/cloud-harness/internal/tools"
)

type sessionKeyType struct{}

var sessionKey sessionKeyType

func (m *Manager) ensureTools() {
	if m.Broker == nil {
		return
	}
	if !m.Broker.Has("update_plan") {
		m.Broker.Register(tools.Spec{
			Name: "update_plan", Description: "Replace the current run plan.",
			Required: []string{"plan"}, Readonly: true, Idempotent: true, SideEffectClass: "none",
			Parameters: json.RawMessage(`{"type":"object","properties":{"plan":{"type":"string"}},"required":["plan"]}`),
			Execute: func(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
				return args, nil
			},
		})
	}
	if !m.Broker.Has("retrieve_resource") {
		m.Broker.Register(tools.Spec{
			Name: "retrieve_resource", Description: "Load one history row by id at summary, excerpt, or verbatim precision.",
			Required: []string{"id"}, Readonly: true, Idempotent: true, SideEffectClass: "none",
			Parameters: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"precision":{"type":"string"}},"required":["id"]}`),
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
			Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
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
			Parameters: json.RawMessage(`{"type":"object","properties":{"target_tool":{"type":"string"},"target_args":{"type":"object"},"until":{"type":"object"},"interval_seconds":{"type":"number"},"max_attempts":{"type":"number"},"timeout_seconds":{"type":"number"}},"required":["target_tool","until","max_attempts"]}`),
			Execute:    m.executePoll,
		})
	}
	if !m.Broker.Has(domain.ToolExecuteProgram) {
		m.Broker.Register(tools.Spec{
			Name: domain.ToolExecuteProgram, Description: "Run a pre-authorized Starlark script. The script cannot request new permissions.",
			Required: []string{"script", "allow"}, Readonly: true, Idempotent: false, SideEffectClass: "none",
			Parameters: json.RawMessage(`{"type":"object","properties":{"script":{"type":"string"},"allow":{"type":"array"},"confirm_side_effects":{"type":"array"}},"required":["script","allow"]}`),
			Execute:    m.executeProgram,
		})
	}
	if !m.Broker.Has(domain.ToolDelegateTask) {
		m.Broker.Register(tools.Spec{
			Name: domain.ToolDelegateTask, Description: "Run a child task with its own trace, checkpoint, and budget.",
			Required: []string{"goal"}, Readonly: true, Idempotent: false, SideEffectClass: "none",
			Parameters: json.RawMessage(`{"type":"object","properties":{"goal":{"type":"string"},"relevant_context":{"type":"string"},"allowed_tools":{"type":"array"},"max_model_calls":{"type":"number"},"max_tool_calls":{"type":"number"}},"required":["goal"]}`),
			Execute:    m.executeDelegate,
		})
	}
}

type loopState struct {
	sensitive bool
	reminded  bool
	obs       []runtime.Observation
	versions  map[string]int
	prefixN   int
	userAt    int
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
		if runtime.NeedsRepeatReminder(st.obs) && !st.reminded {
			messages = append(messages, provider.Message{Role: "user", Content: runtime.RepeatReminder})
			st.reminded = true
		}
		if err := m.Store.Charge(ctx, run.ID, domain.ChargeModel, 1); err != nil {
			_ = m.saveCheckpoint(ctx, run, messages, continuation, contProvider, st)
			return m.finish(ctx, run, domain.EventBudgetExhausted, "", asDomain(err))
		}
		if err := m.Store.RequireLease(ctx, run.ID, m.owner(), leaseVersion, m.now()); err != nil {
			return m.finish(ctx, run, domain.EventFatal, "", asDomain(err))
		}
		beforePrefix := ""
		if st.prefixN > 0 && st.prefixN <= len(messages) {
			beforePrefix = contextpkg.PrefixHash(toContext(messages[:st.prefixN]))
		}
		messages = maybeCompact(messages, st.prefixN, st.userAt, m.tokenLimit())
		var prefix []provider.Message
		if st.prefixN > 0 && st.prefixN <= len(messages) {
			prefix = append([]provider.Message(nil), messages[:st.prefixN]...)
			if got := contextpkg.PrefixHash(toContext(prefix)); beforePrefix != "" && got != beforePrefix {
				continuation = nil
				contProvider = ""
			}
		}
		started := time.Now()
		resp, err := m.Provider.Complete(ctx, provider.Request{
			Messages:             messages,
			Prefix:               prefix,
			Tools:                m.toolDefs(),
			Continuation:         continuation,
			ContinuationProvider: contProvider,
		})
		usage := usageOf(resp)
		if usage.LatencyMS == 0 {
			usage.LatencyMS = time.Since(started).Milliseconds()
		}
		if err != nil {
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
			"response_id": responseID, "provider": resp.Provider, "action": batch.Action,
			"prefix_hash":  contextpkg.PrefixHash(toContext(prefix)),
			"input_tokens": usage.InputTokens, "cached_input_tokens": usage.CachedInputTokens,
			"output_tokens": usage.OutputTokens, "latency_ms": usage.LatencyMS,
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
		driveErr := domain.Drive(&batch, m.gate(ctx, run, st), m.execute(ctx, run, st))
		persist := ctx
		if ctx.Err() != nil {
			persist = context.WithoutCancel(ctx)
		}
		if driveErr != nil && !errors.Is(driveErr, domain.ErrUnknownOutcome) && batch.Status != domain.BatchProtocolError && ctx.Err() == nil {
			return m.finish(ctx, run, domain.EventFatal, "", asDomain(driveErr))
		} else if errors.Is(driveErr, domain.ErrUnknownOutcome) {
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
		if err := m.event(ctx, run.ID, "tool.batch", map[string]any{
			"batch_id": batch.ID, "status": batch.Status, "calls": callTrace(batch),
		}); err != nil {
			return storage.Run{}, err
		}
		switch batch.Status {
		case domain.BatchWaitingApproval:
			_ = m.saveCheckpoint(ctx, run, messages, resp.Items, resp.Provider, st)
			return m.finish(ctx, run, domain.EventApproval, "", nil)
		case domain.BatchNeedsReconciliation:
			_ = m.saveCheckpoint(ctx, run, messages, resp.Items, resp.Provider, st)
			return m.finish(ctx, run, domain.EventNeedsReconcile, "", domain.E(domain.CodeConflict, "a tool result is unknown and was not replayed"))
		}
		messages = append(messages, assistantMessage(resp))
		messages = append(messages, toolResultMessages(batch)...)
		continuation = resp.Items
		contProvider = resp.Provider
	}
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
	if err := m.runSummaryJob(ctx, finished, job, answer); err != nil {
		return finished, nil
	}
	return m.Store.GetRun(ctx, finished.ID)
}

func (m *Manager) gate(ctx context.Context, run storage.Run, st *loopState) func(domain.Invocation) domain.GateDecision {
	return func(inv domain.Invocation) domain.GateDecision {
		if inv.Name == "run_command" && tools.Dangerous(jsonString(inv.Arguments, "command"), m.Workspace) {
			return domain.GateNeedsApproval
		}
		if st != nil && st.sensitive && (inv.Name == "run_command" || inv.Name == "http_fetch") {
			return domain.GateDeny
		}
		if m.Broker == nil {
			return domain.GateDeny
		}
		res, err := m.Broker.Authorize(ctx, tools.Request{
			RunID: run.ID, SessionID: run.SessionID, Tool: inv.Name, Args: inv.Arguments,
			ApprovalPossible: inv.Name == "write_file" || inv.Name == "run_command",
			SensitiveSeen:    st != nil && st.sensitive,
			Egress:           inv.Name == "run_command" || inv.Name == "http_fetch",
			Now:              m.now(),
		})
		if err != nil || res.Error != nil && !res.NeedsApproval && !res.OK {
			if res.NeedsApproval {
				return domain.GateNeedsApproval
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
		res, err := m.Broker.Invoke(ctx, tools.Request{
			RunID: run.ID, SessionID: run.SessionID, Tool: inv.Name, Args: inv.Arguments,
			IdempotencyKey:   jsonString(inv.Arguments, "idempotency_key"),
			ApprovalPossible: false,
			SensitiveSeen:    st.sensitive,
			Egress:           inv.Name == "run_command" || inv.Name == "http_fetch",
			Now:              m.now(),
		})
		if err != nil {
			return nil, err
		}
		if res.NeedsApproval {
			return nil, domain.E(domain.CodeAuthDenied, "approval required")
		}
		if res.Error != nil {
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
	case domain.ToolFinishTurn, domain.ToolAskUser, domain.ToolUpdatePlan, domain.ToolPoll, domain.ToolExecuteProgram, domain.ToolDelegateTask:
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

func (m *Manager) installGrants(ctx context.Context, run storage.Run) error {
	grants := append([]domain.Grant{}, m.Grants...)
	allowed := map[string]bool{}
	for _, name := range m.AllowedTools {
		allowed[name] = true
	}
	add := func(name, scope string) {
		if m.Depth > 0 && !allowed[name] {
			return
		}
		grants = append(grants, domain.Grant{
			ID: domain.NewID("grd_"), Subject: "harness", RunID: run.ID, Tool: name, Op: "call", ResourceScope: scope, State: "active",
		})
	}
	if m.Depth > 0 {
		for _, name := range m.AllowedTools {
			scope := ""
			if m.Workspace != "" && (name == "read_file" || name == "run_command") {
				scope = m.Workspace
			}
			add(name, scope)
		}
	} else {
		if m.Workspace != "" {
			add("read_file", m.Workspace)
			add("run_command", m.Workspace)
		}
		add("update_plan", "")
		add("retrieve_history", "")
		add("retrieve_resource", "")
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

func (m *Manager) toolDefs() []provider.ToolDefinition {
	if m.Broker == nil {
		return nil
	}
	var out []provider.ToolDefinition
	for _, spec := range m.Broker.List() {
		out = append(out, provider.ToolDefinition{Name: spec.Name, Description: spec.Description, Parameters: spec.Parameters})
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
		out = append(out, provider.Message{Role: "tool", ToolCallID: inv.ProviderCallID, Content: string(raw)})
	}
	return out
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
