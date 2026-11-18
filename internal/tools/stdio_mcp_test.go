package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/domain"
)

func runMockMCPServer() {
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
		os.Stdout.Write(append(out, '\n'))
	}
}

func TestHelperProcess(t *testing.T) {
	if len(os.Args) < 3 {
		return
	}
	for _, a := range os.Args {
		if a == "mock-mcp-server" {
			runMockMCPServer()
			os.Exit(0)
		}
	}
}

func helperArgs() (string, []string) {
	return os.Args[0], []string{"-test.run=TestHelperProcess", "--", "mock-mcp-server"}
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
	defer adapter.Close()

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
	defer adapter.Close()

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
	defer roAdapter.Close()
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
	defer adapter.Close()

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
	defer adapter.Close()

	specs, err := adapter.Discover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pid := adapter.ProcessPID()
	if pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	time.Sleep(50 * time.Millisecond)

	done := make(chan error, 1)
	go func() {
		_, callErr := specs[0].Execute(ctx, json.RawMessage(`{"msg":"ping"}`))
		done <- callErr
	}()
	select {
	case callErr := <-done:
		if callErr == nil {
			t.Fatal("expected error after process killed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("call hung after process crashed")
	}
}
