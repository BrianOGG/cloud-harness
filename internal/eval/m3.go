package eval

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/agent"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/storage"
	"github.com/BrianOGG/cloud-harness/internal/storage/sqlite"
	"github.com/BrianOGG/cloud-harness/internal/tools"
)

func seedHistory(ctx context.Context, db *sqlite.DB, sessionID, task string) error {
	now := time.Now().UTC()
	var docs []storage.HistoryDoc
	switch task {
	case "retrieve-chinese-term":
		docs = []storage.HistoryDoc{
			{SourceID: "note_search", RawText: "会议记录讨论了检索的索引策略，标识 note_search。"},
			{SourceID: "note_cloud", RawText: "这篇笔记只谈云计算的成本。"},
		}
	case "retrieve-path-and-id":
		docs = []storage.HistoryDoc{
			{SourceID: "evt_9f3a2c", RawText: "handler lives in internal/server.go and event evt_9f3a2c"},
			{SourceID: "note_words", RawText: "the server and go modules were discussed"},
		}
	default:
		return nil
	}
	ids := make([]string, len(docs))
	for i, doc := range docs {
		ids[i] = doc.SourceID
	}
	if err := db.DeleteHistory(ctx, ids...); err != nil {
		return err
	}
	for _, doc := range docs {
		doc.SessionID = sessionID
		doc.CreatedAt = now
		if err := db.IndexHistory(ctx, doc); err != nil {
			return err
		}
	}
	return nil
}

func scoreChinese(status domain.RunStatus, calls []Call) []string {
	var failures []string
	if status != domain.RunCompleted {
		failures = append(failures, "run status is "+string(status))
	}
	var got *Call
	for i := range calls {
		if calls[i].Name == "retrieve_history" {
			got = &calls[i]
			break
		}
	}
	if got == nil || got.Status != string(domain.InvSucceeded) {
		failures = append(failures, "retrieve_history did not succeed")
		return failures
	}
	if !strings.Contains(got.ResultText, "note_search") {
		failures = append(failures, "检索 did not return note_search")
	}
	if strings.Contains(got.ResultText, "note_cloud") {
		failures = append(failures, "检索 also returned note_cloud")
	}
	return failures
}

func scorePath(status domain.RunStatus, calls []Call) []string {
	var failures []string
	if status != domain.RunCompleted {
		failures = append(failures, "run status is "+string(status))
	}
	var queries []Call
	for _, c := range calls {
		if c.Name == "retrieve_history" {
			queries = append(queries, c)
		}
	}
	if len(queries) != 2 {
		failures = append(failures, "expected two retrieve_history calls")
		return failures
	}
	if queries[0].Status != string(domain.InvSucceeded) || !strings.Contains(queries[0].ResultText, "evt_9f3a2c") || strings.Contains(queries[0].ResultText, "note_words") {
		failures = append(failures, "server.go lookup missed evt_9f3a2c or hit the words row")
	}
	if queries[1].Status != string(domain.InvSucceeded) || !strings.Contains(queries[1].ResultText, "evt_9f3a2c") || strings.Contains(queries[1].ResultText, "note_words") {
		failures = append(failures, "evt_9f3a2c lookup failed")
	}
	return failures
}

func scorePoll(status domain.RunStatus, calls []Call) []string {
	var failures []string
	if status != domain.RunCompleted {
		failures = append(failures, "run status is "+string(status))
	}
	var polls []Call
	for _, c := range calls {
		if c.Name == "poll_tool" {
			polls = append(polls, c)
		}
	}
	if len(polls) != 2 {
		failures = append(failures, "expected two poll_tool calls")
		return failures
	}
	if polls[0].Status != string(domain.InvSucceeded) || !strings.Contains(polls[0].ResultText, `"stopped":"condition"`) {
		failures = append(failures, "ready poll did not stop on the condition")
	}
	if polls[1].Status != string(domain.InvSucceeded) || !strings.Contains(polls[1].ResultText, `"stopped":"cap"`) || !strings.Contains(polls[1].ResultText, `"attempts":3`) {
		failures = append(failures, "down poll did not stop on the cap")
	}
	return failures
}

