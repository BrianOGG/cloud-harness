package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/agent"
	"github.com/cloud-harness/cloud-harness/internal/security"
)

const policyUsage = "usage: cloud-harness policy decisions [--run ID] [--tool NAME] [--effect allow|ask|deny] [--since RFC3339] [--limit N] [--db path] [--workspace dir] [--json]"

func cmdPolicy(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		fmt.Fprintln(stdout, policyUsage)
		return 0
	}
	if len(args) == 0 || args[0] != "decisions" {
		fmt.Fprintln(stderr, policyUsage)
		return 2
	}
	fs := flag.NewFlagSet("policy decisions", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "sqlite database path")
	workspace := fs.String("workspace", ".", "workspace directory")
	run := fs.String("run", "", "filter by run ID")
	tool := fs.String("tool", "", "filter by exact tool name")
	effect := fs.String("effect", "", "filter by allow, ask, or deny")
	since := fs.String("since", "", "include decisions at or after this RFC3339 timestamp")
	limit := fs.Int("limit", agent.DefaultPolicyDecisionLimit, "maximum decisions (capped at 500)")
	jsonOutput := fs.Bool("json", false, "emit decision records as JSON")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, policyUsage)
		return 2
	}
	q := agent.PolicyDecisionQuery{RunID: *run, Tool: *tool, Effect: security.Effect(*effect), Limit: *limit}
	if err := q.Validate(); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if *limit == 0 {
		fmt.Fprintln(stderr, "limit must be positive")
		return 2
	}
	if *since != "" {
		var err error
		q.Since, err = time.Parse(time.RFC3339Nano, *since)
		if err != nil {
			fmt.Fprintln(stderr, "since must be an RFC3339 timestamp")
			return 2
		}
	}
	cfg, err := load(&commonFlags{db: *dbPath})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	absWorkspace, err := filepath.Abs(*workspace)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reader, err := openPolicyAudit(ctx, worktreeDBPath(cfg, absWorkspace))
	if err != nil {
		fmt.Fprintln(stderr, "policy:", err)
		return 1
	}
	records := make([]agent.PolicyDecisionRecord, 0)
	if reader != nil {
		defer reader.Close()
		records, err = agent.ListPolicyDecisions(ctx, reader, q)
		if err != nil {
			fmt.Fprintln(stderr, "policy:", err)
			return 1
		}
	}
	if *jsonOutput {
		return writeJSON(stdout, records)
	}
	if err := writePolicyDecisions(stdout, records); err != nil {
		fmt.Fprintln(stderr, "policy:", err)
		return 1
	}
	return 0
}

func writePolicyDecisions(w io.Writer, records []agent.PolicyDecisionRecord) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "TIME\tRUN\tTOOL\tEFFECT\tSOURCE\tRULE"); err != nil {
		return err
	}
	for _, record := range records {
		// Quoting names and rule summaries keeps selectors containing newlines
		// or terminal controls in one printable row.
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%q\t%s\t%s\t%q\n", record.Time.UTC().Format(time.RFC3339Nano),
			record.RunID, record.Tool, record.Effect, record.Source, record.Rule); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func openPolicyAudit(ctx context.Context, path string) (*policyAuditReader, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return newPolicyAuditReader(ctx, path)
}

// checkPolicy is registered alongside the existing doctor checks. Only file
// names, rule counts and decision counts enter doctor's output.
func checkPolicy(workspace, dbPath string) doctorItem {
	item := doctorItem{Name: "policy", Status: "ok"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reader, err := openPolicyAudit(ctx, dbPath)
	if err != nil {
		item.Status, item.Detail = "fail", "policy decision database could not be read"
		return item
	}
	var store agent.PolicyAuditStore
	if reader != nil {
		defer reader.Close()
		store = reader
	}
	summary, err := agent.SummarizePolicy(ctx, workspace, store)
	if err != nil {
		item.Status, item.Detail = "fail", err.Error()
		return item
	}
	parts := make([]string, 0, len(summary.Layers)+1)
	for _, layer := range summary.Layers {
		state := "loaded"
		if !layer.Loaded {
			state = "missing"
		}
		parts = append(parts, fmt.Sprintf("%s: %s (%d rules, %s)", layer.Source, layer.File, layer.Rules, state))
	}
	parts = append(parts, fmt.Sprintf("decisions: allow=%d ask=%d deny=%d", summary.Decisions.Allow, summary.Decisions.Ask, summary.Decisions.Deny))
	item.Detail = strings.Join(parts, "; ")
	return item
}
