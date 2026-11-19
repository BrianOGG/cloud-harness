package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/config"
	"github.com/cloud-harness/cloud-harness/internal/provider"
	"github.com/cloud-harness/cloud-harness/internal/rules"
	"github.com/cloud-harness/cloud-harness/internal/snapshot"
	"github.com/cloud-harness/cloud-harness/internal/storage/sqlite"
	"github.com/cloud-harness/cloud-harness/migrations"
)

const cacheWarningInputTokenThreshold = 100_000

type doctorItem struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

func cmdDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "sqlite database path")
	workspace := fs.String("workspace", "", "workspace directory")
	providerFlag := fs.String("provider", "", "mock or openai")
	jsonOutput := fs.Bool("json", false, "write JSON checks")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "doctor does not accept positional arguments")
		return 2
	}

	common := &commonFlags{db: *dbPath, provider: *providerFlag}
	cfg, configErr := load(common)
	configOK := configErr == nil
	if !configOK {
		cfg = config.Config{DBPath: config.DefaultDB, Provider: "mock", OpenAIBaseURL: config.DefaultOpenAI, OpenAIModel: config.DefaultModel}
		if *dbPath != "" {
			cfg.DBPath = *dbPath
		} else if envPath := os.Getenv("CLOUD_HARNESS_DB"); envPath != "" {
			cfg.DBPath = envPath
		}
		if *providerFlag != "" {
			cfg.Provider = *providerFlag
		} else if envProvider := os.Getenv("CLOUD_HARNESS_PROVIDER"); envProvider != "" {
			cfg.Provider = envProvider
		}
	}

	wsRoot := *workspace
	if wsRoot == "" {
		wsRoot = "."
	}
	if root, err := filepath.Abs(wsRoot); err == nil {
		cfg.DBPath = worktreeDBPath(cfg, root)
	}
	items := make([]doctorItem, 0, 9)
	if configErr != nil {
		items = append(items, doctorItem{Name: "config", Status: "fail", Detail: configErrorDetail(configErr)})
	} else if cfg.Provider != "mock" && cfg.Provider != "openai" {
		items = append(items, doctorItem{Name: "config", Status: "fail", Detail: "field provider: must be mock or openai"})
		configOK = false
	} else if err := config.ValidateModelPrices(cfg.ModelPrices); err != nil {
		items = append(items, doctorItem{Name: "config", Status: "fail", Detail: configErrorDetail(err)})
		configOK = false
	} else if err := cfg.ValidateBind(); err != nil {
		items = append(items, doctorItem{Name: "config", Status: "fail", Detail: configErrorDetail(err)})
		configOK = false
	} else {
		items = append(items, doctorItem{Name: "config", Status: "ok", Detail: "configuration loaded and validated"})
	}

	if configOK {
		if err := validateMCPServers(cfg.Servers); err != nil {
			items = append(items, doctorItem{Name: "mcp", Status: "fail", Detail: err.Error()})
		} else {
			items = append(items, doctorItem{Name: "mcp", Status: "ok", Detail: fmt.Sprintf("%d server(s) validated; no process started", len(cfg.Servers))})
		}
	} else {
		items = append(items, doctorItem{Name: "mcp", Status: "warn", Detail: "skipped because configuration could not be validated"})
	}

	items = append(items, checkDatabase(cfg.DBPath)...)

	ws := *workspace
	if ws == "" {
		ws = "."
	}
	absWorkspace, wsInfo, wsErr := inspectWorkspace(ws)
	if wsErr != nil {
		items = append(items, doctorItem{Name: "workspace", Status: "fail", Detail: scrubDoctor(wsErr.Error())})
	} else {
		items = append(items, doctorItem{Name: "workspace", Status: "ok", Detail: fmt.Sprintf("directory is enterable; mode=%04o", wsInfo.Mode().Perm())})
	}
	items = append(items, checkProjectRules(ws))

	gitBin, gitErr := exec.LookPath("git")
	if gitErr != nil {
		items = append(items, doctorItem{Name: "git", Status: "warn", Detail: "git executable not found; workspace snapshots are unavailable"})
	} else {
		items = append(items, doctorItem{Name: "git", Status: "ok", Detail: "git executable available at " + gitBin})
	}
	if wsErr != nil {
		items = append(items, doctorItem{Name: "shadow_repository", Status: "warn", Detail: "skipped because workspace is unavailable"})
	} else {
		items = append(items, inspectShadowRepository(absWorkspace)...)
	}

	if wsErr == nil && gitErr == nil {
		items = append(items, checkWorktrees(context.Background(), absWorkspace, cfg.DBPath))
	}
	if !configOK {
		items = append(items, doctorItem{Name: "provider", Status: "warn", Detail: "skipped because configuration could not be validated"})
	} else {
		items = append(items, checkProvider(cfg))
	}

	fails, warns := 0, 0
	for _, item := range items {
		switch item.Status {
		case "fail":
			fails++
		case "warn":
			warns++
		}
	}
	overall := "ok"
	if fails > 0 {
		overall = "fail"
	} else if warns > 0 {
		overall = "warn"
	}
	items = append(items, doctorItem{Name: "overall", Status: overall, Detail: fmt.Sprintf("%d fail(s), %d warning(s)", fails, warns)})
	for i := range items {
		items[i].Detail = scrubDoctor(items[i].Detail)
	}
	if *jsonOutput {
		if code := writeJSON(stdout, items); code != 0 {
			return code
		}
	} else {
		for _, item := range items {
			fmt.Fprintf(stdout, "%s: %s - %s\n", item.Name, item.Status, item.Detail)
		}
	}
	if fails > 0 {
		return 1
	}
	return 0
}

