package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/config"
)

// SuiteTasks are the 13 evaluation tasks in docs/eval/tasks.json.
var SuiteTasks = []string{
	"dev-fix-failing-test",
	"dev-explain-call-site",
	"file-summarize-log",
	"file-safe-edit",
	"incident-service-down",
	"incident-wait-for-ready",
	"retrieve-chinese-term",
	"retrieve-path-and-id",
	"batch-three-reads",
	"batch-approval-skip",
	"recover-mid-batch",
	"planner-compare",
	"cache-baseline",
}

// SuiteReport is the effect-evaluation summary for one scripted pass.
type SuiteReport struct {
	Tasks             int              `json:"tasks"`
	Passed            int              `json:"passed"`
	CompletionRate    float64          `json:"completion_rate"`
	Correctness       float64          `json:"correctness"`
	WallMS            int64            `json:"wall_ms"`
	ModelCalls        int              `json:"model_calls"`
	ToolCalls         int              `json:"tool_calls"`
	RepeatedToolCalls int              `json:"repeated_tool_calls"`
	RecoveryPass      bool             `json:"recovery_pass"`
	InputTokens       int              `json:"input_tokens"`
	OutputTokens      int              `json:"output_tokens"`
	CachedTokens      int              `json:"cached_input_tokens"`
	CacheHitRate      float64          `json:"cache_hit_rate"`
	BaselineHitRate   float64          `json:"baseline_hit_rate"`
	Planner           any              `json:"planner,omitempty"`
	Failures          []string         `json:"failures,omitempty"`
	TaskPass          map[string]bool  `json:"task_pass"`
	Comparison        *SuiteComparison `json:"comparison,omitempty"`
}

// TaskChange describes the pass/fail transition of one task between baseline and current.
type TaskChange struct {
	Task     string `json:"task"`
	Status   string `json:"status"`
	Baseline *bool  `json:"baseline,omitempty"`
	Current  *bool  `json:"current,omitempty"`
}

// SuiteComparison summarizes differences between a baseline SuiteReport and a current run.
type SuiteComparison struct {
	BaselineTasks          int          `json:"baseline_tasks"`
	BaselinePassed         int          `json:"baseline_passed"`
	CurrentTasks           int          `json:"current_tasks"`
	CurrentPassed          int          `json:"current_passed"`
	BaselineCompletionRate float64      `json:"baseline_completion_rate"`
	CurrentCompletionRate  float64      `json:"current_completion_rate"`
	CompletionRateDelta    float64      `json:"completion_rate_delta"`
	Regressed              []string     `json:"regressed"`
	Fixed                  []string     `json:"fixed"`
	Added                  []string     `json:"added"`
	Removed                []string     `json:"removed"`
	TaskChanges            []TaskChange `json:"task_changes,omitempty"`
	HasRegression          bool         `json:"has_regression"`
	Summary                string       `json:"summary"`
}

// WriteSuiteReport writes a SuiteReport to path as indented JSON including task_pass.
func WriteSuiteReport(path string, report SuiteReport) error {
	report.Comparison = nil
	if report.TaskPass == nil {
		report.TaskPass = map[string]bool{}
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0o644)
}

// LoadSuiteReport reads and unmarshals a persisted SuiteReport JSON file.
func LoadSuiteReport(path string) (SuiteReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SuiteReport{}, err
	}
	var report SuiteReport
	if err := json.Unmarshal(data, &report); err != nil {
		return SuiteReport{}, err
	}
	if report.TaskPass == nil {
		report.TaskPass = map[string]bool{}
	}
	return report, nil
}

// CompareSuiteReports compares a current SuiteReport against a baseline SuiteReport.
func CompareSuiteReports(baseline, current SuiteReport) SuiteComparison {
	cmp := SuiteComparison{
		BaselineTasks:          baseline.Tasks,
		BaselinePassed:         baseline.Passed,
		CurrentTasks:           current.Tasks,
		CurrentPassed:          current.Passed,
		BaselineCompletionRate: baseline.CompletionRate,
		CurrentCompletionRate:  current.CompletionRate,
		Regressed:              []string{},
		Fixed:                  []string{},
		Added:                  []string{},
		Removed:                []string{},
	}
	if cmp.BaselineCompletionRate == 0 && baseline.Passed > 0 && baseline.Tasks > 0 {
		cmp.BaselineCompletionRate = float64(baseline.Passed) / float64(baseline.Tasks)
	}
	if cmp.CurrentCompletionRate == 0 && current.Passed > 0 && current.Tasks > 0 {
		cmp.CurrentCompletionRate = float64(current.Passed) / float64(current.Tasks)
	}
	cmp.CompletionRateDelta = cmp.CurrentCompletionRate - cmp.BaselineCompletionRate

	seen := map[string]bool{}
	var ordered []string
	for _, t := range SuiteTasks {
		_, inBase := baseline.TaskPass[t]
		_, inCur := current.TaskPass[t]
		if inBase || inCur {
			seen[t] = true
			ordered = append(ordered, t)
		}
	}
	var extras []string
	for t := range baseline.TaskPass {
		if !seen[t] {
			seen[t] = true
			extras = append(extras, t)
		}
	}
	for t := range current.TaskPass {
		if !seen[t] {
			seen[t] = true
			extras = append(extras, t)
		}
	}
	sort.Strings(extras)
	ordered = append(ordered, extras...)

	for _, t := range ordered {
		bVal, inBase := baseline.TaskPass[t]
		cVal, inCur := current.TaskPass[t]
		switch {
		case inBase && inCur:
			bCopy, cCopy := bVal, cVal
			switch {
			case bVal && !cVal:
				cmp.Regressed = append(cmp.Regressed, t)
				cmp.TaskChanges = append(cmp.TaskChanges, TaskChange{Task: t, Status: "regressed", Baseline: &bCopy, Current: &cCopy})
			case !bVal && cVal:
				cmp.Fixed = append(cmp.Fixed, t)
				cmp.TaskChanges = append(cmp.TaskChanges, TaskChange{Task: t, Status: "fixed", Baseline: &bCopy, Current: &cCopy})
			case bVal && cVal:
				cmp.TaskChanges = append(cmp.TaskChanges, TaskChange{Task: t, Status: "unchanged_pass", Baseline: &bCopy, Current: &cCopy})
			default:
				cmp.TaskChanges = append(cmp.TaskChanges, TaskChange{Task: t, Status: "unchanged_fail", Baseline: &bCopy, Current: &cCopy})
			}
		case !inBase && inCur:
			cCopy := cVal
			cmp.Added = append(cmp.Added, t)
			cmp.TaskChanges = append(cmp.TaskChanges, TaskChange{Task: t, Status: "added", Current: &cCopy})
		case inBase && !inCur:
			bCopy := bVal
			cmp.Removed = append(cmp.Removed, t)
			cmp.TaskChanges = append(cmp.TaskChanges, TaskChange{Task: t, Status: "removed", Baseline: &bCopy})
		}
	}
	cmp.HasRegression = len(cmp.Regressed) > 0
	cmp.Summary = fmt.Sprintf(
		"suite comparison: completion_rate %.4f -> %.4f (delta %+.4f, passed %d/%d -> %d/%d); regressed(%d)=[%s] fixed(%d)=[%s] added(%d)=[%s] removed(%d)=[%s]",
		cmp.BaselineCompletionRate, cmp.CurrentCompletionRate, cmp.CompletionRateDelta,
		cmp.BaselinePassed, cmp.BaselineTasks, cmp.CurrentPassed, cmp.CurrentTasks,
		len(cmp.Regressed), strings.Join(cmp.Regressed, ","),
		len(cmp.Fixed), strings.Join(cmp.Fixed, ","),
		len(cmp.Added), strings.Join(cmp.Added, ","),
		len(cmp.Removed), strings.Join(cmp.Removed, ","),
	)
	return cmp
}

