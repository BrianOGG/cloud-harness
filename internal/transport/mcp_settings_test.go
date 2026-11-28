package transport

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/config"
	"github.com/BrianOGG/cloud-harness/internal/tools"
)

// Exercise the browser callback against a loopback resource and public OAuth
// client, with deterministic token expiry and a rotating refresh credential.
func TestMCPOAuthSettingsRoundTrip(t *testing.T) {
	const host = "127.0.0.1:19421"
	const redirect = "http://" + host + mcpCallbackPath
	const access = "fixture-access-one"
	const renewed = "fixture-access-two"
	const refresh = "fixture-refresh-one"
	var exchanges, refreshes, authenticated atomic.Int32
	var challenge atomic.Value
	var resource, issuer string
	mux := http.NewServeMux()
	remote := httptest.NewServer(mux)
	defer remote.Close()
	resource = remote.URL + "/mcp"
	issuer = remote.URL + "/tenant"
	mux.HandleFunc("/meta", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("credentials sent to resource metadata")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"resource": resource, "authorization_servers": []string{issuer}})
	})
	mux.HandleFunc("/.well-known/openid-configuration/tenant", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("credentials sent to issuer metadata")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": remote.URL + "/authorize", "token_endpoint": remote.URL + "/token", "code_challenge_methods_supported": []string{"S256"}, "authorization_response_iss_parameter_supported": true})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.URL.RawQuery != "" || r.Form.Get("resource") != resource || r.Form.Get("client_id") != "public" {
			t.Error("token audience or client mismatch")
		}
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge.Load() || r.Form.Get("code") != "one-use" || r.Form.Get("redirect_uri") != redirect {
				t.Error("incorrect PKCE exchange")
			}
			exchanges.Add(1)
			_, _ = fmt.Fprintf(w, `{"token_type":"Bearer","access_token":%q,"refresh_token":%q,"expires_in":1}`, access, refresh)
		case "refresh_token":
			if refreshes.Add(1) > 1 {
				http.Error(w, "invalid_grant", http.StatusBadRequest)
				return
			}
			if r.Form.Get("refresh_token") != refresh {
				t.Error("incorrect refresh credential")
			}
			_, _ = fmt.Fprintf(w, `{"token_type":"Bearer","access_token":%q,"refresh_token":"rotated","expires_in":7200}`, renewed)
		default:
			t.Error("unexpected OAuth grant")
		}
	})
	mux.HandleFunc("POST /mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+renewed || authenticated.Load() > 2 {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+remote.URL+`/meta", scope="tool.read"`)
			http.Error(w, "redacted upstream response", http.StatusUnauthorized)
			return
		}
		authenticated.Add(1)
		var f struct {
			ID     int
			Method string
		}
		_ = json.NewDecoder(r.Body).Decode(&f)
		w.Header().Set("Content-Type", "application/json")
		switch f.Method {
		case "initialize":
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-11-25"}}`, f.ID)
		case "notifications/initialized":
			w.WriteHeader(202)
		case "tools/list":
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[{"name":"lookup","inputSchema":{"type":"object"}}]}}`, f.ID)
		default:
			t.Error("unexpected MCP method")
		}
	})
	cfg := config.MCPServerConfig{Name: "svc", URL: resource, OAuth: &config.MCPOAuthConfig{Issuer: issuer, ClientID: "public", RedirectURI: redirect}}
	set := tools.StartMCPConnections(context.Background(), nil, []config.MCPServerConfig{cfg}, time.Second, NewRemoteMCPServer)
	defer func() { _ = set.Close() }()
	handler := (&Server{MCP: set, Token: "local-harness-bearer", LoopbackBind: true}).Handler()
	call := func(method, path string, authorized bool, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://"+host+path, nil)
		if authorized {
			r.Header.Set("Authorization", "Bearer local-harness-bearer")
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		for _, secret := range []string{access, renewed, refresh, "rotated", "local-harness-bearer"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Error("credential leaked into HTTP response")
			}
		}
		return w
	}
	if w := call("POST", "/v1/mcp/servers/svc/authorize", false, nil); w.Code != 401 {
		t.Fatal("management endpoint bypassed auth")
	}
	if w := call("GET", "/v1/mcp/servers", true, nil); !strings.Contains(w.Body.String(), "auth_required") {
		t.Fatal("missing initial auth diagnosis")
	}
	w := call("POST", "/v1/mcp/servers/svc/authorize", true, nil)
	var start struct{ URL string }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &start) != nil {
		t.Fatalf("start status %d", w.Code)
	}
	u, _ := url.Parse(start.URL)
	q := u.Query()
	challenge.Store(q.Get("code_challenge"))
	if q.Get("state") == "" || q.Get("code_challenge") == "" || q.Get("resource") != resource || q.Get("scope") != "tool.read" || q.Get("code_challenge_method") != "S256" || q.Get("redirect_uri") != redirect || q.Has("code_verifier") {
		t.Fatal("incorrect authorization request")
	}
	cookie := w.Result().Cookies()[0]
	if cookie.Path != mcpCallbackPath || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatal("callback cookie missing binding")
	}
	callback := mcpCallbackPath + "?" + url.Values{"code": {"one-use"}, "state": {q.Get("state")}, "iss": {issuer}}.Encode()
	if call("GET", callback, false, nil).Code != 400 || exchanges.Load() != 0 {
		t.Fatal("accepted callback without browser cookie")
	}
	if call("GET", callback, false, cookie).Code != 200 || exchanges.Load() != 1 || refreshes.Load() != 1 {
		t.Fatal("OAuth callback or refresh failed")
	}
	if call("GET", callback, false, cookie).Code != 400 || exchanges.Load() != 1 {
		t.Fatal("reused callback")
	}
	if w := call("GET", "/v1/mcp/servers", true, nil); !strings.Contains(w.Body.String(), "mcp.svc.lookup") {
		t.Fatal("authorized server tools not registered")
	}
	if w := call("POST", "/v1/mcp/servers/svc/connect", true, nil); w.Code != 400 || refreshes.Load() != 2 {
		t.Fatal("rejected token did not try refresh")
	}
	if w := call("GET", "/v1/mcp/servers", true, nil); !strings.Contains(w.Body.String(), "auth_required") {
		t.Fatal("missing reauthorization diagnosis")
	}
	if w := call("POST", "/v1/mcp/servers/svc/disconnect", true, nil); w.Code != 204 {
		t.Fatal("disconnect failed")
	}
	if w := call("GET", "/v1/mcp/servers", true, nil); strings.Contains(w.Body.String(), "mcp.svc.lookup") || !strings.Contains(w.Body.String(), "disconnected") {
		t.Fatal("stale inventory")
	}
}