func checkProjectRules(workspace string) doctorItem {
	loaded := rules.Load(workspace)
	item := doctorItem{Name: "project_rules", Status: "ok", Detail: "AGENTS.md: none"}
	if len(loaded.Files) > 0 {
		files := make([]string, 0, len(loaded.Files))
		for _, file := range loaded.Files {
			files = append(files, fmt.Sprintf("%s (%d bytes)", file.Path, file.Bytes))
		}
		item.Detail = "AGENTS.md: " + strings.Join(files, "; ")
	}
	if len(loaded.Omitted) > 0 {
		item.Status = "warn"
		item.Detail += "; truncated at 32 KiB (including source markers)"
	}
	for _, diagnostic := range loaded.Diagnostics {
		item.Status = "warn"
		item.Detail += fmt.Sprintf("; skipped %s: %s", diagnostic.Path, diagnostic.Error)
	}
	return item
}

func configErrorDetail(err error) string {
	message := strings.TrimSpace(strings.TrimPrefix(err.Error(), "invalid_argument: "))
	message = strings.TrimPrefix(message, "config file: ")
	field := "configuration"
	if match := regexp.MustCompile(`unknown field "([^"]+)"`).FindStringSubmatch(message); len(match) == 2 {
		field = match[1]
	} else if match := regexp.MustCompile(`field Config\.([A-Za-z0-9_]+)`).FindStringSubmatch(message); len(match) == 2 {
		field = match[1]
	} else {
		for _, name := range []string{"openai_reasoning_replay", "http_token", "model_prices", "provider", "listen", "db", "servers", "OPENAI_HEADER_TIMEOUT_MS", "OPENAI_IDLE_TIMEOUT_MS", "OPENAI_RETRY_BASE_MS", "CLOUD_HARNESS_RETENTION_DAYS"} {
			if strings.Contains(message, name) {
				field = name
				break
			}
		}
	}
	return "field " + field + ": " + message
}

func validateMCPServers(servers []config.MCPServerConfig) error {
	seen := make(map[string]struct{}, len(servers))
	for i, server := range servers {
		if strings.TrimSpace(server.Name) == "" {
			return fmt.Errorf("servers[%d].name is required", i)
		}
		if strings.TrimSpace(server.Command) == "" {
			return fmt.Errorf("servers[%d].command is required", i)
		}
		if _, ok := seen[server.Name]; ok {
			return fmt.Errorf("servers[%d].name duplicates %q", i, server.Name)
		}
		seen[server.Name] = struct{}{}
	}
	return nil
}

