package eval

import (
	"maps"
	"slices"

	"github.com/BrianOGG/cloud-harness/internal/config"
)

// Bump this version when prompts, fixtures, or metric rubrics change.
const benchmarkVersion = "capabilities-v1"

type Benchmark struct {
	Version  string `json:"version"`
	Mode     string `json:"mode"`
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
}

func benchmark(cfg config.Config) *Benchmark {
	b := &Benchmark{Version: benchmarkVersion, Mode: "live", Provider: cfg.Provider}
	switch cfg.Provider {
	case "", "mock":
		b.Mode, b.Provider = "scripted", "mock"
	case "openai", "openai-responses":
		b.Model = cfg.OpenAIModel
	case "anthropic":
		b.Model = cfg.AnthropicModel
	}
	return b
}

type Metrics struct {
	Tokens
	SuccessRate        *float64 `json:"success_rate"`
	ToolChoiceAccuracy *float64 `json:"tool_choice_accuracy"`
	LatencyMS          *int64   `json:"latency_ms"`
}

type SuiteMetrics struct {
	Metrics
	ChoiceScored  int `json:"tool_choice_scored_tasks"`
	ChoiceCorrect int `json:"tool_choice_correct_tasks"`
	TokenTasks    int `json:"token_complete_tasks"`
}

func summarizeMetrics(report SuiteReport) *SuiteMetrics {
	m := &SuiteMetrics{}
	if report.Tasks == 0 {
		return m
	}
	m.SuccessRate = new(float64(report.Passed) / float64(report.Tasks))
	m.LatencyMS = new(report.WallMS)
	inputs, outputs, withInput, withOutput := 0, 0, 0, 0
	for _, task := range report.TaskMetrics {
		if task.Input != nil {
			inputs += *task.Input
			withInput++
		}
		if task.Output != nil {
			outputs += *task.Output
			withOutput++
		}
		if task.Total != nil {
			m.TokenTasks++
		}
		if task.ChoiceCorrect != nil {
			m.ChoiceScored++
			if *task.ChoiceCorrect {
				m.ChoiceCorrect++
			}
		}
	}
	if withInput == report.Tasks {
		m.Input = &inputs
	}
	if withOutput == report.Tasks {
		m.Output = &outputs
	}
	if m.TokenTasks == report.Tasks {
		m.Total = new(inputs + outputs)
	}
	if m.ChoiceScored == report.Tasks {
		m.ToolChoiceAccuracy = new(float64(m.ChoiceCorrect) / float64(m.ChoiceScored))
	}
	return m
}

type PairedTokens struct {
	Tasks    []string `json:"tasks"`
	Baseline int      `json:"baseline"`
	Current  int      `json:"current"`
	Delta    int      `json:"delta"`
}

type MetricComparison struct {
	Comparable        bool          `json:"comparable"`
	Reason            string        `json:"reason,omitempty"`
	BaselineBenchmark *Benchmark    `json:"baseline_benchmark"`
	CurrentBenchmark  *Benchmark    `json:"current_benchmark"`
	Baseline          *SuiteMetrics `json:"baseline"`
	Current           *SuiteMetrics `json:"current"`
	Delta             *Metrics      `json:"delta"`
	PairedTokens      *PairedTokens `json:"paired_total_tokens"`
}

func compareMetrics(base, current SuiteReport) MetricComparison {
	diff := MetricComparison{BaselineBenchmark: base.Benchmark, CurrentBenchmark: current.Benchmark,
		Baseline: base.Measurements, Current: current.Measurements}
	switch {
	case base.Benchmark == nil || current.Benchmark == nil || diff.Baseline == nil || diff.Current == nil:
		diff.Reason = "report predates benchmark measurements"
	case base.Benchmark.Version == "" || base.Benchmark.Mode == "" || current.Benchmark.Version == "" || current.Benchmark.Mode == "":
		diff.Reason = "benchmark identity missing"
	case base.Benchmark.Version != current.Benchmark.Version || base.Benchmark.Mode != current.Benchmark.Mode:
		diff.Reason = "benchmark version or scripted/live mode differs"
	case len(base.TaskPass) == 0 || base.Tasks != len(base.TaskPass) || current.Tasks != len(current.TaskPass) ||
		!slices.Equal(slices.Sorted(maps.Keys(base.TaskPass)), slices.Sorted(maps.Keys(current.TaskPass))):
		diff.Reason = "task sets differ or are incomplete"
	default:
		diff.Comparable = true
	}
	if !diff.Comparable {
		return diff
	}
	a, b := diff.Baseline, diff.Current
	diff.Delta = &Metrics{
		Tokens:             Tokens{Input: difference(a.Input, b.Input), Output: difference(a.Output, b.Output), Total: difference(a.Total, b.Total)},
		SuccessRate:        difference(a.SuccessRate, b.SuccessRate),
		ToolChoiceAccuracy: difference(a.ToolChoiceAccuracy, b.ToolChoiceAccuracy),
		LatencyMS:          difference(a.LatencyMS, b.LatencyMS),
	}
	var pairs PairedTokens
	for _, name := range slices.Sorted(maps.Keys(base.TaskPass)) {
		left, right := base.TaskMetrics[name].Total, current.TaskMetrics[name].Total
		if left != nil && right != nil {
			pairs.Tasks = append(pairs.Tasks, name)
			pairs.Baseline += *left
			pairs.Current += *right
		}
	}
	if len(pairs.Tasks) != 0 {
		pairs.Delta = pairs.Current - pairs.Baseline
		diff.PairedTokens = &pairs
	}
	return diff
}

func difference[T int | int64 | float64](base, current *T) *T {
	if base == nil || current == nil {
		return nil
	}
	return new(*current - *base)
}