func scoreHistory(ctx context.Context, db *sqlite.DB, sessionID, task string, _ []Call) []string {
	switch task {
	case "retrieve-chinese-term":
		hits, err := db.SearchHistory(ctx, sessionID, "云计算", 10)
		if err != nil {
			return []string{err.Error()}
		}
		for _, hit := range hits {
			if hit.SourceID == "note_search" {
				return []string{"云计算 matched note_search"}
			}
		}
	case "retrieve-path-and-id":
		hits, err := db.SearchHistory(ctx, sessionID, "server.go", 10)
		if err != nil {
			return []string{err.Error()}
		}
		for _, hit := range hits {
			if hit.SourceID == "note_words" {
				return []string{"server.go matched the words-only row"}
			}
		}
	}
	return nil
}

type armReport struct {
	Name         string  `json:"name"`
	Quality      int     `json:"quality"`
	CostTokens   int     `json:"cost_tokens"`
	LatencyMS    int64   `json:"latency_ms"`
	WallMS       int64   `json:"wall_ms"`
	ModelCalls   int     `json:"model_calls"`
	ToolCalls    int     `json:"tool_calls"`
	HitRate      float64 `json:"hit_rate"`
	InputTokens  int     `json:"input_tokens"`
	CachedTokens int     `json:"cached_input_tokens"`
	Answer       string  `json:"answer"`
}

func comparePlanner(ctx context.Context, base *agent.Manager, db *sqlite.DB, useScripted bool) (Result, error) {
	out := Result{Task: "planner-compare", Status: "COMPLETED"}
	type spec struct {
		name    string
		force   bool
		skip    bool
		answers []domain.ModelResponse
		want    string
		tools   int
	}
	arms := []spec{
		{
			name: "planner_on", force: true,
			answers: []domain.ModelResponse{
				textUsage(`{"resources":[{"id":"note_token","precision":"verbatim"}]}`, 20, 10, 40),
				textUsage("harbor-42", 30, 5, 20),
			},
			want: "harbor-42",
		},
		{
			name: "planner_off_fixed", skip: true,
			answers: []domain.ModelResponse{textUsage("unknown", 20, 4, 15)},
		},
		{
			name: "planner_off_retrieve", skip: true,
			answers: []domain.ModelResponse{
				toolUsage(call("r1", "retrieve_history", `{"query":"harbor"}`), 40, 8, 35),
				textUsage("harbor-42", 50, 5, 40),
			},
			want: "harbor-42", tools: 1,
		},
	}
	var reports []armReport
	var failures []string
	for _, arm := range arms {
		mgr := &agent.Manager{
			Store: base.Store, OwnerID: base.OwnerID, Broker: base.Broker, Workspace: base.Workspace,
			DisableSnapshots: base.DisableSnapshots,
			System:           base.System, ToolDigest: base.ToolDigest, Grants: base.Grants,
			ForcePlanner: arm.force, SkipPlanner: arm.skip,
		}
		ses, err := mgr.CreateSession(ctx)
		if err != nil {
			return out, err
		}
		noteID := "note_" + ses.ID
		if err := db.IndexHistory(ctx, storage.HistoryDoc{
			SourceID: noteID, SessionID: ses.ID, RawText: "deployment token is harbor-42", CreatedAt: time.Now().UTC(),
		}); err != nil {
			return out, err
		}
		var mock *provider.Mock
		if useScripted {
			answers := append([]domain.ModelResponse(nil), arm.answers...)
			if arm.force {
				answers[0] = textUsage(`{"resources":[{"id":"`+noteID+`","precision":"verbatim"}]}`, 20, 10, 40)
			}
			mock = &provider.Mock{Responses: answers}
			mgr.Provider = mock
		} else {
			mgr.Provider = base.Provider
		}
		start := time.Now()
		run, err := mgr.RunMessage(ctx, ses.ID, "What was the deployment token from the earlier turn?")
		if err == nil && !useScripted {
			for run.Status == domain.RunWaiting && run.WaitReason == domain.WaitApproval {
				run, err = mgr.Resume(ctx, run.ID, agent.ResumeInput{Decision: "approve"})
				if err != nil {
					break
				}
			}
		}
		wall := time.Since(start).Milliseconds()
		if err != nil {
			return out, err
		}
		usage, err := sumUsage(ctx, db, run.ID)
		if err != nil {
			return out, err
		}
		batches, err := db.LoadBatches(ctx, run.ID)
		if err != nil {
			return out, err
		}
		toolsN := 0
		for _, b := range batches {
			toolsN += len(b.Invocations)
		}
		quality := 0
		if arm.want != "" && strings.Contains(run.FinalAnswer, arm.want) {
			quality = 1
		}
		report := armReport{
			Name: arm.name, Quality: quality, CostTokens: usage.InputTokens + usage.OutputTokens,
			LatencyMS: usage.LatencyMS, WallMS: wall, ModelCalls: run.Budget.UsedModelCalls,
			ToolCalls: toolsN, Answer: run.FinalAnswer,
			InputTokens: usage.InputTokens, CachedTokens: usage.CachedInputTokens,
		}
		reports = append(reports, report)
		if quality != boolQuality(arm.want) {
			failures = append(failures, arm.name+" quality")
		}
		if toolsN != arm.tools {
			failures = append(failures, arm.name+" tool count")
		}
		if usage.InputTokens <= 0 || usage.LatencyMS <= 0 {
			failures = append(failures, arm.name+" missing cost or latency")
		}
		if mock != nil {
			if arm.name == "planner_on" {
				if len(mock.Requests) < 2 || !strings.Contains(messageBlob(mock.Requests[1].Messages), "deployment token is harbor-42") {
					failures = append(failures, "planner did not inject the history note")
				}
			}
			if arm.name == "planner_off_fixed" {
				if strings.Contains(messageBlob(mock.Requests[0].Messages), "deployment token is harbor-42") {
					failures = append(failures, "fixed context included the old fact")
				}
			}
		}
	}
	if reports[0].CostTokens >= reports[2].CostTokens {
		failures = append(failures, "planner cost was not below the retrieve round trip")
	}
	raw, err := json.Marshal(map[string]any{"arms": reports})
	if err != nil {
		return out, err
	}
	out.Metrics = raw
	out.Failures = failures
	out.Pass = len(failures) == 0
	if len(reports) > 0 {
		out.RunID = "planner-compare"
	}
	return out, nil
}

