package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/agent"
	"github.com/cloud-harness/cloud-harness/internal/hooks"
	"github.com/cloud-harness/cloud-harness/internal/security"
)

const trustUsage = `usage: cloud-harness trust status [--workspace dir] [--json]
       cloud-harness trust approve [--workspace dir] [--yes]
       cloud-harness trust revoke [--workspace dir]

Workspace .cloud-harness/hooks.json and policy.json take effect only after
their exact content is trusted. Decisions are stored by content hash in the
user configuration directory, outside the workspace.`

func cmdTrust(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		fmt.Fprintln(stdout, trustUsage)
		return 0
	}
	if len(args) == 0 || args[0] != "status" && args[0] != "approve" && args[0] != "revoke" {
		fmt.Fprintln(stderr, trustUsage)
		return 2
	}
	action := args[0]
	fs := flag.NewFlagSet("trust "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	workspace := fs.String("workspace", ".", "workspace directory")
	jsonOutput, yes := new(bool), new(bool)
	switch action {
	case "status":
		fs.BoolVar(jsonOutput, "json", false, "emit trust status as JSON")
	case "approve":
		fs.BoolVar(yes, "yes", false, "trust the displayed content without prompting")
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, trustUsage)
		return 2
	}
	abs, err := filepath.Abs(*workspace)
	if err == nil {
		_, _, err = inspectWorkspace(abs)
	}
	if err != nil {
		fmt.Fprintln(stderr, "trust:", err)
		return 1
	}
	configDir, err := security.ConfigDirectory()
	if err != nil {
		fmt.Fprintln(stderr, "trust:", err)
		return 1
	}
	fail := func(err error) int {
		fmt.Fprintln(stderr, "trust:", err)
		return 1
	}
	switch action {
	case "status":
		statuses, err := security.WorkspaceTrustFrom(abs, configDir)
		if err != nil {
			return fail(err)
		}
		if *jsonOutput {
			return writeJSON(stdout, statuses)
		}
		for _, status := range statuses {
			fmt.Fprintln(stdout, trustLine(status))
		}
		fmt.Fprintln(stdout, "trust store:", security.TrustStorePath(configDir))
		return 0
	case "revoke":
		removed, err := security.RevokeTrust(abs, configDir)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "revoked %d trust decision(s) for %s\n", removed, abs)
		return 0
	}

	// Refuse to trust a file the loaders would reject at the next run.
	if _, err := hooks.LoadFrom(abs, configDir); err != nil {
		return fail(err)
	}
	if _, err := security.LoadPolicyFrom(abs, configDir); err != nil {
		return fail(err)
	}
	// The bytes shown here are the bytes whose hash is recorded.
	pending := make(map[string][]byte)
	for _, name := range security.TrustedFiles {
		raw, exists, err := security.ReadControlFile(abs, name)
		if err != nil {
			return fail(fmt.Errorf("%s: %w", name, err))
		}
		status, err := security.CheckTrust(abs, configDir, name, raw, exists)
		if err != nil {
			return fail(err)
		}
		if status.Reason != "" {
			return fail(errors.New(status.Reason))
		}
		if !status.Pending() {
			fmt.Fprintln(stdout, trustLine(status))
			continue
		}
		pending[name] = raw
		fmt.Fprintln(stdout, trustLine(status))
		fmt.Fprintln(stdout, trustEffect(name))
		for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
			fmt.Fprintln(stdout, "    | "+strings.ToValidUTF8(strings.TrimRight(line, "\r"), "\uFFFD"))
		}
	}
	if len(pending) == 0 {
		fmt.Fprintln(stdout, "nothing to trust")
		return 0
	}
	if !*yes {
		if !readerIsTTY(stdin) {
			fmt.Fprintln(stderr, "trust approve requires confirmation on a TTY; review the content above and re-run with --yes to proceed")
			return 1
		}
		fmt.Fprintf(stdout, "Trust this content for %s? [y/N]: ", abs)
		line, _, readErr := readChatLine(bufio.NewReader(stdin))
		answer := strings.ToLower(strings.TrimSpace(line))
		if readErr != nil || answer != "y" && answer != "yes" {
			fmt.Fprintln(stdout, "not trusted")
			return 1
		}
	}
	if err := security.TrustContent(abs, configDir, pending, time.Now()); err != nil {
		return fail(err)
	}
	for _, name := range security.TrustedFiles {
		if _, ok := pending[name]; ok {
			fmt.Fprintf(stdout, "trusted %s; any later change needs a new approval\n", name)
		}
	}
	return 0
}

func trustEffect(name string) string {
	if name == security.TrustHooksFile {
		return "  Trusting it lets its hooks run shell commands and scripts on every run in this workspace, without approval:"
	}
	return "  Trusting it lets its allow rules run matching tools in this workspace without approval:"
}

func trustLine(status security.TrustStatus) string {
	line := fmt.Sprintf("%s: %s (%s)", status.File, status.State, status.Path)
	switch status.State {
	case security.TrustMissing:
		return fmt.Sprintf("%s: missing (%s)", status.File, status.Path)
	case security.TrustTrusted:
		return line + " sha256=" + status.SHA256
	}
	line += " sha256=" + status.SHA256
	if status.File == security.TrustHooksFile {
		line += "; its hooks are not run"
	} else {
		line += "; its allow and ask rules are not applied"
	}
	if status.Reason != "" {
		line += "; " + status.Reason
	}
	return line
}

// warnUntrusted tells the user, before a run starts, which workspace control
// files are being ignored. Each run also records a config.untrusted event.
func warnUntrusted(stderr io.Writer, workspace string) {
	statuses, err := security.WorkspaceTrust(workspace)
	if err != nil {
		return
	}
	for _, status := range statuses {
		if status.Pending() {
			fmt.Fprintln(stderr, "warning:", agent.UntrustedMessage(status))
		}
	}
}

// checkTrust is the doctor view of the same decisions.
func checkTrust(workspace string) doctorItem {
	item := doctorItem{Name: "workspace_trust", Status: "ok"}
	statuses, err := security.WorkspaceTrust(workspace)
	if err != nil {
		// Loader errors can quote configuration values.
		item.Status, item.Detail = "fail", "workspace trust could not be determined; check .cloud-harness/hooks.json, policy.json and the trust store"
		return item
	}
	parts := make([]string, 0, len(statuses)+1)
	for _, status := range statuses {
		if status.Pending() {
			item.Status = "warn"
		}
		parts = append(parts, trustLine(status))
	}
	if item.Status == "warn" {
		parts = append(parts, "review the files, then run `cloud-harness trust approve`")
	}
	item.Detail = strings.Join(parts, "; ")
	return item
}
