package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/agent"
	"github.com/cloud-harness/cloud-harness/internal/config"
	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/eval"
	"github.com/cloud-harness/cloud-harness/internal/program"
	"github.com/cloud-harness/cloud-harness/internal/provider"
	"github.com/cloud-harness/cloud-harness/internal/storage"
	"github.com/cloud-harness/cloud-harness/internal/storage/sqlite"
	"github.com/cloud-harness/cloud-harness/internal/tools"
	"github.com/cloud-harness/cloud-harness/internal/trace"
	"github.com/cloud-harness/cloud-harness/internal/transport"
)

const version = "0.5.0-m5"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "worker" {
		program.ServeWorker(os.Stdin, os.Stdout)
		return
	}
	os.Exit(Main(os.Args[1:], os.Stdout, os.Stderr))
}

// Main runs one CLI invocation. It is split from main so tests can call it.
func Main(args []string, stdout, stderr io.Writer) int {
	return MainWithIO(args, os.Stdin, stdout, stderr)
}

// MainWithIO runs one CLI invocation with an injectable stdin reader.
func MainWithIO(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version", "-v", "--version":
		fmt.Fprintf(stdout, "cloud-harness %s %s\n", version, config.DefaultListen)
		return 0
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, usage)
		return 0
	case "serve":
		return cmdServe(args[1:], stdout, stderr)
	case "run":
		return cmdRun(args[1:], stdout, stderr)
	case "chat":
		return cmdChat(args[1:], stdin, stdout, stderr)
	case "runs":
		return cmdRuns(args[1:], stdout, stderr)
	case "trace":
		return cmdTrace(args[1:], stdout, stderr)
	case "isolation":
		return cmdIsolation(stdout, stderr)
	case "program":
		return cmdProgram(args[1:], stdout, stderr)
	case "eval":
		return cmdEval(args[1:], stdout, stderr)
	case "retain":
		return cmdRetain(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n%s\n", args[0], usage)
		return 2
	}
}

const usage = `Cloud Harness M5

  cloud-harness version
  cloud-harness serve [--addr 127.0.0.1:19421] [--db path] [--workspace dir] [--provider mock|openai]
  cloud-harness run --message TEXT [--db path] [--workspace dir] [--provider mock|openai]
  cloud-harness chat [--message TEXT] [--session ID] [--db path] [--workspace dir] [--provider mock|openai]
  cloud-harness eval run --task TASK [--provider mock|openai] [--db path] [--workspace dir]
  cloud-harness eval suite [--db path] [--workspace dir] [--out path] [--compare path]
  cloud-harness runs list [--session ID] [--db path]
  cloud-harness runs inspect RUN_ID
  cloud-harness runs cancel RUN_ID
  cloud-harness runs recover [RUN_ID | --all-expired] [--db path] [--workspace dir]
  cloud-harness runs resume --decision approve|deny|child_done|reconcile_success|reconcile_fail [--answer TEXT] [--extra-model-calls N] RUN_ID
  cloud-harness trace RUN_ID
  cloud-harness retain [--days N] [--prune-runs-days N] [--db path]
  cloud-harness isolation
  cloud-harness program check

Flags for runs resume come before the run id.
The API key is read only from OPENAI_API_KEY. Default provider is mock.
Default listen address is 127.0.0.1:19421.`

type commonFlags struct {
	db                string
	provider          string
	addr              string
	workspace         string
	httpToken         string
	httpAllowInsecure bool
}

func addCommon(fs *flag.FlagSet) *commonFlags {
	c := &commonFlags{}
	fs.StringVar(&c.db, "db", "", "sqlite database path")
	fs.StringVar(&c.provider, "provider", "", "mock or openai")
	fs.StringVar(&c.addr, "addr", "", "listen address")
	fs.StringVar(&c.workspace, "workspace", "", "workspace root for file and command tools")
	fs.StringVar(&c.httpToken, "http-token", "", "bearer token for HTTP API")
	fs.BoolVar(&c.httpAllowInsecure, "http-allow-insecure", false, "allow non-loopback HTTP bind without http_token")
	return c
}

