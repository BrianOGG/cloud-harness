package eval

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloud-harness/cloud-harness/internal/config"
	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/provider"
	"github.com/cloud-harness/cloud-harness/internal/storage/sqlite"
)

func TestApprovalSkipAndCrashRecovery(t *testing.T) {
	ctx := context.Background()
	for _, task := range []string{"batch-approval-skip", "recover-mid-batch"} {
		root := t.TempDir()
		result, err := Run(ctx, Options{
			Task:      task,
			Config:    config.Config{DBPath: filepath.Join(root, "eval.db"), Provider: "mock"},
			Workspace: root,
		})
		if err != nil {
			t.Fatal(task, err)
		}
		if !result.Pass {
			t.Fatalf("%s failures %#v calls %#v status %s", task, result.Failures, result.ToolCalls, result.Status)
		}
		if result.RunID == "" {
			t.Fatal("missing run id")
		}
	}
}

func TestRetrievalPlannerAndCacheEvals(t *testing.T) {
	ctx := context.Background()
	for _, task := range []string{"retrieve-chinese-term", "retrieve-path-and-id", "planner-compare", "cache-baseline", "incident-wait-for-ready"} {
		root := t.TempDir()
		result, err := Run(ctx, Options{
			Task:      task,
			Config:    config.Config{DBPath: filepath.Join(root, "eval.db"), Provider: "mock"},
			Workspace: root,
		})
		if err != nil {
			t.Fatal(task, err)
		}
		if !result.Pass {
			t.Fatalf("%s failures %#v metrics %s", task, result.Failures, result.Metrics)
		}
	}
}

