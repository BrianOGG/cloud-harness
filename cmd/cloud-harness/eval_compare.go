package main

import (
	"fmt"
	"io"

	"github.com/BrianOGG/cloud-harness/internal/eval"
)

// Compare saved artifacts without starting another (potentially live) suite.
func cmdEvalCompare(paths []string, stdout, stderr io.Writer) int {
	if len(paths) != 2 {
		_, _ = fmt.Fprintln(stderr, "usage: cloud-harness eval compare BASELINE.json CURRENT.json")
		return 2
	}
	baseline, err := eval.LoadSuiteReport(paths[0])
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	current, err := eval.LoadSuiteReport(paths[1])
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	cmp := eval.CompareSuiteReports(baseline, current)
	if code := writeJSON(stdout, cmp); code != 0 {
		return code
	}
	_, _ = fmt.Fprintln(stderr, cmp.Summary)
	if cmp.HasRegression || !cmp.Metrics.Comparable {
		return 1
	}
	return 0
}
