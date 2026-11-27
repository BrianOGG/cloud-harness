package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/config"
	"github.com/BrianOGG/cloud-harness/internal/domain"
)

type exportDocument struct {
	Run    map[string]any `json:"run"`
	Usage  map[string]any `json:"usage"`
	Events []exportRecord `json:"events"`
}

type exportRecord struct {
	Timestamp string         `json:"timestamp"`
	Sequence  int64          `json:"sequence,omitempty"`
	Type      string         `json:"type"`
	Role      string         `json:"role,omitempty"`
	Content   string         `json:"content,omitempty"`
	Data      map[string]any `json:"data,omitempty"`

	sortTime time.Time
	order    int
}

var (
	exportLongTokenRE    = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}\b`)
	exportHsurrRE        = regexp.MustCompile(`(?i)hsurr:[^\s,;}\]\)"']+`)
	exportBearerRE       = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/-]+=*`)
	exportAuthHeaderRE   = regexp.MustCompile(`(?im)(\bauthorization\s*:\s*)[^\r\n]+`)
	exportAuthJSONRE     = regexp.MustCompile(`(?i)(authorization["']?\s*[:=]\s*")([^"]*)(")`)
	exportAuthJSONSRE    = regexp.MustCompile(`(?i)(authorization["']?\s*[:=]\s*')([^']*)(')`)
	exportSensitiveKeyRE = regexp.MustCompile(`(?i)(?:^|[_-])(?:api[_-]?key|apikey|token|secret|password|authorization)(?:$|[_-])`)
	exportSecretJSONRE   = regexp.MustCompile(`(?i)((?:api[_-]?key|apikey|token|secret|password)["']?\s*[:=]\s*")([^"]*)(")`)
	exportSecretJSONSRE  = regexp.MustCompile(`(?i)((?:api[_-]?key|apikey|token|secret|password)["']?\s*[:=]\s*')([^']*)(')`)
	exportSecretBareRE   = regexp.MustCompile(`(?i)((?:api[_-]?key|apikey|token|secret|password)["']?\s*[:=]\s*)([^,\s;&}\]"']+)`)
)

func cmdExport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "sqlite database path")
	outPath := fs.String("out", "", "write export to a new file")
	sanitize := fs.Bool("sanitize", true, "redact sensitive strings (default)")
	raw := fs.Bool("raw", false, "disable redaction")
	force := fs.Bool("force", false, "overwrite the output file")
	jsonOutput := fs.Bool("json", false, "emit structured JSON instead of Markdown")

	var runID string
	parseArgs := args
	if len(parseArgs) > 0 && !strings.HasPrefix(parseArgs[0], "-") {
		runID = parseArgs[0]
		parseArgs = parseArgs[1:]
	}
	if err := fs.Parse(parseArgs); err != nil {
		return 2
	}
	if runID != "" {
		if fs.NArg() != 0 {
			_, _ = fmt.Fprintln(stderr, "exactly one run id is required")
			return 2
		}
	} else if fs.NArg() == 1 {
		runID = fs.Arg(0)
	} else {
		_, _ = fmt.Fprintln(stderr, "run id required")
		return 2
	}
	if *raw && visitedFlag(fs, "sanitize") {
		_, _ = fmt.Fprintln(stderr, "--sanitize and --raw are mutually exclusive")
		return 2
	}
	if !*sanitize && !*raw {
		_, _ = fmt.Fprintln(stderr, "use --raw to disable redaction")
		return 2
	}
	if *force && strings.TrimSpace(*outPath) == "" {
		_, _ = fmt.Fprintln(stderr, "--force requires --out")
		return 2
	}
	redact := *sanitize && !*raw
	if !redact {
		_, _ = fmt.Fprintln(stderr, "exporting without redaction")
	}

	c := &commonFlags{db: *dbPath}
	cfg, err := load(c)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if strings.TrimSpace(*outPath) != "" && exportOutputTargetsDatabase(*outPath, cfg.DBPath) {
		_, _ = fmt.Fprintln(stderr, "--out cannot target the database or its SQLite sidecars")
		return 2
	}
	ctx := context.Background()
	db, cleanupSnapshot, err := openExportReadOnly(ctx, cfg.DBPath)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	defer cleanupSnapshot()
	defer func() { _ = db.Close() }()

	doc, err := readExport(ctx, db, runID, cfg)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if redact {
		data, err = redactExportJSON(data, cfg.OpenAIAPIKey)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
	}
	if !*jsonOutput {
		data, err = renderExportMarkdown(data)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
	}
	data = append(data, '\n')
	if strings.TrimSpace(*outPath) == "" {
		if _, err := stdout.Write(data); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	if err := writeExportFile(*outPath, data, *force); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func visitedFlag(fs *flag.FlagSet, name string) bool {
	visited := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			visited = true
		}
	})
	return visited
}