func TestBatchThreeReadsStreamingOpenAI(t *testing.T) {
	const key = "sk-eval-fixture"
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+key {
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), key) {
			t.Error("request body contains the api key")
		}
		n++
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			chunks := []string{
				`data: {"id":"chatcmpl_1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"alpha.txt\"}"}}]}}]}`,
				`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"beta.txt\"}"}}]}}]}`,
				`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":2,"id":"call_c","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"gamma.txt\"}"}}]}}]}`,
				`data: {"choices":[{"index":0,"finish_reason":"tool_calls","delta":{}}]}`,
				"data: [DONE]",
			}
			for _, c := range chunks {
				io.WriteString(w, c+"\n\n")
			}
			return
		}
		io.WriteString(w, `data: {"id":"chatcmpl_2","choices":[{"index":0,"delta":{"content":"alpha, beta, and gamma were read."}}]}`+"\n\n")
		io.WriteString(w, `data: {"choices":[{"index":0,"finish_reason":"stop","delta":{}}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	result, err := Run(context.Background(), Options{
		Task: "batch-three-reads",
		Config: config.Config{
			DBPath:   filepath.Join(t.TempDir(), "eval.db"),
			Provider: "openai",
		},
		Provider: &provider.OpenAICompatible{
			BaseURL: srv.URL, APIKey: key, Model: "agnes-2.5-flash", Client: srv.Client(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), key) || strings.Contains(string(raw), "Bearer") {
		t.Fatalf("result leaked credentials: %s", raw)
	}
	if !result.Pass {
		t.Fatalf("failures %#v calls %#v", result.Failures, result.ToolCalls)
	}
	if result.Status != "COMPLETED" || result.RunID == "" || len(result.ToolCalls) != 3 {
		t.Fatalf("%+v", result)
	}
	if result.ToolCalls[0].ProviderCallID != "call_a" || result.ToolCalls[1].ProviderCallID != "call_b" || result.ToolCalls[2].ProviderCallID != "call_c" {
		t.Fatalf("order %+v", result.ToolCalls)
	}
	if n != 2 {
		t.Fatalf("provider calls %d", n)
	}
}

func TestEvalFiveNewScenariosScripted(t *testing.T) {
	ctx := context.Background()
	fiveTasks := []string{
		"dev-fix-failing-test",
		"dev-explain-call-site",
		"file-summarize-log",
		"file-safe-edit",
		"incident-service-down",
	}
	for _, task := range fiveTasks {
		root := t.TempDir()
		result, err := Run(ctx, Options{
			Task:      task,
			Config:    config.Config{DBPath: filepath.Join(root, "eval.db"), Provider: "mock"},
			Workspace: root,
		})
		if err != nil {
			t.Fatalf("task=%s err=%v", task, err)
		}
		if !result.Pass {
			t.Fatalf("task=%s failed: failures=%#v calls=%#v status=%s", task, result.Failures, result.ToolCalls, result.Status)
		}
		if result.RunID == "" || result.Status != string(domain.RunCompleted) {
			t.Fatalf("task=%s unexpected result: %+v", task, result)
		}
	}
	// Also verify empty Provider defaults to scripted/mock path.
	emptyProvRoot := t.TempDir()
	emptyProvRes, err := Run(ctx, Options{
		Task:      "dev-explain-call-site",
		Config:    config.Config{DBPath: filepath.Join(emptyProvRoot, "eval.db"), Provider: ""},
		Workspace: emptyProvRoot,
	})
	if err != nil || !emptyProvRes.Pass {
		t.Fatalf("empty provider failed: %+v %v", emptyProvRes, err)
	}

	// Verify scorer rejects tampered test file or missing go binary for dev-fix-failing-test.
	t.Run("dev-fix-failing-test-scorer-guards", func(t *testing.T) {
		ws := t.TempDir()
		if err := prepare("dev-fix-failing-test", ws); err != nil {
			t.Fatal(err)
		}
		// Bug still present -> go test fails and no write call was made.
		fails := scoreDevFixFailingTest(ctx, ws, domain.RunCompleted, nil)
		if len(fails) < 2 {
			t.Fatalf("expected failures for unedited buggy package, got %#v", fails)
		}
		// Tamper with calc_test.go -> sha256 check fails.
		if err := os.WriteFile(filepath.Join(ws, "calc_test.go"), []byte("package calc\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		fails = scoreDevFixFailingTest(ctx, ws, domain.RunCompleted, []Call{
			{Name: "edit_file", Arguments: json.RawMessage(`{"path":"calc.go"}`), Status: string(domain.InvSucceeded), OK: true},
		})
		hasSHA := false
		for _, f := range fails {
			if strings.Contains(f, "sha256") {
				hasSHA = true
			}
		}
		if !hasSHA {
			t.Fatalf("expected sha256 failure when test file modified, got %#v", fails)
		}
		// Missing go binary in PATH -> fails with clear reason.
		t.Setenv("PATH", "")
		noGoFails := scoreDevFixFailingTest(ctx, ws, domain.RunCompleted, []Call{
			{Name: "edit_file", Arguments: json.RawMessage(`{"path":"calc.go"}`), Status: string(domain.InvSucceeded), OK: true},
		})
		hasNoGo := false
		for _, f := range noGoFails {
			if strings.Contains(f, "go binary not found") {
				hasNoGo = true
			}
		}
		if !hasNoGo {
			t.Fatalf("expected missing go binary failure reason, got %#v", noGoFails)
		}
	})

	// Verify full 13-task Suite passes.
	t.Run("full-suite-13-of-13", func(t *testing.T) {
		ws := t.TempDir()
		report, err := Suite(ctx, config.Config{DBPath: filepath.Join(ws, "suite.db"), Provider: "mock"}, ws)
		if err != nil {
			t.Fatal(err)
		}
		if report.Tasks != 13 || report.Passed != 13 || len(report.Failures) != 0 {
			t.Fatalf("expected 13/13 pass, got tasks=%d passed=%d failures=%#v task_pass=%#v", report.Tasks, report.Passed, report.Failures, report.TaskPass)
		}
	})
}

type fakeOpenAIProvider struct {
	calls atomic.Int32
	step  func(n int32, req provider.Request) (domain.ModelResponse, error)
}

func (f *fakeOpenAIProvider) Name() string { return "openai" }

func (f *fakeOpenAIProvider) Complete(_ context.Context, req provider.Request) (domain.ModelResponse, error) {
	n := f.calls.Add(1)
	if f.step != nil {
		return f.step(n, req)
	}
	return domain.ModelResponse{
		Provider:           "openai",
		ProviderResponseID: "fake_openai_resp",
		Complete:           true,
		Items: []domain.ResponseItem{
			{Ordinal: 0, Kind: domain.ItemText, Text: "fake-openai-live-response"},
			{Ordinal: 1, Kind: domain.ItemUsage, Usage: &domain.Usage{InputTokens: 10, OutputTokens: 5, LatencyMS: 5}},
		},
	}, nil
}

func TestEvalOpenAIProviderNeverScripted(t *testing.T) {
	ctx := context.Background()

	// 1. Verify that for every single one of the 13 tasks, when Config.Provider == "openai",
	// an injected provider is called and scriptedProvider is never injected.
	for _, task := range SuiteTasks {
		root := t.TempDir()
		spy := &fakeOpenAIProvider{}
		result, err := Run(ctx, Options{
			Task:      task,
			Config:    config.Config{DBPath: filepath.Join(root, "eval.db"), Provider: "openai"},
			Workspace: root,
			Provider:  spy,
		})
		if err != nil {
			t.Fatalf("task %s returned unexpected error: %v", task, err)
		}
		if spy.calls.Load() == 0 {
			t.Fatalf("task %s did not invoke the openai provider; scriptedProvider was injected", task)
		}
		// Since spy returns only text ("fake-openai-live-response") and no tool calls,
		// any task whose scriptedProvider normally emits tool calls must have 0 tool calls here.
		if len(result.ToolCalls) != 0 {
			t.Fatalf("task %s executed scripted tool calls under openai provider: %#v", task, result.ToolCalls)
		}
	}

	// 2. Verify the CLI path (Options.Provider == nil, Config.Provider == "openai") builds
	// the OpenAI provider and never injects scriptedProvider, while preserving harness-level
	// deterministic hooks (recover-mid-batch CrashOnTool + Recover, batch-approval-skip deny).
	t.Run("cli-nil-provider-and-harness-hooks", func(t *testing.T) {
		const apiKey = "sk-test-no-script"
		var explainHits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+apiKey {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			explainHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"cmpl_1","choices":[{"index":0,"message":{"role":"assistant","content":"live-from-httptest"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":4}}`)
		}))
		defer srv.Close()

		root := t.TempDir()
		res, err := Run(ctx, Options{
			Task: "dev-explain-call-site",
			Config: config.Config{
				DBPath:        filepath.Join(root, "eval.db"),
				Provider:      "openai",
				OpenAIBaseURL: srv.URL,
				OpenAIAPIKey:  apiKey,
				OpenAIModel:   "gpt-4.1-mini",
			},
			Workspace: root,
		})
		if err != nil {
			t.Fatal(err)
		}
		if explainHits.Load() != 1 {
			t.Fatalf("expected 1 call to openai httptest server when Options.Provider==nil, got %d", explainHits.Load())
		}
		if len(res.ToolCalls) != 0 || res.Pass {
			t.Fatalf("expected unscripted text response to have 0 tool calls and fail dev-explain-call-site scorer, got %+v", res)
		}

		// Verify recover-mid-batch harness hook (CrashOnTool + Recover) still works under Provider="openai".
		recoverRoot := t.TempDir()
		recoverProv := &fakeOpenAIProvider{
			step: func(_ int32, _ provider.Request) (domain.ModelResponse, error) {
				return toolCalls(
					call("c1", "read_file", `{"path":"alpha.txt"}`),
					call("c2", "write_file", `{"path":"out.txt","content":"x"}`),
					call("c3", "read_file", `{"path":"gamma.txt"}`),
				), nil
			},
		}
		recoverRes, err := Run(ctx, Options{
			Task:      "recover-mid-batch",
			Config:    config.Config{DBPath: filepath.Join(recoverRoot, "eval.db"), Provider: "openai"},
			Workspace: recoverRoot,
			Provider:  recoverProv,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !recoverRes.Pass || recoverRes.WaitReason != string(domain.WaitReconciliation) {
			t.Fatalf("recover-mid-batch hook did not fire under openai provider: %+v", recoverRes)
		}
	})
}

