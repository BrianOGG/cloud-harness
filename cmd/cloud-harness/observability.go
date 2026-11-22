package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/config"
	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/hooks"
	"github.com/cloud-harness/cloud-harness/internal/storage/sqlite"
	"github.com/cloud-harness/cloud-harness/internal/tools"
)

const observabilityFailureLimit = 12

type serveDiagnostics struct {
	cfg       config.Config
	workspace string
	// mcp is the server set owned by this serve process; doctor reports its live state.
	mcp *tools.MCPSet
}

// Config is captured from serve; every database query made by the dashboard is read-only.
func (d serveDiagnostics) stats(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	db, err := openDiagnosticsDB(ctx, d.cfg.DBPath)
	if err != nil {
		writeHTTPJSON(w, http.StatusInternalServerError, map[string]string{"error": "统计数据库无法读取：" + scrubDoctor(err.Error())})
		return
	}
	defer db.Close()

	summary, err := readStoreSummary(ctx, db)
	if err != nil {
		writeHTTPJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取统计摘要失败：" + scrubDoctor(err.Error())})
		return
	}

	since := time.Now().UTC().AddDate(0, 0, -30)
	usage, err := sqlite.ReadUsage(ctx, d.cfg.DBPath, since)
	if err != nil {
		writeHTTPJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取模型用量失败：" + scrubDoctor(err.Error())})
		return
	}
	usageReport := aggregateStats(usage, "day", 30, since, d.cfg)
	summary.Usage = usageReport.Totals
	// The CLI uses a display placeholder; the HTTP contract uses a number or null.
	if cost, ok := summary.Usage.Cost.(float64); !ok || math.IsNaN(cost) || math.IsInf(cost, 0) {
		summary.Usage.Cost = nil
	}
	summary.ModelCalls = usageReport.Totals.ModelCalls

	if err := readPolicyCounts(ctx, db, &summary.Policy); err != nil {
		writeHTTPJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取策略决策失败：" + scrubDoctor(err.Error())})
		return
	}
	if err := readHookSummary(ctx, db, &summary.Hooks); err != nil {
		writeHTTPJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取 hook 统计失败：" + scrubDoctor(err.Error())})
		return
	}
	summary.RecentErrors, err = readRecentFailures(ctx, db)
	if err != nil {
		writeHTTPJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取最近错误失败：" + scrubDoctor(err.Error())})
		return
	}
	writeHTTPJSON(w, http.StatusOK, summary)
}

func (d serveDiagnostics) doctor(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	var liveMCP func() []tools.MCPServerStatus
	if d.mcp != nil {
		liveMCP = d.mcp.Status
	}
	items := collectDoctorChecksMCP(ctx, d.cfg, d.workspace, nil, liveMCP)
	// collectDoctorChecks appends a summary for the CLI. Recompute it after the
	// serve-only failed-run check so the HTTP response contains one overall row.
	if len(items) > 0 && items[len(items)-1].Name == "overall" {
		items = items[:len(items)-1]
	}

	failedRuns, failedID, err := readFailedRunSummary(ctx, d.cfg.DBPath)
	if err != nil {
		items = append(items, doctorItem{Name: "failed_runs", Status: "fail", Detail: "failed run records could not be read"})
	} else if failedRuns > 0 {
		items = append(items, doctorItem{Name: "failed_runs", Status: "fail", RunID: failedID, Detail: countLabel(failedRuns, "failed run", "failed runs") + " recorded"})
	} else {
		items = append(items, doctorItem{Name: "failed_runs", Status: "ok", Detail: "no failed runs recorded"})
	}

	fails, warns := 0, 0
	for _, item := range items {
		if item.Status == "fail" {
			fails++
		} else if item.Status == "warn" {
			warns++
		}
	}
	overall := "ok"
	if fails > 0 {
		overall = "fail"
	} else if warns > 0 {
		overall = "warn"
	}
	items = append(items, doctorItem{Name: "overall", Status: overall, Detail: countLabel(fails, "failure", "failures") + ", " + countLabel(warns, "warning", "warnings")})
	for i := range items {
		items[i].Detail = scrubDoctor(items[i].Detail)
	}
	writeHTTPJSON(w, http.StatusOK, items)
}

func writeHTTPJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func countLabel(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

type statsSummary struct {
	Sessions     int64           `json:"sessions"`
	Runs         int64           `json:"runs"`
	Events       int64           `json:"events"`
	ActiveRuns   int64           `json:"active_runs"`
	FailedRuns   int64           `json:"failed_runs"`
	ModelCalls   int             `json:"model_calls"`
	Usage        statsRow        `json:"usage"`
	Budget       budgetSummary   `json:"budget"`
	Policy       policyCounts    `json:"policy"`
	Hooks        hookSummary     `json:"hooks"`
	RecentErrors []recentFailure `json:"recent_errors"`
}

type budgetSummary struct {
	ModelUsed int64 `json:"model_used"`
	ModelMax  int64 `json:"model_max"`
	ToolUsed  int64 `json:"tool_used"`
	ToolMax   int64 `json:"tool_max"`
}

type policyCounts struct {
	Allow int `json:"allow"`
	Ask   int `json:"ask"`
	Deny  int `json:"deny"`
}

type hookSummary struct {
	Total    int64 `json:"total"`
	Warnings int64 `json:"warnings"`
	Blocked  int64 `json:"blocked"`
}

type recentFailure struct {
	RunID     string    `json:"run_id"`
	SessionID string    `json:"session_id"`
	Type      string    `json:"type"`
	Message   string    `json:"message"`
	At        time.Time `json:"at"`
}

func readStoreSummary(ctx context.Context, db *sql.DB) (statsSummary, error) {
	var out statsSummary
	err := db.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM sessions),
			(SELECT COUNT(*) FROM runs),
			(SELECT COUNT(*) FROM events),
			(SELECT COUNT(*) FROM runs WHERE status IN ('RUNNING', 'WAITING')),
			(SELECT COUNT(*) FROM runs WHERE status = 'FAILED'),
			COALESCE(SUM(json_extract(budget_json, '$.used_model_calls')), 0),
			COALESCE(SUM(json_extract(budget_json, '$.max_model_calls')), 0),
			COALESCE(SUM(json_extract(budget_json, '$.used_tool_calls')), 0),
			COALESCE(SUM(json_extract(budget_json, '$.max_tool_calls')), 0)
		FROM runs WHERE parent_run_id = ''`).Scan(
		&out.Sessions, &out.Runs, &out.Events, &out.ActiveRuns, &out.FailedRuns,
		&out.Budget.ModelUsed, &out.Budget.ModelMax, &out.Budget.ToolUsed, &out.Budget.ToolMax,
	)
	return out, err
}

func readPolicyCounts(ctx context.Context, db *sql.DB, out *policyCounts) error {
	return db.QueryRowContext(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE effect = 'allow'),
			COUNT(*) FILTER (WHERE effect = 'ask'),
			COUNT(*) FILTER (WHERE effect = 'deny')
		FROM (SELECT json_extract(payload_json, '$.effect') AS effect FROM events WHERE type = 'policy.decided')`,
	).Scan(&out.Allow, &out.Ask, &out.Deny)
}

func readHookSummary(ctx context.Context, db *sql.DB, out *hookSummary) error {
	rows, err := db.QueryContext(ctx, `SELECT type, payload_json FROM events WHERE type IN ('hook.completed', 'hook.warning')`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var typ, payload string
		if err := rows.Scan(&typ, &payload); err != nil {
			return err
		}
		var event hooks.Event
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return err
		}
		out.Total++
		if typ == "hook.warning" || event.Warning {
			out.Warnings++
		}
		if event.Blocked {
			out.Blocked++
		}
	}
	return rows.Err()
}

func readRecentFailures(ctx context.Context, db *sql.DB) ([]recentFailure, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT run_id, session_id, failure_type, message, created_at FROM (
			SELECT id AS run_id, session_id, 'run.failed' AS failure_type, error_json AS message, updated_at AS created_at
			FROM runs WHERE status = 'FAILED'
			UNION ALL
			SELECT e.run_id, r.session_id, e.type, e.payload_json, e.created_at
			FROM events e JOIN runs r ON r.id = e.run_id
			WHERE e.type = 'hook.warning'
			UNION ALL
			SELECT e.run_id, r.session_id, e.type, i.result_json, e.created_at
			FROM events e JOIN runs r ON r.id = e.run_id JOIN tool_invocations i ON i.id = e.invocation_id
			WHERE e.type = 'invocation.FAILED'
		) ORDER BY created_at DESC, run_id DESC, failure_type ASC LIMIT ?`, observabilityFailureLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]recentFailure, 0)
	for rows.Next() {
		var row recentFailure
		var raw, created string
		if err := rows.Scan(&row.RunID, &row.SessionID, &row.Type, &raw, &created); err != nil {
			return nil, err
		}
		row.At, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, err
		}
		row.Message = failureMessage(row.Type, raw)
		out = append(out, row)
	}
	return out, rows.Err()
}

func failureMessage(typ, raw string) string {
	switch typ {
	case "run.failed":
		var runError domain.Error
		if json.Unmarshal([]byte(raw), &runError) == nil && runError.Message != "" {
			if runError.Code != "" {
				return scrubDoctor(runError.Code + ": " + runError.Message)
			}
			return scrubDoctor(runError.Message)
		}
	case "hook.warning":
		var event hooks.Event
		if json.Unmarshal([]byte(raw), &event) == nil {
			return scrubDoctor(event.Reason + hookOutput(event.Stderr, event.Stdout))
		}
	case "invocation.FAILED":
		var result struct {
			Error *domain.Error `json:"error"`
		}
		if json.Unmarshal([]byte(raw), &result) == nil && result.Error != nil {
			return scrubDoctor(result.Error.Code + ": " + result.Error.Message)
		}
	}
	return "Execution failed."
}

func hookOutput(stderr, stdout string) string {
	if stderr != "" {
		return ": " + stderr
	}
	if stdout != "" {
		return ": " + stdout
	}
	return ""
}

func readFailedRunSummary(ctx context.Context, path string) (int, string, error) {
	db, err := openDiagnosticsDB(ctx, path)
	if err != nil {
		return 0, "", err
	}
	defer db.Close()
	var count int
	var id sql.NullString
	err = db.QueryRowContext(ctx, `SELECT COUNT(*), (SELECT id FROM runs WHERE status = 'FAILED' ORDER BY updated_at DESC, id DESC LIMIT 1) FROM runs WHERE status = 'FAILED'`).Scan(&count, &id)
	if err != nil {
		return 0, "", err
	}
	return count, id.String, nil
}
