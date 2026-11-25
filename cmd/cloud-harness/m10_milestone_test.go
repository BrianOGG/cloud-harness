package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/snapshot"
	"github.com/BrianOGG/cloud-harness/internal/storage"
	"github.com/BrianOGG/cloud-harness/internal/storage/sqlite"
)

func TestDoctorReadOnlyAndMissingDatabase(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"provider":"mock"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	setDiagnosticEnv(t, configPath, "")
	dbPath := filepath.Join(root, "harness.db")
	db, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	workspaceID, err := snapshot.WorkspaceIDForPath(root)
	if err != nil {
		t.Fatal(err)
	}
	shadowDir := filepath.Join(root, ".cloud-harness", "snapshots", workspaceID)
	if err := os.MkdirAll(shadowDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shadowDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	code := Main([]string{"doctor", "--db", dbPath, "--workspace", root, "--provider", "mock", "--json"}, &out, &stderr)
	if code != 0 {
		t.Fatalf("doctor code=%d stderr=%s stdout=%s", code, stderr.String(), out.String())
	}
	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("doctor changed database bytes")
	}
	var checks []doctorItem
	if err := json.Unmarshal(out.Bytes(), &checks); err != nil {
		t.Fatal(err)
	}
	if got := findDoctor(checks, "database"); got == nil || got.Status != "ok" || !strings.Contains(got.Detail, "schema version") {
		t.Fatalf("database check: %+v", got)
	}
	if got := findDoctor(checks, "overall"); got == nil || got.Status != "ok" {
		t.Fatalf("overall check: %+v", got)
	}
	if got := findDoctor(checks, "shadow_repository"); got == nil || got.Status != "ok" || got.Detail != "HEAD is readable" {
		t.Fatalf("shadow repository check: %+v", got)
	}
	if got := findDoctor(checks, "mcp"); got == nil || got.Status != "ok" || !strings.Contains(got.Detail, "0 server(s)") {
		t.Fatalf("MCP check: %+v", got)
	}

	missing := filepath.Join(root, "missing.db")
	out.Reset()
	stderr.Reset()
	code = Main([]string{"doctor", "--db", missing, "--workspace", root, "--provider", "mock", "--json"}, &out, &stderr)
	if code != 0 {
		t.Fatalf("missing-db doctor code=%d stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("doctor created missing db: %v", err)
	}
	checks = nil
	if err := json.Unmarshal(out.Bytes(), &checks); err != nil {
		t.Fatal(err)
	}
	if got := findDoctor(checks, "database"); got == nil || got.Status != "warn" || !strings.Contains(got.Detail, "尚未创建") {
		t.Fatalf("missing database check: %+v", got)
	}
}

func TestDoctorBadConfigFailsWithField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"provider":"mock","unknown_setting":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	setDiagnosticEnv(t, path, "")
	var out, stderr bytes.Buffer
	code := Main([]string{"doctor", "--db", filepath.Join(t.TempDir(), "missing.db"), "--json"}, &out, &stderr)
	if code != 1 {
		t.Fatalf("bad config code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	var checks []doctorItem
	if err := json.Unmarshal(out.Bytes(), &checks); err != nil {
		t.Fatal(err)
	}
	configCheck := findDoctor(checks, "config")
	if configCheck == nil || configCheck.Status != "fail" || !strings.Contains(configCheck.Detail, "unknown_setting") {
		t.Fatalf("config check: %+v", configCheck)
	}
}

func TestDoctorReportsSchemaAndForeignKeyFailuresReadOnly(t *testing.T) {
	setDiagnosticEnv(t, "", "")
	for _, tc := range []struct {
		name   string
		mutate string
		want   string
	}{
		{name: "schema", mutate: `UPDATE schema_migrations SET version=999 WHERE version=(SELECT MAX(version) FROM schema_migrations)`, want: "expected 9"},
		{name: "foreign-key", mutate: `INSERT INTO turns(id,session_id,user_input,status,created_at) VALUES('orphan','missing','x','completed','2026-11-19T00:00:00.000000000Z')`, want: "foreign_key_check found 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "broken.db")
			db, err := sqlite.Open(context.Background(), dbPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			writable, err := sql.Open("sqlite", dbPath)
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "foreign-key" {
				if _, err := writable.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := writable.Exec(tc.mutate); err != nil {
				t.Fatal(err)
			}
			if err := writable.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			workspace := t.TempDir()
			var out, stderr bytes.Buffer
			code := Main([]string{"doctor", "--db", dbPath, "--workspace", workspace, "--provider", "mock", "--json"}, &out, &stderr)
			if code != 1 {
				t.Fatalf("doctor code=%d stderr=%s stdout=%s", code, stderr.String(), out.String())
			}
			var checks []doctorItem
			if err := json.Unmarshal(out.Bytes(), &checks); err != nil {
				t.Fatal(err)
			}
			database := findDoctor(checks, "database")
			if database == nil || database.Status != "fail" || !strings.Contains(database.Detail, tc.want) {
				t.Fatalf("database check: %+v", database)
			}
			after, err := os.ReadFile(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("doctor changed damaged database bytes")
			}
		})
	}
}

func TestDoctorOpenAIProbeRedactsKey(t *testing.T) {
	const key = "m10-doctor-secret-key"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/models" || r.Header.Get("Authorization") != "Bearer "+key {
			t.Errorf("unexpected probe request %s %s", r.Method, r.URL.Path)
		}
		http.Error(w, "invalid credential "+key, http.StatusUnauthorized)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "config.json")
	body, _ := json.Marshal(map[string]any{"provider": "openai", "openai_base_url": server.URL})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	setDiagnosticEnv(t, path, key)
	var out, stderr bytes.Buffer
	code := Main([]string{"doctor", "--db", filepath.Join(t.TempDir(), "missing.db"), "--workspace", t.TempDir(), "--json"}, &out, &stderr)
	if code != 1 {
		t.Fatalf("invalid provider probe code=%d stderr=%s stdout=%s", code, stderr.String(), out.String())
	}
	if strings.Contains(out.String(), key) || strings.Contains(stderr.String(), key) {
		t.Fatalf("doctor leaked API key: stdout=%s stderr=%s", out.String(), stderr.String())
	}
}

func TestDoctorOpenAIMissingKeyWarns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"provider":"openai"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	setDiagnosticEnv(t, path, "")
	var out, stderr bytes.Buffer
	code := Main([]string{"doctor", "--db", filepath.Join(t.TempDir(), "missing.db"), "--workspace", t.TempDir(), "--json"}, &out, &stderr)
	if code != 0 {
		t.Fatalf("missing-key doctor code=%d stderr=%s stdout=%s", code, stderr.String(), out.String())
	}
	var checks []doctorItem
	if err := json.Unmarshal(out.Bytes(), &checks); err != nil {
		t.Fatal(err)
	}
	providerCheck := findDoctor(checks, "provider")
	if providerCheck == nil || providerCheck.Status != "warn" || !strings.Contains(providerCheck.Detail, "OPENAI_API_KEY") {
		t.Fatalf("provider check: %+v", providerCheck)
	}
}