func TestEvalSuiteRerunnable(t *testing.T) {
	ctx := context.Background()

	ws := t.TempDir()
	suiteCfg := config.Config{
		DBPath:   filepath.Join(ws, "suite.db"),
		Provider: "mock",
	}
	for i := 1; i <= 2; i++ {
		report, err := Suite(ctx, suiteCfg, ws)
		if err != nil {
			t.Fatalf("Suite run %d returned error: %v", i, err)
		}
		if report.Tasks != 13 || report.Passed != 13 || len(report.Failures) != 0 {
			t.Fatalf("Suite run %d expected 13/13 pass, got tasks=%d passed=%d failures=%#v task_pass=%#v",
				i, report.Tasks, report.Passed, report.Failures, report.TaskPass)
		}
	}

	runDir := t.TempDir()
	runCfg := config.Config{
		DBPath:   filepath.Join(runDir, "eval.db"),
		Provider: "mock",
	}
	var prevSession string
	for i := 1; i <= 3; i++ {
		res, err := Run(ctx, Options{
			Task:      "retrieve-chinese-term",
			Config:    runCfg,
			Workspace: runDir,
		})
		if err != nil {
			t.Fatalf("retrieve-chinese-term run %d returned error: %v", i, err)
		}
		if !res.Pass || len(res.Failures) != 0 {
			t.Fatalf("retrieve-chinese-term run %d failed: %+v", i, res)
		}
		db, err := sqlite.Open(ctx, runCfg.DBPath)
		if err != nil {
			t.Fatal(err)
		}
		runRec, err := db.GetRun(ctx, res.RunID)
		if err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		for _, id := range []string{"note_search", "note_cloud"} {
			doc, err := db.GetHistory(ctx, id)
			if err != nil || doc.SessionID != runRec.SessionID {
				_ = db.Close()
				t.Fatalf("run %d doc %s session=%q, want %q (err=%v)", i, id, doc.SessionID, runRec.SessionID, err)
			}
		}
		curHits, err := db.SearchHistory(ctx, runRec.SessionID, "检索", 10)
		if err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		for _, h := range curHits {
			if h.SessionID != runRec.SessionID {
				_ = db.Close()
				t.Fatalf("run %d current session search polluted by session %s: %#v", i, h.SessionID, h)
			}
		}
		if prevSession != "" {
			prevHits, err := db.SearchHistory(ctx, prevSession, "检索", 10)
			if err != nil {
				_ = db.Close()
				t.Fatal(err)
			}
			for _, h := range prevHits {
				if h.SourceID == "note_search" || h.SourceID == "note_cloud" {
					_ = db.Close()
					t.Fatalf("run %d left seeded doc %s in previous session %s: %#v", i, h.SourceID, prevSession, h)
				}
			}
		}
		prevSession = runRec.SessionID
		_ = db.Close()
	}
	for i := 1; i <= 3; i++ {
		res, err := Run(ctx, Options{
			Task:      "retrieve-path-and-id",
			Config:    runCfg,
			Workspace: runDir,
		})
		if err != nil {
			t.Fatalf("retrieve-path-and-id run %d returned error: %v", i, err)
		}
		if !res.Pass || len(res.Failures) != 0 {
			t.Fatalf("retrieve-path-and-id run %d failed: %+v", i, res)
		}
	}
}