func openExportReadOnly(ctx context.Context, path string) (*sql.DB, func(), error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, err
	}
	tempDir, err := os.MkdirTemp("", "cloud-harness-export-")
	if err != nil {
		return nil, nil, fmt.Errorf("create temporary export snapshot: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tempDir) }
	copyPath := filepath.Join(tempDir, "export.db")
	if err := copyStableDatabase(abs, copyPath); err != nil {
		cleanup()
		return nil, nil, err
	}
	// SQLite may create temporary WAL index files beside this isolated copy.
	// The source database and its sidecars are never opened by SQLite.
	db, err := sql.Open("sqlite", copyPath)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		cleanup()
		return nil, nil, fmt.Errorf("open database snapshot: %w", err)
	}
	return db, cleanup, nil
}

func copyStableDatabase(sourcePath, destinationPath string) error {
	paths := []string{sourcePath, sourcePath + "-wal"}
	for attempt := 0; attempt < 3; attempt++ {
		before, err := readDatabaseSnapshot(paths)
		if err != nil {
			return fmt.Errorf("read database snapshot: %w", err)
		}
		after, err := readDatabaseSnapshot(paths)
		if err != nil {
			return fmt.Errorf("read database snapshot: %w", err)
		}
		if !equalDatabaseSnapshot(before, after) {
			continue
		}
		if !before[0].exists {
			return fmt.Errorf("database %q does not exist", sourcePath)
		}
		copied := make([]databaseSnapshotFile, len(paths))
		stableCopy := true
		for i, path := range paths {
			name := destinationPath
			if i == 1 {
				name += "-wal"
			}
			_ = os.Remove(name)
			if !before[i].exists {
				continue
			}
			copied[i], stableCopy, err = copyDatabaseFile(path, name)
			if err != nil {
				return fmt.Errorf("write temporary database snapshot: %w", err)
			}
			if !stableCopy {
				break
			}
		}
		after, err = readDatabaseSnapshot(paths)
		if err != nil {
			return fmt.Errorf("verify database snapshot: %w", err)
		}
		if stableCopy && equalDatabaseSnapshot(before, copied) && equalDatabaseSnapshot(before, after) {
			return nil
		}
	}
	return fmt.Errorf("database %q changed while preparing export; retry when writes have stopped", sourcePath)
}

type databaseSnapshotFile struct {
	exists bool
	size   int64
	digest [sha256.Size]byte
}

func readDatabaseSnapshot(paths []string) ([]databaseSnapshotFile, error) {
	files := make([]databaseSnapshotFile, len(paths))
	for i, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		before, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		hash := sha256.New()
		size, err := io.Copy(hash, f)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		after, err := f.Stat()
		closeErr := f.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if size != before.Size() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
			return nil, fmt.Errorf("%q changed while reading", path)
		}
		var digest [sha256.Size]byte
		copy(digest[:], hash.Sum(nil))
		files[i] = databaseSnapshotFile{exists: true, size: size, digest: digest}
	}
	return files, nil
}

func copyDatabaseFile(sourcePath, destinationPath string) (databaseSnapshotFile, bool, error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		return databaseSnapshotFile{}, false, err
	}
	before, err := source.Stat()
	if err != nil {
		_ = source.Close()
		return databaseSnapshotFile{}, false, err
	}
	destination, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		_ = source.Close()
		return databaseSnapshotFile{}, false, err
	}
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(destination, hash), source)
	after, statErr := source.Stat()
	sourceCloseErr := source.Close()
	destinationCloseErr := destination.Close()
	if copyErr != nil {
		return databaseSnapshotFile{}, false, copyErr
	}
	if statErr != nil {
		return databaseSnapshotFile{}, false, statErr
	}
	if sourceCloseErr != nil {
		return databaseSnapshotFile{}, false, sourceCloseErr
	}
	if destinationCloseErr != nil {
		return databaseSnapshotFile{}, false, destinationCloseErr
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	fingerprint := databaseSnapshotFile{exists: true, size: size, digest: digest}
	stable := size == before.Size() && before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
	return fingerprint, stable, nil
}

