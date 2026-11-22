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

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

const (
	// maxMCPListPages bounds tools/list pagination for a server that never stops returning a cursor.
	maxMCPListPages = 100
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

	// writeMu serializes frames on stdin. It is separate from mu so a server
	// that stops reading cannot block state checks such as Alive.
	writeMu sync.Mutex
	// lifeMu serializes process startup, handshake, and restart so two calls
	// cannot race to replace a dead server.
	lifeMu      sync.Mutex
	mu          sync.Mutex
	conn        *mcpConn
	seq         atomic.Int64
	pending     map[int64]chan rpcResponse
	closed      bool
	closedErr   error
	shutdown    bool
	initialized bool
	// closeOnce makes Close idempotent. It is separate from closed: a server
	// that exits on its own is reaped and may be restarted by the next call.
	closeOnce sync.Once
}

type rpcResponse struct {
	result    json.RawMessage
	err       error
	transport bool
}

// mcpConn is one server process generation. waitOnce guarantees the child is
// reaped exactly once even when both the read loop and Close reach teardown.
type mcpConn struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	waitOnce sync.Once
}

func (c *mcpConn) reap() {
	c.waitOnce.Do(func() {
		if c.stdin != nil {
			_ = c.stdin.Close()
		}
		if c.cmd != nil {
			_ = killGroup(c.cmd)
			_ = c.cmd.Wait()
		}
	})
}

// uncertainOutcomeError carries a human message while marking the call as
// uncertain to the broker through domain.ErrUncertainOutcome.
type uncertainOutcomeError struct{ msg string }

func (e *uncertainOutcomeError) Error() string { return e.msg }
func (e *uncertainOutcomeError) Is(target error) bool {
	return target == domain.ErrUncertainOutcome
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
	if s.conn != nil && s.conn.cmd != nil && s.conn.cmd.Process != nil {
		return s.conn.cmd.Process.Pid
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
	return s.conn != nil && !s.closed && !s.shutdown
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
		// stuck on a full stdin pipe.
		s.cancel()
		s.mu.Lock()
		s.shutdown = true
		s.closed = true
		conn := s.conn
		s.mu.Unlock()
		if conn != nil {
			conn.reap()
		}
	})
	return nil
}

// ensureReady starts the server when needed and repeats the initialize
// handshake after a restart. The handshake shares the caller's deadline so a
// hung server cannot hold a tool call open past its own timeout.
func (s *StdioMCP) ensureReady(ctx context.Context) error {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if err := s.ensureProcessLocked(); err != nil {
		return err
	}
	s.mu.Lock()
	initialized := s.initialized
	s.mu.Unlock()
	if initialized {
		return nil
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
		return domain.E(domain.CodeToolError, "mcp initialize failed: "+err.Error())
	}
	if err := s.notify(ctx, "notifications/initialized", map[string]any{}); err != nil {
		s.mu.Lock()
		closed := s.closed || s.shutdown || s.conn == nil
		s.mu.Unlock()
		if closed {
			return domain.E(domain.CodeToolError, "mcp initialized notification failed: "+err.Error())
		}
	}
	s.mu.Lock()
	if s.closed || s.shutdown || s.conn == nil {
		s.mu.Unlock()
		return domain.E(domain.CodeToolError, "mcp server process terminated")
	}
	s.initialized = true
	s.mu.Unlock()
	return nil
}

// ensureProcessLocked starts a new process generation when the current one is
// missing or has exited. The caller must hold lifeMu.
func (s *StdioMCP) ensureProcessLocked() error {
	s.mu.Lock()
	if s.shutdown {
		s.mu.Unlock()
		return domain.E(domain.CodeToolError, "mcp adapter is closed")
	}
	if s.conn != nil && !s.closed {
		s.mu.Unlock()
		return nil
	}
	dead := s.conn
	s.conn = nil
	s.closed = false
	s.closedErr = nil
	s.initialized = false
	s.pending = make(map[int64]chan rpcResponse)
	s.mu.Unlock()

	if dead != nil {
		dead.reap()
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

	conn := &mcpConn{cmd: cmd, stdin: stdin, stdout: stdout}
	s.mu.Lock()
	if s.shutdown {
		s.mu.Unlock()
		conn.reap()
		return domain.E(domain.CodeToolError, "mcp adapter is closed")
	}
	s.conn = conn
	s.mu.Unlock()

	go s.readLoop(conn)
	return nil
}

func (s *StdioMCP) readLoop(conn *mcpConn) {
	br := bufio.NewReader(conn.stdout)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 {
				s.dispatchResponse(conn, trimmed)
			}
		}
		if err != nil {
			s.handleDisconnect(conn, err)
			return
		}
	}
}