func TestStatsLegacyUsageRemainsUnknownWithCurrentPrices(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "stats.db")
	db, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.CreateSession(ctx, storage.Session{ID: "ses_stats", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateRun(ctx, storage.Run{ID: "run_stats", SessionID: "ses_stats", Status: domain.RunRunning, Budget: domain.DefaultBudget(), Version: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for i, usage := range []domain.Usage{{InputTokens: 100, OutputTokens: 10, CachedInputTokens: 20}, {InputTokens: 50, OutputTokens: 5, CachedInputTokens: 10}} {
		responseID := "rsp_stats_" + string(rune('a'+i))
		if err := db.SaveModelResponse(ctx, storage.StoredResponse{
			ID: responseID, RunID: "run_stats", Provider: "openai", Complete: true,
			Usage: usage, Action: domain.ActionAnswer, CreatedAt: now.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
		payload, err := json.Marshal(map[string]any{
			"response_id": responseID, "provider": "openai", "input_tokens": usage.InputTokens,
			"output_tokens": usage.OutputTokens, "cached_input_tokens": usage.CachedInputTokens,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.AppendEvent(ctx, storage.Event{
			ID: "evt_stats_" + string(rune('a'+i)), RunID: "run_stats", Type: "model.completed",
			Payload: payload, Persistent: true, CreatedAt: now.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"provider":"openai","openai_model":"gpt-test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	setDiagnosticEnv(t, configPath, "")

	var out, stderr bytes.Buffer
	code := Main([]string{"stats", "--db", dbPath, "--by", "run", "--json"}, &out, &stderr)
	if code != 0 {
		t.Fatalf("stats without prices code=%d stderr=%s", code, stderr.String())
	}
	var report statsReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Totals.ModelCalls != 2 || report.Totals.InputTokens != 150 || report.Totals.OutputTokens != 15 || report.Totals.CachedTokens != 30 {
		t.Fatalf("totals: %+v", report.Totals)
	}
	if report.Totals.CacheHitRate != 0.2 {
		t.Fatalf("cache hit rate=%v", report.Totals.CacheHitRate)
	}
	if report.Totals.Cost != "—" {
		t.Fatalf("unpriced cost=%#v", report.Totals.Cost)
	}
	if len(report.Results) != 1 || report.Results[0].Group != "run_stats" {
		t.Fatalf("results: %+v", report.Results)
	}

	if err := os.WriteFile(configPath, []byte(`{"provider":"openai","openai_model":"gpt-test","model_prices":{"gpt-test":{"input":1,"output":2,"cached":0.5}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	stderr.Reset()
	code = Main([]string{"stats", "--db", dbPath, "--by", "model", "--json"}, &out, &stderr)
	if code != 0 {
		t.Fatalf("stats with prices code=%d stderr=%s", code, stderr.String())
	}
	report = statsReport{}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Totals.Cost != "—" {
		t.Fatalf("legacy cost changed with current prices: %#v", report.Totals.Cost)
	}
	if len(report.Results) != 1 || report.Results[0].Group != "unknown" {
		t.Fatalf("model results: %+v", report.Results)
	}
	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("stats changed database bytes")
	}
}

func TestStatsCacheWarningThreshold(t *testing.T) {
	now := time.Now().UTC()
	base := sqlite.UsageRecord{Provider: "openai", RunID: "run", CreatedAt: now}
	base.Usage = domain.Usage{InputTokens: cacheWarningInputTokenThreshold, CachedInputTokens: 0}
	report := aggregateStats([]sqlite.UsageRecord{base}, "day", 30, now.Add(-30*24*time.Hour))
	if len(report.Warnings) != 0 {
		t.Fatalf("warning at threshold: %v", report.Warnings)
	}
	base.Usage.InputTokens++
	report = aggregateStats([]sqlite.UsageRecord{base}, "day", 30, now.Add(-30*24*time.Hour))
	if len(report.Warnings) != 1 || !strings.Contains(report.Warnings[0], "unstable") {
		t.Fatalf("warning above threshold: %v", report.Warnings)
	}
}

func TestPersistedModelDoesNotInferFromProvider(t *testing.T) {
	for _, tc := range []struct {
		provider string
		model    string
		want     string
	}{
		{"openai", "gpt-call", "gpt-call"},
		{"openai-responses", "", "unknown"},
		{"anthropic", "claude-call", "claude-call"},
		{"mock", "", "unknown"},
		{"", "", "unknown"},
	} {
		record := sqlite.UsageRecord{Provider: tc.provider, Usage: domain.Usage{Model: tc.model}}
		if got := persistedModel(record); got != tc.want {
			t.Fatalf("persistedModel(%+v) = %q; want %q", record, got, tc.want)
		}
	}
}

func TestFormatCostPreservesSmallAmounts(t *testing.T) {
	if got := formatCost(0.000165); got != "0.000165" {
		t.Fatalf("formatted cost=%q", got)
	}
}

func findDoctor(items []doctorItem, name string) *doctorItem {
	for i := range items {
		if items[i].Name == name {
			return &items[i]
		}
	}
	return nil
}

func setDiagnosticEnv(t *testing.T, configPath, apiKey string) {
	t.Helper()
	t.Setenv("CLOUD_HARNESS_CONFIG", configPath)
	t.Setenv("CLOUD_HARNESS_DB", "")
	t.Setenv("CLOUD_HARNESS_PROVIDER", "")
	t.Setenv("CLOUD_HARNESS_ADDR", "")
	t.Setenv("CLOUD_HARNESS_HTTP_TOKEN", "")
	t.Setenv("OPENAI_API_KEY", apiKey)
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("OPENAI_MODEL", "")
	t.Setenv("OPENAI_REASONING_REPLAY", "")
	t.Setenv("CLOUD_HARNESS_OPENAI_REASONING_REPLAY", "")
	t.Setenv("CLOUD_HARNESS_RETENTION_DAYS", "")
}
