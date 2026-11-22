package tools

import (
	"context"
	"fmt"
	"regexp"
	"sync"
	"time"
)

// DefaultMCPStartTimeout bounds one server's start, initialize, and tools/list.
const DefaultMCPStartTimeout = 30 * time.Second

const (
	// MCPStateRunning means discovery succeeded and the subprocess is still connected.
	MCPStateRunning = "running"
	// MCPStateFailed means the server did not start, failed discovery, or exited later.
	MCPStateFailed = "failed"
)

var (
	mcpServerNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	mcpToolNameRE   = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
)

// MCPServerStatus is the observed state of one configured stdio MCP server.
type MCPServerStatus struct {
	Name     string   `json:"name"`
	Readonly bool     `json:"readonly"`
	State    string   `json:"state"`
	Tools    []string `json:"tools,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	Error    string   `json:"error,omitempty"`
}

type mcpServer struct {
	adapter *StdioMCP
	status  MCPServerStatus
}

// MCPSet owns the stdio MCP subprocesses started for one process lifetime.
type MCPSet struct {
	mu      sync.Mutex
	servers []*mcpServer
}

// StartMCPServers starts every configured server, discovers its tools, and
// registers them on broker as mcp.<server>.<tool>. A server that cannot start
// or be discovered is recorded as failed and never blocks the others. ctx and
// timeout bound discovery only; subprocesses live until Close. A nil broker
// probes the servers without registering anything.
func StartMCPServers(ctx context.Context, broker *Broker, cfgs []StdioMCPConfig, timeout time.Duration) *MCPSet {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = DefaultMCPStartTimeout
	}
	set := &MCPSet{servers: make([]*mcpServer, len(cfgs))}
	discovered := make([][]Spec, len(cfgs))
	seen := make(map[string]struct{}, len(cfgs))
	var wg sync.WaitGroup
	for i, cfg := range cfgs {
		srv := &mcpServer{status: MCPServerStatus{Name: cfg.Name, Readonly: cfg.Readonly, State: MCPStateFailed}}
		set.servers[i] = srv
		if !mcpServerNameRE.MatchString(cfg.Name) {
			srv.status.Error = "server name must contain only letters, digits, underscore, or dash"
			continue
		}
		if _, dup := seen[cfg.Name]; dup {
			srv.status.Error = "duplicate server name"
			continue
		}
		seen[cfg.Name] = struct{}{}
		wg.Add(1)
		go func(i int, cfg StdioMCPConfig) {
			defer wg.Done()
			adapter, err := NewStdioMCP(context.Background(), cfg)
			if err != nil {
				srv.status.Error = err.Error()
				return
			}
			dctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			specs, err := adapter.Discover(dctx)
			if err != nil {
				_ = adapter.Close()
				srv.status.Error = err.Error()
				return
			}
			srv.adapter = adapter
			discovered[i] = specs
		}(i, cfg)
	}
	wg.Wait()

	// Register in configuration order so name conflicts resolve deterministically.
	for i, srv := range set.servers {
		if srv.adapter == nil {
			continue
		}
		srv.status.State = MCPStateRunning
		for _, spec := range discovered[i] {
			switch {
			case spec.Name == "mcp."+srv.status.Name+"." || !mcpToolNameRE.MatchString(spec.Name):
				srv.status.Warnings = append(srv.status.Warnings, fmt.Sprintf("skipped tool %q: name must contain only letters, digits, underscore, dash, or dot", spec.Name))
				continue
			case broker != nil && broker.Has(spec.Name):
				srv.status.Warnings = append(srv.status.Warnings, fmt.Sprintf("skipped tool %q: name is already registered", spec.Name))
				continue
			}
			if broker != nil {
				broker.Register(spec)
			}
			srv.status.Tools = append(srv.status.Tools, spec.Name)
		}
	}
	return set
}

// Status returns one entry per configured server, in configuration order. A
// server whose subprocess exited after startup is reported as failed.
func (s *MCPSet) Status() []MCPServerStatus {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]MCPServerStatus, 0, len(s.servers))
	for _, srv := range s.servers {
		st := srv.status
		st.Tools = append([]string(nil), st.Tools...)
		st.Warnings = append([]string(nil), st.Warnings...)
		if srv.adapter != nil && !srv.adapter.Alive() {
			st.State = MCPStateFailed
			st.Error = "server process exited after startup"
			if err := srv.adapter.Err(); err != nil {
				st.Error += ": " + err.Error()
			}
		}
		out = append(out, st)
	}
	return out
}

// Close terminates every subprocess and waits for them. It is safe to call more than once.
func (s *MCPSet) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	servers := append([]*mcpServer(nil), s.servers...)
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, srv := range servers {
		if srv.adapter == nil {
			continue
		}
		wg.Add(1)
		go func(a *StdioMCP) {
			defer wg.Done()
			_ = a.Close()
		}(srv.adapter)
	}
	wg.Wait()
	return nil
}