func equalDatabaseSnapshot(a, b []databaseSnapshotFile) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func readExport(ctx context.Context, db *sql.DB, runID string, cfg config.Config) (exportDocument, error) {
	var (
		parentID, sessionID, turnID, workspaceID     string
		status, waitReason, created, updated, errRaw string
	)
	if err := db.QueryRowContext(ctx, `
		SELECT parent_run_id, session_id, turn_id, workspace_id, status, wait_reason, error_json, created_at, updated_at
		FROM runs WHERE id = ?`, runID).Scan(&parentID, &sessionID, &turnID, &workspaceID, &status, &waitReason, &errRaw, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return exportDocument{}, fmt.Errorf("run %q not found", runID)
		}
		return exportDocument{}, fmt.Errorf("read run %q: %w", runID, err)
	}
	startedAt, err := parseExportTime(created)
	if err != nil {
		return exportDocument{}, fmt.Errorf("decode run start time: %w", err)
	}
	updatedAt, err := parseExportTime(updated)
	if err != nil {
		return exportDocument{}, fmt.Errorf("decode run update time: %w", err)
	}
	var runError any
	if errRaw != "" && errRaw != "null" {
		runError, err = decodeExportJSON([]byte(errRaw))
		if err != nil {
			return exportDocument{}, fmt.Errorf("decode run error: %w", err)
		}
	}
	run := map[string]any{
		"id": runID, "status": status, "wait_reason": waitReason,
		"parent_run_id": parentID, "session_id": sessionID, "turn_id": turnID,
		"workspace_id": workspaceID, "started_at": startedAt, "updated_at": updatedAt,
		"error": runError,
	}
	if status == string(domain.RunCompleted) || status == string(domain.RunFailed) || status == string(domain.RunCancelled) {
		run["ended_at"] = updatedAt
	}

	doc := exportDocument{Run: run, Usage: map[string]any{}, Events: make([]exportRecord, 0)}
	add := func(at string, seq int64, typ, role, content string, data map[string]any) error {
		when, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return fmt.Errorf("decode export timestamp %q: %w", at, err)
		}
		doc.Events = append(doc.Events, exportRecord{
			Timestamp: at, Sequence: seq, Type: typ, Role: role, Content: content, Data: data,
			sortTime: when, order: len(doc.Events),
		})
		return nil
	}

	if turnID != "" {
		var input, turnCreated string
		err := db.QueryRowContext(ctx, `SELECT user_input, created_at FROM turns WHERE id = ?`, turnID).Scan(&input, &turnCreated)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return exportDocument{}, fmt.Errorf("read user message: %w", err)
		}
		if err == nil {
			at, err := parseExportTime(turnCreated)
			if err != nil {
				return exportDocument{}, fmt.Errorf("decode user message time: %w", err)
			}
			if err := add(at, 0, "message.user", "user", input, nil); err != nil {
				return exportDocument{}, err
			}
		}
	}

	providers := make(map[string]struct{})
	usageTotal := domain.Usage{}
	responseCount := 0
	responseRows, err := db.QueryContext(ctx, `
		SELECT id, provider, usage_json, created_at
		FROM model_responses WHERE run_id = ? ORDER BY created_at ASC, id ASC`, runID)
	if err != nil {
		return exportDocument{}, fmt.Errorf("read model responses: %w", err)
	}
	type responseRow struct {
		id, provider, usage, createdAt string
	}
	var responses []responseRow
	for responseRows.Next() {
		var row responseRow
		if err := responseRows.Scan(&row.id, &row.provider, &row.usage, &row.createdAt); err != nil {
			_ = responseRows.Close()
			return exportDocument{}, fmt.Errorf("read model response row: %w", err)
		}
		responses = append(responses, row)
	}
	if err := responseRows.Err(); err != nil {
		_ = responseRows.Close()
		return exportDocument{}, err
	}
	_ = responseRows.Close()
	for _, response := range responses {
		providers[response.provider] = struct{}{}
		responseCount++
		var usage domain.Usage
		if err := json.Unmarshal([]byte(response.usage), &usage); err != nil {
			return exportDocument{}, fmt.Errorf("decode model usage %s: %w", response.id, err)
		}
		usageTotal.InputTokens += usage.InputTokens
		usageTotal.OutputTokens += usage.OutputTokens
		usageTotal.CachedInputTokens += usage.CachedInputTokens
		usageTotal.CacheCreationInputTokens += usage.CacheCreationInputTokens
		usageTotal.ReasoningTokens += usage.ReasoningTokens
		usageTotal.CacheReported = usageTotal.CacheReported || usage.CacheReported
		usageTotal.ReasoningReported = usageTotal.ReasoningReported || usage.ReasoningReported
		usageTotal.LatencyMS += usage.LatencyMS
		responseAt, err := parseExportTime(response.createdAt)
		if err != nil {
			return exportDocument{}, fmt.Errorf("decode model response time: %w", err)
		}
		itemRows, err := db.QueryContext(ctx, `
			SELECT ordinal, kind, content_json FROM model_response_items
			WHERE response_id = ? ORDER BY ordinal ASC`, response.id)
		if err != nil {
			return exportDocument{}, fmt.Errorf("read model response items: %w", err)
		}
		for itemRows.Next() {
			var ordinal int
			var kind, raw string
			if err := itemRows.Scan(&ordinal, &kind, &raw); err != nil {
				_ = itemRows.Close()
				return exportDocument{}, err
			}
			var item map[string]any
			if err := json.Unmarshal([]byte(raw), &item); err != nil {
				item = map[string]any{"raw": raw}
			}
			if kind == string(domain.ItemOpaque) || item["opaque_ref"] == true {
				continue
			}
			role := "assistant"
			typ := "message.assistant"
			content, _ := item["text"].(string)
			if kind == string(domain.ItemToolCall) {
				typ = "tool.call"
				role = "tool_call"
			} else if kind == string(domain.ItemToolResult) {
				typ = "tool.result"
				role = "tool_result"
			}
			if kind == string(domain.ItemUsage) {
				continue
			}
			if kind == string(domain.ItemToolCall) || kind == string(domain.ItemToolResult) {
				content = ""
			}
			data := map[string]any{"response_id": response.id, "ordinal": ordinal}
			if kind != string(domain.ItemText) {
				data["item"] = item
			}
			if kind == string(domain.ItemToolCall) {
				data["provider"] = response.provider
			}
			if err := add(responseAt, 0, typ, role, content, data); err != nil {
				_ = itemRows.Close()
				return exportDocument{}, err
			}
		}
		if err := itemRows.Err(); err != nil {
			_ = itemRows.Close()
			return exportDocument{}, err
		}
		_ = itemRows.Close()
	}
	providerList := make([]string, 0, len(providers))
	for name := range providers {
		providerList = append(providerList, name)
	}
	sort.Strings(providerList)
	doc.Run["providers"] = providerList
	doc.Run["provider"] = strings.Join(providerList, ", ")
	model, modelSource := "unknown (not persisted)", "not_persisted"
	if len(providerList) == 1 && providerList[0] == "openai" && cfg.OpenAIModel != "" {
		model, modelSource = cfg.OpenAIModel, "current_configuration"
	} else if len(providerList) == 1 && providerList[0] != "openai" {
		model = providerList[0]
		modelSource = "provider_name"
	}
	doc.Run["model"] = model
	doc.Run["model_source"] = modelSource
	doc.Usage = map[string]any{
		"model_calls":                 responseCount,
		"input_tokens":                usageTotal.InputTokens,
		"output_tokens":               usageTotal.OutputTokens,
		"cached_input_tokens":         usageTotal.CachedInputTokens,
		"cache_creation_input_tokens": usageTotal.CacheCreationInputTokens,
		"reasoning_tokens":            usageTotal.ReasoningTokens,
		"cache_reported":              usageTotal.CacheReported,
		"reasoning_reported":          usageTotal.ReasoningReported,
		"latency_ms":                  usageTotal.LatencyMS,
	}

	invocationTimes := make(map[string]string)
	batchTimes := make(map[string]string)
	eventRows, err := db.QueryContext(ctx, `
		SELECT event_id, seq, invocation_id, type, payload_json, created_at
		FROM events WHERE run_id = ? ORDER BY seq ASC`, runID)
	if err != nil {
		return exportDocument{}, fmt.Errorf("read run events: %w", err)
	}
	for eventRows.Next() {
		var id, invocationID, typ, payload, createdAt string
		var seq int64
		if err := eventRows.Scan(&id, &seq, &invocationID, &typ, &payload, &createdAt); err != nil {
			_ = eventRows.Close()
			return exportDocument{}, err
		}
		at, err := parseExportTime(createdAt)
		if err != nil {
			_ = eventRows.Close()
			return exportDocument{}, fmt.Errorf("decode event time: %w", err)
		}
		value, err := decodeExportJSON([]byte(payload))
		if err != nil {
			_ = eventRows.Close()
			return exportDocument{}, fmt.Errorf("decode event %s: %w", id, err)
		}
		if invocationID != "" {
			invocationTimes[invocationID] = at
		}
		if data, ok := value.(map[string]any); ok {
			if batchID, ok := data["batch_id"].(string); ok && batchID != "" {
				batchTimes[batchID] = at
			}
		}
		var payloadMap map[string]any
		if m, ok := value.(map[string]any); ok {
			payloadMap = m
		} else {
			payloadMap = map[string]any{"value": value}
		}
		if err := add(at, seq, "event."+typ, "event", "", map[string]any{"event_id": id, "payload": payloadMap}); err != nil {
			_ = eventRows.Close()
			return exportDocument{}, err
		}
	}
	if err := eventRows.Err(); err != nil {
		_ = eventRows.Close()
		return exportDocument{}, err
	}
	_ = eventRows.Close()

	invRows, err := db.QueryContext(ctx, `
		SELECT id, batch_id, provider_call_id, tool_name, args_json, class, status, dispatched, result_json
		FROM tool_invocations WHERE run_id = ? ORDER BY rowid ASC`, runID)
	if err != nil {
		return exportDocument{}, fmt.Errorf("read tool invocations: %w", err)
	}
	type invocationRow struct {
		id, batchID, callID, name, args, class, status, result string
		dispatched                                             int
	}
	var invocations []invocationRow
	for invRows.Next() {
		var row invocationRow
		if err := invRows.Scan(&row.id, &row.batchID, &row.callID, &row.name, &row.args, &row.class, &row.status, &row.dispatched, &row.result); err != nil {
			_ = invRows.Close()
			return exportDocument{}, err
		}
		invocations = append(invocations, row)
	}
	if err := invRows.Err(); err != nil {
		_ = invRows.Close()
		return exportDocument{}, err
	}
	_ = invRows.Close()
	for _, inv := range invocations {
		at := invocationTimes[inv.id]
		if at == "" {
			at = updatedAt
		}
		args, err := decodeExportJSON([]byte(inv.args))
		if err != nil {
			args = inv.args
		}
		var result any
		if inv.result != "" && inv.result != "null" {
			result, err = decodeExportJSON([]byte(inv.result))
			if err != nil {
				result = inv.result
			}
		}
		if err := add(at, 0, "tool.invocation", "tool_call", "", map[string]any{
			"invocation_id": inv.id, "batch_id": inv.batchID, "provider_call_id": inv.callID,
			"name": inv.name, "arguments": args, "class": inv.class, "status": inv.status,
			"dispatched": inv.dispatched == 1,
		}); err != nil {
			return exportDocument{}, err
		}
		if result != nil {
			if err := add(at, 0, "tool.result", "tool", "", map[string]any{
				"invocation_id": inv.id, "provider_call_id": inv.callID, "name": inv.name, "result": result,
			}); err != nil {
				return exportDocument{}, err
			}
		}
	}

	batchRows, err := db.QueryContext(ctx, `SELECT batch_id, protocol_json FROM tool_batches WHERE run_id = ? ORDER BY rowid ASC`, runID)
	if err != nil {
		return exportDocument{}, fmt.Errorf("read tool batches: %w", err)
	}
	for batchRows.Next() {
		var batchID, protocol string
		if err := batchRows.Scan(&batchID, &protocol); err != nil {
			_ = batchRows.Close()
			return exportDocument{}, err
		}
		if protocol == "" || protocol == "null" {
			continue
		}
		value, err := decodeExportJSON([]byte(protocol))
		if err != nil {
			value = protocol
		}
		at := batchTimes[batchID]
		if at == "" {
			at = updatedAt
		}
		if err := add(at, 0, "tool.protocol_error", "event", "", map[string]any{"batch_id": batchID, "error": value}); err != nil {
			_ = batchRows.Close()
			return exportDocument{}, err
		}
	}
	if err := batchRows.Err(); err != nil {
		_ = batchRows.Close()
		return exportDocument{}, err
	}
	_ = batchRows.Close()

	approvalRows, err := db.QueryContext(ctx, `
		SELECT id, invocation_id, tool, resource_scope, state, valid_until, created_at
		FROM approvals WHERE run_id = ? ORDER BY created_at ASC, id ASC`, runID)
	if err != nil {
		return exportDocument{}, fmt.Errorf("read approvals: %w", err)
	}
	for approvalRows.Next() {
		var id, invocationID, tool, scope, state, validUntil, approvalCreated string
		if err := approvalRows.Scan(&id, &invocationID, &tool, &scope, &state, &validUntil, &approvalCreated); err != nil {
			_ = approvalRows.Close()
			return exportDocument{}, err
		}
		at, err := parseExportTime(approvalCreated)
		if err != nil {
			_ = approvalRows.Close()
			return exportDocument{}, fmt.Errorf("decode approval time: %w", err)
		}
		until := ""
		if validUntil != "" {
			until, err = parseExportTime(validUntil)
			if err != nil {
				_ = approvalRows.Close()
				return exportDocument{}, fmt.Errorf("decode approval expiry: %w", err)
			}
		}
		if err := add(at, 0, "approval", "approval", "", map[string]any{
			"id": id, "invocation_id": invocationID, "tool": tool,
			"resource_scope": scope, "state": state, "valid_until": until,
		}); err != nil {
			_ = approvalRows.Close()
			return exportDocument{}, err
		}
	}
	if err := approvalRows.Err(); err != nil {
		_ = approvalRows.Close()
		return exportDocument{}, err
	}
	_ = approvalRows.Close()

	sort.SliceStable(doc.Events, func(i, j int) bool {
		if !doc.Events[i].sortTime.Equal(doc.Events[j].sortTime) {
			return doc.Events[i].sortTime.Before(doc.Events[j].sortTime)
		}
		if doc.Events[i].Sequence != 0 && doc.Events[j].Sequence != 0 && doc.Events[i].Sequence != doc.Events[j].Sequence {
			return doc.Events[i].Sequence < doc.Events[j].Sequence
		}
		return doc.Events[i].order < doc.Events[j].order
	})
	return doc, nil
}