func load(c *commonFlags) (config.Config, error) {
	cfg, err := config.Load("", nil)
	if err != nil {
		return config.Config{}, err
	}
	if c != nil {
		if c.db != "" {
			cfg.DBPath = c.db
		}
		if c.provider != "" {
			cfg.Provider = c.provider
		}
		if c.addr != "" {
			cfg.ListenAddr = c.addr
		}
		if c.httpToken != "" {
			cfg.HTTPToken = c.httpToken
		}
		if c.httpAllowInsecure {
			cfg.HTTPAllowInsecure = true
		}
	}
	if err := config.ValidateHTTPToken(cfg.HTTPToken); err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

func openManager(ctx context.Context, cfg config.Config, workspace string) (*agent.Manager, *sqlite.DB, error) {
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o755); err != nil && filepath.Dir(cfg.DBPath) != "." {
		return nil, nil, err
	}
	db, err := sqlite.Open(ctx, cfg.DBPath)
	if err != nil {
		return nil, nil, err
	}
	prov, err := buildProvider(cfg)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	if workspace == "" {
		workspace = "."
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	broker := tools.NewBroker(db, db)
	broker.SetCommandEnvAllow(cfg.CommandEnvAllow)
	if err := tools.RegisterLocal(broker, abs); err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return &agent.Manager{Store: db, Provider: prov, OwnerID: cfg.OwnerID, Broker: broker, Workspace: abs}, db, nil
}

func cmdEvalSuite(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("eval suite", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := addCommon(fs)
	outPath := fs.String("out", "", "write suite report JSON to file")
	comparePath := fs.String("compare", "", "compare suite report against baseline JSON file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := load(c)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if c.provider == "" {
		cfg.Provider = "mock"
	}
	if c.db == "" {
		cfg.DBPath = filepath.Join(os.TempDir(), "cloud-harness-eval-suite.db")
	}
	var baseline eval.SuiteReport
	hasCompare := strings.TrimSpace(*comparePath) != ""
	if hasCompare {
		baseline, err = eval.LoadSuiteReport(*comparePath)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	report, err := eval.Suite(context.Background(), cfg, c.workspace)
	if err != nil {
		_ = writeJSON(stdout, report)
		fmt.Fprintln(stderr, err)
		return 1
	}
	var cmp eval.SuiteComparison
	if hasCompare {
		cmp = eval.CompareSuiteReports(baseline, report)
	}
	if strings.TrimSpace(*outPath) != "" {
		if werr := eval.WriteSuiteReport(*outPath, report); werr != nil {
			fmt.Fprintln(stderr, werr)
			return 1
		}
	}
	if hasCompare {
		report.Comparison = &cmp
		fmt.Fprintln(stderr, cmp.Summary)
	}
	_ = writeJSON(stdout, report)
	if hasCompare {
		if cmp.HasRegression {
			return 1
		}
		return 0
	}
	if report.Passed != report.Tasks {
		return 1
	}
	return 0
}

func buildProvider(cfg config.Config) (provider.Provider, error) {
	switch cfg.Provider {
	case "", "mock":
		return &provider.Mock{}, nil
	case "openai":
		return &provider.OpenAICompatible{
			BaseURL:               cfg.OpenAIBaseURL,
			APIKey:                cfg.OpenAIAPIKey,
			Model:                 cfg.OpenAIModel,
			ReasoningReplay:       cfg.OpenAIReasoningReplay,
			PrivatePassthrough:    cfg.OpenAIPrivatePassthrough,
			ResponseHeaderTimeout: time.Duration(cfg.OpenAIHeaderTimeoutMS) * time.Millisecond,
			IdleTimeout:           time.Duration(cfg.OpenAIIdleTimeoutMS) * time.Millisecond,
			RetryBaseDelay:        time.Duration(cfg.OpenAIRetryBaseMS) * time.Millisecond,
		}, nil
	default:
		return nil, domain.E(domain.CodeInvalidArgument, "unknown provider "+cfg.Provider)
	}
}

func cmdServe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := addCommon(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := load(c)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := cfg.ValidateBind(); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if loopback, _ := config.IsLoopback(cfg.ListenAddr); !loopback && cfg.HTTPToken == "" {
		fmt.Fprintf(stderr, "WARNING: listening on non-loopback address %s without http_token (http_allow_insecure=true); /v1/ endpoints are unauthenticated\n", cfg.ListenAddr)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	mgr, db, err := openManager(ctx, cfg, c.workspace)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer db.Close()
	if _, err := mgr.RecoverExpired(ctx); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           (&transport.Server{Manager: mgr, Token: cfg.HTTPToken}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	safe, _ := json.Marshal(cfg.Redacted())
	fmt.Fprintf(stdout, "listening on http://%s %s\n", cfg.ListenAddr, safe)
	select {
	case <-ctx.Done():
		shut, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
		return 0
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
}

func cmdRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := addCommon(fs)
	message := fs.String("message", "", "user message")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *message == "" {
		fmt.Fprintln(stderr, "--message is required")
		return 2
	}
	cfg, err := load(c)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx := context.Background()
	mgr, db, err := openManager(ctx, cfg, c.workspace)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer db.Close()
	ses, err := mgr.CreateSession(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	run, err := mgr.RunMessage(ctx, ses.ID, *message)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	enc := json.NewEncoder(stdout)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]any{
		"session_id":   ses.ID,
		"run_id":       run.ID,
		"status":       run.Status,
		"wait_reason":  run.WaitReason,
		"final_answer": run.FinalAnswer,
		"error":        run.Error,
	})
	if run.Status == domain.RunFailed {
		return 1
	}
	return 0
}

func cmdRuns(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "runs requires list, inspect, cancel, recover, or resume")
		return 2
	}
	fs := flag.NewFlagSet("runs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := addCommon(fs)
	session := fs.String("session", "", "session id for list")
	decision := fs.String("decision", "", "resume decision: approve, deny, child_done, reconcile_success, reconcile_fail")
	answer := fs.String("answer", "", "answer for an ask_user wait")
	extra := fs.Int("extra-model-calls", 0, "model calls to add when resuming a budget wait")
	allExpired := fs.Bool("all-expired", false, "recover all lease-expired running runs")
	var leadingID string
	parseArgs := args[1:]
	if (args[0] == "recover" || args[0] == "inspect" || args[0] == "cancel") && len(parseArgs) > 0 && !strings.HasPrefix(parseArgs[0], "-") {
		leadingID = parseArgs[0]
		parseArgs = parseArgs[1:]
	}
	if err := fs.Parse(parseArgs); err != nil {
		return 2
	}
	posArgs := fs.Args()
	if leadingID != "" {
		posArgs = append([]string{leadingID}, posArgs...)
	}
	cfg, err := load(c)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx := context.Background()
	mgr, db, err := openManager(ctx, cfg, c.workspace)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer db.Close()
	switch args[0] {
	case "list":
		runs, err := db.ListRuns(ctx, *session)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return writeJSON(stdout, runs)
	case "recover":
		if *allExpired {
			if len(posArgs) != 0 {
				fmt.Fprintln(stderr, "cannot combine --all-expired with a run id")
				return 2
			}
			runs, err := mgr.RecoverExpired(ctx)
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			return writeJSON(stdout, runs)
		}
		if len(posArgs) != 1 {
			fmt.Fprintln(stderr, "run id or --all-expired required")
			return 2
		}
		run, err := mgr.Recover(ctx, posArgs[0])
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return writeJSON(stdout, run)
	case "inspect", "cancel", "resume":
		if len(posArgs) != 1 {
			fmt.Fprintln(stderr, "run id required")
			return 2
		}
		id := posArgs[0]
		switch args[0] {
		case "inspect":
			run, err := db.GetRun(ctx, id)
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			return writeJSON(stdout, run)
		case "cancel":
			run, err := mgr.Cancel(ctx, id)
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			return writeJSON(stdout, run)
		default:
			run, err := mgr.Resume(ctx, id, agent.ResumeInput{Decision: *decision, Answer: *answer, ExtraModelCalls: *extra})
			if err != nil {
				fmt.Fprintln(stderr, err)
				_ = writeJSON(stdout, run)
				return 1
			}
			return writeJSON(stdout, run)
		}
	default:
		fmt.Fprintf(stderr, "unknown runs subcommand %q\n", args[0])
		return 2
	}
}

func cmdTrace(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("trace", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := addCommon(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "run id required")
		return 2
	}
	cfg, err := load(c)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx := context.Background()
	_, db, err := openManager(ctx, cfg, c.workspace)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer db.Close()
	id := fs.Arg(0)
	run, err := db.GetRun(ctx, id)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	responses, err := db.ListVisibleResponses(ctx, id)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	batches, err := db.LoadBatches(ctx, id)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	siblings, err := db.ListRuns(ctx, run.SessionID)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	var children []trace.Node
	for _, other := range siblings {
		if other.ParentRunID == run.ID {
			children = append(children, trace.Node{
				ID: other.ID, ParentID: other.ParentRunID, SessionID: other.SessionID,
				Status: string(other.Status), WaitReason: string(other.WaitReason),
			})
		}
	}
	return writeJSON(stdout, trace.Assemble(run, responses, batches, children))
}

func cmdRetain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("retain", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := addCommon(fs)
	days := fs.Int("days", -1, "delete history index rows older than this many days")
	pruneRunsDays := fs.Int("prune-runs-days", 0, "delete terminal runs and child rows older than this many days")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := load(c)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	n := *days
	if n < 0 {
		n = cfg.RetentionDays
	}
	ctx := context.Background()
	_, db, err := openManager(ctx, cfg, c.workspace)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer db.Close()

	if n <= 0 && *pruneRunsDays <= 0 {
		return writeJSON(stdout, map[string]any{"pruned": 0, "retention_days": n})
	}

	var prunedHistory int
	var cutHistory time.Time
	if n > 0 {
		cutHistory = time.Now().UTC().AddDate(0, 0, -n)
		pruned, err := db.PruneHistory(ctx, cutHistory)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		prunedHistory = pruned
	}

	if *pruneRunsDays <= 0 {
		return writeJSON(stdout, map[string]any{"pruned": prunedHistory, "retention_days": n, "before": cutHistory.Format(time.RFC3339)})
	}

	cutRuns := time.Now().UTC().AddDate(0, 0, -*pruneRunsDays)
	pruneRes, err := db.PruneRuns(ctx, cutRuns)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	out := map[string]any{
		"pruned":            prunedHistory,
		"retention_days":    n,
		"prune_runs_days":   *pruneRunsDays,
		"prune_runs_before": cutRuns.Format(time.RFC3339),
		"pruned_runs":       pruneRes.RunsDeleted,
		"deleted_runs":      pruneRes.RunsDeleted,
		"deleted_details":   pruneRes.Tables,
		"tables":            pruneRes.Tables,
	}
	if n > 0 {
		out["before"] = cutHistory.Format(time.RFC3339)
	}
	if pruneRes.WALCheckpoint != "" {
		out["wal_checkpoint"] = pruneRes.WALCheckpoint
	}
	return writeJSON(stdout, out)
}

func cmdIsolation(stdout, stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	probe, err := program.Isolate(ctx)
	_ = writeJSON(stdout, probe)
	if err != nil || !probe.ProgrammaticEnabled {
		if err != nil {
			fmt.Fprintln(stderr, err)
		}
		return 1
	}
	return 0
}

func cmdProgram(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "check" {
		fmt.Fprintln(stderr, "program check runs one pre-approved echo script in the worker")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	charge := &memoryCharge{b: domain.DefaultBudget()}
	grants := memoryGrants{items: []domain.Grant{{
		ID: "g_echo", Tool: "harness.echo", Op: "call", State: "active", RunID: "diag",
	}}}
	broker := tools.NewBroker(charge, grants)
	broker.Register(tools.Spec{
		Name: "harness.echo", Description: "returns its arguments", Readonly: true, Idempotent: true, SideEffectClass: "none",
		Execute: func(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
			return args, nil
		},
	})
	res, err := program.Execute(ctx, program.Request{Script: `
def main():
    r = broker.invoke("harness.echo", {"ping": True})
    if not r["ok"]:
        return r
    return r["data"]
`}, func(ctx context.Context, tool string, args json.RawMessage) (bool, json.RawMessage, *domain.Error) {
		out, callErr := broker.Invoke(ctx, tools.Request{RunID: "diag", Tool: tool, Args: args})
		if callErr != nil {
			return false, nil, domain.E(domain.CodeToolError, callErr.Error())
		}
		if out.Error != nil {
			return false, out.Data, out.Error
		}
		return out.OK, out.Data, nil
	})
	_ = writeJSON(stdout, map[string]any{"ok": res.OK, "data": json.RawMessage(res.Data), "error": res.Error, "hash": res.Hash, "probe": res.Probe})
	if err != nil || !res.OK {
		if err != nil {
			fmt.Fprintln(stderr, err)
		}
		return 1
	}
	return 0
}

func cmdEval(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "run" && args[0] != "suite") {
		fmt.Fprintln(stderr, "eval run --task TASK [--provider openai] | eval suite")
		return 2
	}
	if args[0] == "suite" {
		return cmdEvalSuite(args[1:], stdout, stderr)
	}
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := addCommon(fs)
	task := fs.String("task", "", "eval task id")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *task == "" {
		fmt.Fprintln(stderr, "--task is required")
		return 2
	}
	cfg, err := load(c)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if c.db == "" {
		cfg.DBPath = filepath.Join(os.TempDir(), "cloud-harness-eval.db")
	}
	ctx := context.Background()
	result, err := eval.Run(ctx, eval.Options{Task: *task, Config: cfg, Workspace: c.workspace})
	_ = writeJSON(stdout, result)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if !result.Pass {
		return 1
	}
	return 0
}

func cmdChat(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := addCommon(fs)
	message := fs.String("message", "", "initial user message")
	session := fs.String("session", "", "session id")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	initMsg := strings.TrimSpace(*message)
	if initMsg == "" && fs.NArg() > 0 {
		initMsg = strings.TrimSpace(strings.Join(fs.Args(), " "))
	}
	cfg, err := load(c)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx := context.Background()
	mgr, db, err := openManager(ctx, cfg, c.workspace)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer db.Close()
	return runChat(ctx, mgr, *session, initMsg, stdin, stdout, stderr)
}

func runChat(ctx context.Context, mgr *agent.Manager, sessionID, initialMessage string, stdin io.Reader, stdout, stderr io.Writer) int {
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	reader := bufio.NewReader(stdin)
	if sessionID == "" {
		ses, err := mgr.CreateSession(ctx)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		sessionID = ses.ID
	} else if _, err := mgr.Store.GetSession(ctx, sessionID); err != nil {
		now := time.Now().UTC()
		if cerr := mgr.Store.CreateSession(ctx, storage.Session{ID: sessionID, CreatedAt: now, UpdatedAt: now}); cerr != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}

	if initialMessage != "" {
		_, code, _ := runChatTurn(ctx, mgr, sessionID, initialMessage, reader, stdout, stderr)
		return code
	}

	for {
		line, eof, err := readChatLine(reader)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		msg := strings.TrimSpace(line)
		if msg == "" {
			if eof {
				return 0
			}
			continue
		}
		_, code, stoppedOnEOF := runChatTurn(ctx, mgr, sessionID, msg, reader, stdout, stderr)
		if code != 0 || stoppedOnEOF || eof {
			return code
		}
	}
}

func runChatTurn(ctx context.Context, mgr *agent.Manager, sessionID, message string, reader *bufio.Reader, stdout, stderr io.Writer) (storage.Run, int, bool) {
	run, err := mgr.RunMessage(ctx, sessionID, message)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return run, 1, false
	}
	for {
		run, err = pollRunState(ctx, mgr, run.ID, 5*time.Millisecond)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return run, 1, false
		}
		if domain.TerminalRun(run.Status) {
			if run.FinalAnswer != "" {
				fmt.Fprintln(stdout, run.FinalAnswer)
			}
			if run.Status == domain.RunFailed {
				if run.Error != nil {
					fmt.Fprintln(stderr, run.Error)
				}
				return run, 1, false
			}
			return run, 0, false
		}
		if run.Status != domain.RunWaiting {
			continue
		}
		info, err := mgr.WaitInfo(ctx, run)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return run, 1, false
		}
		switch run.WaitReason {
		case domain.WaitUser:
			question := info.Question
			if question == "" {
				question = "Input required:"
			}
			fmt.Fprintln(stdout, question)
			var answer string
			for {
				line, eof, rerr := readChatLine(reader)
				if rerr != nil {
					fmt.Fprintln(stderr, rerr)
					return run, 1, false
				}
				answer = strings.TrimSpace(line)
				if answer != "" {
					break
				}
				if eof {
					return run, 0, true
				}
			}
			run, err = mgr.Resume(ctx, run.ID, agent.ResumeInput{Answer: answer})
			if err != nil {
				fmt.Fprintln(stderr, err)
				return run, 1, false
			}
		case domain.WaitApproval:
			toolName := ""
			argsSummary := "{}"
			resources := "none"
			if info.PendingInvocation != nil {
				toolName = info.PendingInvocation.Tool
				if info.PendingInvocation.ArgsSummary != "" {
					argsSummary = info.PendingInvocation.ArgsSummary
				}
				if len(info.PendingInvocation.Resources) > 0 {
					resources = strings.Join(info.PendingInvocation.Resources, ", ")
				} else if info.PendingInvocation.ResourceScope != "" {
					resources = info.PendingInvocation.ResourceScope
				}
			}
			fmt.Fprintf(stdout, "Approval required: tool=%s args=%s resources=%s\n", toolName, argsSummary, resources)
			var decision string
			for {
				fmt.Fprint(stdout, "Approve? [y/n]: ")
				line, eof, rerr := readChatLine(reader)
				if rerr != nil {
					fmt.Fprintln(stderr, rerr)
					return run, 1, false
				}
				if d, ok := parseApprovalDecision(line); ok {
					decision = d
					break
				}
				if eof {
					return run, 0, true
				}
			}
			run, err = mgr.Resume(ctx, run.ID, agent.ResumeInput{Decision: decision})
			if err != nil {
				fmt.Fprintln(stderr, err)
				return run, 1, false
			}
		default:
			fmt.Fprintf(stdout, "run %s waiting (%s)\n", run.ID, run.WaitReason)
			return run, 0, true
		}
	}
}

