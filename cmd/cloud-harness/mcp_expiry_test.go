package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/agent"
	"github.com/BrianOGG/cloud-harness/internal/config"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/tools"
)

func TestRemoteMCPKnownExpiryPreservesPendingApproval(t *testing.T) {
	const tool = "mcp.files.echo"
	var sessions, calls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var frame struct {
			ID     int
			Method string
		}
		if err := json.NewDecoder(r.Body).Decode(&frame); err != nil {
			t.Error(err)
			return
		}
		session := r.Header.Get("Mcp-Session-Id")
		w.Header().Set("Content-Type", "application/json")
		var result any
		switch frame.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", fmt.Sprint(sessions.Add(1)))
			result = map[string]string{"protocolVersion": "2025-11-25"}
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "tools/list":
			schema := map[string]string{"type": "object"}
			result = map[string]any{"tools": []any{
				map[string]any{"name": "echo", "inputSchema": schema},
				map[string]any{"name": "session_" + session, "inputSchema": schema},
			}}
		case "tools/call":
			calls.Add(1)
			if session == "1" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if session != "2" {
				t.Error("call used an unexpected session")
			}
			result = map[string]any{"content": []any{map[string]string{"type": "text", "text": "done"}}}
		default:
			t.Error("unexpected MCP method")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": result})
	}))
	defer remote.Close()

	env := newMCPTestEnv(t, echoScript, func(string) []config.MCPServerConfig {
		return []config.MCPServerConfig{{Name: "files", URL: remote.URL}}
	})
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBPath = env.db
	ctx := t.Context()
	mgr, db, err := openManager(ctx, cfg, env.workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	set := startMCP(ctx, cfg, mgr, io.Discard)
	defer func() { _ = set.Close() }()
	root := mgr.Broker
	fork, err := root.ForkWorkspace(env.workspace)
	if err != nil {
		t.Fatal(err)
	}
	mgr.Broker = fork
	mgr.SkipPlanner = true
	waiting := func() string {
		t.Helper()
		s, err := mgr.CreateSession(ctx)
		if err != nil {
			t.Fatal(err)
		}
		run, err := mgr.RunMessage(ctx, s.ID, "echo through MCP")
		if err != nil || run.WaitReason != domain.WaitApproval {
			t.Fatalf("expected pending approval: %s %v", run.WaitReason, err)
		}
		return run.ID
	}
	// Two runs share a session. The first learns of expiry by dispatching;
	// the second must keep its approval until discovery succeeds again.
	first, second := waiting(), waiting()
	approve := agent.ResumeInput{Decision: "approve"}
	if _, err := mgr.Resume(ctx, first, approve); err != nil || calls.Load() != 1 {
		t.Fatalf("first approved request should discover the expired session: %v (calls %d)", err, calls.Load())
	}

	for _, b := range []*tools.Broker{root, fork} {
		if b.Has(tool) || slices.ContainsFunc(b.Descriptors(), func(s tools.PublicSpec) bool { return strings.HasPrefix(s.Name, "mcp.files.") }) {
			t.Error("expired session still exposes tools")
		}
	}
	if st := set.Status()[0]; st.State != tools.MCPStateFailed || len(st.Tools) != 0 || !strings.Contains(st.Error, "expired") {
		t.Errorf("incorrect status after expiry: %+v", st)
	}
	_, err = mgr.Resume(ctx, second, approve)
	if err == nil || !strings.Contains(err.Error(), "is not available") {
		t.Errorf("known expired tool passed approval preflight: %v", err)
	}
	run, err := db.GetRun(ctx, second)
	if err != nil || run.Status != domain.RunWaiting || run.WaitReason != domain.WaitApproval || run.Budget.UsedToolCalls != 0 || calls.Load() != 1 {
		t.Fatalf("second approval was consumed: %s/%s, charges=%d calls=%d err=%v", run.Status, run.WaitReason, run.Budget.UsedToolCalls, calls.Load(), err)
	}

	if err := set.Connect(ctx, "files"); err != nil {
		t.Fatal(err)
	}
	for _, b := range []*tools.Broker{root, fork} {
		if !b.Has(tool) || !b.Has("mcp.files.session_2") || b.Has("mcp.files.session_1") {
			t.Fatal("reconnect did not publish exactly the new inventory")
		}
	}
	run, err = mgr.Resume(ctx, second, approve)
	if err != nil || run.Status != domain.RunCompleted || run.Budget.UsedToolCalls != 1 || calls.Load() != 2 || sessions.Load() != 2 {
		t.Fatalf("pending approval failed to dispatch exactly once on reconnect: %v (%s, %d calls)", err, run.Status, calls.Load())
	}
}
