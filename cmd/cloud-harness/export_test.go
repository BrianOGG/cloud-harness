package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/config"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/storage"
	"github.com/BrianOGG/cloud-harness/internal/storage/sqlite"
)

func TestExportRedactsAllRequiredPatterns(t *testing.T) {
	input := strings.Join([]string{
		"prefix sk-LongFakeToken0123456789ABCDEF suffix",
		"substitute hsurr:simulated-substitute-secret",
		"authorization header Authorization: Basic HeaderCredentialValue",
		`Bearer bearerCredentialValue`,
		`api_key=bareApiKeyValue apikey:bareApikeyValue token=bareTokenValue secret:bareSecretValue password=barePasswordValue`,
		`json {"api_key":"quotedApiKeyValue","Authorization":"Basic quotedAuthorizationValue"}`,
		"configured configuredCredentialValue",
	}, "\n")
	got := redactExportText(input, "configuredCredentialValue")
	for _, secret := range []string{
		"sk-LongFakeToken0123456789ABCDEF", "hsurr:simulated-substitute-secret",
		"HeaderCredentialValue", "bearerCredentialValue", "bareApiKeyValue", "bareApikeyValue",
		"bareTokenValue", "bareSecretValue", "barePasswordValue", "quotedApiKeyValue",
		"quotedAuthorizationValue", "configuredCredentialValue",
	} {
		if strings.Contains(got, secret) {
			t.Errorf("redacted text still contains %q: %s", secret, got)
		}
	}
	for _, context := range []string{"prefix ", " suffix", "substitute ", "authorization header Authorization:", "configured [redacted]"} {
		if !strings.Contains(got, context) {
			t.Errorf("redaction lost surrounding context %q: %s", context, got)
		}
	}

	jsonInput := []byte(`{"nested":{"token":"nested-token-value","message":"Bearer nestedBearerValue"},"content":"plain context"}`)
	jsonOutput, err := redactExportJSON(jsonInput, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(jsonOutput), "nested-token-value") || strings.Contains(string(jsonOutput), "nestedBearerValue") || !strings.Contains(string(jsonOutput), "plain context") {
		t.Fatalf("structured redaction output=%s", jsonOutput)
	}
}