func compareCache(ctx context.Context, db *sqlite.DB, workspace string, liveProv provider.Provider, useScripted bool) (Result, error) {
	out := Result{Task: "cache-baseline", Status: "COMPLETED"}
	stable, err := cacheArm(ctx, db, workspace, liveProv, useScripted, true)
	if err != nil {
		return out, err
	}
	base, err := cacheArm(ctx, db, workspace, liveProv, useScripted, false)
	if err != nil {
		return out, err
	}
	var failures []string
	if stable.CachedTokens <= 0 || stable.HitRate <= 0 {
		failures = append(failures, "stable prefix recorded no cache hit")
	}
	if base.CachedTokens != 0 || base.HitRate != 0 {
		failures = append(failures, "baseline reported a cache hit")
	}
	if stable.InputTokens <= 0 || base.InputTokens <= 0 {
		failures = append(failures, "missing input tokens")
	}
	raw, err := json.Marshal(map[string]any{
		"stable": stable, "baseline": base,
		"hit_rate_delta": stable.HitRate - base.HitRate,
	})
	if err != nil {
		return out, err
	}
	out.Metrics = raw
	out.Failures = failures
	out.Pass = len(failures) == 0
	out.RunID = "cache-baseline"
	return out, nil
}

func cacheArm(ctx context.Context, db *sqlite.DB, workspace string, liveProv provider.Provider, useScripted, hits bool) (armReport, error) {
	var inner provider.Provider = liveProv
	if useScripted || inner == nil {
		inner = &provider.Mock{Responses: []domain.ModelResponse{
			toolCalls(call("c1", "read_file", `{"path":"alpha.txt"}`)),
			textResp("alpha-one"),
		}}
	}
	name := "baseline"
	if hits {
		name = "stable_prefix"
	}
	broker := tools.NewBroker(db, db)
	if err := tools.RegisterLocal(broker, workspace); err != nil {
		return armReport{}, err
	}
	mgr := &agent.Manager{
		Store: db, Provider: &provider.PrefixCache{Inner: inner, ReportHits: hits},
		OwnerID: "eval", Broker: broker, Workspace: workspace, SkipPlanner: true,
		System: "You are Cloud Harness. Read the file the user names.",
	}
	if !useScripted {
		mgr.Grants = appendLiveDefaultGrants(mgr.Grants, workspace)
	}
	ses, err := mgr.CreateSession(ctx)
	if err != nil {
		return armReport{}, err
	}
	start := time.Now()
	run, err := mgr.RunMessage(ctx, ses.ID, "Read alpha.txt")
	if err == nil && !useScripted {
		for run.Status == domain.RunWaiting && run.WaitReason == domain.WaitApproval {
			run, err = mgr.Resume(ctx, run.ID, agent.ResumeInput{Decision: "approve"})
			if err != nil {
				break
			}
		}
	}
	wall := time.Since(start).Milliseconds()
	if err != nil {
		return armReport{}, err
	}
	usage, err := sumUsage(ctx, db, run.ID)
	if err != nil {
		return armReport{}, err
	}
	rate := 0.0
	if usage.InputTokens > 0 {
		rate = float64(usage.CachedInputTokens) / float64(usage.InputTokens)
	}
	return armReport{
		Name: name, Quality: 1, CostTokens: usage.InputTokens + usage.OutputTokens,
		LatencyMS: usage.LatencyMS, WallMS: wall, ModelCalls: run.Budget.UsedModelCalls,
		ToolCalls: 1, HitRate: rate, InputTokens: usage.InputTokens, CachedTokens: usage.CachedInputTokens,
		Answer: run.FinalAnswer,
	}, nil
}