func (s *StdioMCP) handleDisconnect(conn *mcpConn, err error) {
	s.mu.Lock()
	current := s.conn == conn
	if current {
		if !s.closed {
			s.closedErr = err
		}
		s.closed = true
		s.conn = nil
	}
	var pending map[int64]chan rpcResponse
	if current {
		pending = s.pending
		s.pending = make(map[int64]chan rpcResponse)
	}
	s.mu.Unlock()

	for _, ch := range pending {
		ch <- rpcResponse{err: domain.E(domain.CodeToolError, "mcp server process terminated: "+err.Error()), transport: true}
	}
	conn.reap()
}

func (s *StdioMCP) dispatchResponse(conn *mcpConn, data []byte) {
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
	if s.closed || s.conn != conn {
		s.mu.Unlock()
		return
	}
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

type callMeta struct {
	sent      bool
	transport bool
}

func (s *StdioMCP) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	result, _, err := s.callDetailed(ctx, method, params)
	return result, err
}

func (s *StdioMCP) callDetailed(ctx context.Context, method string, params any) (json.RawMessage, callMeta, error) {
	s.mu.Lock()
	if s.shutdown || s.closed || s.conn == nil {
		err := s.closedErr
		s.mu.Unlock()
		if err == nil {
			err = io.EOF
		}
		return nil, callMeta{}, domain.E(domain.CodeToolError, "mcp server process terminated: "+err.Error())
	}
	conn := s.conn
	id := s.seq.Add(1)
	ch := make(chan rpcResponse, 1)
	s.pending[id] = ch
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
		return nil, callMeta{}, domain.E(domain.CodeInvalidArgument, "failed to marshal json-rpc request: "+err.Error())
	}

	sent, writeErr := s.writeFrame(ctx, conn, raw)
	if writeErr != nil {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, callMeta{sent: sent, transport: true}, domain.E(domain.CodeToolError, "failed to write to mcp server stdin: "+writeErr.Error())
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, callMeta{sent: true, transport: true}, domain.E(domain.CodeToolError, "mcp server closed connection")
		}
		return resp.result, callMeta{sent: true, transport: resp.transport}, resp.err
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, callMeta{sent: true, transport: true}, domain.E(domain.CodeToolError, "mcp "+method+" request timed out")
		}
		return nil, callMeta{sent: true, transport: true}, domain.E(domain.CodeToolError, "mcp "+method+" request cancelled")
	}
}

// writeFrame sends one newline-terminated frame and gives up when ctx ends. A
// write that started but cannot finish means the server stopped reading stdin;
// the frame may be half written, so the server is stopped instead of reused.
// The boolean reports whether the complete frame was written.
func (s *StdioMCP) writeFrame(ctx context.Context, conn *mcpConn, raw []byte) (bool, error) {
	const (
		frameQueued int32 = iota
		frameWriting
		frameAbandoned
	)
	var state atomic.Int32
	done := make(chan error, 1)
	go func() {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		if !state.CompareAndSwap(frameQueued, frameWriting) {
			return
		}
		s.mu.Lock()
		closed := s.closed || s.shutdown || s.conn != conn
		s.mu.Unlock()
		if closed {
			done <- errors.New("mcp server process terminated")
			return
		}
		_, err := conn.stdin.Write(append(raw, '\n'))
		done <- err
	}()
	select {
	case err := <-done:
		return err == nil, err
	case <-ctx.Done():
	}
	if state.CompareAndSwap(frameQueued, frameAbandoned) {
		// Still waiting behind another frame: nothing was sent.
		return false, ctx.Err()
	}
	select {
	case err := <-done:
		return err == nil, err
	default:
	}
	s.handleDisconnect(conn, errors.New("server stopped reading stdin"))
	return false, ctx.Err()
}

func (s *StdioMCP) notify(ctx context.Context, method string, params any) error {
	s.mu.Lock()
	if s.shutdown || s.closed || s.conn == nil {
		s.mu.Unlock()
		return domain.E(domain.CodeToolError, "mcp server process terminated")
	}
	conn := s.conn
	s.mu.Unlock()
	reqBody := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	_, err = s.writeFrame(ctx, conn, raw)
	return err
}

