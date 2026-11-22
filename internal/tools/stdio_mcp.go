package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/domain"
)

const (
	// DefaultMCPTimeout is the default per-call timeout for MCP tool calls.
	DefaultMCPTimeout = 15 * time.Second
	// MaxMCPResultBytes is the maximum allowed response body size for an MCP tool call (64 KiB).
	MaxMCPResultBytes = 64 * 1024
	// ProtocolMCP is the protocol label for MCP adapters.
	ProtocolMCP = "mcp"
)

// StdioMCPConfig declares one local stdio MCP server subprocess configuration.
type StdioMCPConfig struct {
	Name     string   `json:"name"`
	Command  string   `json:"command"`
	Args     []string `json:"args"`
	Readonly bool     `json:"readonly"`
}

// StdioMCP adapts a local MCP server subprocess over stdio JSON-RPC 2.0 into Spec descriptors.
// StdioMCP strictly implements MCPAdapter and does not execute tools itself; execution is
// exposed exclusively through the registered Spec.Execute function managed by Broker.
type StdioMCP struct {
	name     string
	command  string
	args     []string
	readonly bool
	timeout  time.Duration

	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.Mutex
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	seq       atomic.Int64
	pending   map[int64]chan rpcResponse
	closed    bool
	closedErr error
	// closeOnce guards process teardown separately from closed: a server that
	// exits on its own marks the connection closed but must still be reaped.
	closeOnce sync.Once
}

type rpcResponse struct {
	result json.RawMessage
	err    error
}

// NewStdioMCP builds a local stdio MCP adapter with the given parent context and configuration.
func NewStdioMCP(ctx context.Context, cfg StdioMCPConfig) (*StdioMCP, error) {
	name := strings.TrimSpace(cfg.Name)
	if name == "" {
		return nil, domain.E(domain.CodeInvalidArgument, "mcp server name is required")
	}
	cmd := strings.TrimSpace(cfg.Command)
	if cmd == "" {
		return nil, domain.E(domain.CodeInvalidArgument, "mcp server command is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	mcpCtx, cancel := context.WithCancel(ctx)
	s := &StdioMCP{
		name:     name,
		command:  cmd,
		args:     append([]string(nil), cfg.Args...),
		readonly: cfg.Readonly,
		timeout:  DefaultMCPTimeout,
		ctx:      mcpCtx,
		cancel:   cancel,
		pending:  make(map[int64]chan rpcResponse),
	}
	go func() {
		<-mcpCtx.Done()
		_ = s.Close()
	}()
	return s, nil
}

// Protocol returns "mcp".
func (s *StdioMCP) Protocol() string { return ProtocolMCP }

// ServerLabel returns the configured server name.
func (s *StdioMCP) ServerLabel() string { return s.name }

// ProcessPID returns the PID of the spawned MCP server subprocess, or 0 if not started.
func (s *StdioMCP) ProcessPID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd != nil && s.cmd.Process != nil {
		return s.cmd.Process.Pid
	}
	return 0
}

// SetTimeout overrides the default per-call timeout.
func (s *StdioMCP) SetTimeout(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.timeout = d
}

// Alive reports whether the subprocess was started and its stdio connection is still usable.
func (s *StdioMCP) Alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmd != nil && !s.closed
}

// Err returns why the connection ended, or nil while it is usable or after a plain Close.
func (s *StdioMCP) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closedErr
}

// Close terminates the subprocess cleanly, releasing pipes and avoiding orphan processes.
// It also reaps a server that already exited on its own. Concurrent callers wait for teardown.
func (s *StdioMCP) Close() error {
	s.closeOnce.Do(func() {
		// Cancel first: it kills the process group, which unblocks a writer
		// holding s.mu on a full stdin pipe.
		s.cancel()
		s.mu.Lock()
		s.closed = true
		cmd := s.cmd
		stdin := s.stdin
		s.mu.Unlock()

		if stdin != nil {
			_ = stdin.Close()
		}
		if cmd != nil {
			_ = killGroup(cmd)
			_ = cmd.Wait()
		}
	})
	return nil
}

func (s *StdioMCP) ensureStarted() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd != nil {
		return nil
	}
	if s.closed {
		return domain.E(domain.CodeToolError, "mcp adapter is closed")
	}
	cmd := exec.CommandContext(s.ctx, s.command, s.args...)
	cmd.Env = buildSafeEnv()
	applySysProc(cmd)
	cmd.Cancel = func() error { return killGroup(cmd) }

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return domain.E(domain.CodeToolError, "failed to create stdin pipe: "+err.Error())
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return domain.E(domain.CodeToolError, "failed to create stdout pipe: "+err.Error())
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return domain.E(domain.CodeToolError, "failed to start mcp server process: "+err.Error())
	}

	s.cmd = cmd
	s.stdin = stdin
	s.stdout = stdout

	go s.readLoop(stdout)
	return nil
}

