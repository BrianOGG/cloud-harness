package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/agent"
	"github.com/BrianOGG/cloud-harness/internal/config"
	"github.com/BrianOGG/cloud-harness/internal/tools"
)

// mcpStartTimeout bounds one server's startup and discovery for commands that execute runs.
var mcpStartTimeout = tools.DefaultMCPStartTimeout

// doctorMCPProbeTimeout bounds one server's startup and discovery when doctor probes it.
var doctorMCPProbeTimeout = 10 * time.Second

func mcpServerConfigs(servers []config.MCPServerConfig) []tools.StdioMCPConfig {
	out := make([]tools.StdioMCPConfig, 0, len(servers))
	for _, s := range servers {
		out = append(out, tools.StdioMCPConfig{Name: s.Name, Command: s.Command, Args: s.Args, Readonly: s.Readonly})
	}
	return out
}

// startMCP starts the configured stdio MCP servers and registers their tools on
// the manager's broker, so every call goes through the same validation, budget,
// policy, approval, hook, and audit path as a built-in tool. A server that
// fails is reported on stderr and skipped. The caller must Close the result.
func startMCP(ctx context.Context, cfg config.Config, mgr *agent.Manager, stderr io.Writer) *tools.MCPSet {
	if len(cfg.Servers) == 0 || mgr == nil || mgr.Broker == nil {
		return nil
	}
	set := tools.StartMCPServers(ctx, mgr.Broker, mcpServerConfigs(cfg.Servers), mcpStartTimeout)
	for _, st := range set.Status() {
		if st.State != tools.MCPStateRunning {
			_, _ = fmt.Fprintf(stderr, "warning: mcp server %q is unavailable, its tools are not registered: %s\n", st.Name, scrubDoctor(st.Error))
		}
		for _, w := range st.Warnings {
			_, _ = fmt.Fprintf(stderr, "warning: mcp server %q: %s\n", st.Name, scrubDoctor(w))
		}
	}
	return set
}

// checkMCP reports the real state of the configured servers. live is the state
// of the servers owned by a running serve process; when it is nil the servers
// are started, discovered, and stopped again just for this check.
func checkMCP(ctx context.Context, servers []config.MCPServerConfig, live func() []tools.MCPServerStatus) doctorItem {
	if err := validateMCPServers(servers); err != nil {
		return doctorItem{Name: "mcp", Status: "fail", Detail: err.Error()}
	}
	if len(servers) == 0 {
		return doctorItem{Name: "mcp", Status: "ok", Detail: "0 server(s) configured"}
	}
	var status []tools.MCPServerStatus
	source := "running in this process"
	if live != nil {
		status = live()
	} else {
		source = "probed and stopped"
		set := tools.StartMCPServers(ctx, nil, mcpServerConfigs(servers), doctorMCPProbeTimeout)
		status = set.Status()
		_ = set.Close()
	}
	// Server output can echo environment values; scrub it like every other doctor detail.
	for i := range status {
		status[i].Error = scrubDoctor(status[i].Error)
		for j := range status[i].Warnings {
			status[i].Warnings[j] = scrubDoctor(status[i].Warnings[j])
		}
	}
	failed, warned := 0, 0
	parts := make([]string, 0, len(status))
	for _, st := range status {
		label := st.Name
		if st.Readonly {
			label += " (readonly)"
		}
		if st.State != tools.MCPStateRunning {
			failed++
			parts = append(parts, fmt.Sprintf("%s: unavailable: %s", label, st.Error))
			continue
		}
		part := fmt.Sprintf("%s: %d tool(s)", label, len(st.Tools))
		if len(st.Warnings) > 0 {
			warned++
			part += "; " + strings.Join(st.Warnings, "; ")
		}
		parts = append(parts, part)
	}
	item := doctorItem{Name: "mcp", Status: "ok", Servers: status}
	switch {
	case failed > 0:
		item.Status = "fail"
		item.Detail = fmt.Sprintf("%d of %d server(s) unavailable (%s)", failed, len(status), source)
	case warned > 0:
		item.Status = "warn"
		item.Detail = fmt.Sprintf("%d server(s) %s with skipped tools", len(status), source)
	default:
		item.Detail = fmt.Sprintf("%d server(s) %s", len(status), source)
	}
	item.Detail += " - " + strings.Join(parts, " | ")
	return item
}
