package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

// runMockMCPServer serves the mock tools. mode "paged" splits tools/list over
// two pages; mode "deaf" stops reading stdin once discovery is done.
func runMockMCPServer(mode string) {
	s := bufio.NewScanner(os.Stdin)
	s.Buffer(make([]byte, 1024), 256*1024)
	for s.Scan() {
		var req struct {
			ID     any             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(s.Bytes(), &req) != nil || req.ID == nil {
			continue
		}
		var res any
		switch {
		case req.Method == "tools/list" && mode == "paged":
			var p struct {
				Cursor string `json:"cursor"`
			}
			_ = json.Unmarshal(req.Params, &p)
			tool := func(name string) any {
				return map[string]any{"name": name, "description": name, "inputSchema": map[string]any{"type": "object"}}
			}
			page := map[string]any{"tools": []any{tool("first")}, "nextCursor": "page-2"}
			if p.Cursor == "page-2" {
				page = map[string]any{"tools": []any{tool("second")}}
			}
			out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": page})
			_, _ = os.Stdout.Write(append(out, '\n'))
			continue
		case req.Method == "tools/call" && mode == "media":
			out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"content": []any{
					map[string]any{"type": "image", "data": "aGVsbG8=", "mimeType": "image/png"},
					map[string]any{"type": "resource", "resource": map[string]any{"uri": "file:///notes.txt", "mimeType": "text/plain", "text": "note body"}},
					map[string]any{"type": "resource", "resource": map[string]any{"uri": "file:///a.bin", "mimeType": "application/octet-stream", "blob": "AAAA"}},
					map[string]any{"type": "resource_link", "uri": "file:///big.log", "name": "big.log"},
				},
			}})
			_, _ = os.Stdout.Write(append(out, '\n'))
			continue
		case req.Method == "tools/call" && mode == "raw":
			var p struct {
				Args json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"content": []any{map[string]string{"type": "text", "text": string(p.Args)}},
			}})
			_, _ = os.Stdout.Write(append(out, '\n'))
			continue
		}
		switch req.Method {
		case "initialize":
			res = map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"protocolVersion": "2024-11-05", "serverInfo": map[string]string{"name": "mock", "version": "1.0"},
			}}
		case "tools/list":
			res = map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"tools": []any{
				map[string]any{"name": "echo", "description": "echo tool", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"msg": map[string]string{"type": "string"}}}},
				map[string]any{"name": "large", "description": "large tool", "inputSchema": map[string]any{"type": "object"}},
				map[string]any{"name": "slow", "description": "slow tool", "inputSchema": map[string]any{"type": "object"}},
			}}}
		case "tools/call":
			var p struct {
				Name string         `json:"name"`
				Args map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			switch p.Name {
			case "large":
				res = map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
					"content": []any{map[string]string{"type": "text", "text": strings.Repeat("A", 100*1024)}},
				}}
			case "slow":
				time.Sleep(20 * time.Second)
				res = map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"content": []any{}}}
			default:
				raw, _ := json.Marshal(p.Args)
				res = map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
					"content": []any{map[string]string{"type": "text", "text": string(raw)}},
				}}
			}
		}
		out, _ := json.Marshal(res)
		_, _ = os.Stdout.Write(append(out, '\n'))
		if mode == "deaf" && req.Method == "tools/list" {
			time.Sleep(time.Hour)
		}
	}
}

func TestHelperProcess(t *testing.T) {
	if len(os.Args) < 3 {
		return
	}
	for i, a := range os.Args {
		if a == "mock-mcp-server" {
			mode := ""
			if i+1 < len(os.Args) {
				mode = os.Args[i+1]
			}
			runMockMCPServer(mode)
			os.Exit(0)
		}
	}
}

func helperArgs() (string, []string) {
	return os.Args[0], []string{"-test.run=TestHelperProcess", "--", "mock-mcp-server"}
}

func helperModeArgs(mode string) (string, []string) {
	bin, args := helperArgs()
	return bin, append(args, mode)
}

func discoverMock(t *testing.T, mode string) (*StdioMCP, map[string]Spec) {
	t.Helper()
	bin, args := helperModeArgs(mode)
	adapter, err := NewStdioMCP(context.Background(), StdioMCPConfig{Name: "srv", Command: bin, Args: args})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adapter.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	specs, err := adapter.Discover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]Spec, len(specs))
	for _, sp := range specs {
		byName[sp.Name] = sp
	}
	return adapter, byName
}

