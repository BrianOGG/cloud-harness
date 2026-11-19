package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/agent"
	"github.com/cloud-harness/cloud-harness/internal/config"
	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/eval"
	"github.com/cloud-harness/cloud-harness/internal/provider"
	"github.com/cloud-harness/cloud-harness/internal/storage"
	"github.com/cloud-harness/cloud-harness/internal/storage/sqlite"
	"github.com/cloud-harness/cloud-harness/internal/tools"
)

func TestOpenManagerSnapshotConfig(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		cfg := config.Config{Provider: "mock", DBPath: filepath.Join(t.TempDir(), "h.db"), DisableSnapshots: disabled}
		mgr, db, err := openManager(context.Background(), cfg, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if mgr.DisableSnapshots != disabled {
			t.Fatalf("manager DisableSnapshots=%t, want %t", mgr.DisableSnapshots, disabled)
		}
	}
}

func TestVersionAndMockRun(t *testing.T) {
	var out, err bytes.Buffer
	if code := Main([]string{"version"}, &out, &err); code != 0 || !strings.Contains(out.String(), "0.5.0-m5") {
		t.Fatalf("version %d %s %s", code, out.String(), err.String())
	}
	out.Reset()
	db := t.TempDir() + "/h.db"
	code := Main([]string{"run", "--db", db, "--provider", "mock", "--message", "status"}, &out, &err)
	if code != 0 {
		t.Fatalf("run %d %s %s", code, out.String(), err.String())
	}
	if !strings.Contains(out.String(), "Mock answer: status") || !strings.Contains(out.String(), "COMPLETED") {
		t.Fatalf("output %s", out.String())
	}
}

