package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/config"
	"github.com/BrianOGG/cloud-harness/internal/domain"
)

func TestRemoteMCPToolForbiddenKeepsSession(t *testing.T) {
	for _, authMode := range []string{"oauth", "token_env"} {
		t.Run(authMode, func(t *testing.T) {
			const bearer = "fixture-read-access"
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				if r.Header.Get("Mcp-Session-Id") != "live" || r.Header.Get("Authorization") != "Bearer "+bearer {
					t.Error("request lost its session or read credential")
				}
				var call struct {
					ID     int
					Params struct{ Name string }
				}
				if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
					t.Error(err)
					return
				}
				requests.Add(1)
				switch call.Params.Name {
				case "write":
					w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="read write"`)
					http.Error(w, "upstream diagnostic", http.StatusForbidden)
				case "read":
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": map[string]any{"content": []any{}}})
				default:
					t.Error("unexpected tool call")
				}
			}))
			defer server.Close()
			cfg := config.MCPServerConfig{URL: server.URL}
			var auth *mcpOAuth
			if authMode == "oauth" {
				auth = &mcpOAuth{access: bearer, refresh: "fixture-refresh", metadataURL: server.URL + "/metadata"}
			} else {
				cfg.TokenEnv = "MCP_SCOPE_FIXTURE"
				t.Setenv(cfg.TokenEnv, bearer)
			}
			conn := newRemoteMCP(cfg, auth)
			conn.session = "live"
			defer func() { _ = conn.Close() }()
			_, err := conn.execute(context.Background(), "write", nil)
			var denial *domain.Error
			if !errors.As(err, &denial) || denial.Code != domain.CodeAuthDenied {
				t.Errorf("expected a tool-level authorization error, got %v", err)
			}
			if !conn.Alive() {
				t.Error("tool-specific denial disabled the established session")
			}
			if auth != nil && (auth.access != bearer || auth.refresh != "fixture-refresh" || auth.challengeScope != "read write" || auth.metadataURL != server.URL+"/metadata") {
				t.Error("valid credentials or incremental authorization hints were lost")
			}
			if _, err := conn.execute(context.Background(), "read", nil); err != nil || requests.Load() != 2 {
				t.Errorf("permitted read should dispatch once, after the denied write: %v (requests %d)", err, requests.Load())
			}
		})
	}
}
