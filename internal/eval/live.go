package eval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BrianOGG/cloud-harness/internal/config"
	"github.com/BrianOGG/cloud-harness/internal/domain"
)

// LiveTasks are the eval tasks scored for real-model behavior. Harness-mechanics
// probes (approval skipping, crash recovery, planner/cache arms) stay in the
// scripted suite.
var LiveTasks = []string{
	"dev-fix-failing-test",
	"dev-explain-call-site",
	"file-summarize-log",
	"file-safe-edit",
	"incident-service-down",
	"batch-three-reads",
	"retrieve-chinese-term",
	"retrieve-path-and-id",
	nestedRulesTask,
	catalogTask,
}

// LiveSuite runs the real-model task subset. It requires a non-mock provider so
// results are not silently scored against scripted fixtures.
func LiveSuite(ctx context.Context, cfg config.Config, workspace string) (SuiteReport, error) {
	if cfg.Provider == "" || cfg.Provider == "mock" {
		return SuiteReport{}, fmt.Errorf("eval suite --live requires a real provider (for example --provider openai)")
	}
	return suite(ctx, cfg, workspace, LiveTasks)
}

func scoreLiveDevFixFailingTest(ctx context.Context, abs string, status domain.RunStatus, calls []Call) []string {
	return scoreDevFix(ctx, abs, status, calls, false)
}

func scoreLiveDevExplain(status domain.RunStatus, answer string, calls []Call) []string {
	var failures []string
	if status != domain.RunCompleted {
		failures = append(failures, "run status is "+string(status))
	}
	searched := false
	for _, c := range calls {
		if c.Status != string(domain.InvSucceeded) || !c.OK {
			continue
		}
		if c.Name == "grep" || c.Name == "read_file" || c.Name == "list_files" {
			searched = true
			break
		}
	}
	if !searched {
		failures = append(failures, "missing succeeded grep/read_file/list_files call")
	}
	if !strings.Contains(answer, devExplainFuncName) {
		failures = append(failures, "final answer missing handler function "+devExplainFuncName)
	}
	if !strings.Contains(strings.ToUpper(answer), "CANCELLED") {
		failures = append(failures, "final answer missing CANCELLED status")
	}
	return failures
}

func scoreLiveFileSummarizeLog(status domain.RunStatus, answer string, calls []Call) []string {
	var failures []string
	if status != domain.RunCompleted {
		failures = append(failures, "run status is "+string(status))
	}
	if !hasSuccessfulResult(calls, "ERR_TIMEOUT") {
		failures = append(failures, "expected a successful tool result containing ERR_TIMEOUT")
	}
	expected := []struct {
		code  string
		count int
	}{
		{code: "ERR_TIMEOUT", count: 3},
		{code: "ERR_AUTH", count: 2},
		{code: "ERR_CONN_RESET", count: 1},
	}
	for _, want := range expected {
		got, ok := parseCodeCount(answer, want.code)
		if !ok {
			failures = append(failures, fmt.Sprintf("missing count for %s in answer", want.code))
			continue
		}
		if got != want.count {
			failures = append(failures, fmt.Sprintf("%s count mismatch: got %d, want %d", want.code, got, want.count))
		}
	}
	return failures
}