func TestSkillCLIUsesWorkspaceLoader(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	path := filepath.Join(root, ".agents", "skills", "demo", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\nname: demo\ndescription: Demo\n---\nSkill body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	if code := Main([]string{"/skill", "demo", "--workspace", root}, &out, &errBuf); code != 0 || errBuf.Len() != 0 || out.String() != "---\nname: demo\ndescription: Demo\n---\nSkill body\n" {
		t.Fatalf("skill code=%d out=%q err=%q", code, out.String(), errBuf.String())
	}
	out.Reset()
	errBuf.Reset()
	if code := Main([]string{"skill", "missing", "--workspace", root}, &out, &errBuf); code != 1 || out.Len() != 0 || !strings.Contains(errBuf.String(), "not found") {
		t.Fatalf("missing skill code=%d out=%q err=%q", code, out.String(), errBuf.String())
	}
}

func TestRunPlanFlag(t *testing.T) {
	setDiagnosticEnv(t, "", "")
	var out, errBuf bytes.Buffer
	db := filepath.Join(t.TempDir(), "h.db")
	code := Main([]string{"run", "--plan", "--workspace", t.TempDir(), "--db", db, "--provider", "mock", "--message", "outline the change"}, &out, &errBuf)
	var view struct {
		Plan   bool             `json:"plan"`
		Status domain.RunStatus `json:"status"`
	}
	if err := json.Unmarshal(out.Bytes(), &view); code != 0 || err != nil || !view.Plan || view.Status != domain.RunCompleted {
		t.Fatalf("plan code=%d out=%q stderr=%q err=%v", code, out.String(), errBuf.String(), err)
	}
}

func TestResumeFlagOrder(t *testing.T) {
	var out, err bytes.Buffer
	db := t.TempDir() + "/h.db"
	code := Main([]string{"runs", "resume", "run_missing", "--decision", "deny", "--db", db}, &out, &err)
	if code != 2 || !strings.Contains(err.String(), "run id required") {
		t.Fatalf("id-first %d %s %s", code, out.String(), err.String())
	}
	out.Reset()
	err.Reset()
	code = Main([]string{"runs", "resume", "--decision", "deny", "--db", db, "run_missing"}, &out, &err)
	if code == 2 || strings.Contains(err.String(), "run id required") {
		t.Fatalf("flags-first %d %s %s", code, out.String(), err.String())
	}
}

func TestExternalBindRefused(t *testing.T) {
	setDiagnosticEnv(t, "", "")
	var out, err bytes.Buffer
	code := Main([]string{"serve", "--addr", "0.0.0.0:19421", "--db", t.TempDir() + "/h.db"}, &out, &err)
	if code != 1 || !strings.Contains(err.String(), "non-loopback listen requires an HTTP token") || !strings.Contains(err.String(), "--http-token") {
		t.Fatalf("serve %d %s %s", code, out.String(), err.String())
	}
	out.Reset()
	err.Reset()
	code = Main([]string{"serve", "--addr", "127.0.0.1:19421", "--http-token", "short", "--db", t.TempDir() + "/h.db"}, &out, &err)
	if code != 1 || !strings.Contains(err.String(), "16") || strings.Contains(err.String(), "short") {
		t.Fatalf("expected short token to fail: %d %s %s", code, out.String(), err.String())
	}
}

func TestRunsRecoverCLI(t *testing.T) {
	ctx := context.Background()
	dbPath := t.TempDir() + "/h.db"
	db, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-10 * time.Minute)
	if err := db.CreateSession(ctx, storage.Session{ID: "ses_1", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, runID := range []string{"run_exp_1", "run_exp_2"} {
		if err := db.CreateRun(ctx, storage.Run{
			ID: runID, SessionID: "ses_1", Status: domain.RunRunning,
			Budget: domain.DefaultBudget(), Version: 1, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		if err := db.SaveBatch(ctx, storage.StoredBatch{
			ID: "bat_" + runID, RunID: runID, Status: domain.BatchOpen, Action: domain.ActionExecute,
			Invocations: []domain.Invocation{{
				ID: "inv_" + runID, Ordinal: 0, ProviderCallID: "c1", Name: "write_file",
				Arguments: json.RawMessage(`{"path":"out.txt","content":"x"}`), Class: domain.ActionExecute,
				Status: domain.InvRunning, Dispatched: true,
			}},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.AcquireLease(ctx, runID, "dead-worker", now, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()

	var out, errBuf bytes.Buffer
	if code := Main([]string{"runs", "recover", "run_exp_1", "--db", dbPath}, &out, &errBuf); code != 0 {
		t.Fatalf("recover single %d %s %s", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), `"RECONCILIATION"`) {
		t.Fatalf("single recover output: %s", out.String())
	}

	out.Reset()
	errBuf.Reset()
	if code := Main([]string{"runs", "recover", "--all-expired", "--db", dbPath}, &out, &errBuf); code != 0 {
		t.Fatalf("recover all-expired %d %s %s", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "run_exp_2") || !strings.Contains(out.String(), `"RECONCILIATION"`) {
		t.Fatalf("all-expired recover output: %s", out.String())
	}
}

type scriptedProvider struct {
	step func(call int, req provider.Request) (domain.ModelResponse, error)
	call int
}

func (s *scriptedProvider) Name() string { return "scripted" }

func (s *scriptedProvider) Complete(_ context.Context, req provider.Request) (domain.ModelResponse, error) {
	s.call++
	return s.step(s.call, req)
}

func newChatTestManager(t *testing.T, prov provider.Provider) (*agent.Manager, *sqlite.DB, string) {
	t.Helper()
	ctx := context.Background()
	ws := t.TempDir()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "chat.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	broker := tools.NewBroker(db, db)
	if err := tools.RegisterLocal(broker, ws); err != nil {
		t.Fatal(err)
	}
	return &agent.Manager{Store: db, Provider: prov, Broker: broker, Workspace: ws, OwnerID: "chat-cli"}, db, ws
}

func TestChatMultiTurnAskUser(t *testing.T) {
	ctx := context.Background()
	prov := &scriptedProvider{
		step: func(call int, req provider.Request) (domain.ModelResponse, error) {
			if call == 1 {
				return domain.ModelResponse{
					Provider: "mock", Complete: true,
					Items: []domain.ResponseItem{{
						Ordinal: 0, Kind: domain.ItemToolCall,
						ToolCall: &domain.ToolCall{
							ProviderCallID: "ask_1",
							Name:           domain.ToolAskUser,
							Arguments:      json.RawMessage(`{"question":"Which environment should I deploy to?"}`),
						},
					}},
				}, nil
			}
			supplied := ""
			for _, m := range req.Messages {
				if m.Role == "user" && strings.HasPrefix(m.Content, "User reply: ") {
					supplied = strings.TrimPrefix(m.Content, "User reply: ")
				}
			}
			return domain.ModelResponse{
				Provider: "mock", Complete: true,
				Items: []domain.ResponseItem{{
					Ordinal: 0, Kind: domain.ItemText,
					Text: "Final deployment target: " + supplied,
				}},
			}, nil
		},
	}
	mgr, db, _ := newChatTestManager(t, prov)

	var out, errBuf bytes.Buffer
	code := runChat(ctx, mgr, "ses_chat_ask", "deploy the service", strings.NewReader("staging-eu\n"), &out, &errBuf)
	if code != 0 {
		t.Fatalf("chat exited %d: out=%s err=%s", code, out.String(), errBuf.String())
	}
	output := out.String()
	if !strings.Contains(output, "Which environment should I deploy to?") {
		t.Fatalf("expected pending question in stdout, got:\n%s", output)
	}
	if !strings.Contains(output, "Final deployment target: staging-eu") {
		t.Fatalf("expected final answer reflecting supplied answer in stdout, got:\n%s", output)
	}
	runs, err := db.ListRuns(ctx, "ses_chat_ask")
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs: %+v %v", runs, err)
	}
	if runs[0].Status != domain.RunCompleted || runs[0].FinalAnswer != "Final deployment target: staging-eu" {
		t.Fatalf("unexpected run state: %+v", runs[0])
	}

	// Verify stdin EOF while waiting exits gracefully without cancelling the run.
	eofProv := &provider.Mock{Responses: []domain.ModelResponse{{
		Provider: "mock", Complete: true,
		Items: []domain.ResponseItem{{
			Ordinal: 0, Kind: domain.ItemToolCall,
			ToolCall: &domain.ToolCall{
				ProviderCallID: "ask_eof",
				Name:           domain.ToolAskUser,
				Arguments:      json.RawMessage(`{"question":"Still there?"}`),
			},
		}},
	}}}
	eofMgr, eofDB, _ := newChatTestManager(t, eofProv)
	out.Reset()
	errBuf.Reset()
	code = runChat(ctx, eofMgr, "ses_eof", "start and disconnect", strings.NewReader(""), &out, &errBuf)
	if code != 0 {
		t.Fatalf("chat EOF exited %d: out=%s err=%s", code, out.String(), errBuf.String())
	}
	eofRuns, err := eofDB.ListRuns(ctx, "ses_eof")
	if err != nil || len(eofRuns) != 1 {
		t.Fatalf("eof runs: %+v %v", eofRuns, err)
	}
	if eofRuns[0].Status != domain.RunWaiting || eofRuns[0].WaitReason != domain.WaitUser {
		t.Fatalf("expected run to remain WAITING(USER) on stdin EOF, got %+v", eofRuns[0])
	}
}

func TestChatApprovalPromptApproveAndDeny(t *testing.T) {
	ctx := context.Background()

	t.Run("approve", func(t *testing.T) {
		mock := &provider.Mock{Responses: []domain.ModelResponse{
			{
				Provider: "mock", Complete: true,
				Items: []domain.ResponseItem{{
					Ordinal: 0, Kind: domain.ItemToolCall,
					ToolCall: &domain.ToolCall{
						ProviderCallID: "w_approve",
						Name:           "write_file",
						Arguments:      json.RawMessage(`{"path":"approved.txt","content":"approved-side-effect"}`),
					},
				}},
			},
			{
				Provider: "mock", Complete: true,
				Items: []domain.ResponseItem{{Ordinal: 0, Kind: domain.ItemText, Text: "approved write completed"}},
			},
		}}
		mgr, db, ws := newChatTestManager(t, mock)
		var out, errBuf bytes.Buffer
		code := runChat(ctx, mgr, "ses_approve", "write approved.txt", strings.NewReader("Approve\n"), &out, &errBuf)
		if code != 0 {
			t.Fatalf("approve chat exited %d: out=%s err=%s", code, out.String(), errBuf.String())
		}
		output := out.String()
		if !strings.Contains(output, "write_file") || !strings.Contains(output, "approved.txt") {
			t.Fatalf("expected approval prompt with tool name, args summary, and resource in stdout, got:\n%s", output)
		}
		if !strings.Contains(output, "approved write completed") {
			t.Fatalf("expected final answer in stdout, got:\n%s", output)
		}
		data, err := os.ReadFile(filepath.Join(ws, "approved.txt"))
		if err != nil || string(data) != "approved-side-effect" {
			t.Fatalf("expected side effect on disk, got %q %v", data, err)
		}
		runs, err := db.ListRuns(ctx, "ses_approve")
		if err != nil || len(runs) != 1 || runs[0].Status != domain.RunCompleted {
			t.Fatalf("runs: %+v %v", runs, err)
		}
	})

	t.Run("deny", func(t *testing.T) {
		mock := &provider.Mock{Responses: []domain.ModelResponse{
			{
				Provider: "mock", Complete: true,
				Items: []domain.ResponseItem{{
					Ordinal: 0, Kind: domain.ItemToolCall,
					ToolCall: &domain.ToolCall{
						ProviderCallID: "w_deny",
						Name:           "write_file",
						Arguments:      json.RawMessage(`{"path":"denied.txt","content":"must-not-write"}`),
					},
				}},
			},
			{
				Provider: "mock", Complete: true,
				Items: []domain.ResponseItem{{Ordinal: 0, Kind: domain.ItemText, Text: "write was denied"}},
			},
		}}
		mgr, db, ws := newChatTestManager(t, mock)
		var out, errBuf bytes.Buffer
		code := runChat(ctx, mgr, "ses_deny", "write denied.txt", strings.NewReader("no\n"), &out, &errBuf)
		if code != 0 {
			t.Fatalf("deny chat exited %d: out=%s err=%s", code, out.String(), errBuf.String())
		}
		output := out.String()
		if !strings.Contains(output, "write_file") || !strings.Contains(output, "denied.txt") {
			t.Fatalf("expected approval prompt in stdout, got:\n%s", output)
		}
		if _, err := os.Stat(filepath.Join(ws, "denied.txt")); !os.IsNotExist(err) {
			t.Fatalf("denied write_file must not create file, err=%v", err)
		}
		runs, err := db.ListRuns(ctx, "ses_deny")
		if err != nil || len(runs) != 1 {
			t.Fatalf("runs: %+v %v", runs, err)
		}
		batches, err := db.LoadBatches(ctx, runs[0].ID)
		if err != nil || len(batches) != 1 || len(batches[0].Invocations) != 1 {
			t.Fatalf("batches: %+v %v", batches, err)
		}
		inv := batches[0].Invocations[0]
		if inv.Status != domain.InvDenied || inv.Dispatched {
			t.Fatalf("expected DENIED invocation without dispatch, got %+v", inv)
		}
	})
}

func TestEvalSuiteOutAndCompare(t *testing.T) {
	tmp := t.TempDir()
	baselinePath := filepath.Join(tmp, "baseline.json")
	ws1 := filepath.Join(tmp, "ws1")
	db1 := filepath.Join(tmp, "suite1.db")

	// 1. --out writes indented SuiteReport JSON with task_pass and reads back identically.
	var out, errBuf bytes.Buffer
	code := Main([]string{"eval", "suite", "--db", db1, "--workspace", ws1, "--out", baselinePath}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("eval suite --out exited %d: out=%s err=%s", code, out.String(), errBuf.String())
	}
	rawFile, err := os.ReadFile(baselinePath)
	if err != nil {
		t.Fatalf("read baseline file: %v", err)
	}
	if !strings.Contains(string(rawFile), "\n  \"task_pass\": {") {
		t.Fatalf("expected indented JSON with task_pass in %s, got:\n%s", baselinePath, string(rawFile))
	}
	var stdoutReport, fileReport eval.SuiteReport
	if err := json.Unmarshal(out.Bytes(), &stdoutReport); err != nil {
		t.Fatalf("unmarshal stdout report: %v", err)
	}
	if err := json.Unmarshal(rawFile, &fileReport); err != nil {
		t.Fatalf("unmarshal file report: %v", err)
	}
	if fileReport.Tasks != 13 || fileReport.Passed != 13 || fileReport.CompletionRate != 1.0 || len(fileReport.TaskPass) != 13 {
		t.Fatalf("unexpected file report: %+v", fileReport)
	}
	if stdoutReport.Tasks != fileReport.Tasks || stdoutReport.Passed != fileReport.Passed || stdoutReport.CompletionRate != fileReport.CompletionRate {
		t.Fatalf("stdout vs file mismatch: stdout=%+v file=%+v", stdoutReport, fileReport)
	}
	for k, v := range stdoutReport.TaskPass {
		if fileReport.TaskPass[k] != v || !v {
			t.Fatalf("task_pass[%q]: file=%v stdout=%v", k, fileReport.TaskPass[k], v)
		}
	}

	// 2. --compare with no regressions exits 0.
	out.Reset()
	errBuf.Reset()
	ws2 := filepath.Join(tmp, "ws2")
	db2 := filepath.Join(tmp, "suite2.db")
	code = Main([]string{"eval", "suite", "--db", db2, "--workspace", ws2, "--compare", baselinePath}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("eval suite --compare (no regression) exited %d: out=%s err=%s", code, out.String(), errBuf.String())
	}
	var cmpReport eval.SuiteReport
	if err := json.Unmarshal(out.Bytes(), &cmpReport); err != nil {
		t.Fatalf("unmarshal compare report: %v", err)
	}
	if cmpReport.Comparison == nil || cmpReport.Comparison.HasRegression || len(cmpReport.Comparison.Regressed) != 0 {
		t.Fatalf("expected non-nil comparison with 0 regressions, got %+v", cmpReport.Comparison)
	}

	// 3. --compare with only fixes (plus added/removed) and combined --out exits 0 and writes new baseline.
	fixBase := fileReport
	fixBase.TaskPass = map[string]bool{}
	for k, v := range fileReport.TaskPass {
		fixBase.TaskPass[k] = v
	}
	fixBase.TaskPass["dev-fix-failing-test"] = false
	delete(fixBase.TaskPass, "cache-baseline")
	fixBase.TaskPass["legacy-removed-task"] = false
	fixBase.Passed = 11
	fixBase.Tasks = 13
	fixBase.CompletionRate = 11.0 / 13.0
	fixBasePath := filepath.Join(tmp, "fix-base.json")
	if err := eval.WriteSuiteReport(fixBasePath, fixBase); err != nil {
		t.Fatal(err)
	}
	newBasePath := filepath.Join(tmp, "new-base.json")
	out.Reset()
	errBuf.Reset()
	ws3 := filepath.Join(tmp, "ws3")
	db3 := filepath.Join(tmp, "suite3.db")
	code = Main([]string{"eval", "suite", "--db", db3, "--workspace", ws3, "--compare", fixBasePath, "--out", newBasePath}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("eval suite --compare (fix only) --out exited %d: out=%s err=%s", code, out.String(), errBuf.String())
	}
	var fixOutReport eval.SuiteReport
	if err := json.Unmarshal(out.Bytes(), &fixOutReport); err != nil {
		t.Fatal(err)
	}
	if fixOutReport.Comparison == nil || fixOutReport.Comparison.HasRegression {
		t.Fatalf("expected no regression on fix-only compare, got %+v", fixOutReport.Comparison)
	}
	if len(fixOutReport.Comparison.Fixed) != 1 || fixOutReport.Comparison.Fixed[0] != "dev-fix-failing-test" {
		t.Fatalf("expected fixed=[dev-fix-failing-test], got %+v", fixOutReport.Comparison.Fixed)
	}
	if len(fixOutReport.Comparison.Added) != 1 || fixOutReport.Comparison.Added[0] != "cache-baseline" {
		t.Fatalf("expected added=[cache-baseline], got %+v", fixOutReport.Comparison.Added)
	}
	if len(fixOutReport.Comparison.Removed) != 1 || fixOutReport.Comparison.Removed[0] != "legacy-removed-task" {
		t.Fatalf("expected removed=[legacy-removed-task], got %+v", fixOutReport.Comparison.Removed)
	}
	reloadedNewBase, err := eval.LoadSuiteReport(newBasePath)
	if err != nil {
		t.Fatal(err)
	}
	if reloadedNewBase.Passed != 13 || !reloadedNewBase.TaskPass["dev-fix-failing-test"] {
		t.Fatalf("expected new baseline to record 13/13 pass, got %+v", reloadedNewBase)
	}

	// 4. --compare with regression (baseline pass -> current fail) exits non-zero and names the regressed task.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"cmpl_reg","choices":[{"index":0,"message":{"role":"assistant","content":"noop"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	}))
	defer srv.Close()
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	t.Setenv("OPENAI_API_KEY", "sk-test-regression")

	regBase := eval.SuiteReport{
		Tasks:          13,
		Passed:         1,
		CompletionRate: 1.0 / 13.0,
		Correctness:    1.0 / 13.0,
		TaskPass:       map[string]bool{},
	}
	for _, task := range eval.SuiteTasks {
		regBase.TaskPass[task] = false
	}
	regBase.TaskPass["dev-fix-failing-test"] = true
	regBasePath := filepath.Join(tmp, "reg-base.json")
	if err := eval.WriteSuiteReport(regBasePath, regBase); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errBuf.Reset()
	ws4 := filepath.Join(tmp, "ws4")
	db4 := filepath.Join(tmp, "suite4.db")
	code = Main([]string{"eval", "suite", "--provider", "openai", "--db", db4, "--workspace", ws4, "--compare", regBasePath}, &out, &errBuf)
	if code == 0 {
		t.Fatalf("expected non-zero exit code on regression, got 0 (out=%s err=%s)", out.String(), errBuf.String())
	}
	var regOutReport eval.SuiteReport
	if err := json.Unmarshal(out.Bytes(), &regOutReport); err != nil {
		t.Fatalf("unmarshal regression stdout: %v (raw=%s)", err, out.String())
	}
	if regOutReport.Comparison == nil || !regOutReport.Comparison.HasRegression {
		t.Fatalf("expected HasRegression=true, got %+v", regOutReport.Comparison)
	}
	if len(regOutReport.Comparison.Regressed) != 1 || regOutReport.Comparison.Regressed[0] != "dev-fix-failing-test" {
		t.Fatalf("expected regressed=[dev-fix-failing-test], got %+v", regOutReport.Comparison.Regressed)
	}
	if !strings.Contains(errBuf.String(), "dev-fix-failing-test") || !strings.Contains(out.String(), "dev-fix-failing-test") {
		t.Fatalf("expected diff summary to name regressed task dev-fix-failing-test, stdout=%s stderr=%s", out.String(), errBuf.String())
	}
}

func TestRetainPruneRunsDays(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "retain_test.db")

	populateDB := func() {
		db, err := sqlite.Open(ctx, dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()

		now := time.Now().UTC()
		tOld := now.AddDate(0, 0, -30)
		tNew := now.AddDate(0, 0, -1)

		if err := db.CreateSession(ctx, storage.Session{
			ID: "ses_retain", CreatedAt: tOld, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}

		type runSpec struct {
			id         string
			status     domain.RunStatus
			waitReason domain.WaitReason
			t          time.Time
			respID     string
		}

		specs := []runSpec{
			{id: "run_overdue_done", status: domain.RunCompleted, waitReason: domain.WaitNone, t: tOld, respID: "rsp_overdue_done"},
			{id: "run_recent_done", status: domain.RunCompleted, waitReason: domain.WaitNone, t: tNew, respID: "rsp_recent_done"},
			{id: "run_overdue_waiting", status: domain.RunWaiting, waitReason: domain.WaitUser, t: tOld, respID: "rsp_overdue_waiting"},
			{id: "run_active", status: domain.RunRunning, waitReason: domain.WaitNone, t: now, respID: "rsp_active"},
		}

		for _, s := range specs {
			if err := db.CreateRun(ctx, storage.Run{
				ID: s.id, SessionID: "ses_retain", Status: s.status, WaitReason: s.waitReason,
				Budget: domain.DefaultBudget(), Version: 1, CreatedAt: s.t, UpdatedAt: s.t,
			}); err != nil {
				t.Fatalf("CreateRun %s: %v", s.id, err)
			}
			if err := db.SaveCheckpoint(ctx, s.id, 1, []byte(`{"snapshot":"ok"}`), s.t); err != nil {
				t.Fatalf("SaveCheckpoint %s: %v", s.id, err)
			}
			if _, err := db.AppendEvent(ctx, storage.Event{
				ID: "ev_" + s.id, RunID: s.id, Seq: 1, Type: "step",
				Payload: json.RawMessage(`{}`), CreatedAt: s.t, Persistent: true,
			}); err != nil {
				t.Fatalf("AppendEvent %s: %v", s.id, err)
			}
			if err := db.SaveModelResponse(ctx, storage.StoredResponse{
				ID: s.respID, RunID: s.id, Provider: "mock", Complete: true,
				Opaque: []byte("continuation_payload_" + s.id), CreatedAt: s.t,
				Items: []domain.VisibleItem{{Ordinal: 0, Kind: "text", Text: "hello"}},
			}); err != nil {
				t.Fatalf("SaveModelResponse %s: %v", s.id, err)
			}
		}
	}

	populateDB()

	// 1. Assert retain without --prune-runs-days preserves existing behavior (no runs deleted).
	var out, errBuf bytes.Buffer
	code := Main([]string{"retain", "--db", dbPath}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("retain without prune-runs-days exited %d: out=%s err=%s", code, out.String(), errBuf.String())
	}
	var noPruneRes map[string]any
	if err := json.Unmarshal(out.Bytes(), &noPruneRes); err != nil {
		t.Fatalf("unmarshal retain output: %v", err)
	}
	if pruned, ok := noPruneRes["pruned"].(float64); !ok || pruned != 0 {
		t.Fatalf("expected pruned=0, got %+v", noPruneRes)
	}

	// Verify all 4 runs are still intact
	checkDB, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"run_overdue_done", "run_recent_done", "run_overdue_waiting", "run_active"} {
		if _, err := checkDB.GetRun(ctx, id); err != nil {
			t.Fatalf("run %s should still exist before prune, got %v", id, err)
		}
	}
	_ = checkDB.Close()

	// 2. Run with --prune-runs-days 7
	out.Reset()
	errBuf.Reset()
	code = Main([]string{"retain", "--prune-runs-days", "7", "--db", dbPath}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("retain --prune-runs-days 7 exited %d: out=%s err=%s", code, out.String(), errBuf.String())
	}

	var pruneRes struct {
		PrunedRuns     int            `json:"pruned_runs"`
		DeletedRuns    int            `json:"deleted_runs"`
		PruneRunsDays  int            `json:"prune_runs_days"`
		DeletedDetails map[string]int `json:"deleted_details"`
		WALCheckpoint  string         `json:"wal_checkpoint"`
	}
	if err := json.Unmarshal(out.Bytes(), &pruneRes); err != nil {
		t.Fatalf("unmarshal prune result: %v (raw: %s)", err, out.String())
	}
	if pruneRes.PrunedRuns != 1 || pruneRes.DeletedRuns != 1 {
		t.Fatalf("expected 1 pruned run, got pruned_runs=%d deleted_runs=%d", pruneRes.PrunedRuns, pruneRes.DeletedRuns)
	}
	if pruneRes.DeletedDetails["checkpoints"] < 1 {
		t.Fatalf("expected deleted checkpoints >= 1, got %v", pruneRes.DeletedDetails)
	}
	if pruneRes.DeletedDetails["events"] < 1 {
		t.Fatalf("expected deleted events >= 1, got %v", pruneRes.DeletedDetails)
	}
	if pruneRes.DeletedDetails["provider_continuations"] < 1 {
		t.Fatalf("expected deleted provider_continuations >= 1, got %v", pruneRes.DeletedDetails)
	}

	// 3. Inspect database state post-prune
	postDB, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer postDB.Close()

	// Overdue terminal run and all associated rows must be deleted
	if _, err := postDB.GetRun(ctx, "run_overdue_done"); err == nil {
		t.Fatal("expected run_overdue_done to be deleted")
	}
	if _, err := postDB.LatestCheckpoint(ctx, "run_overdue_done"); err == nil {
		t.Fatal("expected checkpoints for run_overdue_done to be deleted")
	}
	if evs, err := postDB.ListEvents(ctx, "run_overdue_done", 0); err != nil || len(evs) != 0 {
		t.Fatalf("expected events for run_overdue_done to be deleted, got %d events, err=%v", len(evs), err)
	}
	if _, err := postDB.LoadContinuation(ctx, "rsp_overdue_done"); err == nil {
		t.Fatal("expected continuation for run_overdue_done to be deleted")
	}

	// The other three runs and their associated details must remain intact
	survivors := []struct {
		id         string
		wantStatus domain.RunStatus
		respID     string
	}{
		{id: "run_recent_done", wantStatus: domain.RunCompleted, respID: "rsp_recent_done"},
		{id: "run_overdue_waiting", wantStatus: domain.RunWaiting, respID: "rsp_overdue_waiting"},
		{id: "run_active", wantStatus: domain.RunRunning, respID: "rsp_active"},
	}

	for _, s := range survivors {
		r, err := postDB.GetRun(ctx, s.id)
		if err != nil {
			t.Fatalf("expected %s to survive, got error: %v", s.id, err)
		}
		if r.Status != s.wantStatus {
			t.Fatalf("expected %s status %s, got %s", s.id, s.wantStatus, r.Status)
		}
		if _, err := postDB.LatestCheckpoint(ctx, s.id); err != nil {
			t.Fatalf("expected checkpoint for %s to survive, got %v", s.id, err)
		}
		evs, err := postDB.ListEvents(ctx, s.id, 0)
		if err != nil || len(evs) != 1 {
			t.Fatalf("expected 1 event for %s to survive, got %d, err=%v", s.id, len(evs), err)
		}
		if cont, err := postDB.LoadContinuation(ctx, s.respID); err != nil || len(cont) == 0 {
			t.Fatalf("expected continuation %s for %s to survive, got err=%v", s.respID, s.id, err)
		}
	}

	// 4. Assert PRAGMA foreign_key_check reports zero errors
	violations, err := postDB.ForeignKeyCheck(ctx)
	if err != nil {
		t.Fatalf("foreign_key_check error: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("expected 0 foreign key violations, got: %v", violations)
	}
	_ = postDB.Close()

	// 5. Assert retain without --prune-runs-days behaves as before and deletes no runs
	out.Reset()
	errBuf.Reset()
	code = Main([]string{"retain", "--db", dbPath}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("subsequent retain without prune-runs-days exited %d: out=%s err=%s", code, out.String(), errBuf.String())
	}
	var afterRetain map[string]any
	if err := json.Unmarshal(out.Bytes(), &afterRetain); err != nil {
		t.Fatalf("unmarshal retain output: %v", err)
	}
	if pruned, ok := afterRetain["pruned"].(float64); !ok || pruned != 0 {
		t.Fatalf("expected pruned=0, got %+v", afterRetain)
	}
	finalDB, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer finalDB.Close()
	for _, s := range survivors {
		if _, err := finalDB.GetRun(ctx, s.id); err != nil {
			t.Fatalf("run %s must remain after retain without prune-runs-days: %v", s.id, err)
		}
	}
}