func pollRunState(ctx context.Context, mgr *agent.Manager, runID string, interval time.Duration) (storage.Run, error) {
	if interval <= 0 {
		interval = 5 * time.Millisecond
	}
	for {
		run, err := mgr.Store.GetRun(ctx, runID)
		if err != nil {
			return storage.Run{}, err
		}
		if run.Status != domain.RunRunning {
			return run, nil
		}
		select {
		case <-ctx.Done():
			return run, ctx.Err()
		case <-time.After(interval):
		}
	}
}

func readChatLine(r *bufio.Reader) (string, bool, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			if len(line) == 0 {
				return "", true, nil
			}
			return strings.TrimRight(line, "\r\n"), false, nil
		}
		return "", false, err
	}
	return strings.TrimRight(line, "\r\n"), false, nil
}

func parseApprovalDecision(input string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "y", "yes", "approve":
		return "approve", true
	case "n", "no", "deny":
		return "deny", true
	default:
		return "", false
	}
}

type memoryCharge struct{ b domain.Budget }

func (m *memoryCharge) Charge(context.Context, string, domain.ChargeKind, int) error {
	next, err := m.b.Charge(domain.ChargeTool, 1)
	if err != nil {
		return err
	}
	m.b = next
	return nil
}

type memoryGrants struct{ items []domain.Grant }

func (m memoryGrants) ListGrants(context.Context, string, string, time.Time) ([]domain.Grant, error) {
	return m.items, nil
}

func writeJSON(w io.Writer, v any) int {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return 1
	}
	return 0
}
