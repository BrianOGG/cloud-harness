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
	"sort"
	"strings"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/agent"
	"github.com/cloud-harness/cloud-harness/internal/config"
	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/eval"
	"github.com/cloud-harness/cloud-harness/internal/hooks"
	"github.com/cloud-harness/cloud-harness/internal/imageinput"
	"github.com/cloud-harness/cloud-harness/internal/notify"
	"github.com/cloud-harness/cloud-harness/internal/program"
	"github.com/cloud-harness/cloud-harness/internal/provider"
	workspacesnapshot "github.com/cloud-harness/cloud-harness/internal/snapshot"
	"github.com/cloud-harness/cloud-harness/internal/storage"
	"github.com/cloud-harness/cloud-harness/internal/storage/sqlite"
	"github.com/cloud-harness/cloud-harness/internal/tools"
	"github.com/cloud-harness/cloud-harness/internal/trace"
	"github.com/cloud-harness/cloud-harness/internal/transport"
)

const version = "0.5.0-m5"

// StateNew connections can keep net/http.Shutdown waiting even when every
// handler has finished. Bound draining so connection and SQLite cleanup still
// fit inside the caller's shutdown grace (see serveStopGrace in serve_test.go).
const serveShutdownTimeout = time.Second

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
	case "compact":
		return cmdCompact(args[1:], stdout, stderr)
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
	case "rewind":
		return cmdRewind(args[1:], stdin, stdout, stderr)
	case "doctor":
		return cmdDoctor(args[1:], stdout, stderr)
	case "stats":
		return cmdStats(args[1:], stdout, stderr)
	case "export":
		return cmdExport(args[1:], stdout, stderr)
	case "review":
		return cmdReview(args[1:], stdout, stderr)
	case "worktrees":
		return cmdWorktrees(args[1:], stdout, stderr)
	case "agents":
		return cmdAgents(args[1:], stdout, stderr)
	case "skill", "/skill":
		return cmdSkill(args[1:], stdout, stderr)
	case "repomap":
		return cmdRepoMap(args[1:], stdout, stderr)
	case "policy":
		return cmdPolicy(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n%s\n", args[0], usage)
		return 2
	}
}

