package transport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptrace"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/config"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/tools"
)

const mcpWireLimit = 4 << 20
const mcpVersion = "2025-11-25"

var mcpHTTP = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// NewRemoteMCPServer supplies the transport behind broker-owned registrations.
func NewRemoteMCPServer(cfg config.MCPServerConfig) tools.RemoteMCPServer {
	var auth *mcpOAuth
	remote := tools.RemoteMCPServer{}
	if cfg.OAuth != nil {
		auth = &mcpOAuth{cfg: *cfg.OAuth, resource: cfg.URL}
		remote.OAuth = auth
	}
	remote.Open = func() tools.MCPConnection { return newRemoteMCP(cfg, auth) }
	return remote
}

type remoteMCP struct {
	cfg              config.MCPServerConfig
	auth             *mcpOAuth
	life             context.Context
	cancel           context.CancelFunc
	seq              atomic.Int64
	mu               sync.RWMutex
	session, version string
	failure          error
}

func newRemoteMCP(cfg config.MCPServerConfig, auth *mcpOAuth) *remoteMCP {
	ctx, cancel := context.WithCancel(context.Background())
	return &remoteMCP{cfg: cfg, auth: auth, life: ctx, cancel: cancel}
}

func (s *remoteMCP) Err() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.failure
}
func (s *remoteMCP) Alive() bool { return s.life.Err() == nil && s.Err() == nil }
func (s *remoteMCP) Close() error {
	s.cancel()
	s.mu.RLock()
	session, version := s.session, s.version
	s.mu.RUnlock()
	if session == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, s.cfg.URL, nil)
	if err := s.headers(ctx, req, session, version); err != nil {
		return nil
	}
	if resp, err := mcpHTTP.Do(req); err == nil {
		_ = resp.Body.Close()
	}
	return nil
}