func sumUsage(ctx context.Context, db *sqlite.DB, runID string) (domain.Usage, error) {
	rows, err := db.ListVisibleResponses(ctx, runID)
	if err != nil {
		return domain.Usage{}, err
	}
	var u domain.Usage
	for _, row := range rows {
		u.InputTokens += row.Usage.InputTokens
		u.OutputTokens += row.Usage.OutputTokens
		u.CachedInputTokens += row.Usage.CachedInputTokens
		u.CacheCreationInputTokens += row.Usage.CacheCreationInputTokens
		u.ReasoningTokens += row.Usage.ReasoningTokens
		u.CacheReported = u.CacheReported || row.Usage.CacheReported
		u.ReasoningReported = u.ReasoningReported || row.Usage.ReasoningReported
		u.LatencyMS += row.Usage.LatencyMS
	}
	return u, nil
}

func textUsage(text string, in, out int, latency int64) domain.ModelResponse {
	return domain.ModelResponse{Provider: "mock", Complete: true, Items: []domain.ResponseItem{
		{Ordinal: 0, Kind: domain.ItemText, Text: text},
		{Ordinal: 1, Kind: domain.ItemUsage, Usage: &domain.Usage{InputTokens: in, OutputTokens: out, LatencyMS: latency}},
	}}
}

func toolUsage(c domain.ToolCall, in, out int, latency int64) domain.ModelResponse {
	cp := c
	return domain.ModelResponse{Provider: "mock", Complete: true, Items: []domain.ResponseItem{
		{Ordinal: 0, Kind: domain.ItemToolCall, ToolCall: &cp},
		{Ordinal: 1, Kind: domain.ItemUsage, Usage: &domain.Usage{InputTokens: in, OutputTokens: out, LatencyMS: latency}},
	}}
}

func boolQuality(want string) int {
	if want == "" {
		return 0
	}
	return 1
}

func messageBlob(msgs []provider.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Content)
		b.WriteByte('\n')
	}
	return b.String()
}