// Suite runs the scripted tasks and returns only metrics the runs produced.
func Suite(ctx context.Context, cfg config.Config, workspace string) (SuiteReport, error) {
	return suite(ctx, cfg, workspace, SuiteTasks)
}

func suite(ctx context.Context, cfg config.Config, workspace string, tasks []string) (SuiteReport, error) {
	report := SuiteReport{TaskPass: map[string]bool{}}
	var planner any
	for _, task := range tasks {
		start := time.Now()
		taskWS := ""
		if workspace != "" {
			taskWS = filepath.Join(workspace, task)
		}
		result, err := Run(ctx, Options{Task: task, Config: cfg, Workspace: taskWS})
		wall := time.Since(start).Milliseconds()
		report.Tasks++
		report.WallMS += wall
		if err != nil {
			report.Failures = append(report.Failures, task+": "+err.Error())
			report.TaskPass[task] = false
			continue
		}
		report.TaskPass[task] = result.Pass
		report.ModelCalls += result.ModelCalls
		report.ToolCalls += len(result.ToolCalls)
		report.InputTokens += result.Usage.InputTokens
		report.OutputTokens += result.Usage.OutputTokens
		report.CachedTokens += result.Usage.CachedInputTokens
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
			addPlannerMetrics(&report, result.Metrics)
		}
		if task == "cache-baseline" && len(result.Metrics) > 0 {
			var body struct {
				Stable struct {
					HitRate      float64 `json:"hit_rate"`
					ModelCalls   int     `json:"model_calls"`
					ToolCalls    int     `json:"tool_calls"`
					InputTokens  int     `json:"input_tokens"`
					CachedTokens int     `json:"cached_input_tokens"`
					CostTokens   int     `json:"cost_tokens"`
				} `json:"stable"`
				Baseline struct {
					HitRate    float64 `json:"hit_rate"`
					ModelCalls int     `json:"model_calls"`
					ToolCalls  int     `json:"tool_calls"`
				} `json:"baseline"`
			}
			if err := json.Unmarshal(result.Metrics, &body); err == nil {
				report.CacheHitRate = body.Stable.HitRate
				report.BaselineHitRate = body.Baseline.HitRate
				report.ModelCalls += body.Stable.ModelCalls + body.Baseline.ModelCalls
				report.ToolCalls += body.Stable.ToolCalls + body.Baseline.ToolCalls
				report.InputTokens += body.Stable.InputTokens
				report.CachedTokens += body.Stable.CachedTokens
				report.OutputTokens += body.Stable.CostTokens - body.Stable.InputTokens
			}
		}
	}
	report.Planner = planner
	if report.CacheHitRate == 0 && report.InputTokens > 0 {
		report.CacheHitRate = float64(report.CachedTokens) / float64(report.InputTokens)
	}
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

func addPlannerMetrics(report *SuiteReport, raw json.RawMessage) {
	var body struct {
		Arms []struct {
			ModelCalls   int `json:"model_calls"`
			ToolCalls    int `json:"tool_calls"`
			InputTokens  int `json:"input_tokens"`
			CostTokens   int `json:"cost_tokens"`
			CachedTokens int `json:"cached_input_tokens"`
		} `json:"arms"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return
	}
	for _, arm := range body.Arms {
		report.ModelCalls += arm.ModelCalls
		report.ToolCalls += arm.ToolCalls
		report.InputTokens += arm.InputTokens
		report.CachedTokens += arm.CachedTokens
		if arm.CostTokens >= arm.InputTokens {
			report.OutputTokens += arm.CostTokens - arm.InputTokens
		}
	}
}
