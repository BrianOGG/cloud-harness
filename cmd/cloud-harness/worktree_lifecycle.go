package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/agent"
	"github.com/BrianOGG/cloud-harness/internal/config"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/storage"
	"github.com/BrianOGG/cloud-harness/internal/storage/sqlite"
	"github.com/BrianOGG/cloud-harness/internal/tools"
	"github.com/BrianOGG/cloud-harness/internal/worktree"
)

// A process-wide queue covers re-reading the run, Git integration, test gates,
// the DB decision and cleanup. Separate CLI processes must not merge at once.
var worktreeLifecycleQueue = make(chan struct{}, 1)

func serializeWorktreeLifecycle(ctx context.Context, fn func() error) error {
	select {
	case worktreeLifecycleQueue <- struct{}{}:
		defer func() { <-worktreeLifecycleQueue }()
		return fn()
	case <-ctx.Done():
		return ctx.Err()
	}
}

type worktreeRow struct {
	worktree.Info
	State     string           `json:"state"`
	RunStatus domain.RunStatus `json:"run_status,omitempty"`
	Exists    bool             `json:"exists"`
	Orphan    string           `json:"orphan,omitempty"`
	Error     string           `json:"error,omitempty"`
}

func worktreeDBPath(cfg config.Config, workspace string) string {
	if cfg.DBPath == config.DefaultDB {
		return filepath.Join(workspace, config.DefaultDB)
	}
	return cfg.DBPath
}

func readWorktreeRuns(ctx context.Context, path string) ([]storage.Run, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return []storage.Run{}, nil
	} else if err != nil {
		return nil, err
	}
	return sqlite.ReadWorktreeRuns(ctx, path)
}

func reconcileWorktrees(ctx context.Context, root string, runs []storage.Run) ([]worktreeRow, error) {
	inventory, err := worktree.List(ctx, root)
	if err != nil {
		return nil, err
	}
	entries := map[string]worktree.Entry{}
	for _, e := range inventory {
		entries[e.RunID] = e
	}
	rows := make([]worktreeRow, 0, len(inventory)+len(runs))
	for _, r := range runs {
		// A shared explicit DB may contain bindings for multiple workspaces.
		if filepath.Dir(r.WorktreePath) != filepath.Join(root, ".cloud-harness", "worktrees") {
			continue
		}
		info := worktree.Info{RunID: r.ID, BaseSHA: r.WorktreeBaseSHA, Branch: r.WorktreeBranch, Path: r.WorktreePath}
		e, observed := entries[r.ID]
		delete(entries, r.ID)
		row := worktreeRow{Info: info, Exists: e.Exists, RunStatus: r.Status, State: r.WorktreeState, Error: e.Error}
		if row.State == "" {
			if !domain.TerminalRun(r.Status) {
				row.State = "running"
			} else {
				row.State = "pending"
			}
			if !e.Exists {
				row.Orphan = "missing_worktree"
			}
		} else if observed {
			row.Orphan = "unexpected_worktree"
		}
		if e.Exists && e.Info != info {
			row.Orphan = "binding_mismatch"
		}
		if e.Error != "" {
			row.Orphan = "unreadable_worktree"
		}
		rows = append(rows, row)
	}
	for _, e := range entries {
		rows = append(rows, worktreeRow{Info: e.Info, State: "pending", Exists: e.Exists, Orphan: "missing_run", Error: e.Error})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].RunID < rows[j].RunID })
	return rows, nil
}

const worktreesUsage = "usage: cloud-harness worktrees list [--json] [--workspace dir] [--db path]\n       cloud-harness worktrees diff|merge|discard <run-id> [flags]"

