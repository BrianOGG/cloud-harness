package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/cloud-harness/cloud-harness/internal/agents"
)

const agentsUsage = "usage: cloud-harness agents [list] [--workspace dir] [--json]\n       cloud-harness agents show <name> [--workspace dir] [--json]"

func cmdAgents(args []string, stdout, stderr io.Writer) int {
	show, name := false, ""
	if len(args) > 0 {
		switch args[0] {
		case "list":
			args = args[1:]
		case "show":
			show = true
			args = args[1:]
			if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
				name, args = args[0], args[1:]
			}
		}
	}
	fs := flag.NewFlagSet("agents", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, agentsUsage)
		fs.PrintDefaults()
	}
	workspace := fs.String("workspace", ".", "workspace root for .agents/agents definitions")
	jsonOutput := fs.Bool("json", false, "emit full definitions as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if show && name == "" && fs.NArg() == 1 {
		name = fs.Arg(0)
	} else if fs.NArg() != 0 {
		fmt.Fprintln(stderr, agentsUsage)
		return 2
	}
	if show && name == "" {
		fmt.Fprintln(stderr, agentsUsage)
		return 2
	}
	registry, err := agents.Load(*workspace)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if show {
		def, ok := registry.Get(name)
		if !ok {
			names := make([]string, 0)
			for _, available := range registry.List() {
				names = append(names, available.Name)
			}
			fmt.Fprintf(stderr, "agents: unknown agent %q; available agents: %s\n", name, strings.Join(names, ", "))
			return 1
		}
		if *jsonOutput {
			return writeJSON(stdout, def)
		}
		err = writeAgentDefinition(stdout, def)
	} else {
		defs := registry.List()
		if *jsonOutput {
			return writeJSON(stdout, defs)
		}
		err = writeAgentsList(stdout, defs)
	}
	if err != nil {
		fmt.Fprintln(stderr, "agents:", err)
		return 1
	}
	return 0
}

func writeAgentsList(w io.Writer, defs []agents.Definition) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "NAME\tSOURCE\tOVERRIDES_BUILTIN\tDESCRIPTION\tTOOLS\tMODEL"); err != nil {
		return err
	}
	for _, def := range defs {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%t\t%s\t%s\t%s\n", def.Name, def.Source,
			def.OverridesBuiltin, def.Description, agentToolsSummary(def.Tools), agentOptional(def.Model)); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func writeAgentDefinition(w io.Writer, def agents.Definition) error {
	_, err := fmt.Fprintf(w, "Name: %s\nSource: %s\nOverrides builtin: %t\nDescription: %s\nTools: %s\nModel: %s\nMax model calls: %s\nMax tool calls: %s\n\nSystem:\n%s\n",
		def.Name, def.Source, def.OverridesBuiltin, def.Description, agentToolsSummary(def.Tools),
		agentOptional(def.Model), agentBudget(def.MaxModelCalls), agentBudget(def.MaxToolCalls), def.System)
	return err
}

func agentToolsSummary(names []string) string {
	if names == nil {
		return "inherit"
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

func agentOptional(value string) string {
	if value == "" {
		return "—"
	}
	return value
}

func agentBudget(n int) string {
	if n == 0 {
		return "—"
	}
	return strconv.Itoa(n)
}