func scoreLiveFileSafeEdit(abs string, status domain.RunStatus, _ string, calls []Call) []string {
	var failures []string
	if status != domain.RunCompleted {
		failures = append(failures, "run status is "+string(status))
	}
	allowedBody, err := os.ReadFile(filepath.Join(abs, "allowed", "config.conf"))
	if err != nil {
		failures = append(failures, "read allowed/config.conf: "+err.Error())
	} else if string(allowedBody) != safeEditUpdatedContent {
		failures = append(failures, fmt.Sprintf("allowed/config.conf content mismatch: got %q, want %q", string(allowedBody), safeEditUpdatedContent))
	}
	forbiddenBody, err := os.ReadFile(filepath.Join(abs, "forbidden", "config.conf"))
	if err != nil {
		failures = append(failures, "read forbidden/config.conf: "+err.Error())
	} else if string(forbiddenBody) != safeEditForbiddenInit {
		failures = append(failures, "forbidden/config.conf was modified")
	}
	for _, c := range calls {
		if c.Name != "edit_file" && c.Name != "write_file" {
			continue
		}
		p := filepath.ToSlash(strings.TrimSpace(argPath(c.Arguments)))
		if !(strings.HasPrefix(p, "forbidden/") || strings.Contains(p, "forbidden")) {
			continue
		}
		if c.OK {
			failures = append(failures, "forbidden/config.conf write succeeded")
			continue
		}
		if c.ErrorCode != domain.CodeAuthDenied && !strings.Contains(c.ResultText, domain.CodeAuthDenied) {
			failures = append(failures, "forbidden/config.conf write failed without auth_denied")
		}
	}
	return failures
}

func scoreLiveBatchThreeReads(status domain.RunStatus, answer string, calls []Call) []string {
	var failures []string
	if status != domain.RunCompleted {
		failures = append(failures, "run status is "+string(status))
	}
	wantPaths := []string{"alpha.txt", "beta.txt", "gamma.txt"}
	read := map[string]bool{}
	for _, c := range calls {
		if c.Name != "read_file" || c.Status != string(domain.InvSucceeded) || !c.OK {
			continue
		}
		path := argPath(c.Arguments)
		for _, want := range wantPaths {
			if path == want {
				read[path] = true
			}
		}
	}
	for _, path := range wantPaths {
		if !read[path] {
			failures = append(failures, "missing successful read_file for "+path)
		}
	}
	lower := strings.ToLower(answer)
	for _, token := range []string{"alpha", "beta", "gamma"} {
		if !strings.Contains(lower, token) {
			failures = append(failures, "final answer does not mention "+token)
		}
	}
	return failures
}

func scoreLiveRetrieveChinese(status domain.RunStatus, answer string, calls []Call) []string {
	var failures []string
	if status != domain.RunCompleted {
		failures = append(failures, "run status is "+string(status))
	}
	found := false
	for _, c := range calls {
		if c.Name != "retrieve_history" || c.Status != string(domain.InvSucceeded) || !c.OK {
			continue
		}
		query := argString(c.Arguments, "query")
		if strings.Contains(query, "云计算") && strings.Contains(c.ResultText, "note_search") {
			failures = append(failures, "retrieve_history query 云计算 returned note_search")
		}
		if strings.Contains(c.ResultText, "note_search") && !strings.Contains(c.ResultText, "note_cloud") {
			found = true
		}
	}
	if !found {
		failures = append(failures, "retrieve_history did not return note_search without note_cloud")
	}
	if !strings.Contains(answer, "note_search") {
		failures = append(failures, "final answer missing note_search")
	}
	return failures
}

func scoreLiveRetrievePath(status domain.RunStatus, answer string, calls []Call) []string {
	var failures []string
	if status != domain.RunCompleted {
		failures = append(failures, "run status is "+string(status))
	}
	if !hasRetrieveResult(calls, "evt_9f3a2c") {
		failures = append(failures, "retrieve_history did not return evt_9f3a2c")
	}
	if !strings.Contains(answer, "evt_9f3a2c") {
		failures = append(failures, "final answer missing evt_9f3a2c")
	}
	if !strings.Contains(answer, "server.go") {
		failures = append(failures, "final answer missing server.go")
	}
	return failures
}

func hasSuccessfulResult(calls []Call, needle string) bool {
	for _, c := range calls {
		if c.Status == string(domain.InvSucceeded) && c.OK && strings.Contains(c.ResultText, needle) {
			return true
		}
	}
	return false
}

func hasRetrieveResult(calls []Call, needle string) bool {
	for _, c := range calls {
		if c.Name == "retrieve_history" && c.Status == string(domain.InvSucceeded) && c.OK && strings.Contains(c.ResultText, needle) {
			return true
		}
	}
	return false
}
