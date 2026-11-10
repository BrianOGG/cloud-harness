package eval

import (
	"context"
	"encoding/json"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/config"
)

// SuiteTasks are the scripted tasks that do not need an API key.
var SuiteTasks = []string{
	"batch-approval-skip",
	"recover-mid-batch",
	"retrieve-chinese-term",
	"retrieve-path-and-id",
	"planner-compare",
	"cache-baseline",
	"incident-wait-for-ready",
}

// SuiteReport is the effect-evaluation summary for one scripted pass.
type SuiteReport struct {
	Tasks             int             `json:"tasks"`
	Passed            int             `json:"passed"`
	CompletionRate    float64         `json:"completion_rate"`
	Correctness       float64         `json:"correctness"`
	WallMS            int64           `json:"wall_ms"`
	RepeatedToolCalls int             `json:"repeated_tool_calls"`
	RecoveryPass      bool            `json:"recovery_pass"`
	InputTokens       int             `json:"input_tokens"`
	OutputTokens      int             `json:"output_tokens"`
	CachedTokens      int             `json:"cached_input_tokens"`
	CacheHitRate      float64         `json:"cache_hit_rate"`
	BaselineHitRate   float64         `json:"baseline_hit_rate"`
	Planner           any             `json:"planner,omitempty"`
	Failures          []string        `json:"failures,omitempty"`
	TaskPass          map[string]bool `json:"task_pass"`
}

// Suite runs the scripted tasks and returns only metrics the runs produced.
func Suite(ctx context.Context, cfg config.Config, workspace string) (SuiteReport, error) {
	report := SuiteReport{TaskPass: map[string]bool{}}
	var planner any
	for _, task := range SuiteTasks {
		start := time.Now()
		result, err := Run(ctx, Options{Task: task, Config: cfg, Workspace: workspace})
		wall := time.Since(start).Milliseconds()
		report.Tasks++
		report.WallMS += wall
		if err != nil {
			report.Failures = append(report.Failures, task+": "+err.Error())
			report.TaskPass[task] = false
			continue
		}
		report.TaskPass[task] = result.Pass
		if result.Pass {
			report.Passed++
		} else {
			report.Failures = append(report.Failures, task)
		}
		report.RepeatedToolCalls += repeated(result.ToolCalls)
		if task == "recover-mid-batch" {
			report.RecoveryPass = result.Pass
		}
		if task == "planner-compare" && len(result.Metrics) > 0 {
			_ = json.Unmarshal(result.Metrics, &planner)
			addTokens(&report, result.Metrics)
		}
		if task == "cache-baseline" && len(result.Metrics) > 0 {
			var body struct {
				Stable struct {
					HitRate      float64 `json:"hit_rate"`
					InputTokens  int     `json:"input_tokens"`
					CachedTokens int     `json:"cached_input_tokens"`
					CostTokens   int     `json:"cost_tokens"`
				} `json:"stable"`
				Baseline struct {
					HitRate float64 `json:"hit_rate"`
				} `json:"baseline"`
			}
			if err := json.Unmarshal(result.Metrics, &body); err == nil {
				report.CacheHitRate = body.Stable.HitRate
				report.BaselineHitRate = body.Baseline.HitRate
				report.InputTokens += body.Stable.InputTokens
				report.CachedTokens += body.Stable.CachedTokens
				report.OutputTokens += body.Stable.CostTokens - body.Stable.InputTokens
			}
		}
	}
	report.Planner = planner
	if report.Tasks > 0 {
		report.CompletionRate = float64(report.Passed) / float64(report.Tasks)
		report.Correctness = report.CompletionRate
	}
	return report, nil
}

func repeated(calls []Call) int {
	seen := map[string]int{}
	extra := 0
	for _, call := range calls {
		key := call.Name + " " + string(call.Arguments)
		seen[key]++
		if seen[key] == 2 {
			extra++
		}
	}
	return extra
}

func addTokens(report *SuiteReport, raw json.RawMessage) {
	var body struct {
		Arms []struct {
			InputTokens  int `json:"input_tokens"`
			CostTokens   int `json:"cost_tokens"`
			CachedTokens int `json:"cached_input_tokens"`
		} `json:"arms"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return
	}
	for _, arm := range body.Arms {
		report.InputTokens += arm.InputTokens
		report.CachedTokens += arm.CachedTokens
		if arm.CostTokens >= arm.InputTokens {
			report.OutputTokens += arm.CostTokens - arm.InputTokens
		}
	}
}