func (s *StdioMCP) readLoop(r io.Reader) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 {
				s.dispatchResponse(trimmed)
			}
		}
		if err != nil {
			s.handleDisconnect(err)
			// Reap the exited server now instead of leaving a zombie until shutdown.
			_ = s.Close()
			return
		}
	}
}

func (s *StdioMCP) handleDisconnect(err error) {
	s.mu.Lock()
	if !s.closed {
		s.closedErr = err
	}
	s.closed = true
	pending := s.pending
	s.pending = make(map[int64]chan rpcResponse)
	s.mu.Unlock()

	for _, ch := range pending {
		ch <- rpcResponse{err: domain.E(domain.CodeToolError, "mcp server process terminated: "+err.Error())}
	}
}

func (s *StdioMCP) dispatchResponse(data []byte) {
	var raw struct {
		ID     *int64          `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &raw); err != nil || raw.ID == nil {
		return
	}
	s.mu.Lock()
	ch, ok := s.pending[*raw.ID]
	if ok {
		delete(s.pending, *raw.ID)
	}
	s.mu.Unlock()

	if ok {
		var resp rpcResponse
		if raw.Error != nil {
			resp.err = domain.E(domain.CodeToolError, raw.Error.Message)
		} else {
			resp.result = raw.Result
		}
		ch <- resp
	}
}

func (s *StdioMCP) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	s.mu.Lock()
	if s.closed {
		err := s.closedErr
		s.mu.Unlock()
		if err == nil {
			err = io.EOF
		}
		return nil, domain.E(domain.CodeToolError, "mcp server process terminated: "+err.Error())
	}
	id := s.seq.Add(1)
	ch := make(chan rpcResponse, 1)
	s.pending[id] = ch
	stdin := s.stdin
	s.mu.Unlock()

	reqBody := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, domain.E(domain.CodeInvalidArgument, "failed to marshal json-rpc request: "+err.Error())
	}

	s.mu.Lock()
	if s.closed || s.stdin == nil {
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, domain.E(domain.CodeToolError, "mcp server process terminated")
	}
	_, err = stdin.Write(append(raw, '\n'))
	s.mu.Unlock()
	if err != nil {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, domain.E(domain.CodeToolError, "failed to write to mcp server stdin: "+err.Error())
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, domain.E(domain.CodeToolError, "mcp server closed connection")
		}
		return resp.result, resp.err
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, domain.E(domain.CodeToolError, "mcp "+method+" request timed out")
		}
		return nil, domain.E(domain.CodeToolError, "mcp "+method+" request cancelled")
	}
}

func (s *StdioMCP) notify(method string, params any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.stdin == nil {
		return domain.E(domain.CodeToolError, "mcp server process terminated")
	}
	reqBody := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	_, err = s.stdin.Write(append(raw, '\n'))
	return err
}

// Discover performs initialize handshake and tools/list discovery. It returns Spec items
// mapped with the naming format mcp.<server>.<tool>. Discover only returns Specs and does
// not execute tools.
func (s *StdioMCP) Discover(ctx context.Context) ([]Spec, error) {
	if err := s.ensureStarted(); err != nil {
		return nil, err
	}
	initParams := map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]string{
			"name":    "cloud-harness",
			"version": "1.0.0",
		},
	}
	if _, err := s.call(ctx, "initialize", initParams); err != nil {
		return nil, domain.E(domain.CodeToolError, "mcp initialize failed: "+err.Error())
	}
	_ = s.notify("notifications/initialized", map[string]any{})

	listResp, err := s.call(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, domain.E(domain.CodeToolError, "mcp tools/list failed: "+err.Error())
	}

	var listResult struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(listResp, &listResult); err != nil {
		return nil, domain.E(domain.CodeToolError, "invalid tools/list response: "+err.Error())
	}

	specs := make([]Spec, 0, len(listResult.Tools))
	for _, t := range listResult.Tools {
		origName := t.Name
		toolName := fmt.Sprintf("mcp.%s.%s", s.name, origName)

		var reqFields []string
		if len(t.InputSchema) > 0 {
			var schemaMap map[string]any
			if err := json.Unmarshal(t.InputSchema, &schemaMap); err == nil {
				if reqArr, ok := schemaMap["required"].([]any); ok {
					for _, r := range reqArr {
						if str, ok := r.(string); ok {
							reqFields = append(reqFields, str)
						}
					}
				}
			}
		}

		readonly := s.readonly
		sideEffect := "reversible"
		egressScope := s.name
		if readonly {
			sideEffect = "none"
			egressScope = ""
		}

		schema := CleanSchema(t.InputSchema)
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}

		spec := Spec{
			Name:            toolName,
			Description:     t.Description,
			Required:        reqFields,
			Timeout:         s.timeout,
			Readonly:        readonly,
			Idempotent:      readonly,
			SideEffectClass: sideEffect,
			EgressScope:     egressScope,
			Parameters:      schema,
			Execute: func(execCtx context.Context, args json.RawMessage) (json.RawMessage, error) {
				return s.executeTool(execCtx, origName, args)
			},
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

func (s *StdioMCP) executeTool(ctx context.Context, origName string, args json.RawMessage) (json.RawMessage, error) {
	timeout := s.timeout
	if timeout <= 0 {
		timeout = DefaultMCPTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var argMap any
	if len(args) > 0 {
		_ = json.Unmarshal(args, &argMap)
	}
	if argMap == nil {
		argMap = map[string]any{}
	}

	callParams := map[string]any{
		"name":      origName,
		"arguments": argMap,
	}

	respRaw, err := s.call(cctx, "tools/call", callParams)
	if err != nil {
		if errors.Is(cctx.Err(), context.DeadlineExceeded) || strings.Contains(err.Error(), "timed out") {
			return nil, domain.E(domain.CodeToolError, "mcp tool call timed out after 15s")
		}
		return nil, asDomain(err)
	}

	var callRes struct {
		Content []MCPText `json:"content"`
		IsError bool      `json:"isError"`
	}
	_ = json.Unmarshal(respRaw, &callRes)

	truncated := false
	totalLen := 0
	var truncatedContent []MCPText

	if len(callRes.Content) > 0 {
		for _, block := range callRes.Content {
			textBytes := []byte(block.Text)
			if totalLen+len(textBytes) > MaxMCPResultBytes {
				rem := MaxMCPResultBytes - totalLen
				if rem > 0 {
					truncatedContent = append(truncatedContent, MCPText{
						Type: block.Type,
						Text: string(textBytes[:rem]),
					})
				}
				truncated = true
				totalLen = MaxMCPResultBytes
				break
			}
			truncatedContent = append(truncatedContent, block)
			totalLen += len(textBytes)
		}
	} else if len(respRaw) > 0 {
		text := string(respRaw)
		if len(text) > MaxMCPResultBytes {
			text = text[:MaxMCPResultBytes]
			truncated = true
		}
		truncatedContent = []MCPText{{Type: "text", Text: text}}
	}

	out := map[string]any{
		"content":   truncatedContent,
		"isError":   callRes.IsError,
		"untrusted": true,
		"truncated": truncated,
	}
	if truncated {
		out["notice"] = fmt.Sprintf("truncated: mcp output capped at %d bytes (64KiB limit)", MaxMCPResultBytes)
	}

	outRaw, _ := json.Marshal(out)
	if callRes.IsError {
		errMsg := "mcp tool reported error"
		if len(truncatedContent) > 0 && truncatedContent[0].Text != "" {
			errMsg = truncatedContent[0].Text
		}
		return outRaw, domain.E(domain.CodeToolError, errMsg)
	}
	return outRaw, nil
}

func isSensitiveEnv(key string) bool {
	up := strings.ToUpper(strings.TrimSpace(key))
	switch {
	case up == "OPENAI_API_KEY", up == "ANTHROPIC_API_KEY", up == "AWS_SECRET_ACCESS_KEY":
		return true
	case strings.Contains(up, "SECRET"), strings.Contains(up, "PASSWORD"), strings.Contains(up, "TOKEN"):
		return true
	case strings.HasSuffix(up, "_KEY") && up != "CLOUD_HARNESS_WORKER":
		return true
	}
	return false
}

func buildSafeEnv() []string {
	safeKeys := []string{"PATH", "HOME", "LANG", "LC_ALL", "TERM", "TMPDIR"}
	var env []string
	for _, k := range safeKeys {
		if val, ok := os.LookupEnv(k); ok && !isSensitiveEnv(k) {
			env = append(env, k+"="+val)
		}
	}
	return env
}
