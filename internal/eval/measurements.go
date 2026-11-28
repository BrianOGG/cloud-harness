package eval

import (
	"context"
	"slices"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/storage"
	"github.com/BrianOGG/cloud-harness/internal/storage/sqlite"
)

// Each group requires at least one of its names. Other names are allowed only
// if listed in another group or Optional. Order and repetitions do not count.
type ToolExpectation struct {
	RequiredAny [][]string `json:"required_any"`
	Optional    []string   `json:"optional,omitempty"`
}

var expectedChoices = map[string]ToolExpectation{
	nestedRulesTask:           {[][]string{{"edit_file", "write_file"}}, []string{"read_file", "list_files", "list_dir"}},
	catalogTask:               {RequiredAny: [][]string{{"search_mcp_tools"}, {catalogTool}}},
	"dev-fix-failing-test":    {[][]string{{"read_file"}, {"edit_file", "write_file"}}, []string{"run_command"}},
	"dev-explain-call-site":   {RequiredAny: [][]string{{"read_file", "grep", "list_files"}}},
	"file-summarize-log":      {RequiredAny: [][]string{{"read_file", "grep"}}},
	"file-safe-edit":          {[][]string{{"edit_file", "write_file"}}, []string{"read_file"}},
	"incident-service-down":   {RequiredAny: [][]string{{"http_fetch"}, {"read_file"}}},
	"incident-wait-for-ready": {RequiredAny: [][]string{{"poll_tool"}}},
	"retrieve-chinese-term":   {RequiredAny: [][]string{{"retrieve_history"}}},
	"retrieve-path-and-id":    {RequiredAny: [][]string{{"retrieve_history"}}},
	"batch-three-reads":       {RequiredAny: [][]string{{"read_file"}}},
	"batch-approval-skip":     {RequiredAny: [][]string{{"read_file"}, {"write_file"}}},
	"recover-mid-batch":       {RequiredAny: [][]string{{"read_file"}, {"write_file"}}},
	"planner-compare":         {RequiredAny: [][]string{{"retrieve_history"}}},
	"cache-baseline":          {RequiredAny: [][]string{{"read_file"}}},
}

func (e ToolExpectation) matches(chosen []string) bool {
	allowed := slices.Clone(e.Optional)
	for _, group := range e.RequiredAny {
		if !slices.ContainsFunc(group, func(name string) bool { return slices.Contains(chosen, name) }) {
			return false
		}
		allowed = append(allowed, group...)
	}
	return !slices.ContainsFunc(chosen, func(name string) bool { return !slices.Contains(allowed, name) })
}

type Tokens struct {
	Input  *int `json:"input_tokens"`
	Output *int `json:"output_tokens"`
	Total  *int `json:"total_tokens"`
}

type TaskMetrics struct {
	Tokens
	LatencyMS     int64            `json:"latency_ms"`
	Expected      *ToolExpectation `json:"expected_tools"`
	Chosen        []string         `json:"chosen_tools"`
	ChoiceCorrect *bool            `json:"tool_choice_correct"`
}

// The trace holds one model response per recorded call. A failed provider
// attempt can charge the budget without leaving a response. Keep coverage
// apart from totals so a partial trace never looks like a complete sample.
type coverage struct {
	responses, complete, withInput, withOutput int
	repeatedCalls                              int
	choices                                    []string
}

func (r *Result) observeRun(ctx context.Context, db *sqlite.DB, run storage.Run) (domain.Usage, int, error) {
	rows, err := db.ListVisibleResponses(ctx, run.ID)
	if err != nil {
		return domain.Usage{}, 0, err
	}
	batches, err := db.LoadBatches(ctx, run.ID)
	if err != nil {
		return domain.Usage{}, 0, err
	}
	var usage domain.Usage
	for _, row := range rows {
		addUsage(&usage, row.Usage)
		r.coverage.responses++
		if row.Complete {
			r.coverage.complete++
		}
		// The persisted integer counter cannot distinguish a missing value
		// from an explicit zero, even when the other counter is present.
		if row.Usage.InputTokens > 0 {
			r.coverage.withInput++
		}
		if row.Usage.OutputTokens > 0 {
			r.coverage.withOutput++
		}
		for _, item := range row.Items {
			if item.ToolCall != nil && item.ToolCall.Name != domain.ToolFinishTurn {
				r.coverage.choices = append(r.coverage.choices, item.ToolCall.Name)
			}
		}
	}
	r.ModelCalls += run.Budget.UsedModelCalls
	addUsage(&r.Usage, usage)
	before := len(r.ToolCalls)
	for _, batch := range batches {
		for _, inv := range batch.Invocations {
			call := Call{
				Ordinal: inv.Ordinal, ProviderCallID: inv.ProviderCallID, Name: inv.Name,
				Arguments: inv.Arguments, Status: string(inv.Status),
			}
			if inv.Result != nil {
				call.OK = inv.Result.OK
				if len(inv.Result.Data) > 0 {
					call.ResultText = string(inv.Result.Data)
				} else if inv.Result.Error != nil {
					call.ResultText = inv.Result.Error.Error()
				}
				if inv.Result.Error != nil {
					call.ErrorCode = inv.Result.Error.Code
				}
			}
			r.ToolCalls = append(r.ToolCalls, call)
		}
	}
	calls := r.ToolCalls[before:]
	r.coverage.repeatedCalls += repeated(calls)
	return usage, len(calls), nil
}

func addUsage(total *domain.Usage, u domain.Usage) {
	total.InputTokens += u.InputTokens
	total.OutputTokens += u.OutputTokens
	total.CachedInputTokens += u.CachedInputTokens
	total.CacheCreationInputTokens += u.CacheCreationInputTokens
	total.ReasoningTokens += u.ReasoningTokens
	total.CacheReported = total.CacheReported || u.CacheReported
	total.ReasoningReported = total.ReasoningReported || u.ReasoningReported
	total.LatencyMS += u.LatencyMS
}

func (r Result) measurements(wallMS int64, err error) TaskMetrics {
	m := TaskMetrics{LatencyMS: wallMS}
	expect, hasRubric := expectedChoices[r.Task]
	if hasRubric {
		m.Expected = &expect
	}
	c := r.coverage
	if err != nil || r.ModelCalls == 0 || c.responses != r.ModelCalls {
		return m
	}
	if c.withInput == r.ModelCalls {
		m.Input = &r.Usage.InputTokens
	}
	if c.withOutput == r.ModelCalls {
		m.Output = &r.Usage.OutputTokens
	}
	if m.Input != nil && m.Output != nil {
		m.Total = new(*m.Input + *m.Output)
	}
	if c.complete == r.ModelCalls {
		m.Chosen = append([]string{}, c.choices...)
		slices.Sort(m.Chosen)
		m.Chosen = slices.Compact(m.Chosen)
		if hasRubric {
			m.ChoiceCorrect = new(expect.matches(m.Chosen))
		}
	}
	return m
}