func TestEvalLiveModeRunCommandNotParked(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	stub := &fakeOpenAIProvider{
		step: func(n int32, _ provider.Request) (domain.ModelResponse, error) {
			switch n {
			case 1:
				return toolCalls(
					call("f1", "read_file", `{"path":"calc.go"}`),
					call("f2", "read_file", `{"path":"calc_test.go"}`),
				), nil
			case 2:
				return toolCalls(
					call("f3", "edit_file", `{"path":"calc.go","old_string":"return a - b","new_string":"return a + b"}`),
				), nil
			case 3:
				return toolCalls(
					call("f4", "run_command", `{"command":"go test ./..."}`),
				), nil
			default:
				return textResp("Fixed Add in calc.go and verified go test passes."), nil
			}
		},
	}

	res, err := Run(ctx, Options{
		Task:      "dev-fix-failing-test",
		Config:    config.Config{DBPath: filepath.Join(root, "eval.db"), Provider: "openai"},
		Workspace: root,
		Provider:  stub,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if res.Status != string(domain.RunCompleted) {
		t.Fatalf("expected COMPLETED status, got %q (wait_reason=%q, failures=%#v)", res.Status, res.WaitReason, res.Failures)
	}
	if !res.Pass || len(res.Failures) != 0 {
		t.Fatalf("expected Pass=true, got Pass=%v Failures=%#v ToolCalls=%#v", res.Pass, res.Failures, res.ToolCalls)
	}
	sawRunCommand := false
	for _, c := range res.ToolCalls {
		if c.Name == "run_command" {
			sawRunCommand = true
			if c.Status != string(domain.InvSucceeded) || !c.OK {
				t.Fatalf("expected run_command to succeed, got %+v", c)
			}
		}
	}
	if !sawRunCommand {
		t.Fatalf("expected run_command in tool calls, got %#v", res.ToolCalls)
	}
}

func TestEvalScriptedGrantsUnchanged(t *testing.T) {
	ctx := context.Background()

	// 1. In live mode (Provider="openai", non-scripted), file-safe-edit must still
	// deny writes to forbidden/config.conf with auth_denied and keep its restricted grants.
	liveRoot := t.TempDir()
	liveStub := &fakeOpenAIProvider{
		step: func(n int32, _ provider.Request) (domain.ModelResponse, error) {
			if n == 1 {
				return toolCalls(
					call("s1", "edit_file", `{"path":"allowed/config.conf","old_string":"mode=readonly","new_string":"mode=active"}`),
					call("s2", "write_file", `{"path":"forbidden/config.conf","content":"secret_mode=unlocked\n"}`),
				), nil
			}
			return textResp("Updated allowed/config.conf; forbidden/config.conf was denied with auth_denied."), nil
		},
	}
	liveRes, err := Run(ctx, Options{
		Task:      "file-safe-edit",
		Config:    config.Config{DBPath: filepath.Join(liveRoot, "eval.db"), Provider: "openai"},
		Workspace: liveRoot,
		Provider:  liveStub,
	})
	if err != nil {
		t.Fatalf("file-safe-edit live mode returned error: %v", err)
	}
	if liveRes.Status != string(domain.RunCompleted) || !liveRes.Pass {
		t.Fatalf("expected file-safe-edit to complete and pass in live mode, got status=%s pass=%v failures=%#v calls=%#v",
			liveRes.Status, liveRes.Pass, liveRes.Failures, liveRes.ToolCalls)
	}
	sawForbiddenAuthDenied := false
	for _, c := range liveRes.ToolCalls {
		if c.ProviderCallID == "s2" {
			if c.Status == string(domain.InvDenied) && !c.OK && c.ErrorCode == domain.CodeAuthDenied {
				sawForbiddenAuthDenied = true
			} else {
				t.Fatalf("expected forbidden write call s2 to be DENIED with auth_denied, got %+v", c)
			}
		}
	}
	if !sawForbiddenAuthDenied {
		t.Fatalf("did not observe denied forbidden write call in %#v", liveRes.ToolCalls)
	}
	forbiddenBytes, err := os.ReadFile(filepath.Join(liveRoot, "forbidden", "config.conf"))
	if err != nil || string(forbiddenBytes) != safeEditForbiddenInit {
		t.Fatalf("forbidden/config.conf was modified or unreadable: %q %v", forbiddenBytes, err)
	}

	// 2. In live mode, batch-approval-skip must also preserve its ungranted write_file denial & skip semantics.
	skipRoot := t.TempDir()
	skipStub := &fakeOpenAIProvider{
		step: func(n int32, _ provider.Request) (domain.ModelResponse, error) {
			if n == 1 {
				return toolCalls(
					call("c1", "read_file", `{"path":"alpha.txt"}`),
					call("c2", "write_file", `{"path":"out.txt","content":"nope"}`),
					call("c3", "read_file", `{"path":"beta.txt"}`),
				), nil
			}
			return textResp("paired"), nil
		},
	}
	skipRes, err := Run(ctx, Options{
		Task:      "batch-approval-skip",
		Config:    config.Config{DBPath: filepath.Join(skipRoot, "eval.db"), Provider: "openai"},
		Workspace: skipRoot,
		Provider:  skipStub,
	})
	if err != nil {
		t.Fatalf("batch-approval-skip live mode returned error: %v", err)
	}
	if !skipRes.Pass || skipRes.Status != string(domain.RunCompleted) {
		t.Fatalf("expected batch-approval-skip to pass in live mode, got %+v", skipRes)
	}

	// 3. Scripted mode grants and behavior remain 13/13 unchanged.
	scriptedRoot := t.TempDir()
	scriptedRes, err := Run(ctx, Options{
		Task:      "file-safe-edit",
		Config:    config.Config{DBPath: filepath.Join(scriptedRoot, "eval.db"), Provider: "mock"},
		Workspace: scriptedRoot,
	})
	if err != nil || !scriptedRes.Pass {
		t.Fatalf("scripted file-safe-edit failed: %+v %v", scriptedRes, err)
	}
}

func TestEvalSuiteOutAndCompare(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	report, err := Suite(ctx, config.Config{DBPath: filepath.Join(ws, "suite.db"), Provider: "mock"}, ws)
	if err != nil {
		t.Fatalf("Suite returned error: %v", err)
	}
	outPath := filepath.Join(t.TempDir(), "baseline.json")
	if err := WriteSuiteReport(outPath, report); err != nil {
		t.Fatalf("WriteSuiteReport failed: %v", err)
	}
	loaded, err := LoadSuiteReport(outPath)
	if err != nil {
		t.Fatalf("LoadSuiteReport failed: %v", err)
	}
	if loaded.Tasks != report.Tasks || loaded.Passed != report.Passed || loaded.CompletionRate != report.CompletionRate || len(loaded.TaskPass) != len(report.TaskPass) {
		t.Fatalf("loaded report mismatch: got %+v, want %+v", loaded, report)
	}
	for k, v := range report.TaskPass {
		if loaded.TaskPass[k] != v {
			t.Fatalf("task_pass[%q] = %v, want %v", k, loaded.TaskPass[k], v)
		}
	}

	// No regression (identical) -> HasRegression == false.
	noReg := CompareSuiteReports(loaded, report)
	if noReg.HasRegression || len(noReg.Regressed) != 0 || noReg.CompletionRateDelta != 0 {
		t.Fatalf("expected no regression, got %+v", noReg)
	}

	// Fix only -> HasRegression == false, Fixed contains task.
	baseWithFail := loaded
	baseWithFail.TaskPass = map[string]bool{}
	for k, v := range loaded.TaskPass {
		baseWithFail.TaskPass[k] = v
	}
	baseWithFail.TaskPass["dev-fix-failing-test"] = false
	baseWithFail.Passed = 12
	baseWithFail.CompletionRate = 12.0 / 13.0
	fixCmp := CompareSuiteReports(baseWithFail, report)
	if fixCmp.HasRegression || len(fixCmp.Fixed) != 1 || fixCmp.Fixed[0] != "dev-fix-failing-test" {
		t.Fatalf("expected only fixed dev-fix-failing-test, got %+v", fixCmp)
	}

	// Regression -> HasRegression == true and summary names regressed task.
	curWithReg := report
	curWithReg.TaskPass = map[string]bool{}
	for k, v := range report.TaskPass {
		curWithReg.TaskPass[k] = v
	}
	curWithReg.TaskPass["dev-fix-failing-test"] = false
	curWithReg.Passed = 12
	curWithReg.CompletionRate = 12.0 / 13.0
	regCmp := CompareSuiteReports(loaded, curWithReg)
	if !regCmp.HasRegression || len(regCmp.Regressed) != 1 || regCmp.Regressed[0] != "dev-fix-failing-test" {
		t.Fatalf("expected regression on dev-fix-failing-test, got %+v", regCmp)
	}
	if !strings.Contains(regCmp.Summary, "dev-fix-failing-test") {
		t.Fatalf("expected summary to name regressed task dev-fix-failing-test, got %q", regCmp.Summary)
	}
}
