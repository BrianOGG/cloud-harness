package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/provider"
	"github.com/cloud-harness/cloud-harness/internal/security"
	"github.com/cloud-harness/cloud-harness/internal/storage"
	"github.com/cloud-harness/cloud-harness/internal/storage/sqlite"
)

var auditEpoch = time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)

func newPolicyAuditDB(t *testing.T) *sqlite.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.db")
	db, err := sqlite.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, id := range []string{"session-a", "session-b"} {
		if err := db.CreateSession(t.Context(), storage.Session{ID: id, CreatedAt: auditEpoch, UpdatedAt: auditEpoch}); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func addPolicyAuditRun(t *testing.T, db *sqlite.DB, id, session string) {
	t.Helper()
	if err := db.CreateRun(t.Context(), storage.Run{
		ID: id, SessionID: session, Status: domain.RunCompleted, Budget: domain.DefaultBudget(),
		Version: 1, CreatedAt: auditEpoch, UpdatedAt: auditEpoch,
	}); err != nil {
		t.Fatal(err)
	}
}

func addPolicyAuditEvent(t *testing.T, db *sqlite.DB, run, id string, offset int, d security.PolicyDecision) {
	t.Helper()
	if _, err := db.AppendEvent(t.Context(), storage.Event{
		ID: id, RunID: run, InvocationID: id, Type: "policy.decided", Payload: must(t, d),
		CreatedAt: auditEpoch.Add(time.Duration(offset) * time.Second), Persistent: true,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestListPolicyDecisionsFilters(t *testing.T) {
	db := newPolicyAuditDB(t)
	addPolicyAuditRun(t, db, "run-a", "session-a")
	addPolicyAuditRun(t, db, "run-b", "session-b")
	addPolicyAuditRun(t, db, "run-empty", "session-b")
	addPolicyAuditEvent(t, db, "run-a", "allow", 1, security.PolicyDecision{Effect: security.EffectAllow, Source: "workspace", Tool: "write_file", Rule: "tool=write_file effect=allow path_glob=generated/**", RuleIndex: 1})
	addPolicyAuditEvent(t, db, "run-b", "deny", 2, security.PolicyDecision{Effect: security.EffectDeny, Source: "builtin", Tool: "read_file", Rule: "file read matches **/.env or **/.env.* (except .env.example)", RuleIndex: 1})
	addPolicyAuditEvent(t, db, "run-a", "ask", 3, security.PolicyDecision{Effect: security.EffectAsk, Source: "default", Tool: "run_command", Rule: "no matching policy rule; use existing permissions"})
	addPolicyAuditEvent(t, db, "run-b", "global-deny", 4, security.PolicyDecision{Effect: security.EffectDeny, Source: "global", Tool: "write_file", Rule: "tool=write_file effect=deny path_glob=private/**", RuleIndex: 2})
	if _, err := db.AppendEvent(t.Context(), storage.Event{ID: "unrelated", RunID: "run-a", Type: "tool.finished", Payload: json.RawMessage(`{"content":"secret"}`), CreatedAt: auditEpoch.Add(time.Hour), Persistent: true}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		q    PolicyDecisionQuery
		want []string
	}{
		{"all descending", PolicyDecisionQuery{}, []string{"global-deny", "ask", "deny", "allow"}},
		{"run", PolicyDecisionQuery{RunID: "run-a"}, []string{"ask", "allow"}},
		{"tool", PolicyDecisionQuery{Tool: "write_file"}, []string{"global-deny", "allow"}},
		{"allow", PolicyDecisionQuery{Effect: security.EffectAllow}, []string{"allow"}},
		{"ask", PolicyDecisionQuery{Effect: security.EffectAsk}, []string{"ask"}},
		{"deny", PolicyDecisionQuery{Effect: security.EffectDeny}, []string{"global-deny", "deny"}},
		{"since inclusive", PolicyDecisionQuery{Since: auditEpoch.Add(3 * time.Second)}, []string{"global-deny", "ask"}},
		{"combined before limit", PolicyDecisionQuery{RunID: "run-a", Tool: "write_file", Effect: security.EffectAllow, Since: auditEpoch.Add(time.Second), Limit: 1}, []string{"allow"}},
		{"no tool match", PolicyDecisionQuery{Tool: "missing"}, []string{}},
		{"future", PolicyDecisionQuery{Since: auditEpoch.Add(time.Hour)}, []string{}},
		{"empty run", PolicyDecisionQuery{RunID: "run-empty"}, []string{}},
		{"missing run", PolicyDecisionQuery{RunID: "missing"}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records, err := ListPolicyDecisions(t.Context(), db, tc.q)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(records))
			for _, record := range records {
				got = append(got, record.InvocationID)
			}
			if records == nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPolicyAuditEmptyLimitsAndAllRuns(t *testing.T) {
	isolatedPolicyConfig(t)
	db := newPolicyAuditDB(t)
	records, err := ListPolicyDecisions(t.Context(), db, PolicyDecisionQuery{})
	if err != nil || records == nil || len(records) != 0 {
		t.Fatalf("empty DB = %v, %v", records, err)
	}
	for i := 0; i < 550; i++ {
		runID := fmt.Sprintf("run-%03d", min(i, 204))
		if i <= 204 {
			addPolicyAuditRun(t, db, runID, "session-a")
		}
		addPolicyAuditEvent(t, db, runID, fmt.Sprintf("event-%03d", i), i, security.PolicyDecision{
			Tool: "read_file", Effect: security.EffectAsk, Source: "default", Rule: "no matching policy rule; use existing permissions",
		})
	}
	for _, tc := range []struct{ limit, want int }{{0, 50}, {1, 1}, {500, 500}, {999, 500}} {
		records, err := ListPolicyDecisions(t.Context(), db, PolicyDecisionQuery{Limit: tc.limit})
		if err != nil || len(records) != tc.want || records[0].InvocationID != "event-549" {
			t.Fatalf("limit %d: len=%d, err=%v", tc.limit, len(records), err)
		}
	}
	summary, err := SummarizePolicy(t.Context(), t.TempDir(), db)
	if err != nil || summary.Decisions != (PolicyDecisionCounts{Ask: 550}) {
		t.Fatalf("unbounded counts = %+v, %v", summary.Decisions, err)
	}
}

func TestPolicyAuditSummaryLayers(t *testing.T) {
	global := isolatedPolicyConfig(t)
	workspace := t.TempDir()
	for _, populated := range []bool{false, true} {
		if populated {
			putAgentPolicy(t, filepath.Join(workspace, ".cloud-harness", "policy.json"), `{"rules":[{"tool":"write_file","effect":"allow"},{"tool":"read_file","path_glob":"private/**","effect":"deny"}]}`)
			putAgentPolicy(t, global, `{"rules":[{"tool":"*","effect":"ask"}]}`)
		}
		summary, err := SummarizePolicy(t.Context(), workspace, nil)
		if err != nil || len(summary.Layers) != 3 || summary.Decisions != (PolicyDecisionCounts{}) {
			t.Fatalf("summary = %+v, %v", summary, err)
		}
		wantWorkspace, wantGlobal, wantTrust := 0, 0, security.TrustState("")
		if populated {
			wantWorkspace, wantGlobal, wantTrust = 2, 1, security.TrustTrusted
		}
		want := []PolicyLayerSummary{
			{Source: "workspace", File: filepath.Join(workspace, ".cloud-harness", "policy.json"), Loaded: populated, Rules: wantWorkspace, Trust: wantTrust},
			{Source: "global", File: global, Loaded: populated, Rules: wantGlobal},
			{Source: "builtin", File: "embedded", Loaded: true, Rules: 3},
		}
		if !reflect.DeepEqual(summary.Layers, want) {
			t.Fatalf("layers = %+v, want %+v", summary.Layers, want)
		}
	}
	putAgentPolicy(t, global, `{"rules":[]}`)
	summary, err := SummarizePolicy(t.Context(), workspace, nil)
	if err != nil || !summary.Layers[1].Loaded || summary.Layers[1].Rules != 0 {
		t.Fatalf("empty file was treated as missing: %+v, %v", summary, err)
	}
	putAgentPolicy(t, global, `{"rules":[{"tool":"*","effect":"private-policy-secret"}]}`)
	if _, err := SummarizePolicy(t.Context(), workspace, nil); err == nil || strings.Contains(err.Error(), "private-policy-secret") {
		t.Fatalf("unsafe policy load error: %v", err)
	}
}

func TestPolicyAuditSensitiveInvocations(t *testing.T) {
	isolatedPolicyConfig(t)
	const content = "sensitive-file-body-for-audit"
	const command = "printf sensitive-command-argument-for-audit"
	m, db, workspace := testManager(t, &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "write", Name: "write_file", Arguments: must(t, map[string]string{"path": "audit.txt", "content": content})}),
		toolResp(domain.ToolCall{ProviderCallID: "command", Name: "run_command", Arguments: must(t, map[string]string{"command": command})}),
		textOnly("done"),
	}})
	m.DisableSnapshots = true
	putAgentPolicy(t, filepath.Join(workspace, ".cloud-harness", "policy.json"), string(must(t, map[string]any{"rules": []security.Rule{
		{Tool: "write_file", PathGlob: "audit.txt", Effect: security.EffectAllow},
		{Tool: "run_command", CommandPrefix: command, Effect: security.EffectAllow},
	}})))
	session, err := m.CreateSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessage(t.Context(), session.ID, "perform operations")
	if err != nil || run.Status != domain.RunCompleted {
		t.Fatalf("run = %+v, %v", run, err)
	}
	rows, err := ListPolicyDecisions(t.Context(), db, PolicyDecisionQuery{})
	if err != nil || len(rows) != 2 {
		t.Fatalf("decisions = %+v, %v", rows, err)
	}
	output := string(must(t, rows))
	for _, secret := range []string{content, command, "sensitive-command-argument-for-audit", "command_prefix\"", "arguments", "content\""} {
		if strings.Contains(output, secret) {
			t.Fatalf("decision projection exposed %q", secret)
		}
	}
	if !strings.Contains(output, "path_glob=audit.txt") || !strings.Contains(output, "command_prefix_sha256=") {
		t.Fatalf("missing safe rule summaries: %s", output)
	}
}

func TestPolicyAuditValidationAndErrors(t *testing.T) {
	db := newPolicyAuditDB(t)
	for _, q := range []PolicyDecisionQuery{{Effect: "invalid"}, {Limit: -1}} {
		if _, err := ListPolicyDecisions(t.Context(), db, q); err == nil {
			t.Fatalf("invalid query accepted: %+v", q)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ListPolicyDecisions(ctx, db, PolicyDecisionQuery{RunID: "run"}); err == nil {
		t.Fatal("ignored cancelled context")
	}
	addPolicyAuditRun(t, db, "run-a", "session-a")
	if _, err := db.AppendEvent(t.Context(), storage.Event{
		ID: "bad", RunID: "run-a", Type: "policy.decided", Payload: json.RawMessage(`{"private-json-secret":`), CreatedAt: auditEpoch, Persistent: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ListPolicyDecisions(t.Context(), db, PolicyDecisionQuery{}); err == nil || strings.Contains(err.Error(), "private-json-secret") {
		t.Fatalf("unsafe event decode error: %v", err)
	}
}