func TestExportSanitizesReadOnlyAndEmitsMarkdownAndJSON(t *testing.T) {
	dbPath, runID, secrets := createExportFixture(t)
	setDiagnosticEnv(t, "", secrets.configured)
	before := readExportDatabaseFiles(t, dbPath)

	var markdown, stderr bytes.Buffer
	if code := Main([]string{"export", runID, "--db", dbPath}, &markdown, &stderr); code != 0 {
		t.Fatalf("export markdown code=%d stdout=%s stderr=%s", code, markdown.String(), stderr.String())
	}
	if !strings.Contains(markdown.String(), "# Run "+runID) || !strings.Contains(markdown.String(), "## Timeline") || !strings.Contains(markdown.String(), "tool.invocation") || !strings.Contains(markdown.String(), "approval") || !strings.Contains(markdown.String(), "input, 12 output") {
		t.Fatalf("markdown export is missing structure or usage: %s", markdown.String())
	}
	assertExportSecretsAbsent(t, markdown.String(), secrets.all()...)

	var structured, jsonErr bytes.Buffer
	if code := Main([]string{"export", runID, "--db", dbPath, "--json"}, &structured, &jsonErr); code != 0 {
		t.Fatalf("export json code=%d stderr=%s", code, jsonErr.String())
	}
	var report struct {
		Run struct {
			ID       string   `json:"id"`
			Status   string   `json:"status"`
			Provider []string `json:"providers"`
		} `json:"run"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			ModelCalls   int `json:"model_calls"`
		} `json:"usage"`
		Events []struct {
			Type string `json:"type"`
		} `json:"events"`
	}
	if err := json.Unmarshal(structured.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Run.ID != runID || report.Run.Status != string(domain.RunCompleted) || len(report.Run.Provider) != 1 || report.Run.Provider[0] != "openai" {
		t.Fatalf("json run metadata: %+v", report.Run)
	}
	if report.Usage.InputTokens != 101 || report.Usage.OutputTokens != 12 || report.Usage.ModelCalls != 1 {
		t.Fatalf("json usage totals: %+v", report.Usage)
	}
	seen := map[string]bool{}
	for _, event := range report.Events {
		seen[event.Type] = true
	}
	for _, typ := range []string{"message.user", "message.assistant", "tool.call", "tool.invocation", "tool.result", "approval", "event.provider.error"} {
		if !seen[typ] {
			t.Errorf("JSON timeline missing %q: %v", typ, seen)
		}
	}
	assertExportSecretsAbsent(t, structured.String(), secrets.all()...)
	if after := readExportDatabaseFiles(t, dbPath); !equalExportFiles(before, after) {
		t.Fatalf("export changed database files: before=%v after=%v", sortedFileKeys(before), sortedFileKeys(after))
	}
}

func TestExportReadsCommittedWALWithoutChangingSourceFiles(t *testing.T) {
	dbPath, runID, _ := createExportFixture(t)
	setDiagnosticEnv(t, "", "")
	db, err := sqlite.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendEvent(context.Background(), storage.Event{
		ID: "evt_export_wal", RunID: runID, Type: "wal.visible", Payload: json.RawMessage(`{"marker":"committed-wal-marker"}`),
		Persistent: true, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	before := readExportDatabaseFiles(t, dbPath)
	var out, stderr bytes.Buffer
	if code := Main([]string{"export", runID, "--db", dbPath, "--json"}, &out, &stderr); code != 0 {
		_ = db.Close()
		t.Fatalf("export with open WAL code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(out.String(), "committed-wal-marker") {
		_ = db.Close()
		t.Fatalf("export missed committed WAL contents: %s", out.String())
	}
	if after := readExportDatabaseFiles(t, dbPath); !equalExportFiles(before, after) {
		_ = db.Close()
		t.Fatalf("export changed live database files: before=%v after=%v", sortedFileKeys(before), sortedFileKeys(after))
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExportRawWarningUnknownRunAndOutForce(t *testing.T) {
	dbPath, runID, secrets := createExportFixture(t)
	setDiagnosticEnv(t, "", secrets.configured)
	dbBefore, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var dbOut, dbErr bytes.Buffer
	if code := Main([]string{"export", runID, "--db", dbPath, "--out", dbPath, "--force"}, &dbOut, &dbErr); code != 2 {
		t.Fatalf("database output path code=%d stderr=%s", code, dbErr.String())
	}
	dbAfter, err := os.ReadFile(dbPath)
	if err != nil || !bytes.Equal(dbBefore, dbAfter) {
		t.Fatalf("database output path changed DB bytes: err=%v", err)
	}

	var raw, rawErr bytes.Buffer
	if code := Main([]string{"export", runID, "--raw", "--json", "--db", dbPath}, &raw, &rawErr); code != 0 {
		t.Fatalf("raw export code=%d stderr=%s", code, rawErr.String())
	}
	if rawErr.String() != "exporting without redaction\n" {
		t.Fatalf("raw warning=%q", rawErr.String())
	}
	if !strings.Contains(raw.String(), secrets.user) || !strings.Contains(raw.String(), secrets.configured) || !strings.Contains(raw.String(), secrets.toolArgs) {
		t.Fatalf("raw export did not preserve source secrets: %s", raw.String())
	}

	var invalidOut, invalidErr bytes.Buffer
	if code := Main([]string{"export", runID, "--sanitize", "--raw", "--db", dbPath}, &invalidOut, &invalidErr); code != 2 || !strings.Contains(invalidErr.String(), "mutually exclusive") {
		t.Fatalf("conflicting flags code=%d stderr=%s", code, invalidErr.String())
	}

	var missingOut, missingErr bytes.Buffer
	if code := Main([]string{"export", "run_unknown_export", "--db", dbPath}, &missingOut, &missingErr); code != 1 || !strings.Contains(missingErr.String(), "not found") {
		t.Fatalf("unknown run code=%d stderr=%s", code, missingErr.String())
	}

	outPath := filepath.Join(t.TempDir(), "session.md")
	var out, errOut bytes.Buffer
	if code := Main([]string{"export", runID, "--db", dbPath, "--out", outPath}, &out, &errOut); code != 0 {
		t.Fatalf("file export code=%d stderr=%s", code, errOut.String())
	}
	if out.Len() != 0 {
		t.Fatalf("--out also wrote to stdout: %s", out.String())
	}
	initial, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if code := Main([]string{"export", runID, "--db", dbPath, "--out", outPath}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "already exists") {
		t.Fatalf("existing output code=%d stderr=%s", code, errOut.String())
	}
	kept, err := os.ReadFile(outPath)
	if err != nil || !bytes.Equal(kept, initial) {
		t.Fatalf("existing file changed after refused overwrite: err=%v", err)
	}
	errOut.Reset()
	if code := Main([]string{"export", runID, "--db", dbPath, "--out", outPath, "--force"}, &out, &errOut); code != 0 {
		t.Fatalf("forced output code=%d stderr=%s", code, errOut.String())
	}
	forced, err := os.ReadFile(outPath)
	if err != nil || !bytes.Contains(forced, []byte("# Run "+runID)) || bytes.Equal(forced, []byte("keep")) {
		t.Fatalf("forced output was not replaced: err=%v data=%q", err, forced)
	}
}

func TestM10MilestoneSanitizedExport(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	toolOutput := "token=m10-e2e-tool-result-fake-secret"
	if err := os.WriteFile(filepath.Join(workspace, toolOutput), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, "harness.db")
	mgr, db, err := openManager(ctx, config.Config{DBPath: dbPath, Provider: "mock"}, workspace)
	if err != nil {
		t.Fatal(err)
	}
	userSecret := "sk-M10E2EUserFakeToken0123456789"
	assistantSecret := "assistant_e2e_api_key_fake_value"
	argumentSecret := "m10-e2e-tool-result-fake-secret"
	mgr.SkipPlanner = true
	mgr.Provider = &scriptedProvider{step: func(call int, _ provider.Request) (domain.ModelResponse, error) {
		if call == 1 {
			return domain.ModelResponse{Provider: "mock", Complete: true, Items: []domain.ResponseItem{{
				Ordinal: 0, Kind: domain.ItemToolCall,
				ToolCall: &domain.ToolCall{ProviderCallID: "e2e_call", Name: "run_command", Arguments: json.RawMessage(`{"command":"ls ` + toolOutput + `"}`)},
			}}}, nil
		}
		return domain.ModelResponse{Provider: "mock", Complete: true, Items: []domain.ResponseItem{{
			Ordinal: 0, Kind: domain.ItemText, Text: "Answer contains api_key=" + assistantSecret,
		}}}, nil
	}}
	session, err := mgr.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := mgr.RunMessage(ctx, session.ID, "inspect this "+userSecret)
	if err != nil || run.Status != domain.RunCompleted {
		t.Fatalf("milestone run=%+v err=%v", run, err)
	}
	when := time.Now().UTC().Add(time.Second)
	errorPayload, _ := json.Marshal(map[string]string{"error": "provider error with secret=m10-e2e-provider-error-fake-value"})
	if _, err := db.AppendEvent(ctx, storage.Event{RunID: run.ID, Type: "provider.error", Payload: errorPayload, Persistent: true, CreatedAt: when}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveApproval(ctx, domain.Approval{ID: "apr_m10_export", RunID: run.ID, InvocationID: "e2e_call", Tool: "run_command", ResourceScope: "workspace", State: "approved", CreatedAt: when}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	setDiagnosticEnv(t, "", "")

	var out, stderr bytes.Buffer
	if code := Main([]string{"export", run.ID, "--sanitize", "--json", "--db", dbPath}, &out, &stderr); code != 0 {
		t.Fatalf("milestone export code=%d stderr=%s", code, stderr.String())
	}
	for _, secret := range []string{userSecret, assistantSecret, argumentSecret, "m10-e2e-provider-error-fake-value"} {
		if strings.Contains(out.String(), secret) {
			t.Errorf("milestone export leaked %q: %s", secret, out.String())
		}
	}
	for _, marker := range []string{run.ID, "COMPLETED", "message.user", "tool.invocation", "tool.result", "event.provider.error", "approval"} {
		if !strings.Contains(out.String(), marker) {
			t.Errorf("milestone export missing %q: %s", marker, out.String())
		}
	}
}

type exportSecrets struct {
	user, assistant, toolArgs, toolResult, providerError, configured string
	hsurr, bearer, authorization                                     string
}

func (s exportSecrets) all() []string {
	return []string{s.user, s.assistant, s.toolArgs, s.toolResult, s.providerError, s.configured, s.hsurr, s.bearer, s.authorization}
}

func createExportFixture(t *testing.T) (string, string, exportSecrets) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "export.db")
	db, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	secrets := exportSecrets{
		user:          "sk-ExportUserFakeToken0123456789",
		assistant:     "hsurr:export-assistant-substitute-secret",
		toolArgs:      "export-tool-argument-api-key-secret",
		toolResult:    "export-tool-result-password-secret",
		providerError: "export-provider-error-secret-value",
		configured:    "configured_openai_key_secret_value",
		bearer:        "exportBearerCredentialValue",
		authorization: "exportAuthorizationCredentialValue",
	}
	if err := db.CreateSession(ctx, storage.Session{ID: "ses_export", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTurn(ctx, storage.Turn{ID: "trn_export", SessionID: "ses_export", UserInput: "question " + secrets.user, Status: "completed", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	run := storage.Run{
		ID: "run_export", SessionID: "ses_export", TurnID: "trn_export", Status: domain.RunCompleted,
		Budget: domain.DefaultBudget(), Version: 2, FinalAnswer: "done", CreatedAt: now, UpdatedAt: now.Add(time.Minute),
		Error: domain.E(domain.CodeProviderHTTP, "provider returned secret="+secrets.providerError),
	}
	if err := db.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	args := json.RawMessage(`{"api_key":"` + secrets.toolArgs + `","token":"arg-token-value","password":"arg-password-value"}`)
	call := &domain.ToolCall{ProviderCallID: "export_call", Name: "write_file", Arguments: args, Ordinal: 1}
	toolData := json.RawMessage(`{"token":"` + secrets.toolResult + `","message":"Authorization: Basic ` + secrets.authorization + `"}`)
	errorResult := domain.E(domain.CodeToolError, "Bearer "+secrets.bearer)
	items := []domain.VisibleItem{
		{Ordinal: 0, Kind: domain.ItemText, Text: "assistant reply " + secrets.assistant + " configured=" + secrets.configured},
		{Ordinal: 1, Kind: domain.ItemToolCall, ToolCall: call, ProviderCallID: call.ProviderCallID},
		{Ordinal: 2, Kind: domain.ItemToolResult, ToolResult: &domain.ToolResultView{ProviderCallID: call.ProviderCallID, OK: false, Data: toolData, Error: errorResult}, ProviderCallID: call.ProviderCallID},
	}
	if err := db.SaveModelResponse(ctx, storage.StoredResponse{
		ID: "rsp_export", RunID: run.ID, Provider: "openai", Complete: true,
		Usage: domain.Usage{InputTokens: 101, OutputTokens: 12, CachedInputTokens: 20, LatencyMS: 44}, Items: items,
		CreatedAt: now.Add(10 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveBatch(ctx, storage.StoredBatch{ID: "bat_export", RunID: run.ID, ModelResponseID: "rsp_export", Status: domain.BatchComplete, Invocations: []domain.Invocation{{
		ID: "inv_export", Ordinal: 0, ProviderCallID: call.ProviderCallID, Name: call.Name, Arguments: args,
		Class: domain.ActionExecute, Status: domain.InvFailed, Dispatched: true,
		Result: &domain.CallResult{OK: false, Data: toolData, Error: errorResult},
	}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendEvent(ctx, storage.Event{
		ID: "evt_export", RunID: run.ID, Type: "provider.error", Payload: json.RawMessage(`{"authorization":"Bearer ` + secrets.authorization + `","detail":"request ended"}`),
		Persistent: true, CreatedAt: now.Add(20 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveApproval(ctx, domain.Approval{
		ID: "apr_export", RunID: run.ID, InvocationID: "inv_export", Tool: "write_file", ResourceScope: "secret-path-scope",
		State: "approved", CreatedAt: now.Add(30 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return dbPath, run.ID, secrets
}

func assertExportSecretsAbsent(t *testing.T, output string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if secret != "" && strings.Contains(output, secret) {
			t.Errorf("export leaked %q", secret)
		}
	}
}

func readExportDatabaseFiles(t *testing.T, dbPath string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(dbPath)
	files := make(map[string][]byte)
	for _, entry := range entries {
		if entry.IsDir() || (entry.Name() != base && !strings.HasPrefix(entry.Name(), base+"-")) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(filepath.Dir(dbPath), entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[entry.Name()] = data
	}
	return files
}

func equalExportFiles(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for name, content := range a {
		if !bytes.Equal(content, b[name]) {
			return false
		}
	}
	return true
}

func sortedFileKeys(files map[string][]byte) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
