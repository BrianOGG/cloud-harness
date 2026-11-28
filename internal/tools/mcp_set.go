package tools

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/config"
)

// DefaultMCPStartTimeout bounds one server's start, initialize, and tools/list.
const DefaultMCPStartTimeout = 30 * time.Second

const (
	// MCPStateRunning means discovery succeeded and the transport is connected.
	MCPStateRunning = "running"
	// MCPStateFailed means the server did not start, failed discovery, or exited later.
	MCPStateFailed = "failed"
)

var (
	mcpServerNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	mcpToolNameRE   = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
)

// MCPServerStatus is the observed state of one configured MCP server.
type MCPServerStatus struct {
	Name      string   `json:"name"`
	Transport string   `json:"transport"`
	Auth      string   `json:"auth"`
	Readonly  bool     `json:"readonly"`
	State     string   `json:"state"`
	Tools     []string `json:"tools,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
	Error     string   `json:"error,omitempty"`
}

var ErrMCPAuthorization = errors.New("MCP authorization required; authorize in Settings")

// RemoteMCPServer binds one trusted config to its transport and optional OAuth client.
type RemoteMCPServer struct {
	Open  func() MCPConnection
	OAuth MCPAuthorization
}
type MCPAuthorization interface {
	Begin(context.Context) (location, state string, err error)
	Finish(ctx context.Context, state, code, issuer, host string) (matched bool, err error)
	Forget()
}

type MCPConnection interface {
	Discover(context.Context) ([]Spec, error)
	Alive() bool
	Err() error
	Close() error
}

type mcpRegistry struct {
	mu    sync.RWMutex
	specs map[string]Spec
}

type mcpServer struct {
	mu              sync.Mutex
	cfg             config.MCPServerConfig
	remote          RemoteMCPServer
	adapter         MCPConnection
	status          MCPServerStatus
	invalid, closed bool
}

// MCPSet owns connections and their broker registrations for the process.
type MCPSet struct {
	broker  *Broker
	timeout time.Duration
	servers []*mcpServer
}

func StartMCPServers(ctx context.Context, broker *Broker, cfgs []StdioMCPConfig, timeout time.Duration) *MCPSet {
	return StartMCPConnections(ctx, broker, cfgs, timeout, nil)
}

// StartMCPConnections starts independent connections in parallel. Only their
// descriptors and broker-owned executors enter the tools package.
func StartMCPConnections(ctx context.Context, broker *Broker, cfgs []config.MCPServerConfig, timeout time.Duration, remote func(config.MCPServerConfig) RemoteMCPServer) *MCPSet {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = DefaultMCPStartTimeout
	}
	set := &MCPSet{broker: broker, timeout: timeout}
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for _, cfg := range cfgs {
		st := MCPServerStatus{Name: cfg.Name, Readonly: cfg.Readonly, State: MCPStateFailed, Transport: cfg.TransportName(), Auth: "none"}
		srv := &mcpServer{cfg: cfg, status: st}
		set.servers = append(set.servers, srv)
		switch {
		case !mcpServerNameRE.MatchString(cfg.Name):
			srv.status.Error = "server name must contain only letters, digits, underscore, or dash"
		case seen[cfg.Name]:
			srv.status.Error = "duplicate server name"
		default:
			if err := cfg.Validate(); err != nil {
				srv.status.Error = err.Error()
			}
		}
		seen[cfg.Name] = true
		if srv.status.Error != "" {
			srv.invalid = true
			continue
		}
		if cfg.TokenEnv != "" {
			srv.status.Auth = "token_env"
		}
		if srv.status.Transport != "stdio" {
			if remote == nil {
				srv.invalid = true
				srv.status.Error = "HTTP MCP transport unavailable"
				continue
			}
			srv.remote = remote(cfg)
		}
		if cfg.OAuth != nil {
			srv.status.Auth = "oauth"
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = set.connect(ctx, srv)
		}()
	}
	wg.Wait()
	return set
}

// unregister first so an approval resumed while disconnected finds no tool.
func (s *MCPSet) disconnect(srv *mcpServer) {
	if b := s.broker; b != nil {
		b.mcp.mu.Lock()
		for _, name := range srv.status.Tools {
			delete(b.mcp.specs, name)
		}
		b.mcp.mu.Unlock()
	}
	if srv.adapter != nil {
		_ = srv.adapter.Close()
		srv.adapter = nil
	}
	srv.status.Tools = nil
	srv.status.Warnings = nil
	srv.status.Error = ""
	srv.status.State = "disconnected"
}
func (s *MCPSet) connect(ctx context.Context, srv *mcpServer) error {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return s.connectLocked(ctx, srv)
}
func (s *MCPSet) connectLocked(ctx context.Context, srv *mcpServer) error {
	if srv.invalid || srv.closed {
		return errors.New("MCP server configuration is unavailable")
	}
	s.disconnect(srv)
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	var a MCPConnection
	var err error
	if srv.status.Transport == "stdio" {
		a, err = NewStdioMCP(context.Background(), srv.cfg)
	} else {
		a = srv.remote.Open()
	}
	var specs []Spec
	if err == nil {
		specs, err = a.Discover(ctx)
		if err != nil {
			_ = a.Close()
		}
	}
	if err != nil {
		srv.status.State = MCPStateFailed
		srv.status.Error = err.Error()
		if errors.Is(err, ErrMCPAuthorization) {
			srv.status.State = "auth_required"
		}
		return err
	}
	srv.adapter = a
	srv.status.State = MCPStateRunning
	if s.broker != nil {
		s.broker.mu.RLock()
		defer s.broker.mu.RUnlock()
		s.broker.mcp.mu.Lock()
		defer s.broker.mcp.mu.Unlock()
	}
	seen := map[string]bool{}
	for _, spec := range specs {
		if spec.Name == "mcp."+srv.cfg.Name+"." || !mcpToolNameRE.MatchString(spec.Name) {
			srv.status.Warnings = append(srv.status.Warnings, "skipped tool: invalid name")
			continue
		}
		if seen[spec.Name] || (s.broker != nil && (s.broker.reg[spec.Name].Name != "" || s.broker.mcp.specs[spec.Name].Name != "")) {
			srv.status.Warnings = append(srv.status.Warnings, fmt.Sprintf("skipped tool %q: name is already registered", spec.Name))
			continue
		}
		seen[spec.Name] = true
		if s.broker != nil {
			s.broker.mcp.specs[spec.Name] = spec
		}
		srv.status.Tools = append(srv.status.Tools, spec.Name)
	}
	return nil
}

func (s *MCPSet) Status() []MCPServerStatus {
	out := []MCPServerStatus{}
	if s == nil {
		return out
	}
	for _, srv := range s.servers {
		srv.mu.Lock()
		st := srv.status
		st.Tools = append([]string(nil), st.Tools...)
		st.Warnings = append([]string(nil), st.Warnings...)
		if srv.adapter != nil && !srv.adapter.Alive() {
			st.State = MCPStateFailed
			if st.Transport == "stdio" {
				st.Error = "server process exited after startup"
			}
			if err := srv.adapter.Err(); err != nil {
				if st.Error != "" {
					st.Error += ": "
				}
				st.Error += err.Error()
				if errors.Is(err, ErrMCPAuthorization) {
					st.State = "auth_required"
				}
			}
		}
		srv.mu.Unlock()
		out = append(out, st)
	}
	return out
}
func (s *MCPSet) named(name string) (*mcpServer, error) {
	if s != nil {
		for _, srv := range s.servers {
			if srv.cfg.Name == name && !srv.invalid {
				return srv, nil
			}
		}
	}
	return nil, errors.New("unknown or invalid MCP server")
}
func (s *MCPSet) Connect(ctx context.Context, name string) error {
	srv, err := s.named(name)
	if err != nil {
		return err
	}
	return s.connect(ctx, srv)
}
func (s *MCPSet) Disconnect(name string) error {
	srv, err := s.named(name)
	if err != nil {
		return err
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	s.disconnect(srv)
	if srv.remote.OAuth != nil {
		srv.remote.OAuth.Forget()
	}
	return nil
}
func (s *MCPSet) BeginOAuth(ctx context.Context, name, host string) (location, state string, secureCookie bool, err error) {
	srv, err := s.named(name)
	if err != nil {
		return "", "", false, err
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.closed || srv.remote.OAuth == nil {
		return "", "", false, errors.New("OAuth is unavailable for this server")
	}
	redirect, err := config.ParseMCPEndpoint(srv.cfg.OAuth.RedirectURI)
	if err != nil || !strings.EqualFold(redirect.Host, host) {
		return "", "", false, errors.New("open Settings at the configured OAuth callback origin")
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	location, state, err = srv.remote.OAuth.Begin(ctx)
	return location, state, redirect.Scheme == "https", err
}
func (s *MCPSet) FinishOAuth(ctx context.Context, state, code, issuer, host string) error {
	if s != nil && state != "" {
		for _, srv := range s.servers {
			if srv.remote.OAuth == nil {
				continue
			}
			matched, err := s.finishOAuth(ctx, srv, state, code, issuer, host)
			if matched {
				return err
			}
		}
	}
	return errors.New("invalid OAuth state; authorize again")
}
func (s *MCPSet) finishOAuth(ctx context.Context, srv *mcpServer, state, code, issuer, host string) (bool, error) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.closed {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	matched, err := srv.remote.OAuth.Finish(ctx, state, code, issuer, host)
	if matched && err == nil {
		err = s.connectLocked(ctx, srv)
	}
	return matched, err
}
func (s *MCPSet) Close() error {
	if s == nil {
		return nil
	}
	var wg sync.WaitGroup
	for _, srv := range s.servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			srv.mu.Lock()
			defer srv.mu.Unlock()
			srv.closed = true
			s.disconnect(srv)
			if srv.remote.OAuth != nil {
				srv.remote.OAuth.Forget()
			}
		}()
	}
	wg.Wait()
	return nil
}
