package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/config"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/storage/sqlite"
)

// A large remote catalog uses the same real CLI, policy gate and checkpoint
// path as a small stdio catalog. Resume starts a fresh manager and MCP session.
func TestRemoteMCPCatalogResumeAndAudit(t *testing.T) {
	var calls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var f struct {
			Method string
			ID     int
			Params struct{ Name string }
		}
		_ = json.NewDecoder(r.Body).Decode(&f)
		w.Header().Set("Content-Type", "application/json")
		var result any
		switch f.Method {
		case "initialize":
			result = map[string]string{"protocolVersion": "2025-11-25"}
		case "notifications/initialized":
			w.WriteHeader(202)
			return
		case "tools/list":
			var items []map[string]any
			for i := range 100 {
				items = append(items, map[string]any{"name": fmt.Sprintf("item_%02d", i), "inputSchema": map[string]string{"type": "object"}})
			}
			result = map[string]any{"tools": items}
		case "tools/call":
			if f.Params.Name != "item_99" {
				t.Error("wrong tool dispatched")
			}
			calls.Add(1)
			result = map[string]any{"content": []map[string]string{{"type": "text", "text": "remote done"}}}
		default:
			t.Error("unexpected MCP request")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": f.ID, "result": result})
	}))
	defer remote.Close()
	env := newMCPTestEnv(t, func(req mcpModelRequest) string {
		if len(req.Tools) == 0 {
			return mcpTextCompletion("summary")
		}
		if !req.hasTool("search_mcp_tools") || !req.hasTool("read_file") || req.hasTool("mcp_many_item_00") {
			t.Error("unbounded or missing model tools")
		}
		switch len(req.toolResults()) {
		case 0:
			if req.hasTool("mcp_many_item_99") {
				t.Error("tool available before discovery")
			}
			return mcpToolCallCompletion("find", "search_mcp_tools", `{"query":"item_99"}`)
		case 1:
			if !req.hasTool("mcp_many_item_99") {
				t.Error("selected schema missing")
			}
			return mcpToolCallCompletion("act", "mcp_many_item_99", `{}`)
		default:
			if !req.hasTool("mcp_many_item_99") {
				t.Error("selected tool lost on resume")
			}
			return mcpTextCompletion("completed")
		}
	}, func(string) []config.MCPServerConfig {
		return []config.MCPServerConfig{{Name: "many", URL: remote.URL}}
	})
	status, wait, id, _ := env.run("find and invoke the last MCP tool")
	if status != domain.RunWaiting || wait != domain.WaitApproval || calls.Load() != 0 {
		t.Fatalf("approval bypassed: %v/%v", status, wait)
	}
	var out, stderr bytes.Buffer
	if code := Main(append([]string{"runs", "resume", "--decision", "approve"}, append(env.common(), id)...), &out, &stderr); code != 0 {
		t.Fatalf("resume failed: %s", stderr.String())
	}
	if calls.Load() != 1 || !strings.Contains(env.lastToolResults(), "remote done") {
		t.Fatal("remote result missing or call repeated")
	}
	ctx := context.Background()
	db, err := sqlite.Open(ctx, env.db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	run, err := db.GetRun(ctx, id)
	if err != nil || run.Status != domain.RunCompleted || run.Budget.UsedToolCalls != 2 {
		t.Fatal("search and approved execution were not charged")
	}
	events, err := db.ListEvents(ctx, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range events {
		if e.Type == "policy.decided" {
			for _, name := range []string{"search_mcp_tools", "mcp.many.item_99"} {
				if strings.Contains(string(e.Payload), name) {
					names[name] = true
				}
			}
		}
	}
	if len(names) != 2 {
		t.Fatal("missing policy audit events")
	}
}
