package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/agent"
	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/security"
	"github.com/cloud-harness/cloud-harness/internal/storage"
)

func TestPolicyAuditReadOnlyAndEscapedPath(t *testing.T) {
	_, path, db := policyCLIDatabase(t)
	appendCLIPolicyDecision(t, db, "ask", "run-a", 1, security.PolicyDecision{Tool: "read_file", Effect: security.EffectAsk, Source: "default", Rule: "no match"})
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	escapedPath := filepath.Join(filepath.Dir(path), "audit ?# db.sqlite")
	if err := os.Rename(path, escapedPath); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(escapedPath)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := newPolicyAuditReader(t.Context(), escapedPath)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := agent.ListPolicyDecisions(t.Context(), reader, agent.PolicyDecisionQuery{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("escaped path query = %+v, %v", rows, err)
	}
	if _, err := reader.db.ExecContext(t.Context(), `DELETE FROM events`); err == nil {
		t.Fatal("audit connection allowed a write")
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(escapedPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("audit changed DB bytes: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing.db")
	if _, err := newPolicyAuditReader(t.Context(), missing); err == nil {
		t.Fatal("opened missing database")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("created missing database: %v", err)
	}
}

func TestPolicyAuditReadsOnlyDecisionBodies(t *testing.T) {
	setDiagnosticEnv(t, "", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// Deliberately omit all content-bearing tables/columns. Even the non-policy
	// event is invalid JSON; only IDs and policy event bodies may be consumed.
	path := filepath.Join(t.TempDir(), "minimal.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`CREATE TABLE sessions(id TEXT)`,
		`CREATE TABLE runs(id TEXT, session_id TEXT)`,
		`CREATE TABLE events(run_id TEXT, seq INTEGER, invocation_id TEXT, type TEXT, payload_json TEXT, created_at TEXT)`,
		`INSERT INTO sessions VALUES('session')`,
		`INSERT INTO runs VALUES('run','session')`,
		`INSERT INTO events VALUES('run',1,'inv','policy.decided','{"effect":"ask","source":"default","tool":"read_file","rule":"no match"}','2026-11-19T00:00:00Z')`,
		`INSERT INTO events VALUES('run',2,'inv','tool.finished','private-invalid-json','2026-11-19T00:00:01Z')`,
	} {
		if _, err := db.ExecContext(t.Context(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	reader, err := newPolicyAuditReader(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	summary, err := agent.SummarizePolicy(t.Context(), t.TempDir(), reader)
	if err != nil || summary.Decisions != (agent.PolicyDecisionCounts{Ask: 1}) {
		t.Fatalf("summary read sensitive tables or non-policy body: %+v, %v", summary, err)
	}
}

func TestPolicyAuditReadOnlyAllRuns(t *testing.T) {
	workspace, path, db := policyCLIDatabase(t)
	for i := 0; i < 205; i++ {
		id := fmt.Sprintf("run-z%03d", i)
		if err := db.CreateRun(t.Context(), storage.Run{
			ID: id, SessionID: "session", Status: domain.RunCompleted, Budget: domain.DefaultBudget(), Version: 1, CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		appendCLIPolicyDecision(t, db, fmt.Sprint(i), id, i, security.PolicyDecision{Tool: "read_file", Effect: security.EffectAsk, Source: "default", Rule: "no match"})
	}
	rows := invokePolicyDecisions(t, path, "--limit", "500")
	if len(rows) != 205 || rows[0].RunID != "run-z204" {
		t.Fatalf("query truncated run inventory: len=%d", len(rows))
	}
	item := checkPolicy(workspace, path)
	if item.Status != "ok" || !strings.Contains(item.Detail, "allow=0 ask=205 deny=0") {
		t.Fatalf("summary truncated run inventory: %+v", item)
	}
}