func cmdWorktrees(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, worktreesUsage)
		return 2
	}
	action := args[0]
	args = args[1:]
	if action != "list" && action != "diff" && action != "merge" && action != "discard" {
		_, _ = fmt.Fprintln(stderr, worktreesUsage)
		return 2
	}
	// Accept the documented <run-id> before flags as well as Go-style flags first.
	runID := ""
	if action != "list" && len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		runID, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("worktrees "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := addCommon(fs)
	var jsonOutput, force bool
	if action == "list" || action == "diff" {
		fs.BoolVar(&jsonOutput, "json", false, "emit JSON")
	}
	if action == "discard" {
		fs.BoolVar(&force, "force", false, "explicitly destroy unmerged work")
	}
	testCommand := "go test ./..."
	if action == "merge" {
		fs.StringVar(&testCommand, "test-command", testCommand, "required test gate command (arguments separated by spaces; no shell)")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if action != "list" && runID == "" && fs.NArg() == 1 {
		runID = fs.Arg(0)
	} else if fs.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, worktreesUsage)
		return 2
	}
	if action != "list" && runID == "" {
		_, _ = fmt.Fprintln(stderr, worktreesUsage)
		return 2
	}
	if action == "merge" && len(strings.Fields(testCommand)) == 0 {
		_, _ = fmt.Fprintln(stderr, "test command must not be empty")
		return 2
	}
	cfg, err := load(c)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	root, err := filepath.Abs(c.workspaceOrDot())
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	cfg.DBPath = worktreeDBPath(cfg, root)
	ctx, cancel := runWorktreeContext(true)
	defer cancel()
	perform := func() error {
		if err := worktree.Validate(ctx, root); err != nil {
			return err
		}
		runs, err := readWorktreeRuns(ctx, cfg.DBPath)
		if err != nil {
			return err
		}
		rows, err := reconcileWorktrees(ctx, root, runs)
		if err != nil {
			return err
		}
		if action == "list" {
			if jsonOutput {
				if writeJSON(stdout, rows) != 0 {
					return errors.New("write worktrees JSON")
				}
				return nil
			}
			tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "RUN ID\tBRANCH\tBASE SHA\tPATH\tSTATE\tORPHAN")
			for _, row := range rows {
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", row.RunID, row.Branch, row.BaseSHA, row.Path, row.State, row.Orphan)
				if row.Error != "" {
					_, _ = fmt.Fprintln(stderr, row.Error)
				}
			}
			return tw.Flush()
		}
		var selected *worktreeRow
		var run storage.Run
		for i := range rows {
			if rows[i].RunID == runID {
				selected = &rows[i]
				break
			}
		}
		for _, r := range runs {
			if r.ID == runID {
				run = r
				break
			}
		}
		if selected == nil || run.ID == "" {
			return fmt.Errorf("no worktree run record for %q (inspect worktrees list for orphans)", runID)
		}
		if action == "diff" {
			if selected.State == "merged" || selected.State == "discarded" {
				return fmt.Errorf("worktree %s is already %s", runID, selected.State)
			}
			if selected.Orphan != "" {
				return fmt.Errorf("worktree %s: %s %s", runID, selected.Orphan, selected.Error)
			}
			summary, err := worktree.Summarize(ctx, selected.Info)
			if err != nil {
				return err
			}
			if jsonOutput {
				if writeJSON(stdout, summary) != 0 {
					return errors.New("write diff JSON")
				}
				return nil
			}
			_, _ = fmt.Fprintf(stdout, "run %s\nbranch %s\nbase %s\nfiles: %s\n%s\n%s", runID, summary.Branch, summary.BaseSHA, strings.Join(summary.Files, ", "), summary.Stat, summary.Diff)
			if summary.Truncated {
				_, _ = fmt.Fprintln(stdout, "\n[diff truncated at 64 KiB]")
			}
			return nil
		}
		if !domain.TerminalRun(run.Status) {
			return fmt.Errorf("worktree %s is running or waiting; cancel/finish the run first", runID)
		}
		if selected.Orphan != "" && selected.Orphan != "missing_worktree" && selected.Orphan != "unexpected_worktree" {
			return fmt.Errorf("worktree %s: %s %s", runID, selected.Orphan, selected.Error)
		}
		db, err := sqlite.Open(ctx, cfg.DBPath)
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		if action == "discard" {
			if selected.State != "merged" && selected.State != "discarded" && !force {
				return errors.New("unmerged worktree; discard requires explicit --force (including committed or empty pending runs)")
			}
			if err := worktree.Remove(ctx, root, selected.Info); err != nil {
				return err
			}
			if run.WorktreeState == "" {
				if err := db.SetWorktreeState(ctx, run, "discarded"); err != nil {
					return fmt.Errorf("worktree removed; retry discard --force to record state: %w", err)
				}
			}
			_, _ = fmt.Fprintf(stdout, "worktree %s discarded (run record retained)\n", runID)
			return nil
		}
		if selected.State != "pending" || selected.Orphan != "" {
			return fmt.Errorf("cannot merge worktree %s in state %s (%s)", runID, selected.State, selected.Orphan)
		}
		resolve, err := worktreeResolver(cfg, db)
		if err != nil {
			return err
		}
		if err := worktree.Merge(ctx, root, selected.Info, worktree.MergeOptions{
			Test:    func(ctx context.Context, path string) error { return runWorktreeTests(ctx, path, testCommand, stderr) },
			Resolve: resolve,
			Record:  func(ctx context.Context) error { return db.SetWorktreeState(ctx, run, "merged") },
		}); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "worktree %s merged; branch and directory removed\n", runID)
		return nil
	}
	if action == "merge" || action == "discard" {
		err = serializeWorktreeLifecycle(ctx, perform)
	} else {
		err = perform()
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "worktrees:", err)
		return 1
	}
	return 0
}

