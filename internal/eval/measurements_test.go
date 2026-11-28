package eval

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

func TestTaskManifestMatchesBenchmarks(t *testing.T) {
	data, err := os.ReadFile("../../docs/eval/tasks.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Version string
		Live    []string `json:"live_tasks"`
		Tasks   []struct {
			ID    string
			Tools []string
		}
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Version != benchmarkVersion || !slices.Equal(manifest.Live, LiveTasks) || len(manifest.Tasks) != len(SuiteTasks) {
		t.Fatal("benchmark version, live tasks, or full task count differs from manifest")
	}
	for i, task := range manifest.Tasks {
		rubric, ok := expectedChoices[task.ID]
		if !ok || task.ID != SuiteTasks[i] {
			t.Fatalf("unexpected task or missing rubric for %s", task.ID)
		}
		names := slices.Clone(rubric.Optional)
		for _, group := range rubric.RequiredAny {
			names = append(names, group...)
		}
		slices.Sort(names)
		slices.Sort(task.Tools)
		if !slices.Equal(names, task.Tools) {
			t.Errorf("%s: expected tools %v, manifest has %v", task.ID, names, task.Tools)
		}
	}
}

func TestChoiceAndUsageCoverage(t *testing.T) {
	r := Result{
		Task: "dev-fix-failing-test", ModelCalls: 2, Usage: domain.Usage{InputTokens: 17, OutputTokens: 8},
		coverage: coverage{responses: 2, complete: 2, withInput: 2, withOutput: 1},
	}
	for _, tc := range []struct {
		names []string
		want  bool
	}{
		{[]string{"edit_file", "read_file", "read_file"}, true},
		{[]string{"read_file", "run_command", "write_file"}, true},
		{[]string{"edit_file"}, false},
		{[]string{"read_file", "write_file", "http_fetch"}, false},
		{nil, false},
	} {
		r.coverage.choices = tc.names
		got := r.measurements(0, nil)
		if got.ChoiceCorrect == nil || *got.ChoiceCorrect != tc.want || got.Chosen == nil {
			t.Errorf("scoring %v: %+v", tc.names, got)
		}
		if got.LatencyMS != 0 || got.Input == nil || *got.Input != 17 || got.Output != nil || got.Total != nil {
			t.Errorf("partial token usage treated as complete: %+v", got)
		}
	}
	r.coverage.withOutput++
	if got := r.measurements(10, nil); got.Total == nil || *got.Total != 25 {
		t.Fatalf("complete usage: %+v", got)
	}
	r.ModelCalls++ // a charged provider attempt with no response
	if got := r.measurements(10, nil); got.Input != nil || got.Total != nil || got.ChoiceCorrect != nil || got.Chosen != nil {
		t.Fatalf("incomplete response history: %+v", got)
	}
}

func TestCompareMetricCoverage(t *testing.T) {
	report := func(aTokens *int, elapsed int64, success bool) SuiteReport {
		r := SuiteReport{
			Benchmark: &Benchmark{Version: benchmarkVersion, Mode: "live"},
			TaskPass:  map[string]bool{"a": success, "b": true}, Tasks: 2, Passed: 1, WallMS: elapsed,
			TaskMetrics: map[string]TaskMetrics{
				"a": {Tokens: Tokens{Input: aTokens, Output: new(0), Total: aTokens}, ChoiceCorrect: &success},
				"b": {ChoiceCorrect: new(true)},
			},
		}
		if success {
			r.Passed++
		}
		r.Measurements = summarizeMetrics(r)
		return r
	}
	base, cur := report(new(11), 6, false), report(new(7), 3, true)
	diff := compareMetrics(base, cur)
	if !diff.Comparable || diff.Delta.Total != nil || *diff.Delta.ToolChoiceAccuracy != .5 ||
		*diff.Delta.SuccessRate != .5 || *diff.Delta.LatencyMS != -3 || diff.PairedTokens.Delta != -4 ||
		!slices.Equal(diff.PairedTokens.Tasks, []string{"a"}) {
		t.Fatalf("bad metric comparison: %+v", diff)
	}
	if noPairs := compareMetrics(report(nil, 1, false), cur); noPairs.PairedTokens != nil {
		t.Fatalf("unexpected paired sample: %+v", noPairs)
	}
	cur.Benchmark.Mode = "scripted"
	if mixed := compareMetrics(base, cur); mixed.Comparable || mixed.Delta != nil {
		t.Fatalf("mixed modes returned deltas: %+v", mixed)
	}
	cur.Benchmark.Mode = "live"
	delete(cur.TaskPass, "b")
	if mixed := compareMetrics(base, cur); mixed.Comparable || mixed.Delta != nil {
		t.Fatalf("mixed task sets returned deltas: %+v", mixed)
	}
	if legacy := compareMetrics(SuiteReport{}, cur); legacy.Comparable || legacy.Delta != nil {
		t.Fatalf("legacy report returned deltas: %+v", legacy)
	}
}
