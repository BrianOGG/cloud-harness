package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/agent"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/security"
	"github.com/BrianOGG/cloud-harness/internal/storage"
	"github.com/BrianOGG/cloud-harness/internal/storage/sqlite"
)

func policyCLIDatabase(t *testing.T) (string, string, *sqlite.DB) {
	t.Helper()
	setDiagnosticEnv(t, "", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	workspace := t.TempDir()
	path := filepath.Join(workspace, "audit.db")
	db, err := sqlite.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	if err := db.CreateSession(t.Context(), storage.Session{ID: "session", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"run-a", "run-b"} {
		if err := db.CreateRun(t.Context(), storage.Run{
			ID: id, SessionID: "session", Status: domain.RunCompleted, Budget: domain.DefaultBudget(), Version: 1, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return workspace, path, db
}

func appendCLIPolicyDecision(t *testing.T, db *sqlite.DB, id, run string, offset int, d security.PolicyDecision) {
	t.Helper()
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendEvent(t.Context(), storage.Event{
		ID: id, RunID: run, InvocationID: "inv-" + id, Type: "policy.decided", Payload: raw, Persistent: true,
		CreatedAt: time.Date(2026, 11, 19, 0, 0, offset, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
}

func invokePolicyDecisions(t *testing.T, dbPath string, args ...string) []agent.PolicyDecisionRecord {
	t.Helper()
	var stdout, stderr bytes.Buffer
	all := append([]string{"policy", "decisions", "--db", dbPath, "--json"}, args...)
	if code := Main(all, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("policy exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var rows []agent.PolicyDecisionRecord
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if rows == nil {
		t.Fatalf("expected an array, got %s", stdout.String())
	}
	return rows
}

func TestPolicyDecisionsCLI(t *testing.T) {
	_, path, db := policyCLIDatabase(t)
	appendCLIPolicyDecision(t, db, "allow", "run-a", 1, security.PolicyDecision{Effect: security.EffectAllow, Source: "workspace", Tool: "write_file", Rule: "tool=write_file effect=allow path_glob=generated/**", RuleIndex: 1})
	appendCLIPolicyDecision(t, db, "deny", "run-b", 2, security.PolicyDecision{Effect: security.EffectDeny, Source: "builtin", Tool: "read_file", Rule: "file read matches **/.env or **/.env.* (except .env.example)"})
	appendCLIPolicyDecision(t, db, "ask", "run-a", 3, security.PolicyDecision{Effect: security.EffectAsk, Source: "default", Tool: "run_command", Rule: "no matching policy rule; use existing permissions"})
	appendCLIPolicyDecision(t, db, "global-deny", "run-b", 4, security.PolicyDecision{Effect: security.EffectDeny, Source: "global", Tool: "write_file", Rule: "tool=write_file effect=deny path_glob=private/**", RuleIndex: 2})
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"descending", nil, []string{"inv-global-deny", "inv-ask", "inv-deny", "inv-allow"}},
		{"run", []string{"--run", "run-a"}, []string{"inv-ask", "inv-allow"}},
		{"tool", []string{"--tool", "write_file"}, []string{"inv-global-deny", "inv-allow"}},
		{"allow", []string{"--effect", "allow"}, []string{"inv-allow"}},
		{"ask", []string{"--effect", "ask"}, []string{"inv-ask"}},
		{"deny", []string{"--effect", "deny"}, []string{"inv-global-deny", "inv-deny"}},
		{"since inclusive", []string{"--since", "2026-11-19T00:00:03Z"}, []string{"inv-global-deny", "inv-ask"}},
		{"since offset", []string{"--since", "2026-11-19T01:00:03+01:00"}, []string{"inv-global-deny", "inv-ask"}},
		{"combined", []string{"--run", "run-a", "--tool", "write_file", "--effect", "allow", "--since", "2026-11-19T00:00:01Z", "--limit", "1"}, []string{"inv-allow"}},
		{"no match", []string{"--tool", "missing"}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := invokePolicyDecisions(t, path, tc.args...)
			got := make([]string, 0, len(rows))
			for _, row := range rows {
				got = append(got, row.InvocationID)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	var stdout, stderr bytes.Buffer
	if code := Main([]string{"policy", "decisions", "--db", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("text query exit=%d: %s", code, stderr.String())
	}
	for _, want := range []string{"TIME", "RUN", "TOOL", "EFFECT", "SOURCE", "RULE", "2026-11-19T00:00:04Z", "workspace", "global", "builtin", "default", "path_glob=generated/**"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("missing %q: %s", want, stdout.String())
		}
	}
}

func TestPolicyDecisionsCLIEmptyAndLimit(t *testing.T) {
	workspace, path, db := policyCLIDatabase(t)
	if rows := invokePolicyDecisions(t, path); len(rows) != 0 {
		t.Fatalf("empty DB has records: %+v", rows)
	}
	missing := filepath.Join(workspace, "missing.db")
	if rows := invokePolicyDecisions(t, missing); len(rows) != 0 {
		t.Fatalf("missing DB has records: %+v", rows)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("query created missing DB: %v", err)
	}
	for i := 0; i < 520; i++ {
		appendCLIPolicyDecision(t, db, fmt.Sprintf("event-%03d", i), "run-a", i, security.PolicyDecision{Effect: security.EffectAsk, Source: "default", Tool: "read_file", Rule: "no match"})
	}
	for _, tc := range []struct {
		args []string
		want int
	}{{nil, 50}, {[]string{"--limit", "1"}, 1}, {[]string{"--limit", "500"}, 500}, {[]string{"--limit", "9999"}, 500}} {
		rows := invokePolicyDecisions(t, path, tc.args...)
		if len(rows) != tc.want || rows[0].InvocationID != "inv-event-519" {
			t.Fatalf("%v got %d rows", tc.args, len(rows))
		}
	}
	// Doctor counts all records, even when the listing would truncate at 500.
	item := checkPolicy(workspace, path)
	if item.Status != "ok" || !strings.Contains(item.Detail, "allow=0 ask=520 deny=0") {
		t.Fatalf("truncated doctor counts: %+v", item)
	}
}

func TestPolicyDecisionsCLIUsage(t *testing.T) {
	setDiagnosticEnv(t, "", "")
	for _, args := range [][]string{
		{"policy"}, {"policy", "unknown"}, {"policy", "decisions", "extra"},
		{"policy", "decisions", "--effect", "ALLOW"}, {"policy", "decisions", "--effect", "invalid"},
		{"policy", "decisions", "--since", "yesterday"}, {"policy", "decisions", "--limit", "0"},
		{"policy", "decisions", "--limit", "-1"}, {"policy", "decisions", "--limit", "abc"},
		{"policy", "decisions", "--run"}, {"policy", "decisions", "--unknown"},
	} {
		var stdout, stderr bytes.Buffer
		if code := Main(args, &stdout, &stderr); code != 2 || stderr.Len() == 0 {
			t.Fatalf("%v: exit=%d stdout=%s stderr=%s", args, code, stdout.String(), stderr.String())
		}
	}
	for _, args := range [][]string{{"policy", "--help"}, {"policy", "decisions", "--help"}, {"help"}, {"version"}} {
		var stdout, stderr bytes.Buffer
		if code := Main(args, &stdout, &stderr); code != 0 {
			t.Fatalf("%v: exit=%d stderr=%s", args, code, stderr.String())
		}
		if args[0] == "help" && !strings.Contains(stdout.String(), "policy decisions") {
			t.Fatal("policy command missing from help")
		}
	}
}

func TestDoctorPolicyLayersAndDecisions(t *testing.T) {
	for _, populated := range []bool{false, true} {
		t.Run(fmt.Sprint(populated), func(t *testing.T) {
			workspace, path, db := policyCLIDatabase(t)
			workspacePolicy := filepath.Join(workspace, ".cloud-harness", "policy.json")
			globalPolicy := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "cloud-harness", "policy.json")
			if populated {
				for file, body := range map[string]string{
					workspacePolicy: `{"rules":[{"tool":"read_file","path_glob":"private-selector/**","effect":"allow"},{"tool":"run_command","command_prefix":"echo private-doctor-prefix","effect":"ask"}]}`,
					globalPolicy:    `{"rules":[{"tool":"*","effect":"deny"}]}`,
				} {
					if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				approveWorkspaceTrust(t, workspace)
				for i, effect := range []security.Effect{security.EffectAllow, security.EffectAsk, security.EffectDeny} {
					appendCLIPolicyDecision(t, db, string(effect), "run-a", i, security.PolicyDecision{Tool: "write_file", Effect: effect, Source: "workspace", Rule: "tool=write_file effect=" + string(effect)})
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, asJSON := range []bool{false, true} {
				args := []string{"doctor", "--db", path, "--workspace", workspace, "--provider", "mock"}
				if asJSON {
					args = append(args, "--json")
				}
				var stdout, stderr bytes.Buffer
				if code := Main(args, &stdout, &stderr); code != 0 {
					t.Fatalf("doctor exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
				}
				detail := stdout.String()
				if asJSON {
					var items []doctorItem
					if err := json.Unmarshal(stdout.Bytes(), &items); err != nil {
						t.Fatal(err)
					}
					item := findDoctor(items, "policy")
					if item == nil || item.Status != "ok" {
						t.Fatalf("policy check = %+v", item)
					}
					detail = item.Detail
				}
				want := []string{"builtin: embedded (3 rules, loaded)"}
				if populated {
					want = append(want, "workspace: "+workspacePolicy+" (2 rules, loaded)", "global: "+globalPolicy+" (1 rules, loaded)", "decisions: allow=1 ask=1 deny=1")
				} else {
					want = append(want, "workspace: "+workspacePolicy+" (0 rules, missing)", "global: "+globalPolicy+" (0 rules, missing)", "decisions: allow=0 ask=0 deny=0")
				}
				for _, part := range want {
					if !strings.Contains(detail, part) {
						t.Fatalf("missing %q in %s", part, detail)
					}
				}
				if strings.Contains(detail, "private-selector") || strings.Contains(detail, "private-doctor-prefix") {
					t.Fatal("doctor printed rule contents")
				}
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("doctor changed database: %v", err)
			}
			if !populated {
				missing := filepath.Join(workspace, "missing.db")
				if item := checkPolicy(workspace, missing); item.Status != "ok" || !strings.Contains(item.Detail, "allow=0 ask=0 deny=0") {
					t.Fatalf("missing DB summary: %+v", item)
				}
				if _, err := os.Stat(missing); !os.IsNotExist(err) {
					t.Fatalf("doctor created DB: %v", err)
				}
			}
		})
	}
}

func TestPolicyCLIProjectionAndReadOnly(t *testing.T) {
	workspace, path, db := policyCLIDatabase(t)
	// Unknown fields in a decision event must never be echoed. Sensitive tool
	// invocation/result events are present too, but are not queried at all.
	for i, event := range []struct{ kind, payload string }{
		{"policy.decided", `{"effect":"allow","source":"workspace","tool":"write_file","rule":"tool=write_file effect=allow path_glob=generated/**","content":"private-file-content","command":"echo private-command-text","arguments":{"content":"private-argument-content"}}`},
		{"tool.finished", `{"content":"private-result-content","command":"echo private-command-text"}`},
	} {
		if _, err := db.AppendEvent(t.Context(), storage.Event{ID: fmt.Sprint(i), RunID: "run-a", Type: event.kind, Payload: json.RawMessage(event.payload), CreatedAt: time.Now(), Persistent: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"policy", "decisions", "--db", path}, {"policy", "decisions", "--db", path, "--json"},
		{"doctor", "--workspace", workspace, "--db", path, "--provider", "mock", "--json"},
	} {
		var stdout, stderr bytes.Buffer
		if code := Main(args, &stdout, &stderr); code != 0 {
			t.Fatalf("%v exit=%d stdout=%s stderr=%s", args, code, stdout.String(), stderr.String())
		}
		if strings.Contains(stdout.String()+stderr.String(), "private-") {
			t.Fatalf("sensitive output for %v: %s %s", args, stdout.String(), stderr.String())
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("audit commands changed database: %v", err)
	}
}

func TestPolicyDoctorInvalidFileAndCLIReadErrors(t *testing.T) {
	workspace, path, _ := policyCLIDatabase(t)
	file := filepath.Join(workspace, ".cloud-harness", "policy.json")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(`{"private-invalid-selector":"echo private-command-text"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	item := checkPolicy(workspace, path)
	if item.Status != "fail" || strings.Contains(item.Detail, "private-") {
		t.Fatalf("unsafe invalid policy error: %+v", item)
	}
	// Auditing retained decisions does not require reloading today's policy.
	if rows := invokePolicyDecisions(t, path); len(rows) != 0 {
		t.Fatalf("unexpected rows: %+v", rows)
	}
	broken := filepath.Join(workspace, "broken.db")
	if err := os.WriteFile(broken, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Main([]string{"policy", "decisions", "--db", broken}, &stdout, &stderr); code != 1 || stderr.Len() == 0 {
		t.Fatalf("broken DB exit=%d stderr=%s", code, stderr.String())
	}
	if item := checkPolicy(workspace, broken); item.Status != "fail" {
		t.Fatalf("broken DB check: %+v", item)
	}
}