func runWorktreeTests(ctx context.Context, path, command string, output io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	argv := strings.Fields(command)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = path
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	var log bytes.Buffer
	cmd.Stdout = io.MultiWriter(output, &log)
	cmd.Stderr = cmd.Stdout
	_, _ = fmt.Fprintf(output, "test gate (%s): %s\n", path, command)
	if err := cmd.Run(); err != nil {
		text := log.String()
		if len(text) > 8192 {
			text = text[len(text)-8192:]
		}
		return fmt.Errorf("%s: %w\n%s", command, err, text)
	}
	return nil
}

// A repair is a bounded, restricted run in the same worktree and main DB.
// No shell/Git/delegation tools are exposed; staging and merge policy stay in Go.
func worktreeResolver(cfg config.Config, db *sqlite.DB) (func(context.Context, worktree.Info, []string, string) error, error) {
	prov, err := buildProvider(cfg)
	if err != nil {
		return nil, err
	}
	return worktreeResolverForProvider(prov, db), nil
}

func worktreeResolverForProvider(prov provider.Provider, db *sqlite.DB) func(context.Context, worktree.Info, []string, string) error {
	return func(ctx context.Context, info worktree.Info, files []string, feedback string) error {
		broker := tools.NewBroker(db, db)
		if err := tools.RegisterLocal(broker, info.Path); err != nil {
			return err
		}
		budget := domain.DefaultBudget()
		budget.MaxModelCalls = 8
		budget.MaxToolCalls = 24
		budget.MaxChildRuns = 0
		budget.MaxScriptCalls = 0
		mgr := &agent.Manager{Store: db, Provider: prov, Broker: broker, Workspace: info.Path, DisableSnapshots: true, SkipPlanner: true, Depth: 1,
			UseBudget: true, InitialBudget: budget, AllowedTools: []string{"read_file", "write_file", "edit_file", "list_dir", "list_files", "grep", "git_status", "git_diff"},
			Grants: []domain.Grant{{Tool: "write_file", ResourceScope: info.Path}, {Tool: "edit_file", ResourceScope: info.Path}},
			System: "Resolve merge conflicts in this workspace preserving both sides' intended behavior. Edit files only. Do not invoke Git, pick ours/theirs wholesale, rebase, or change test expectations to hide failures. The coordinator will stage, commit and run the test gate. Finish when files are resolved."}
		session, err := mgr.CreateSession(ctx)
		if err != nil {
			return err
		}
		run, err := mgr.RunMessage(ctx, session.ID, fmt.Sprintf("Resolve conflicts for run %s in files %q. Previous result: %s", info.RunID, files, feedback))
		if err != nil {
			return err
		}
		if run.Status != domain.RunCompleted {
			return fmt.Errorf("repair run %s ended %s", run.ID, run.Status)
		}
		return nil
	}
}

