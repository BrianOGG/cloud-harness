package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/config"
	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/storage/sqlite"
	"github.com/cloud-harness/cloud-harness/internal/tools"
)

// TestMCPHelperServer is not a test: the binary re-executes itself through it
// to act as a stdio MCP server. Arguments after "--": mcp-test-server MODE STATE.
func TestMCPHelperServer(t *testing.T) {
	args := os.Args
	for i, a := range args {
		if a == "mcp-test-server" && i+2 < len(args) {
			runMCPTestServer(args[i+1], args[i+2])
			os.Exit(0)
		}
	}
}

func runMCPTestServer(mode, state string) {
	_ = os.WriteFile(state+".pid", []byte(strconv.Itoa(os.Getpid())), 0o600)
	if mode == "crash" {
		fmt.Fprintln(os.Stderr, "mcp-test-server: refusing to start")
		os.Exit(3)
	}
	s := bufio.NewScanner(os.Stdin)
	s.Buffer(make([]byte, 1024), 256*1024)
	for s.Scan() {
		var req struct {
			ID     any             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(s.Bytes(), &req) != nil || req.ID == nil || mode == "silent" {
			continue
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05", "serverInfo": map[string]string{"name": "test", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{
				"name": "echo", "description": "echo a message",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"msg": map[string]string{"type": "string"}}, "required": []string{"msg"}},
			}}}
		case "tools/call":
			var p struct {
				Name string `json:"name"`
				Args struct {
					Msg string `json:"msg"`
				} `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			f, err := os.OpenFile(state, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err == nil {
				fmt.Fprintf(f, "%s %s\n", p.Name, p.Args.Msg)
				_ = f.Close()
			}
			result = map[string]any{"content": []any{map[string]string{"type": "text", "text": "echoed:" + p.Args.Msg}}}
		}
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		_, _ = os.Stdout.Write(append(out, '\n'))
	}
}

func mcpTestServerConfig(name, mode, state string, readonly bool) config.MCPServerConfig {
	return config.MCPServerConfig{
		Name: name, Command: os.Args[0], Readonly: readonly,
		Args: []string{"-test.run=TestMCPHelperServer", "--", "mcp-test-server", mode, state},
	}
}

type mcpModelRequest struct {
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

func (r mcpModelRequest) hasTool(name string) bool {
	for _, tool := range r.Tools {
		if tool.Function.Name == name {
			return true
		}
	}
	return false
}

func (r mcpModelRequest) toolResults() []string {
	var out []string
	for _, m := range r.Messages {
		if m.Role == "tool" {
			out = append(out, string(m.Content))
		}
	}
	return out
}

func mcpToolCallCompletion(id, name, args string) string {
	quoted, _ := json.Marshal(args)
	return `{"id":"cmpl_` + id + `","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"` + id + `","type":"function","function":{"name":"` + name + `","arguments":` + string(quoted) + `}}]}}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`
}

func mcpTextCompletion(text string) string {
	quoted, _ := json.Marshal(text)
	return `{"id":"cmpl_text","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":` + string(quoted) + `}}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`
}

type mcpTestEnv struct {
	t         *testing.T
	workspace string
	db        string
	state     string
	policyDir string

	mu       sync.Mutex
	requests []mcpModelRequest
}

// newMCPTestEnv points the CLI at a scripted OpenAI-compatible model and a
// config file listing servers. script answers each model request.
func newMCPTestEnv(t *testing.T, script func(mcpModelRequest) string, servers func(state string) []config.MCPServerConfig) *mcpTestEnv {
	t.Helper()
	root := t.TempDir()
	env := &mcpTestEnv{t: t, workspace: filepath.Join(root, "ws"), db: filepath.Join(root, "h.db"), state: filepath.Join(root, "mcp-state"), policyDir: root}
	if err := os.MkdirAll(env.workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			// doctor probes the provider with other endpoints.
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[]}`)
			return
		}
		var req mcpModelRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Errorf("model request: %v", err)
		}
		env.mu.Lock()
		env.requests = append(env.requests, req)
		env.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, script(req))
	}))
	t.Cleanup(srv.Close)
	cfg, err := json.Marshal(map[string]any{"provider": "openai", "servers": servers(env.state)})
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.json")
	if err := os.WriteFile(configPath, cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	setDiagnosticEnv(t, configPath, "sk-test-mcp")
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	return env
}

func (e *mcpTestEnv) common() []string {
	return []string{"--db", e.db, "--workspace", e.workspace}
}

func (e *mcpTestEnv) run(message string) (domain.RunStatus, domain.WaitReason, string, string) {
	e.t.Helper()
	var out, errBuf bytes.Buffer
	code := Main(append([]string{"run", "--no-repomap", "--message", message}, e.common()...), &out, &errBuf)
	var view struct {
		RunID      string            `json:"run_id"`
		Status     domain.RunStatus  `json:"status"`
		WaitReason domain.WaitReason `json:"wait_reason"`
	}
	if err := json.Unmarshal(out.Bytes(), &view); err != nil || code != 0 {
		e.t.Fatalf("run code=%d out=%q stderr=%q err=%v", code, out.String(), errBuf.String(), err)
	}
	return view.Status, view.WaitReason, view.RunID, errBuf.String()
}

func (e *mcpTestEnv) calls() []string {
	e.t.Helper()
	raw, err := os.ReadFile(e.state)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func (e *mcpTestEnv) modelRequests() []mcpModelRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]mcpModelRequest(nil), e.requests...)
}

func (e *mcpTestEnv) lastToolResults() string {
	reqs := e.modelRequests()
	if len(reqs) == 0 {
		return ""
	}
	return strings.Join(reqs[len(reqs)-1].toolResults(), "\n")
}

// requireServerGone fails when the server that recorded its PID next to state is still running.
func requireServerGone(t *testing.T, state string) {
	t.Helper()
	raw, err := os.ReadFile(state + ".pid")
	if err != nil {
		t.Fatalf("mcp server never started: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("mcp server %d is still running after the command returned", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// echoScript calls the MCP tool once and then reports what came back.
func echoScript(req mcpModelRequest) string {
	if results := req.toolResults(); len(results) > 0 {
		return mcpTextCompletion("model saw tool result")
	}
	return mcpToolCallCompletion("call_echo", "mcp_files_echo", `{"msg":"hello"}`)
}

func oneEchoServer(readonly bool) func(string) []config.MCPServerConfig {
	return func(state string) []config.MCPServerConfig {
		return []config.MCPServerConfig{mcpTestServerConfig("files", "ok", state, readonly)}
	}
}

func TestMCPToolRunsThroughApprovalEndToEnd(t *testing.T) {
	env := newMCPTestEnv(t, echoScript, func(state string) []config.MCPServerConfig {
		return []config.MCPServerConfig{
			{Name: "broken", Command: filepath.Join(t.TempDir(), "missing-mcp-binary")},
			mcpTestServerConfig("files", "ok", state, false),
			mcpTestServerConfig("crashed", "crash", state+"-crashed", false),
		}
	})
	status, wait, runID, stderr := env.run("echo hello through mcp")
	for _, name := range []string{"broken", "crashed"} {
		if !strings.Contains(stderr, `warning: mcp server "`+name+`" is unavailable`) {
			t.Fatalf("no startup warning for %s: %q", name, stderr)
		}
	}
	if strings.Contains(stderr, `mcp server "files"`) {
		t.Fatalf("healthy server was reported as a problem: %q", stderr)
	}
	if status != domain.RunWaiting || wait != domain.WaitApproval {
		t.Fatalf("mcp call must wait for approval, got %s/%s", status, wait)
	}
	if calls := env.calls(); len(calls) != 0 {
		t.Fatalf("mcp tool executed before approval: %v", calls)
	}
	reqs := env.modelRequests()
	if len(reqs) == 0 || !reqs[0].hasTool("mcp_files_echo") || !reqs[0].hasTool("read_file") {
		t.Fatalf("model was not offered the mcp tool: %+v", reqs)
	}
	if reqs[0].hasTool("mcp_broken_echo") || reqs[0].hasTool("mcp_crashed_echo") {
		t.Fatal("tools of a failed server were offered to the model")
	}
	requireServerGone(t, env.state)

	var out, errBuf bytes.Buffer
	code := Main(append([]string{"runs", "resume", "--decision", "approve"}, append(env.common(), runID)...), &out, &errBuf)
	if code != 0 {
		t.Fatalf("resume code=%d out=%q stderr=%q", code, out.String(), errBuf.String())
	}
	if calls := env.calls(); len(calls) != 1 || calls[0] != "echo hello" {
		t.Fatalf("approved mcp call did not execute exactly once: %v", calls)
	}
	if got := env.lastToolResults(); !strings.Contains(got, "echoed:hello") {
		t.Fatalf("mcp result was not returned to the model: %q", got)
	}
	requireServerGone(t, env.state)

	ctx := context.Background()
	db, err := sqlite.Open(ctx, env.db)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	run, err := db.GetRun(ctx, runID)
	if err != nil || run.Status != domain.RunCompleted || run.FinalAnswer != "model saw tool result" {
		t.Fatalf("run after approval: %+v %v", run, err)
	}
	batches, err := db.LoadBatches(ctx, runID)
	if err != nil || len(batches) != 1 || len(batches[0].Invocations) != 1 {
		t.Fatalf("batches %+v %v", batches, err)
	}
	if inv := batches[0].Invocations[0]; inv.Name != "mcp.files.echo" || inv.Status != domain.InvSucceeded {
		t.Fatalf("stored invocation must keep the harness tool name: %+v", inv)
	}
	out.Reset()
	if code := Main(append([]string{"policy", "decisions", "--tool", "mcp.files.echo", "--json"}, env.common()...), &out, &errBuf); code != 0 {
		t.Fatalf("policy decisions code=%d stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), `"mcp.files.echo"`) || !strings.Contains(out.String(), `"ask"`) {
		t.Fatalf("mcp call left no policy audit record: %s", out.String())
	}
}

func TestMCPToolFollowsPolicyFile(t *testing.T) {
	for _, tc := range []struct {
		effect     string
		workspace  bool
		wantStatus domain.RunStatus
		wantCalls  int
	}{
		{effect: "allow", workspace: true, wantStatus: domain.RunCompleted, wantCalls: 1},
		{effect: "allow", workspace: false, wantStatus: domain.RunCompleted, wantCalls: 1},
		{effect: "deny", workspace: true, wantStatus: domain.RunCompleted, wantCalls: 0},
		{effect: "ask", workspace: true, wantStatus: domain.RunWaiting, wantCalls: 0},
	} {
		t.Run(fmt.Sprintf("%s/workspace=%t", tc.effect, tc.workspace), func(t *testing.T) {
			env := newMCPTestEnv(t, echoScript, oneEchoServer(false))
			dir := env.policyDir
			if tc.workspace {
				dir = filepath.Join(env.workspace, ".cloud-harness")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			policy := `{"rules":[{"tool":"mcp.files.echo","effect":"` + tc.effect + `"}]}`
			if err := os.WriteFile(filepath.Join(dir, "policy.json"), []byte(policy), 0o600); err != nil {
				t.Fatal(err)
			}
			status, _, _, stderr := env.run("echo hello through mcp")
			if status != tc.wantStatus {
				t.Fatalf("status %s, want %s (stderr %q)", status, tc.wantStatus, stderr)
			}
			if calls := env.calls(); len(calls) != tc.wantCalls {
				t.Fatalf("mcp executions %v, want %d", calls, tc.wantCalls)
			}
			results := env.lastToolResults()
			switch tc.effect {
			case "allow":
				if !strings.Contains(results, "echoed:hello") {
					t.Fatalf("allowed call returned %q", results)
				}
			case "deny":
				if !strings.Contains(results, domain.CodeAuthDenied) || strings.Contains(results, "echoed:hello") {
					t.Fatalf("denied call returned %q", results)
				}
			}
		})
	}
}

func TestMCPReadonlyServerToolIsNotGrantedByDefault(t *testing.T) {
	env := newMCPTestEnv(t, echoScript, oneEchoServer(true))
	status, _, _, _ := env.run("echo hello through readonly mcp")
	if calls := env.calls(); len(calls) != 0 {
		t.Fatalf("readonly mcp tool ran without a policy allow: %v (status %s)", calls, status)
	}
	if status != domain.RunCompleted || !strings.Contains(env.lastToolResults(), domain.CodeAuthDenied) {
		t.Fatalf("status %s results %q", status, env.lastToolResults())
	}
}

func TestMCPChildRunDoesNotGetMCPToolsWithoutApproval(t *testing.T) {
	for _, allowed := range []string{``, `,"allowed_tools":["mcp.files.echo"]`} {
		t.Run("allowed_tools="+allowed, func(t *testing.T) {
			script := func(req mcpModelRequest) string {
				root := req.hasTool("delegate_task")
				results := req.toolResults()
				switch {
				case root && len(results) == 0:
					return mcpToolCallCompletion("call_delegate", "delegate_task", `{"goal":"echo hello through mcp"`+allowed+`,"max_model_calls":4,"max_tool_calls":4}`)
				case root:
					return mcpTextCompletion("parent done")
				case len(results) == 0:
					return mcpToolCallCompletion("call_child_echo", "mcp_files_echo", `{"msg":"from-child"}`)
				default:
					return mcpTextCompletion("child done")
				}
			}
			env := newMCPTestEnv(t, script, oneEchoServer(false))
			status, _, _, stderr := env.run("delegate the echo")
			if calls := env.calls(); len(calls) != 0 {
				t.Fatalf("child run executed an mcp tool nobody approved: %v", calls)
			}
			if status != domain.RunCompleted {
				t.Fatalf("parent status %s stderr %q", status, stderr)
			}
			attempted := false
			for _, req := range env.modelRequests() {
				if !req.hasTool("delegate_task") && req.hasTool("mcp_files_echo") {
					attempted = true
				}
			}
			if got := env.lastToolResults(); !strings.Contains(got, "child run entered WAITING (APPROVAL)") {
				t.Fatalf("parent was not told the child needed approval: %q", got)
			}
			if !attempted {
				t.Fatal("child run was never offered the mcp tool, so the test proved nothing")
			}
		})
	}
}

func TestChatStartsAndStopsMCPServers(t *testing.T) {
	env := newMCPTestEnv(t, echoScript, oneEchoServer(false))
	var out, errBuf bytes.Buffer
	code := cmdChat(append([]string{"--message", "echo hello through mcp"}, env.common()...), strings.NewReader("approve\n"), &out, &errBuf)
	if code != 0 {
		t.Fatalf("chat code=%d out=%q stderr=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "mcp.files.echo") || !strings.Contains(out.String(), "model saw tool result") {
		t.Fatalf("chat output %q", out.String())
	}
	if calls := env.calls(); len(calls) != 1 {
		t.Fatalf("mcp executions %v", calls)
	}
	requireServerGone(t, env.state)
}

func TestDoctorProbesMCPServers(t *testing.T) {
	old := doctorMCPProbeTimeout
	doctorMCPProbeTimeout = 2 * time.Second
	t.Cleanup(func() { doctorMCPProbeTimeout = old })
	root := t.TempDir()
	state := filepath.Join(root, "state")
	healthy := mcpTestServerConfig("files", "ok", state, true)
	silent := mcpTestServerConfig("silent", "silent", state+"-silent", false)
	missing := config.MCPServerConfig{Name: "missing", Command: filepath.Join(root, "no-such-binary")}

	item := checkMCP(context.Background(), []config.MCPServerConfig{healthy}, nil)
	if item.Status != "ok" || !strings.Contains(item.Detail, "files (readonly): 1 tool(s)") || !strings.Contains(item.Detail, "probed and stopped") {
		t.Fatalf("healthy probe: %+v", item)
	}
	if len(item.Servers) != 1 || item.Servers[0].State != tools.MCPStateRunning || len(item.Servers[0].Tools) != 1 || item.Servers[0].Tools[0] != "mcp.files.echo" {
		t.Fatalf("healthy probe servers: %+v", item.Servers)
	}
	requireServerGone(t, state)

	item = checkMCP(context.Background(), []config.MCPServerConfig{healthy, silent, missing}, nil)
	if item.Status != "fail" || !strings.Contains(item.Detail, "2 of 3 server(s) unavailable") {
		t.Fatalf("mixed probe: %+v", item)
	}
	for _, want := range []string{"files (readonly): 1 tool(s)", "silent: unavailable", "missing: unavailable"} {
		if !strings.Contains(item.Detail, want) {
			t.Fatalf("mixed probe detail lacks %q: %s", want, item.Detail)
		}
	}
	requireServerGone(t, state)
	requireServerGone(t, state+"-silent")

	if item := checkMCP(context.Background(), nil, nil); item.Status != "ok" || item.Detail != "0 server(s) configured" {
		t.Fatalf("no servers: %+v", item)
	}
	dup := checkMCP(context.Background(), []config.MCPServerConfig{healthy, healthy}, nil)
	if dup.Status != "fail" || !strings.Contains(dup.Detail, "duplicates") {
		t.Fatalf("duplicate names: %+v", dup)
	}
}

func TestDoctorCLIFailsWhenMCPServerCannotStart(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"provider":"mock","servers":[{"name":"gone","command":"`+filepath.Join(root, "no-such-binary")+`"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	setDiagnosticEnv(t, configPath, "")
	var out, errBuf bytes.Buffer
	code := Main([]string{"doctor", "--db", filepath.Join(root, "h.db"), "--workspace", root, "--json"}, &out, &errBuf)
	var checks []doctorItem
	if err := json.Unmarshal(out.Bytes(), &checks); err != nil {
		t.Fatalf("doctor output %q: %v", out.String(), err)
	}
	got := findDoctor(checks, "mcp")
	if code == 0 || got == nil || got.Status != "fail" || !strings.Contains(got.Detail, "gone: unavailable") {
		t.Fatalf("doctor code=%d mcp=%+v", code, got)
	}
	if len(got.Servers) != 1 || got.Servers[0].State != tools.MCPStateFailed || got.Servers[0].Error == "" {
		t.Fatalf("doctor mcp servers: %+v", got.Servers)
	}
}

func TestServeDoctorReportsLiveMCPState(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	cfg := config.Config{Provider: "mock", ListenAddr: "127.0.0.1:0", DBPath: filepath.Join(root, "h.db"), Servers: []config.MCPServerConfig{mcpTestServerConfig("files", "ok", state, false)}}
	mgr, db, err := openManager(context.Background(), cfg, root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var warnings bytes.Buffer
	set := startMCP(context.Background(), cfg, mgr, &warnings)
	defer set.Close()
	if warnings.Len() != 0 || !mgr.Broker.Has("mcp.files.echo") {
		t.Fatalf("startup warnings %q, registered=%t", warnings.String(), mgr.Broker.Has("mcp.files.echo"))
	}
	doctor := func() *doctorItem {
		rec := httptest.NewRecorder()
		serveDiagnostics{cfg: cfg, workspace: root, mcp: set}.doctor(rec, httptest.NewRequest(http.MethodGet, "/v1/doctor", nil))
		var checks []doctorItem
		if err := json.Unmarshal(rec.Body.Bytes(), &checks); err != nil {
			t.Fatalf("doctor body %q: %v", rec.Body.String(), err)
		}
		return findDoctor(checks, "mcp")
	}
	if got := doctor(); got == nil || got.Status != "ok" || !strings.Contains(got.Detail, "running in this process") || !strings.Contains(got.Detail, "files: 1 tool(s)") {
		t.Fatalf("live doctor: %+v", got)
	}
	raw, err := os.ReadFile(state + ".pid")
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(string(raw))
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := doctor()
		if got != nil && got.Status == "fail" && strings.Contains(got.Detail, "files: unavailable: server process exited after startup") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("doctor never reported the dead server: %+v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestServeRunsMCPToolAndStopsServerOnShutdown(t *testing.T) {
	env := newMCPTestEnv(t, echoScript, func(state string) []config.MCPServerConfig {
		return []config.MCPServerConfig{
			mcpTestServerConfig("files", "ok", state, false),
			{Name: "broken", Command: filepath.Join(t.TempDir(), "missing-mcp-binary")},
		}
	})
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := reserved.Addr().String()
	_ = reserved.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// TestServeHTTPBindBaseline doubles as the entry point that runs Main in a child process.
	cmd := exec.Command(executable, append([]string{"-test.run=^TestServeHTTPBindBaseline$", "--", "serve", "--addr", addr}, env.common()...)...)
	cmd.Env = append(os.Environ(), "CLOUD_HARNESS_M1032_SERVE_HELPER=1")
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var processErr error
	go func() {
		processErr = cmd.Wait()
		close(done)
	}()
	stop := func() {
		select {
		case <-done:
			return
		default:
		}
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(serveStopGrace):
			_ = cmd.Process.Kill()
			<-done
		}
	}
	defer stop()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second}
	call := func(method, path, body string, dest any) int {
		t.Helper()
		var reader io.Reader
		if body != "" {
			reader = strings.NewReader(body)
		}
		req, err := http.NewRequest(method, "http://"+addr+path, reader)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if dest != nil {
			if err := json.Unmarshal(raw, dest); err != nil {
				t.Fatalf("%s %s: %q: %v", method, path, raw, err)
			}
		}
		return resp.StatusCode
	}
	deadline := time.Now().Add(20 * time.Second)
	for call(http.MethodGet, "/healthz", "", nil) != http.StatusOK {
		select {
		case <-done:
			t.Fatalf("serve exited before listening: %v stdout=%s stderr=%s", processErr, out.String(), stderr.String())
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve did not become healthy: stdout=%s stderr=%s", out.String(), stderr.String())
		}
	}

	var checks []doctorItem
	if code := call(http.MethodGet, "/v1/doctor", "", &checks); code != http.StatusOK {
		t.Fatalf("doctor status %d", code)
	}
	if got := findDoctor(checks, "mcp"); got == nil || got.Status != "fail" || !strings.Contains(got.Detail, "1 of 2 server(s) unavailable (running in this process)") || !strings.Contains(got.Detail, "files: 1 tool(s)") {
		t.Fatalf("serve doctor mcp: %+v", got)
	}

	var session struct {
		ID string `json:"id"`
	}
	if code := call(http.MethodPost, "/v1/sessions", "", &session); code != http.StatusCreated {
		t.Fatalf("create session %d", code)
	}
	var run struct {
		ID          string            `json:"id"`
		Status      domain.RunStatus  `json:"status"`
		WaitReason  domain.WaitReason `json:"wait_reason"`
		FinalAnswer string            `json:"final_answer"`
	}
	if code := call(http.MethodPost, "/v1/sessions/"+session.ID+"/runs", `{"message":"echo hello through mcp"}`, &run); code != http.StatusCreated {
		t.Fatalf("create run %d", code)
	}
	if run.Status != domain.RunWaiting || run.WaitReason != domain.WaitApproval || len(env.calls()) != 0 {
		t.Fatalf("serve run must wait for approval before the mcp call: %+v calls=%v", run, env.calls())
	}
	if code := call(http.MethodPost, "/v1/runs/"+run.ID+"/responses", `{"decision":"approve"}`, &run); code != http.StatusOK {
		t.Fatalf("approve %d", code)
	}
	if run.Status != domain.RunCompleted || run.FinalAnswer != "model saw tool result" {
		t.Fatalf("serve run after approval: %+v", run)
	}
	if calls := env.calls(); len(calls) != 1 || !strings.Contains(env.lastToolResults(), "echoed:hello") {
		t.Fatalf("calls %v results %q", calls, env.lastToolResults())
	}

	stop()
	if processErr != nil {
		t.Fatalf("serve shutdown: %v stderr=%s", processErr, stderr.String())
	}
	if !strings.Contains(stderr.String(), `warning: mcp server "broken" is unavailable`) {
		t.Fatalf("serve did not warn about the failed server: %q", stderr.String())
	}
	requireServerGone(t, env.state)
}
