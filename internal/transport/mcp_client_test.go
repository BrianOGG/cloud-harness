package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/config"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/tools"
)

type fixtureLedger struct {
	b      domain.Budget
	grants []domain.Grant
}

func (f *fixtureLedger) Charge(_ context.Context, _ string, kind domain.ChargeKind, n int) error {
	next, err := f.b.Charge(kind, n)
	if err == nil {
		f.b = next
	}
	return err
}
func (f *fixtureLedger) ListGrants(context.Context, string, string, time.Time) ([]domain.Grant, error) {
	return f.grants, nil
}

func TestRemoteMCPSSEThroughBroker(t *testing.T) {
	const env = "MCP_FIXTURE_TOKEN"
	const token = "fixture-only-bearer"
	t.Setenv(env, token)
	var calls, deletes atomic.Int32
	var reply atomic.Value
	reply.Store("ok")
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token || r.URL.RawQuery != "" {
			t.Error("incorrect token placement")
		}
		if r.Method == "DELETE" {
			deletes.Add(1)
			w.WriteHeader(204)
			return
		}
		var f struct {
			Method string
			ID     int
			Params json.RawMessage
		}
		_ = json.NewDecoder(r.Body).Decode(&f)
		if f.Method != "initialize" && (r.Header.Get("Mcp-Session-Id") != "session" || r.Header.Get("MCP-Protocol-Version") != "2025-06-18") {
			t.Error("missing negotiated session/version")
		}
		if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			t.Error("missing SSE accept")
		}
		switch f.Method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", "session")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18"}}`, f.ID)
		case "notifications/initialized":
			w.WriteHeader(202)
		case "tools/list":
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			_, _ = fmt.Fprint(w, "id: initial\ndata:\n\n: heartbeat\r\n\r\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
			tool := "write"
			extra := `,"nextCursor":"page2"`
			if strings.Contains(string(f.Params), "page2") {
				tool = "update"
				extra = ""
			}
			_, _ = fmt.Fprintf(w, "data: {\"id\":%d,\"jsonrpc\":\"2.0\",\ndata: \"result\":{\"tools\":[{\"name\":%q,\"inputSchema\":{\"type\":\"object\"}}]%s}}\n\n", f.ID, tool, extra)
		case "tools/call":
			calls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			switch reply.Load() {
			case "wrong-id":
				_, _ = fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"id\":999,\"result\":{}}\n\n")
			case "escaped-token":
				_, _ = fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"\\u0066%s\"}]}}\n\n", f.ID, token[1:])
			case "echo-token":
				_, _ = fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"content\":[{\"type\":\"text\",\"text\":%q}]}}\n\n", f.ID, token)
			default:
				_, _ = fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"done\"}]}}\n\n", f.ID)
			}
		}
	}))
	defer remote.Close()
	grant := domain.Grant{State: "active", Tool: "mcp.svc.update", Op: "call"}
	ledger := &fixtureLedger{b: domain.Budget{MaxToolCalls: 4}, grants: []domain.Grant{grant}}
	b := tools.NewBroker(ledger, ledger)
	set := tools.StartMCPConnections(context.Background(), b, []config.MCPServerConfig{{Name: "svc", URL: remote.URL, TokenEnv: env}}, time.Second, NewRemoteMCPServer)
	defer func() { _ = set.Close() }()
	if st := set.Status()[0]; st.State != "running" || len(st.Tools) != 2 {
		t.Fatalf("inventory %+v", st)
	}
	invoke := func(name string) tools.Result {
		t.Helper()
		res, err := b.Invoke(context.Background(), tools.Request{RunID: "run", Tool: name, Args: json.RawMessage(`{}`), ApprovalPossible: true})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := invoke("mcp.svc.write"); !res.NeedsApproval || calls.Load() != 0 {
		t.Fatal("remote write bypassed approval")
	}
	if res := invoke(grant.Tool); !res.OK || !strings.Contains(string(res.Data), "done") {
		t.Fatal("valid SSE result missing")
	}
	for _, mode := range []string{"wrong-id", "echo-token", "escaped-token"} {
		reply.Store(mode)
		res := invoke(grant.Tool)
		if !res.UncertainOutcome || strings.Contains(string(res.Data)+res.Error.Error(), token) {
			t.Fatal("ambiguous write or credential reflection mishandled")
		}
	}
	if res := invoke(grant.Tool); res.Error == nil || res.Error.Code != domain.CodeBudget || calls.Load() != 4 {
		t.Fatal("budget or ambiguous write retry")
	}
	fork, err := b.ForkWorkspace(t.TempDir())
	if err != nil || !fork.Has(grant.Tool) {
		t.Fatal("workspace broker missing MCP registry")
	}
	if err := set.Disconnect("svc"); err != nil || b.Has(grant.Tool) || fork.Has(grant.Tool) || deletes.Load() != 1 {
		t.Fatal("disconnect retained tools or session")
	}
	if err := set.Connect(context.Background(), "svc"); err != nil || !b.Has(grant.Tool) || !fork.Has(grant.Tool) {
		t.Fatal("reconnect did not restore tools")
	}
}

func TestRemoteMCPAuthBoundaries(t *testing.T) {
	for _, cfg := range []config.MCPServerConfig{
		{URL: "http://example.test/mcp"}, {URL: "https://user:pass@example.test"},
		{URL: "https://example.test/mcp?access_token=reject"}, {URL: "https://example.test", Command: "command"},
		{URL: "https://example.test", TokenEnv: "not a variable"},
		{URL: "https://example.test", TokenEnv: "VAR", OAuth: &config.MCPOAuthConfig{}},
	} {
		if cfg.Validate() == nil {
			t.Error("accepted invalid MCP config")
		}
	}
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("followed a redirect") }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	t.Setenv("MCP_REDIRECT_TOKEN", "fixture-bearer")
	s := tools.StartMCPConnections(context.Background(), nil, []config.MCPServerConfig{{Name: "svc", URL: redirect.URL, TokenEnv: "MCP_REDIRECT_TOKEN"}}, time.Second, NewRemoteMCPServer)
	defer func() { _ = s.Close() }()
	if s.Status()[0].State != tools.MCPStateFailed {
		t.Fatal("redirect was accepted")
	}
	a := mcpOAuth{pending: &pendingOAuth{state: "expired", expires: time.Now().Add(-time.Minute)}, cfg: config.MCPOAuthConfig{RedirectURI: "http://localhost" + mcpCallbackPath}}
	if matched, err := a.Finish(context.Background(), "expired", "code", "", "localhost"); !matched || err == nil || a.pending != nil {
		t.Fatal("expired state accepted")
	}
	a.metadataURL = target.URL
	a.resource = redirect.URL
	if _, _, err := a.Begin(context.Background()); err == nil {
		t.Fatal("accepted unpinned metadata origin")
	}
}