func TestStdioMCPDiscoverFollowsToolListCursor(t *testing.T) {
	_, specs := discoverMock(t, "paged")
	if len(specs) != 2 || specs["mcp.srv.first"].Name == "" || specs["mcp.srv.second"].Name == "" {
		t.Fatalf("tools from every tools/list page must be discovered, got %v", specs)
	}
}

func TestStdioMCPForwardsLargeIntegerArgumentsExactly(t *testing.T) {
	_, specs := discoverMock(t, "raw")
	out, err := specs["mcp.srv.echo"].Execute(context.Background(), json.RawMessage(`{"id":9007199254740993}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `{\"id\":9007199254740993}`) {
		t.Fatalf("integer argument was altered on the way to the server: %s", out)
	}
	out, err = specs["mcp.srv.echo"].Execute(context.Background(), nil)
	if err != nil || !strings.Contains(string(out), `"text":"{}"`) {
		t.Fatalf("missing arguments must be sent as an empty object: %s %v", out, err)
	}
}

func TestStdioMCPReportsNonTextContentToTheModel(t *testing.T) {
	_, specs := discoverMock(t, "media")
	out, err := specs["mcp.srv.echo"].Execute(context.Background(), json.RawMessage(`{"msg":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Content []MCPText `json:"content"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	want := []MCPText{
		{Type: "image", Text: "[image omitted: image/png, 8 base64 bytes; binary content is not forwarded to the model]"},
		{Type: "resource", Text: "[resource file:///notes.txt]\nnote body"},
		{Type: "resource", Text: "[resource file:///a.bin omitted: application/octet-stream, 4 base64 bytes; binary content is not forwarded to the model]"},
		{Type: "resource_link", Text: "[resource link file:///big.log] big.log"},
	}
	if len(got.Content) != len(want) {
		t.Fatalf("content = %+v", got.Content)
	}
	for i := range want {
		if got.Content[i] != want[i] {
			t.Fatalf("content[%d] = %+v, want %+v", i, got.Content[i], want[i])
		}
	}
}

func TestStdioMCPTimeoutOfWriteToolSaysOutcomeIsUnknown(t *testing.T) {
	for _, readonly := range []bool{false, true} {
		bin, args := helperArgs()
		adapter, err := NewStdioMCP(context.Background(), StdioMCPConfig{Name: "srv", Command: bin, Args: args, Readonly: readonly})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = adapter.Close() })
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		specs, err := adapter.Discover(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		adapter.SetTimeout(200 * time.Millisecond)
		var slow Spec
		for _, sp := range specs {
			if sp.Name == "mcp.srv.slow" {
				slow = sp
			}
		}
		_, err = slow.Execute(context.Background(), json.RawMessage(`{}`))
		if err == nil || !strings.Contains(err.Error(), "timed out after 200ms") {
			t.Fatalf("readonly=%v: err = %v", readonly, err)
		}
		if got := strings.Contains(err.Error(), "outcome is unknown"); got != !readonly {
			t.Fatalf("readonly=%v: unknown-outcome note present=%v in %q", readonly, got, err.Error())
		}
		if uncertain := errors.Is(err, domain.ErrUncertainOutcome); uncertain == readonly {
			t.Fatalf("readonly=%v: uncertain outcome=%v in %q", readonly, uncertain, err.Error())
		}
	}
}

func TestStdioMCPCallDoesNotHangWhenServerStopsReadingStdin(t *testing.T) {
	adapter, specs := discoverMock(t, "deaf")
	adapter.SetTimeout(500 * time.Millisecond)
	// Larger than any pipe buffer, so the write cannot complete on its own.
	args, err := json.Marshal(map[string]string{"msg": strings.Repeat("x", 4<<20)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := specs["mcp.srv.echo"].Execute(context.Background(), args)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("call to a server that stopped reading must fail")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("call blocked past its timeout on a server that stopped reading stdin")
	}
	if adapter.Alive() {
		t.Fatal("a server with a half-written frame must be stopped, not reused")
	}
	if err := adapter.Err(); err == nil || !strings.Contains(err.Error(), "stopped reading stdin") {
		t.Fatalf("adapter error = %v", err)
	}
}

func TestStdioMCPDiscoverAndInvoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bin, args := helperArgs()
	adapter, err := NewStdioMCP(ctx, StdioMCPConfig{
		Name:     "mock_server",
		Command:  bin,
		Args:     args,
		Readonly: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adapter.Close() }()

	if adapter.Protocol() != ProtocolMCP {
		t.Fatalf("protocol = %s, want %s", adapter.Protocol(), ProtocolMCP)
	}
	if adapter.ServerLabel() != "mock_server" {
		t.Fatalf("label = %s, want mock_server", adapter.ServerLabel())
	}

	specs, err := adapter.Discover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 3 {
		t.Fatalf("discovered specs count = %d, want 3", len(specs))
	}

	var echoSpec *Spec
	for i := range specs {
		if specs[i].Name == "mcp.mock_server.echo" {
			echoSpec = &specs[i]
			break
		}
	}
	if echoSpec == nil {
		t.Fatalf("expected spec mcp.mock_server.echo not found in %#v", specs)
	}
	if echoSpec.Readonly {
		t.Fatal("expected non-readonly spec by default")
	}
	if echoSpec.SideEffectClass != "reversible" {
		t.Fatalf("SideEffectClass = %s, want reversible", echoSpec.SideEffectClass)
	}
	if echoSpec.Timeout != DefaultMCPTimeout {
		t.Fatalf("timeout = %v, want %v", echoSpec.Timeout, DefaultMCPTimeout)
	}

	charge := &memCharge{b: domain.Budget{MaxToolCalls: 10}}
	grants := memGrants{grants: []domain.Grant{{
		Tool:  "mcp.mock_server.echo",
		Op:    "call",
		State: "active",
	}}}
	broker := NewBroker(charge, grants)
	for _, sp := range specs {
		broker.Register(sp)
	}

	res, err := broker.Invoke(ctx, Request{
		RunID: "run1",
		Tool:  "mcp.mock_server.echo",
		Args:  json.RawMessage(`{"msg":"hello-mcp"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || !res.Dispatched {
		t.Fatalf("invoke failed: res=%+v", res)
	}

	var data map[string]any
	if err := json.Unmarshal(res.Data, &data); err != nil {
		t.Fatalf("unmarshal res.Data failed: %v", err)
	}
	if data["untrusted"] != true {
		t.Fatalf("expected untrusted=true, got %#v", data)
	}
	contentStr, _ := json.Marshal(data["content"])
	if !strings.Contains(string(contentStr), "hello-mcp") {
		t.Fatalf("expected content to contain hello-mcp, got %s", contentStr)
	}
}

func TestStdioMCPBrokerGate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bin, args := helperArgs()
	adapter, err := NewStdioMCP(ctx, StdioMCPConfig{
		Name:     "gate_server",
		Command:  bin,
		Args:     args,
		Readonly: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adapter.Close() }()

	specs, err := adapter.Discover(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// 1. 未授权调用：测试审批/拒绝路径
	charge := &memCharge{b: domain.Budget{MaxToolCalls: 10}}
	// 没有 grants
	broker := NewBroker(charge, memGrants{})
	for _, sp := range specs {
		broker.Register(sp)
	}

	// Case 1a: ApprovalPossible = false -> 拒绝路径
	resDeny, err := broker.Invoke(ctx, Request{
		RunID:            "run_deny",
		Tool:             "mcp.gate_server.echo",
		Args:             json.RawMessage(`{"msg":"test"}`),
		ApprovalPossible: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resDeny.OK || resDeny.Dispatched || resDeny.NeedsApproval {
		t.Fatalf("expected denied, got %+v", resDeny)
	}
	if resDeny.Error == nil || resDeny.Error.Code != domain.CodeAuthDenied {
		t.Fatalf("expected auth_denied error, got %+v", resDeny.Error)
	}

	// Case 1b: ApprovalPossible = true -> 审批路径（与本地 reversible 工具一致）
	resAppr, err := broker.Invoke(ctx, Request{
		RunID:            "run_appr",
		Tool:             "mcp.gate_server.echo",
		Args:             json.RawMessage(`{"msg":"test"}`),
		ApprovalPossible: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resAppr.OK || resAppr.Dispatched || !resAppr.NeedsApproval {
		t.Fatalf("expected needs approval, got %+v", resAppr)
	}
	if resAppr.Error == nil || resAppr.Error.Code != domain.CodeAuthDenied {
		t.Fatalf("expected auth_denied error on approval, got %+v", resAppr.Error)
	}

	// 2. 先读取敏感内容再调用非只读 MCP 工具的外发路径被 Broker 拦截
	callGrant := domain.Grant{
		Tool:  "mcp.gate_server.echo",
		Op:    "call",
		State: "active",
	}
	brokerWithGrant := NewBroker(charge, memGrants{grants: []domain.Grant{callGrant}})
	for _, sp := range specs {
		brokerWithGrant.Register(sp)
	}

	// 未读敏感内容：调用成功
	resNormal, err := brokerWithGrant.Invoke(ctx, Request{
		RunID: "run_normal",
		Tool:  "mcp.gate_server.echo",
		Args:  json.RawMessage(`{"msg":"normal"}`),
	})
	if err != nil || !resNormal.OK {
		t.Fatalf("expected normal call to succeed, got res=%+v, err=%v", resNormal, err)
	}

	// 已读敏感内容（SensitiveSeen = true）：非只读 MCP 工具外发被 Broker 拦截！
	resEgressBlocked, err := brokerWithGrant.Invoke(ctx, Request{
		RunID:         "run_egress",
		Tool:          "mcp.gate_server.echo",
		Args:          json.RawMessage(`{"msg":"leak"}`),
		SensitiveSeen: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resEgressBlocked.OK || resEgressBlocked.Dispatched {
		t.Fatalf("expected egress call to be blocked, got res=%+v", resEgressBlocked)
	}
	if resEgressBlocked.Error == nil || resEgressBlocked.Error.Code != domain.CodeAuthDenied || !strings.Contains(resEgressBlocked.Error.Message, "sensitive data cannot be sent externally") {
		t.Fatalf("expected sensitive egress rejection, got error=%+v", resEgressBlocked.Error)
	}

	// 显式授予 Op: "egress" grant 后外发允许
	egressGrant := domain.Grant{
		Tool:  "mcp.gate_server.echo",
		Op:    "egress",
		State: "active",
	}
	brokerWithEgress := NewBroker(charge, memGrants{grants: []domain.Grant{callGrant, egressGrant}})
	for _, sp := range specs {
		brokerWithEgress.Register(sp)
	}
	resEgressAllowed, err := brokerWithEgress.Invoke(ctx, Request{
		RunID:         "run_egress_allowed",
		Tool:          "mcp.gate_server.echo",
		Args:          json.RawMessage(`{"msg":"leak"}`),
		SensitiveSeen: true,
	})
	if err != nil || !resEgressAllowed.OK {
		t.Fatalf("expected egress with explicit grant to succeed, got res=%+v, err=%v", resEgressAllowed, err)
	}

	// 3. 只读 MCP 工具显式配置：不触发外发拦截
	roAdapter, err := NewStdioMCP(ctx, StdioMCPConfig{
		Name:     "ro_server",
		Command:  bin,
		Args:     args,
		Readonly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = roAdapter.Close() }()
	roSpecs, err := roAdapter.Discover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, sp := range roSpecs {
		if sp.Name == "mcp.ro_server.echo" {
			if !sp.Readonly || sp.SideEffectClass != "none" {
				t.Fatalf("expected readonly spec, got readonly=%v, sideEffect=%s", sp.Readonly, sp.SideEffectClass)
			}
		}
	}
}

func TestStdioMCPTruncateAndTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	bin, args := helperArgs()
	adapter, err := NewStdioMCP(ctx, StdioMCPConfig{
		Name:     "limits_server",
		Command:  bin,
		Args:     args,
		Readonly: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adapter.Close() }()

	specs, err := adapter.Discover(ctx)
	if err != nil {
		t.Fatal(err)
	}

	charge := &memCharge{b: domain.Budget{MaxToolCalls: 10}}
	grants := memGrants{grants: []domain.Grant{
		{Tool: "mcp.limits_server.large", Op: "call", State: "active"},
		{Tool: "mcp.limits_server.slow", Op: "call", State: "active"},
	}}
	broker := NewBroker(charge, grants)
	for _, sp := range specs {
		broker.Register(sp)
	}

	// 1. 超大返回被截断至 64 KiB
	resLarge, err := broker.Invoke(ctx, Request{
		RunID: "run_large",
		Tool:  "mcp.limits_server.large",
		Args:  json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resLarge.OK || !resLarge.Dispatched {
		t.Fatalf("expected large call to succeed, got %+v", resLarge)
	}
	var largeData struct {
		Content   []MCPText `json:"content"`
		Truncated bool      `json:"truncated"`
		Untrusted bool      `json:"untrusted"`
	}
	if err := json.Unmarshal(resLarge.Data, &largeData); err != nil {
		t.Fatalf("unmarshal large result data failed: %v", err)
	}
	if !largeData.Truncated {
		t.Fatal("expected truncated=true for large response")
	}
	if !largeData.Untrusted {
		t.Fatal("expected untrusted=true for large response")
	}
	if len(largeData.Content) == 0 {
		t.Fatal("expected non-empty content in large response")
	}
	if len(largeData.Content[0].Text) != MaxMCPResultBytes {
		t.Fatalf("truncated length = %d, want exactly %d (64 KiB)", len(largeData.Content[0].Text), MaxMCPResultBytes)
	}

	// 2. 慢工具触发 15s 超时错误
	start := time.Now()
	resSlow, err := broker.Invoke(ctx, Request{
		RunID: "run_slow",
		Tool:  "mcp.limits_server.slow",
		Args:  json.RawMessage(`{}`),
	})
	duration := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if resSlow.OK {
		t.Fatalf("expected slow call to fail with timeout, got %+v", resSlow)
	}
	if resSlow.Error == nil || resSlow.Error.Code != domain.CodeToolError {
		t.Fatalf("expected domain.CodeToolError, got %+v", resSlow.Error)
	}
	if !strings.Contains(resSlow.Error.Message, "15s") || !strings.Contains(resSlow.Error.Message, "timed out") {
		t.Fatalf("expected error message to contain '15s' and 'timed out', got %s", resSlow.Error.Message)
	}
	if duration < 14*time.Second {
		t.Fatalf("expected call to wait at least ~15s before timeout, but took %v", duration)
	}
}

func TestStdioMCPChildCleanup(t *testing.T) {
	parentCtx, parentCancel := context.WithCancel(context.Background())

	bin, args := helperArgs()
	adapter, err := NewStdioMCP(parentCtx, StdioMCPConfig{
		Name:     "cleanup_server",
		Command:  bin,
		Args:     args,
		Readonly: false,
	})
	if err != nil {
		t.Fatal(err)
	}

	initCtx, initCancel := context.WithTimeout(parentCtx, 5*time.Second)
	defer initCancel()
	_, err = adapter.Discover(initCtx)
	if err != nil {
		t.Fatal(err)
	}

	pid := adapter.ProcessPID()
	if pid <= 0 {
		t.Fatalf("invalid process pid: %d", pid)
	}

	// 验证子进程正在运行
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("expected process %d to be alive, got %v", pid, err)
	}

	// 取消父 Context
	parentCancel()

	// 等待子进程退出
	dead := false
	for i := 0; i < 30; i++ {
		time.Sleep(100 * time.Millisecond)
		err := syscall.Kill(pid, 0)
		if err != nil && (err == syscall.ESRCH || strings.Contains(err.Error(), "no such process")) {
			dead = true
			break
		}
		// 也检查 /proc/<pid> 是否存在
		if _, statErr := os.Stat("/proc/" + strconv.Itoa(pid)); statErr != nil {
			dead = true
			break
		}
	}
	if !dead {
		t.Fatalf("process %d was not cleaned up after parent context cancellation", pid)
	}
}

func TestStdioMCPSensitiveEnvStripped(t *testing.T) {
	for _, k := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "AWS_SECRET_ACCESS_KEY", "MY_SECRET_TOKEN", "DB_PASSWORD"} {
		if !isSensitiveEnv(k) {
			t.Fatalf("expected %s to be recognized as sensitive", k)
		}
	}
	t.Setenv("OPENAI_API_KEY", "secret-openai")
	t.Setenv("MY_APP_SECRET", "secret-val")
	t.Setenv("SOME_TOKEN", "secret-token")
	env := buildSafeEnv()
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		if isSensitiveEnv(k) {
			t.Fatalf("sensitive env key %s was not stripped from child env: %s", k, e)
		}
	}
}

func TestStdioMCPCrashHandling(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	bin, args := helperArgs()
	adapter, err := NewStdioMCP(ctx, StdioMCPConfig{
		Name:     "crash_server",
		Command:  bin,
		Args:     args,
		Readonly: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adapter.Close() }()

	specs, err := adapter.Discover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pid := adapter.ProcessPID()
	if pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	deadline := time.Now().Add(2 * time.Second)
	for adapter.Alive() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if adapter.Alive() {
		t.Fatal("server still marked alive after SIGKILL")
	}

	out, callErr := specs[0].Execute(ctx, json.RawMessage(`{"msg":"ping"}`))
	if callErr != nil {
		t.Fatalf("call after crash: %v", callErr)
	}
	if !strings.Contains(string(out), "ping") {
		t.Fatalf("unexpected result after restart: %s", out)
	}
	if !adapter.Alive() || adapter.ProcessPID() == 0 {
		t.Fatal("server was not restarted after crash")
	}
}