const usage = `Cloud Harness M5

  cloud-harness version
  cloud-harness serve [--worktree] [--addr 127.0.0.1:19421] [--http-token TOKEN] [--db path] [--workspace dir] [--provider mock|openai]
  cloud-harness run --message TEXT [--plan] [--worktree] [--no-repomap] [--db path] [--workspace dir] [--provider mock|openai]
  cloud-harness chat [--message TEXT] [--session ID] [--worktree] [--db path] [--workspace dir] [--provider mock|openai]
  cloud-harness compact --session ID [--db path] [--workspace dir] [--provider mock|openai]
  cloud-harness eval run --task TASK [--provider mock|openai] [--db path] [--workspace dir]
  cloud-harness eval suite [--db path] [--workspace dir] [--out path] [--compare path]
  cloud-harness runs list [--session ID] [--db path]
  cloud-harness runs inspect RUN_ID
  cloud-harness runs cancel RUN_ID
  cloud-harness runs recover [RUN_ID | --all-expired] [--db path] [--workspace dir]
  cloud-harness runs resume --decision approve|deny|child_done|reconcile_success|reconcile_fail [--answer TEXT] [--extra-model-calls N] RUN_ID
  cloud-harness trace RUN_ID
  cloud-harness retain [--days N] [--prune-runs-days N] [--dry-run] [--db path]
  cloud-harness rewind RUN_ID --to SNAPSHOT_ID [--all|--code-only|--chat-only] [--yes] [--db path] [--workspace dir]
  cloud-harness doctor [--db path] [--workspace dir] [--provider mock|openai] [--json]
  cloud-harness stats [--days N] [--by day|model|run] [--db path] [--json]
  cloud-harness export RUN_ID [--out path] [--sanitize|--raw] [--force] [--json] [--db path]
  cloud-harness review [--workspace dir] [--json] [--diff]
  cloud-harness agents [show NAME] [--workspace dir] [--json]
  cloud-harness /skill NAME [--workspace dir]
  cloud-harness worktrees list|diff|merge|discard [RUN_ID] [--workspace dir] [--db path]
  cloud-harness repomap [--dir path] [--max-bytes N]
  cloud-harness policy decisions [--run ID] [--tool NAME] [--effect allow|ask|deny] [--since RFC3339] [--limit N] [--db path] [--workspace dir] [--json]
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

func cmdCompact(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("compact", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := addCommon(fs)
	session := fs.String("session", "", "session id to compact")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*session) == "" {
		fmt.Fprintln(stderr, "--session is required")
		return 2
	}
	cfg, err := load(c)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	mgr, db, err := openManager(context.Background(), cfg, c.workspaceOrDot())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer db.Close()
	summary, err := mgr.CompactSession(context.Background(), *session)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	_ = writeJSON(stdout, map[string]any{"session_id": *session, "summary": summary})
	return 0
}

func addCommon(fs *flag.FlagSet) *commonFlags {
	c := &commonFlags{}
	fs.StringVar(&c.db, "db", "", "sqlite database path")
	fs.StringVar(&c.provider, "provider", "", "mock or openai")
	fs.StringVar(&c.addr, "addr", "", "listen address")
	fs.StringVar(&c.workspace, "workspace", "", "workspace root for file and command tools")
	fs.StringVar(&c.httpToken, "http-token", "", "bearer token for HTTP API")
	fs.BoolVar(&c.httpAllowInsecure, "http-allow-insecure", false, "deprecated; non-loopback HTTP binds still require --http-token")
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
	notifier, err := notify.New(cfg.Notify)
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
	if _, err := hooks.Load(abs); err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	broker := tools.NewBroker(db, db)
	broker.SetCommandEnvAllow(cfg.CommandEnvAllow)
	broker.SetCommandLimits(tools.CommandLimits(cfg.CommandLimits))
	if err := tools.RegisterLocal(broker, abs); err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return &agent.Manager{Store: db, Provider: prov, Notifier: notifier, ApprovalBaseURL: cfg.Notify.ApprovalBaseURL, OwnerID: cfg.OwnerID, Broker: broker, Workspace: abs, DisableSnapshots: cfg.DisableSnapshots}, db, nil
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
			DisableImages:         cfg.OpenAIDisableImages,
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
	useWorktree := fs.Bool("worktree", false, "run HTTP-created runs in retained isolated Git worktrees")
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	mgr, db, err := openRunManager(ctx, cfg, c.workspace, *useWorktree)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer db.Close()
	if _, err := mgr.RecoverExpired(ctx); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *useWorktree {
		cfg.DBPath = worktreeDBPath(cfg, mgr.Workspace)
	}
	diagnostics := serveDiagnostics{cfg: cfg, workspace: mgr.Workspace}
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           (&transport.Server{Manager: mgr, Token: cfg.HTTPToken, Stats: diagnostics.stats, Doctor: diagnostics.doctor}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	safe, _ := json.Marshal(cfg.Redacted())
	fmt.Fprintf(stdout, "listening on http://%s %s\n", cfg.ListenAddr, safe)
	select {
	case <-ctx.Done():
		shutdownServe(srv)
		return 0
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
}

func shutdownServe(srv *http.Server) {
	shut, cancel := context.WithTimeout(context.Background(), serveShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shut); err != nil {
		// Shutdown leaves connections open on timeout. Close them before the
		// deferred store cleanup, also canceling any remaining request contexts.
		_ = srv.Close()
	}
}

func cmdRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := addCommon(fs)
	message := fs.String("message", "", "user message")
	useWorktree := fs.Bool("worktree", false, "run in a retained Git worktree isolated from the workspace")
	planMode := fs.Bool("plan", false, "run in read-only plan mode")
	disableRepoMap := fs.Bool("no-repomap", false, "disable per-request workspace Repo-Map context")
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
	ctx, stop := runWorktreeContext(*useWorktree)
	defer stop()
	mgr, db, err := openRunManager(ctx, cfg, c.workspaceOrDot(), *useWorktree)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer db.Close()
	mgr.DisableRepoMap = *disableRepoMap
	userText, images, err := imageinput.ParseAndLoad(*message, mgr.Workspace)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ses, err := mgr.CreateSession(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	run, err := mgr.RunMessageWithOptions(ctx, ses.ID, userText, agent.RunOptions{Images: images, Plan: *planMode})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	enc := json.NewEncoder(stdout)
	enc.SetEscapeHTML(false)
	view := map[string]any{
		"session_id":   ses.ID,
		"run_id":       run.ID,
		"status":       run.Status,
		"wait_reason":  run.WaitReason,
		"final_answer": run.FinalAnswer,
		"error":        run.Error,
	}
	if *planMode {
		view["plan"] = true
	}
	_ = enc.Encode(view)
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
	code := 1
	_ = serializeWorktreeLifecycle(context.Background(), func() error {
		code = cmdRetainUnlocked(args, stdout, stderr)
		return nil
	})
	return code
}

func cmdRetainUnlocked(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("retain", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := addCommon(fs)
	days := fs.Int("days", -1, "delete history index rows older than this many days")
	pruneRunsDays := fs.Int("prune-runs-days", 0, "delete terminal runs and child rows older than this many days")
	dryRun := fs.Bool("dry-run", false, "report run and snapshot pruning without changing data")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := load(c)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if root, pathErr := filepath.Abs(c.workspaceOrDot()); pathErr == nil {
		cfg.DBPath = worktreeDBPath(cfg, root)
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
	if n > 0 && !*dryRun {
		cutHistory = time.Now().UTC().AddDate(0, 0, -n)
		pruned, err := db.PruneHistory(ctx, cutHistory)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		prunedHistory = pruned
	} else if n > 0 {
		cutHistory = time.Now().UTC().AddDate(0, 0, -n)
		prunedHistory, err = db.PreviewPruneHistory(ctx, cutHistory)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}

	if *pruneRunsDays <= 0 {
		if *dryRun {
			return writeJSON(stdout, map[string]any{"dry_run": true, "pruned": prunedHistory, "retention_days": n, "before": cutHistory.Format(time.RFC3339)})
		}
		return writeJSON(stdout, map[string]any{"pruned": prunedHistory, "retention_days": n, "before": cutHistory.Format(time.RFC3339)})
	}

	cutRuns := time.Now().UTC().AddDate(0, 0, -*pruneRunsDays)
	worktreePlan := planWorktreeReclamation(ctx, c.workspaceOrDot(), cfg.DBPath, cutRuns)
	var pruneRes storage.PruneRunsResult
	if *dryRun {
		pruneRes, err = db.PreviewPruneRuns(ctx, cutRuns)
	} else {
		pruneRes, err = db.PruneRuns(ctx, cutRuns)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	groups := make(map[string][]storage.WorkspaceSnapshot)
	for _, rec := range pruneRes.Snapshots {
		groups[rec.WorkspaceID] = append(groups[rec.WorkspaceID], rec)
	}
	workspaceIDs := make([]string, 0, len(groups))
	for workspaceID := range groups {
		workspaceIDs = append(workspaceIDs, workspaceID)
	}
	sort.Strings(workspaceIDs)
	workspaceReports := make([]workspaceReclamationReport, 0, len(workspaceIDs))
	var reclaimedWorkspaces int
	for _, workspaceID := range workspaceIDs {
		records := groups[workspaceID]
		workspacePath := ""
		for _, rec := range records {
			if rec.WorkspacePath != "" {
				workspacePath = rec.WorkspacePath
				break
			}
		}
		if workspacePath == "" {
			if currentID, idErr := workspacesnapshot.WorkspaceIDForPath(c.workspaceOrDot()); idErr == nil && currentID == workspaceID {
				workspacePath = c.workspaceOrDot()
			}
		}
		report := workspaceReclamationReport{WorkspaceID: workspaceID, Workspace: workspacePath, Snapshots: len(records)}
		if workspacePath == "" {
			report.Error = "workspace path is unavailable"
		} else {
			stats, reclaimErr := workspacesnapshot.ReclaimWorkspaceSnapshotObjects(ctx, workspacePath, workspaceID, records, *dryRun)
			report.Objects, report.Bytes = stats.Objects, stats.Bytes
			if reclaimErr != nil {
				report.Error = reclaimErr.Error()
			}
		}
		if report.Error != "" {
			if !*dryRun {
				fmt.Fprintf(stderr, "warning: snapshot object reclamation failed for workspace %s (%s): %s\n", workspaceID, workspacePath, report.Error)
			}
		} else if !*dryRun {
			reclaimedWorkspaces++
		}
		workspaceReports = append(workspaceReports, report)
	}

	worktreeReports := reclaimOrphanWorktrees(ctx, c.workspaceOrDot(), db, worktreePlan, *dryRun)
	out := map[string]any{
		"worktree_reclamation":  worktreeReports,
		"pruned":                prunedHistory,
		"retention_days":        n,
		"prune_runs_days":       *pruneRunsDays,
		"prune_runs_before":     cutRuns.Format(time.RFC3339),
		"pruned_runs":           pruneRes.RunsDeleted,
		"deleted_runs":          pruneRes.RunsDeleted,
		"deleted_details":       pruneRes.Tables,
		"tables":                pruneRes.Tables,
		"dry_run":               *dryRun,
		"deleted_snapshots":     pruneRes.SnapshotsDeleted,
		"snapshots_to_delete":   pruneRes.SnapshotsToDelete,
		"unassigned_retained":   pruneRes.UnassignedRetained,
		"affected_workspaces":   len(workspaceReports),
		"reclaimed_workspaces":  reclaimedWorkspaces,
		"workspace_reclamation": workspaceReports,
		"reclamation_failures":  countWorkspaceReclamationFailures(workspaceReports),
	}
	if *dryRun {
		out["runs_to_delete"] = pruneRes.RunsToDelete
	} else {
		out["deleted_runs"] = pruneRes.RunsDeleted
	}
	if n > 0 {
		out["before"] = cutHistory.Format(time.RFC3339)
	}
	if pruneRes.WALCheckpoint != "" {
		out["wal_checkpoint"] = pruneRes.WALCheckpoint
	}
	return writeJSON(stdout, out)
}

type workspaceReclamationReport struct {
	WorkspaceID string `json:"workspace_id"`
	Workspace   string `json:"workspace"`
	Snapshots   int    `json:"snapshots"`
	Objects     int64  `json:"objects"`
	Bytes       int64  `json:"bytes"`
	Error       string `json:"error,omitempty"`
}

func countWorkspaceReclamationFailures(reports []workspaceReclamationReport) int {
	count := 0
	for _, report := range reports {
		if report.Error != "" {
			count++
		}
	}
	return count
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
	useWorktree := fs.Bool("worktree", false, "run each turn in a retained Git worktree isolated from the workspace")
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
	ctx, stop := runWorktreeContext(*useWorktree)
	defer stop()
	mgr, db, err := openRunManager(ctx, cfg, c.workspaceOrDot(), *useWorktree)
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
		if strings.TrimSpace(initialMessage) == "/compact" {
			return runChatCompact(ctx, mgr, sessionID, stdout, stderr)
		}
		if handled, code := runChatSkillCommand(mgr.Workspace, initialMessage, stdout, stderr); handled {
			return code
		}
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
		if handled, _ := runChatSkillCommand(mgr.Workspace, msg, stdout, stderr); handled {
			continue
		}
		if msg == "/compact" {
			if code := runChatCompact(ctx, mgr, sessionID, stdout, stderr); code != 0 {
				return code
			}
			continue
		}
		_, code, stoppedOnEOF := runChatTurn(ctx, mgr, sessionID, msg, reader, stdout, stderr)
		if code != 0 || stoppedOnEOF || eof {
			return code
		}
	}
}

func runChatCompact(ctx context.Context, mgr *agent.Manager, sessionID string, stdout, stderr io.Writer) int {
	summary, err := mgr.CompactSession(ctx, sessionID)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if summary != "" {
		fmt.Fprintln(stdout, "session compacted")
	}
	return 0
}

func runChatTurn(ctx context.Context, mgr *agent.Manager, sessionID, message string, reader *bufio.Reader, stdout, stderr io.Writer) (storage.Run, int, bool) {
	userText, images, err := imageinput.ParseAndLoad(message, mgr.Workspace)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return storage.Run{}, 1, false
	}
	run, err := mgr.RunMessageWithImages(ctx, sessionID, userText, images)
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
			workspace := mgr.Workspace
			if run.WorktreePath != "" {
				workspace = run.WorktreePath
			}
			answerText, answerImages, err := imageinput.ParseAndLoad(answer, workspace)
			if err != nil {
				fmt.Fprintln(stderr, err)
				return run, 1, false
			}
			run, err = mgr.Resume(ctx, run.ID, agent.ResumeInput{Answer: answerText, Images: answerImages})
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