func parseExportTime(raw string) (string, error) {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return "", err
	}
	return t.UTC().Format(time.RFC3339Nano), nil
}

func decodeExportJSON(raw []byte) (any, error) {
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func redactExportJSON(raw []byte, configuredKey string) ([]byte, error) {
	value, err := decodeExportJSON(raw)
	if err != nil {
		return nil, err
	}
	value = redactExportValue(value, configuredKey)
	return json.MarshalIndent(value, "", "  ")
}

func redactExportValue(value any, configuredKey string) any {
	switch v := value.(type) {
	case string:
		return redactExportText(v, configuredKey)
	case []any:
		for i := range v {
			v[i] = redactExportValue(v[i], configuredKey)
		}
		return v
	case map[string]any:
		for key, item := range v {
			if exportSensitiveKeyRE.MatchString(key) {
				if _, ok := item.(string); ok {
					v[key] = "[redacted]"
					continue
				}
			}
			v[key] = redactExportValue(item, configuredKey)
		}
		return v
	default:
		return value
	}
}

func redactExportText(text, configuredKey string) string {
	if configuredKey != "" {
		text = strings.ReplaceAll(text, configuredKey, "[redacted]")
	}
	text = exportAuthHeaderRE.ReplaceAllString(text, `${1}[redacted]`)
	text = exportAuthJSONRE.ReplaceAllString(text, `${1}[redacted]${3}`)
	text = exportAuthJSONSRE.ReplaceAllString(text, `${1}[redacted]${3}`)
	text = exportSecretJSONRE.ReplaceAllString(text, `${1}[redacted]${3}`)
	text = exportSecretJSONSRE.ReplaceAllString(text, `${1}[redacted]${3}`)
	text = exportSecretBareRE.ReplaceAllString(text, `${1}[redacted]`)
	text = exportBearerRE.ReplaceAllString(text, "Bearer [redacted]")
	text = exportLongTokenRE.ReplaceAllString(text, "[redacted]")
	text = exportHsurrRE.ReplaceAllString(text, "[redacted]")
	return text
}

func renderExportMarkdown(raw []byte) ([]byte, error) {
	var doc struct {
		Run    map[string]any `json:"run"`
		Usage  map[string]any `json:"usage"`
		Events []struct {
			Timestamp string         `json:"timestamp"`
			Sequence  int64          `json:"sequence,omitempty"`
			Type      string         `json:"type"`
			Role      string         `json:"role,omitempty"`
			Content   string         `json:"content,omitempty"`
			Data      map[string]any `json:"data,omitempty"`
		} `json:"events"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Run %v\n\n", doc.Run["id"])
	fmt.Fprintf(&b, "- Status: %v\n", doc.Run["status"])
	fmt.Fprintf(&b, "- Wait reason: %v\n", doc.Run["wait_reason"])
	fmt.Fprintf(&b, "- Started: %v\n", doc.Run["started_at"])
	if ended, ok := doc.Run["ended_at"]; ok {
		fmt.Fprintf(&b, "- Ended: %v\n", ended)
	} else {
		fmt.Fprintf(&b, "- Updated: %v\n", doc.Run["updated_at"])
	}
	fmt.Fprintf(&b, "- Provider: %v\n", doc.Run["provider"])
	fmt.Fprintf(&b, "- Model: %v (%v)\n", doc.Run["model"], doc.Run["model_source"])
	fmt.Fprintf(&b, "- Usage: %v input, %v output, %v cached input, %v cache creation and %v reasoning tokens across %v model calls; %v ms latency\n\n",
		doc.Usage["input_tokens"], doc.Usage["output_tokens"], doc.Usage["cached_input_tokens"],
		doc.Usage["cache_creation_input_tokens"], doc.Usage["reasoning_tokens"], doc.Usage["model_calls"], doc.Usage["latency_ms"])
	if doc.Run["error"] != nil {
		b.WriteString("## Run error\n\n```json\n")
		encoded, err := json.MarshalIndent(doc.Run["error"], "", "  ")
		if err != nil {
			return nil, err
		}
		b.Write(encoded)
		b.WriteString("\n```\n\n")
	}
	b.WriteString("## Timeline\n")
	for _, event := range doc.Events {
		label := event.Type
		if event.Role != "" && event.Role != "event" {
			label += " (" + event.Role + ")"
		}
		fmt.Fprintf(&b, "\n### %s | %s", event.Timestamp, label)
		if event.Sequence != 0 {
			fmt.Fprintf(&b, " | seq %d", event.Sequence)
		}
		b.WriteString("\n\n")
		if event.Content != "" {
			b.WriteString("```text\n")
			b.WriteString(event.Content)
			b.WriteString("\n```\n")
		}
		if event.Data != nil {
			encoded, err := json.MarshalIndent(event.Data, "", "  ")
			if err != nil {
				return nil, err
			}
			b.WriteString("```json\n")
			b.Write(encoded)
			b.WriteString("\n```\n")
		}
	}
	return []byte(strings.TrimRight(b.String(), "\n")), nil
}

func writeExportFile(path string, data []byte, force bool) error {
	flags := os.O_WRONLY | os.O_CREATE
	if force {
		flags |= os.O_TRUNC
	} else {
		flags |= os.O_EXCL
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("output file %q already exists (use --force to overwrite)", path)
		}
		return fmt.Errorf("create export file %q: %w", path, err)
	}
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil {
		return fmt.Errorf("write export file %q: %w", path, writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close export file %q: %w", path, closeErr)
	}
	return nil
}

func exportOutputTargetsDatabase(outputPath, databasePath string) bool {
	outputAbs, err := filepath.Abs(outputPath)
	if err != nil {
		return false
	}
	for _, sourcePath := range []string{databasePath, databasePath + "-wal", databasePath + "-shm", databasePath + "-journal"} {
		sourceAbs, err := filepath.Abs(sourcePath)
		if err != nil {
			continue
		}
		if filepath.Clean(outputAbs) == filepath.Clean(sourceAbs) {
			return true
		}
		outputInfo, outputErr := os.Stat(outputAbs)
		sourceInfo, sourceErr := os.Stat(sourceAbs)
		if outputErr == nil && sourceErr == nil && os.SameFile(outputInfo, sourceInfo) {
			return true
		}
	}
	return false
}