func checkDatabase(path string) []doctorItem {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return []doctorItem{{Name: "database", Status: "warn", Detail: "尚未创建"}}
	}
	if err != nil {
		return []doctorItem{{Name: "database", Status: "fail", Detail: "stat failed: " + err.Error()}}
	}
	if !info.Mode().IsRegular() {
		return []doctorItem{{Name: "database", Status: "fail", Detail: "path is not a regular database file"}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dbInfo, err := sqlite.InspectReadOnly(ctx, path)
	if err != nil {
		return []doctorItem{{Name: "database", Status: "fail", Detail: "read-only inspection failed: " + err.Error()}}
	}
	files, err := migrations.Files()
	if err != nil {
		return []doctorItem{{Name: "database", Status: "fail", Detail: "cannot determine current schema version: " + err.Error()}}
	}
	currentVersion := 0
	if len(files) > 0 {
		currentVersion = files[len(files)-1].Version
	}
	status := "ok"
	detail := fmt.Sprintf("schema version %d (current %d); WAL %s; foreign_key_check clean", dbInfo.SchemaVersion, currentVersion, walState(dbInfo.JournalMode))
	if dbInfo.SchemaVersion != currentVersion {
		status = "fail"
		detail = fmt.Sprintf("schema version %d, expected %d; WAL %s; foreign_key_check clean", dbInfo.SchemaVersion, currentVersion, walState(dbInfo.JournalMode))
	}
	if len(dbInfo.ForeignKeyViolations) > 0 {
		status = "fail"
		detail = strings.TrimSuffix(detail, "; foreign_key_check clean") + fmt.Sprintf("; foreign_key_check found %d violation(s): %s", len(dbInfo.ForeignKeyViolations), strings.Join(dbInfo.ForeignKeyViolations, ", "))
	}
	if !strings.EqualFold(dbInfo.JournalMode, "wal") && status == "ok" {
		status = "warn"
	}
	return []doctorItem{{Name: "database", Status: status, Detail: detail}}
}

func walState(mode string) string {
	if strings.EqualFold(mode, "wal") {
		return "enabled"
	}
	return "disabled (mode=" + mode + ")"
}

func inspectWorkspace(path string) (string, os.FileInfo, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", nil, fmt.Errorf("workspace unavailable: %w", err)
	}
	if !info.IsDir() {
		return "", nil, fmt.Errorf("workspace is not a directory")
	}
	dir, err := os.Open(abs)
	if err != nil {
		return "", nil, fmt.Errorf("workspace cannot be entered: %w", err)
	}
	defer dir.Close()
	if _, err := dir.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
		return "", nil, fmt.Errorf("workspace cannot be read: %w", err)
	}
	return abs, info, nil
}

func inspectShadowRepository(workspace string) []doctorItem {
	id, err := snapshot.WorkspaceIDForPath(workspace)
	if err != nil {
		return []doctorItem{{Name: "shadow_repository", Status: "fail", Detail: "cannot identify workspace: " + err.Error()}}
	}
	shadowDir := filepath.Join(workspace, ".cloud-harness", "snapshots", id)
	shadowInfo, statErr := os.Stat(shadowDir)
	if errors.Is(statErr, os.ErrNotExist) {
		return []doctorItem{{Name: "shadow_repository", Status: "ok", Detail: "not initialized"}}
	}
	if statErr != nil {
		return []doctorItem{{Name: "shadow_repository", Status: "fail", Detail: "shadow repository cannot be inspected: " + statErr.Error()}}
	}
	if !shadowInfo.IsDir() {
		return []doctorItem{{Name: "shadow_repository", Status: "fail", Detail: "shadow repository path is not a directory"}}
	}
	head, err := os.ReadFile(filepath.Join(shadowDir, "HEAD"))
	if err != nil {
		return []doctorItem{{Name: "shadow_repository", Status: "fail", Detail: "HEAD cannot be read: " + err.Error()}}
	}
	if strings.TrimSpace(string(head)) == "" {
		return []doctorItem{{Name: "shadow_repository", Status: "fail", Detail: "HEAD is empty"}}
	}
	return []doctorItem{{Name: "shadow_repository", Status: "ok", Detail: "HEAD is readable"}}
}