// Discover performs initialize handshake and tools/list discovery. It returns Spec items
// mapped with the naming format mcp.<server>.<tool>. Discover only returns Specs and does
// not execute tools.
func (s *StdioMCP) Discover(ctx context.Context) ([]Spec, error) {
	if err := s.ensureReady(ctx); err != nil {
		return nil, err
	}

	type listedTool struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	var listed []listedTool
	// tools/list is paginated: keep asking while the server returns a cursor.
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxMCPListPages {
			return nil, domain.E(domain.CodeToolError, fmt.Sprintf("mcp tools/list did not end within %d pages", maxMCPListPages))
		}
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		listResp, err := s.call(ctx, "tools/list", params)
		if err != nil {
			return nil, domain.E(domain.CodeToolError, "mcp tools/list failed: "+err.Error())
		}
		var listResult struct {
			Tools      []listedTool `json:"tools"`
			NextCursor string       `json:"nextCursor"`
		}
		if err := json.Unmarshal(listResp, &listResult); err != nil {
			return nil, domain.E(domain.CodeToolError, "invalid tools/list response: "+err.Error())
		}
		listed = append(listed, listResult.Tools...)
		if listResult.NextCursor == "" {
			break
		}
		cursor = listResult.NextCursor
	}

	specs := make([]Spec, 0, len(listed))
	for _, t := range listed {
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

	// Forward the argument bytes as they are: decoding into any would turn
	// integers above 2^53 into rounded float64 values.
	var arguments any = map[string]any{}
	if trimmed := bytes.TrimSpace(args); len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		if !json.Valid(trimmed) {
			return nil, domain.E(domain.CodeInvalidArgument, "mcp tool arguments are not valid json")
		}
		arguments = json.RawMessage(trimmed)
	}

	if err := s.ensureReady(cctx); err != nil {
		return nil, asDomain(err)
	}

	callParams := map[string]any{
		"name":      origName,
		"arguments": arguments,
	}

	respRaw, meta, err := s.callDetailed(cctx, "tools/call", callParams)
	if err != nil {
		if meta.sent && meta.transport && !s.readonly {
			msg := "mcp tool call did not return a confirmed result; the outcome is unknown: the server may still have performed it, so check before retrying"
			if errors.Is(cctx.Err(), context.DeadlineExceeded) || strings.Contains(err.Error(), "timed out") {
				msg = fmt.Sprintf("mcp tool call timed out after %s; the outcome is unknown: the server may still have performed it, so check before retrying", timeout)
			}
			return nil, &uncertainOutcomeError{msg: msg}
		}
		if errors.Is(cctx.Err(), context.DeadlineExceeded) || strings.Contains(err.Error(), "timed out") {
			return nil, domain.E(domain.CodeToolError, fmt.Sprintf("mcp tool call timed out after %s", timeout))
		}
		return nil, asDomain(err)
	}

	var callRes struct {
		Content []json.RawMessage `json:"content"`
		IsError bool              `json:"isError"`
	}
	_ = json.Unmarshal(respRaw, &callRes)

	truncated := false
	totalLen := 0
	var truncatedContent []MCPText

	if len(callRes.Content) > 0 {
		for _, raw := range callRes.Content {
			block := mcpContentText(raw)
			if rem := MaxMCPResultBytes - totalLen; len(block.Text) > rem {
				if rem > 0 {
					block.Text = block.Text[:rem]
					truncatedContent = append(truncatedContent, block)
				}
				truncated = true
				break
			}
			truncatedContent = append(truncatedContent, block)
			totalLen += len(block.Text)
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

// mcpContentText turns one tools/call content block into text for the model.
// Tool results reach the model as text, so binary payloads are replaced by a
// note saying what was left out instead of being dropped without a trace.
func mcpContentText(raw json.RawMessage) MCPText {
	var block struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Data     string `json:"data"`
		MimeType string `json:"mimeType"`
		URI      string `json:"uri"`
		Name     string `json:"name"`
		Resource *struct {
			URI      string `json:"uri"`
			MimeType string `json:"mimeType"`
			Text     string `json:"text"`
			Blob     string `json:"blob"`
		} `json:"resource"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		return MCPText{Type: "text", Text: string(raw)}
	}
	omitted := func(kind, mime string, encoded int) string {
		if mime == "" {
			mime = "unknown type"
		}
		return fmt.Sprintf("[%s omitted: %s, %d base64 bytes; binary content is not forwarded to the model]", kind, mime, encoded)
	}
	switch block.Type {
	case "text":
		return MCPText{Type: "text", Text: block.Text}
	case "image", "audio":
		return MCPText{Type: block.Type, Text: omitted(block.Type, block.MimeType, len(block.Data))}
	case "resource":
		if block.Resource == nil {
			break
		}
		res := block.Resource
		if res.Blob != "" {
			return MCPText{Type: "resource", Text: omitted("resource "+res.URI, res.MimeType, len(res.Blob))}
		}
		return MCPText{Type: "resource", Text: "[resource " + res.URI + "]\n" + res.Text}
	case "resource_link":
		return MCPText{Type: "resource_link", Text: strings.TrimSpace("[resource link " + block.URI + "] " + block.Name)}
	}
	// Unrecognized block: hand over the JSON as received.
	return MCPText{Type: "text", Text: string(raw)}
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