func (s *remoteMCP) headers(ctx context.Context, req *http.Request, session, version string) error {
	req.Header.Set("Accept", "application/json, text/event-stream")
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	if version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}
	token := ""
	if s.cfg.TokenEnv != "" {
		token = os.Getenv(s.cfg.TokenEnv)
		if token == "" {
			return errors.New("MCP token environment variable is unset")
		}
	}
	if s.auth != nil {
		var err error
		token, err = s.auth.bearer(ctx)
		if err != nil {
			return err
		}
	}
	if token != "" {
		if strings.ContainsAny(token, " \r\n\t") {
			return errors.New("invalid MCP bearer token")
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return nil
}

func (s *remoteMCP) Discover(ctx context.Context) ([]tools.Spec, error) {
	raw, err := s.post(ctx, "initialize", map[string]any{"protocolVersion": mcpVersion, "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "cloud-harness", "version": "1"}}, false)
	if err != nil {
		return nil, err
	}
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(raw, &init) != nil || !slices.Contains([]string{mcpVersion, "2025-06-18", "2025-03-26"}, init.ProtocolVersion) {
		return nil, errors.New("unsupported MCP protocol version")
	}
	s.mu.Lock()
	s.version = init.ProtocolVersion
	s.mu.Unlock()
	if _, err = s.post(ctx, "notifications/initialized", nil, true); err != nil {
		return nil, err
	}
	return tools.DiscoverMCPTools(ctx, s.cfg.Name, s.cfg.Readonly, true, tools.DefaultMCPTimeout, func(ctx context.Context, p map[string]any) (json.RawMessage, error) {
		return s.post(ctx, "tools/list", p, false)
	}, s.execute)
}

func (s *remoteMCP) execute(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, tools.DefaultMCPTimeout)
	defer cancel()
	if err := s.Err(); err != nil {
		return nil, err
	}
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage(`{}`)
	}
	raw, err := s.post(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, false)
	if err != nil {
		return nil, err
	}
	return tools.FormatMCPResult(raw)
}

func (s *remoteMCP) post(ctx context.Context, method string, params any, notify bool) (json.RawMessage, error) {
	return s.postOnce(ctx, method, params, notify, true)
}
func (s *remoteMCP) postOnce(ctx context.Context, method string, params any, notify, retryAuth bool) (json.RawMessage, error) {
	if s.life.Err() != nil {
		return nil, errors.New("MCP server is disconnected")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(s.life, cancel)
	defer stop()
	id := s.seq.Add(1)
	frame := map[string]any{"jsonrpc": "2.0", "method": method}
	if !notify {
		frame["id"] = id
	}
	if params != nil {
		frame["params"] = params
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		return nil, errors.New("invalid MCP arguments")
	}
	var sending atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteHeaders: func() { sending.Store(true) }})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.URL, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	s.mu.RLock()
	session, version := s.session, s.version
	s.mu.RUnlock()
	if err = s.headers(ctx, req, session, version); err != nil {
		if errors.Is(err, tools.ErrMCPAuthorization) {
			s.mu.Lock()
			s.failure = err
			s.mu.Unlock()
		}
		return nil, err
	}
	used := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	if s.auth != nil && used == "" && method != "initialize" {
		return nil, tools.ErrMCPAuthorization
	}
	uncertain := func(reason string) error {
		if sending.Load() && method == "tools/call" && !s.cfg.Readonly {
			return fmt.Errorf("%w: MCP tool outcome unknown: %s", domain.ErrUncertainOutcome, reason)
		}
		return errors.New(reason)
	}
	resp, err := mcpHTTP.Do(req)
	if err != nil {
		return nil, uncertain("MCP connection failed or timed out")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		err = fmt.Errorf("MCP refused access (HTTP %d); check credentials and scopes", resp.StatusCode)
		if s.auth != nil {
			if s.auth.reject(resp.Header.Values("WWW-Authenticate"), used, retryAuth && resp.StatusCode == 401) {
				_ = resp.Body.Close()
				return s.postOnce(ctx, method, params, notify, false)
			}
			err = tools.ErrMCPAuthorization
		}
		s.mu.Lock()
		s.failure = err
		s.mu.Unlock()
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound && session != "" {
		err = errors.New("MCP session expired; reconnect in Settings")
		s.mu.Lock()
		s.failure = err
		s.mu.Unlock()
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := fmt.Sprintf("MCP returned HTTP %d", resp.StatusCode)
		if resp.StatusCode >= 500 {
			return nil, uncertain(msg)
		}
		return nil, errors.New(msg)
	}
	if s.auth != nil && used == "" {
		return nil, tools.ErrMCPAuthorization
	}
	if notify {
		if resp.StatusCode != 202 {
			return nil, errors.New("MCP did not accept the initialized notification")
		}
		return nil, nil
	}
	data, err := readMCPResponse(resp, id)
	if errors.Is(err, errRPC) {
		return nil, err
	}
	if err != nil {
		return nil, uncertain(err.Error())
	}
	// A server that reflects an Authorization header must not put it in the
	// tool registry, model result or audit log.
	if containsMCPCredential(data, used) {
		return nil, uncertain("MCP response contained credential material")
	}
	if method == "initialize" {
		session = resp.Header.Get("Mcp-Session-Id")
		if len(session) > 2048 || strings.IndexFunc(session, func(r rune) bool { return r < 0x21 || r > 0x7e }) >= 0 {
			return nil, errors.New("invalid MCP session ID")
		}
		s.mu.Lock()
		s.session = session
		s.mu.Unlock()
	}
	if method == "tools/call" {
		var result struct {
			Content []json.RawMessage
			IsError bool
		}
		if json.Unmarshal(data, &result) != nil || result.Content == nil {
			return nil, uncertain("invalid MCP tools/call result")
		}
	}
	return data, nil
}

var errRPC = errors.New("MCP server returned a JSON-RPC error")
var errFrame = errors.New("invalid or incomplete MCP response")

// JSON string escapes must not turn a reflected bearer into a model result.
func containsMCPCredential(data []byte, bearer string) bool {
	if bearer == "" {
		return false
	}
	if bytes.Contains(data, []byte(bearer)) {
		return true
	}
	if !bytes.ContainsRune(data, '\\') {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		if value, ok := token.(string); ok && strings.Contains(value, bearer) {
			return true
		}
	}
}

func rpcResult(data []byte, id int64) (json.RawMessage, bool, error) {
	var frame struct {
		Version string `json:"jsonrpc"`
		ID      json.RawMessage
		Result  json.RawMessage
		Error   json.RawMessage
		Method  string
	}
	if json.Unmarshal(data, &frame) != nil || frame.Version != "2.0" {
		return nil, false, errFrame
	}
	if frame.Method != "" || string(frame.ID) != fmt.Sprint(id) {
		return nil, false, nil
	}
	if len(frame.Error) > 0 && string(frame.Error) != "null" {
		var rpcError struct {
			Code    *int
			Message *string
		}
		if len(frame.Result) > 0 || json.Unmarshal(frame.Error, &rpcError) != nil || rpcError.Code == nil || rpcError.Message == nil {
			return nil, true, errFrame
		}
		return nil, true, errRPC
	}
	if len(frame.Result) == 0 || frame.Result[0] != '{' {
		return nil, true, errFrame
	}
	return frame.Result, true, nil
}

func readMCPResponse(resp *http.Response, id int64) (json.RawMessage, error) {
	typ, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	input := io.LimitReader(resp.Body, mcpWireLimit+1)
	switch typ {
	case "application/json":
		data, err := io.ReadAll(input)
		if err != nil || len(data) > mcpWireLimit {
			return nil, errFrame
		}
		result, matched, err := rpcResult(data, id)
		if !matched {
			return nil, errFrame
		}
		return result, err
	case "text/event-stream":
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 4096), mcpWireLimit)
		var event bytes.Buffer
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				if len(bytes.TrimSpace(event.Bytes())) > 0 {
					result, matched, err := rpcResult(event.Bytes(), id)
					if err != nil || matched {
						return result, err
					}
				}
				event.Reset()
			} else if v, ok := strings.CutPrefix(line, "data:"); ok {
				event.WriteString(strings.TrimPrefix(v, " "))
				event.WriteByte('\n')
			}
		}
	}
	return nil, errFrame
}