func checkWorktrees(ctx context.Context, root, dbPath string) doctorItem {
	item := doctorItem{Name: "worktrees", Status: "ok", Detail: "no retained worktrees"}
	if _, err := os.Stat(filepath.Join(root, ".git")); errors.Is(err, os.ErrNotExist) {
		return item
	}
	runs, err := readWorktreeRuns(ctx, dbPath)
	if err != nil {
		item.Status = "warn"
		item.Detail = "cannot reconcile run records: " + err.Error()
		return item
	}
	rows, err := reconcileWorktrees(ctx, root, runs)
	if err != nil {
		item.Status = "warn"
		item.Detail = "cannot reconcile Git worktrees: " + err.Error()
		return item
	}
	orphans := []string{}
	for _, row := range rows {
		if row.Orphan != "" {
			orphans = append(orphans, row.RunID+": "+row.Orphan+" "+row.Error)
		}
	}
	item.Detail = fmt.Sprintf("%d worktree run(s); no orphans", len(rows))
	if len(orphans) > 0 {
		item.Status = "warn"
		item.Detail = "orphan worktrees: " + strings.Join(orphans, "; ")
	}
	return item
}

type worktreeReclamation struct {
	worktree.Info
	Error   string `json:"error,omitempty"`
	Removed bool   `json:"removed"`
	DryRun  bool   `json:"dry_run"`
}

// Use the existing prune-runs cutoff for missing-run artifacts too; metadata
// mtime supplies the age when there is no surviving run record. No scheduler or
// independent cleanup interval is introduced.
func planWorktreeReclamation(ctx context.Context, workspace, dbPath string, before time.Time) []worktreeReclamation {
	root, err := filepath.Abs(workspace)
	if err != nil {
		return []worktreeReclamation{{Error: err.Error()}}
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); errors.Is(err, os.ErrNotExist) {
		return []worktreeReclamation{}
	}
	inventory, err := worktree.List(ctx, root)
	if err != nil {
		return []worktreeReclamation{{Error: err.Error()}}
	}
	runs, err := readWorktreeRuns(ctx, dbPath)
	if err != nil {
		return []worktreeReclamation{{Error: err.Error()}}
	}
	byID := map[string]storage.Run{}
	for _, r := range runs {
		byID[r.ID] = r
	}
	plan := []worktreeReclamation{}
	for _, e := range inventory {
		r, exists := byID[e.RunID]
		eligible := !exists && !e.ModifiedAt.IsZero() && e.ModifiedAt.Before(before)
		if exists {
			eligible = domain.TerminalRun(r.Status) && r.UpdatedAt.Before(before) && r.WorktreePath == e.Path
		}
		if !eligible {
			continue
		}
		plan = append(plan, worktreeReclamation{Info: e.Info, Error: e.Error})
	}
	return plan
}

func reclaimOrphanWorktrees(ctx context.Context, workspace string, db *sqlite.DB, plan []worktreeReclamation, dryRun bool) []worktreeReclamation {
	for i := range plan {
		plan[i].DryRun = dryRun
		if plan[i].Error != "" || dryRun {
			continue
		}
		if _, err := db.GetRun(ctx, plan[i].RunID); err == nil {
			plan[i].Error = "run record still exists; worktree retained"
			continue
		} else if !domain.IsCode(err, domain.CodeNotFound) {
			plan[i].Error = err.Error()
			continue
		}
		if err := worktree.Remove(ctx, workspace, plan[i].Info); err != nil {
			plan[i].Error = err.Error()
		} else {
			plan[i].Removed = true
		}
	}
	return plan
}