func checkProvider(cfg config.Config) doctorItem {
	switch cfg.Provider {
	case "mock":
		return doctorItem{Name: "provider", Status: "ok", Detail: "mock provider is available"}
	case "openai":
		if strings.TrimSpace(cfg.OpenAIAPIKey) == "" {
			return doctorItem{Name: "provider", Status: "warn", Detail: "OPENAI_API_KEY is not set; OpenAI connectivity probe skipped"}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		client := &http.Client{Timeout: 3 * time.Second}
		probe := &provider.OpenAICompatible{BaseURL: cfg.OpenAIBaseURL, APIKey: cfg.OpenAIAPIKey, Client: client}
		if err := probe.Probe(ctx); err != nil {
			return doctorItem{Name: "provider", Status: "fail", Detail: "GET /models failed: " + err.Error()}
		}
		return doctorItem{Name: "provider", Status: "ok", Detail: "GET /models succeeded"}
	default:
		return doctorItem{Name: "provider", Status: "fail", Detail: "field provider: unsupported provider"}
	}
}

func scrubDoctor(s string) string {
	key := os.Getenv("OPENAI_API_KEY")
	if key != "" {
		s = strings.ReplaceAll(s, key, "[redacted]")
	}
	return s
}

type statsRow struct {
	Group        string  `json:"group"`
	ModelCalls   int     `json:"model_calls"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CachedTokens int64   `json:"cached_tokens"`
	CacheHitRate float64 `json:"cache_hit_rate"`
	Cost         any     `json:"cost"`
}

type statsReport struct {
	By       string     `json:"by"`
	Days     int        `json:"days"`
	Since    string     `json:"since"`
	Results  []statsRow `json:"results"`
	Totals   statsRow   `json:"totals"`
	Warnings []string   `json:"warnings"`
}

type statsAccumulator struct {
	row         statsRow
	cost        float64
	costCovered bool
}

func cmdStats(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "sqlite database path")
	days := fs.Int("days", 30, "include usage from the last N days")
	by := fs.String("by", "day", "aggregate by day, model, or run")
	jsonOutput := fs.Bool("json", false, "write JSON report")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "stats does not accept positional arguments")
		return 2
	}
	if *days < 1 {
		fmt.Fprintln(stderr, "--days must be at least 1")
		return 2
	}
	if *by != "day" && *by != "model" && *by != "run" {
		fmt.Fprintln(stderr, "--by must be day, model, or run")
		return 2
	}
	common := &commonFlags{db: *dbPath}
	cfg, err := load(common)
	if err != nil {
		fmt.Fprintln(stderr, scrubDoctor(configErrorDetail(err)))
		return 1
	}
	if cfg.Provider != "mock" && cfg.Provider != "openai" {
		fmt.Fprintln(stderr, "field provider: must be mock or openai")
		return 1
	}
	since := time.Now().UTC().AddDate(0, 0, -*days)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	records, err := sqlite.ReadUsage(ctx, cfg.DBPath, since)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	report := aggregateStats(records, *by, *days, since, cfg)
	if *jsonOutput {
		return writeJSON(stdout, report)
	}
	fmt.Fprintf(stdout, "Stats by %s, last %d days (since %s UTC)\n", report.By, report.Days, report.Since)
	fmt.Fprintln(stdout, "GROUP\tMODEL CALLS\tINPUT\tOUTPUT\tCACHED\tCACHE HIT\tCOST")
	for _, row := range report.Results {
		fmt.Fprintf(stdout, "%s\t%d\t%d\t%d\t%d\t%.1f%%\t%s\n", row.Group, row.ModelCalls, row.InputTokens, row.OutputTokens, row.CachedTokens, row.CacheHitRate*100, formatCost(row.Cost))
	}
	fmt.Fprintf(stdout, "TOTAL\t%d\t%d\t%d\t%d\t%.1f%%\t%s\n", report.Totals.ModelCalls, report.Totals.InputTokens, report.Totals.OutputTokens, report.Totals.CachedTokens, report.Totals.CacheHitRate*100, formatCost(report.Totals.Cost))
	for _, warning := range report.Warnings {
		fmt.Fprintln(stdout, "warn: "+warning)
	}
	return 0
}

func aggregateStats(records []sqlite.UsageRecord, by string, days int, since time.Time, cfg config.Config) statsReport {
	buckets := map[string]*statsAccumulator{}
	totals := &statsAccumulator{row: statsRow{Group: "total"}, costCovered: len(records) > 0}
	for _, record := range records {
		model := attributedModel(record.Provider, cfg)
		group := model
		switch by {
		case "day":
			group = record.CreatedAt.UTC().Format("2006-01-02")
		case "run":
			group = record.RunID
		}
		bucket := buckets[group]
		if bucket == nil {
			bucket = &statsAccumulator{row: statsRow{Group: group}, costCovered: true}
			buckets[group] = bucket
		}
		addUsage(bucket, record, model, cfg.ModelPrices)
		addUsage(totals, record, model, cfg.ModelPrices)
	}
	groups := make([]string, 0, len(buckets))
	for group := range buckets {
		groups = append(groups, group)
	}
	sort.Strings(groups)
	rows := make([]statsRow, 0, len(groups))
	for _, group := range groups {
		rows = append(rows, finishStatsRow(buckets[group]))
	}
	total := finishStatsRow(totals)
	warnings := make([]string, 0, 1)
	if total.InputTokens > cacheWarningInputTokenThreshold && total.CacheHitRate < 0.2 {
		warnings = append(warnings, fmt.Sprintf("cache hit rate %.1f%% is below 20%% with %d input tokens (threshold: >%d); prompt prefix may be unstable", total.CacheHitRate*100, total.InputTokens, cacheWarningInputTokenThreshold))
	}
	return statsReport{By: by, Days: days, Since: since.Format("2006-01-02T15:04:05"), Results: rows, Totals: total, Warnings: warnings}
}

func addUsage(bucket *statsAccumulator, record sqlite.UsageRecord, model string, prices map[string]config.ModelPrice) {
	bucket.row.ModelCalls++
	bucket.row.InputTokens += int64(record.Usage.InputTokens)
	bucket.row.OutputTokens += int64(record.Usage.OutputTokens)
	bucket.row.CachedTokens += int64(record.Usage.CachedInputTokens)
	price, ok := prices[model]
	if !ok {
		bucket.costCovered = false
		return
	}
	uncached := record.Usage.InputTokens - record.Usage.CachedInputTokens
	if uncached < 0 {
		uncached = 0
	}
	bucket.cost += (float64(uncached)*price.Input + float64(record.Usage.OutputTokens)*price.Output + float64(record.Usage.CachedInputTokens)*price.Cached) / 1_000_000
}

func finishStatsRow(bucket *statsAccumulator) statsRow {
	row := bucket.row
	if row.InputTokens > 0 {
		row.CacheHitRate = float64(row.CachedTokens) / float64(row.InputTokens)
	}
	if bucket.costCovered {
		row.Cost = bucket.cost
	} else {
		row.Cost = "—"
	}
	return row
}

func attributedModel(providerName string, cfg config.Config) string {
	if providerName == "openai" && cfg.OpenAIModel != "" {
		return cfg.OpenAIModel
	}
	if strings.TrimSpace(providerName) == "" {
		return "unknown"
	}
	return providerName
}

func formatCost(cost any) string {
	if text, ok := cost.(string); ok {
		return text
	}
	if value, ok := cost.(float64); ok {
		return strconv.FormatFloat(value, 'f', -1, 64)
	}
	return "—"
}
