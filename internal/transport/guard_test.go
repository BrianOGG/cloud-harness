package transport

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/agent"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/storage"
	"github.com/BrianOGG/cloud-harness/internal/storage/sqlite"
	"github.com/BrianOGG/cloud-harness/internal/trace"
)

// startGuarded serves srv on a loopback listener with the http.Server that
// `serve` uses, so the timeouts under test are the configured ones.
func startGuarded(t *testing.T, srv *Server) (*sqlite.DB, string) {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if srv.Manager == nil {
		srv.Manager = &agent.Manager{Store: db, Provider: &provider.Mock{}, OwnerID: "api"}
	} else {
		srv.Manager.Store = db
	}
	ts := httptest.NewUnstartedServer(nil)
	ts.Config = srv.NewHTTPServer("")
	ts.Start()
	t.Cleanup(ts.Close)
	return db, ts.URL
}

type guardedRequest struct {
	method, path, host, origin, contentType, body, token string
}

func (g guardedRequest) do(t *testing.T, base string) (int, string) {
	t.Helper()
	var body io.Reader
	if g.body != "" {
		body = strings.NewReader(g.body)
	}
	req, err := http.NewRequest(g.method, base+g.path, body)
	if err != nil {
		t.Fatal(err)
	}
	if g.host != "" {
		req.Host = g.host
	}
	if g.origin != "" {
		req.Header.Set("Origin", g.origin)
	}
	if g.contentType != "" {
		req.Header.Set("Content-Type", g.contentType)
	}
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func sessionCount(t *testing.T, db *sqlite.DB) int {
	t.Helper()
	sessions, err := db.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return len(sessions)
}

// A page on another site could create a session on a token-free loopback
// serve with a simple request: this returned 201 before the Host and Origin
// checks existed.
func TestLoopbackRefusesForeignHostAndOrigin(t *testing.T) {
	db, base := startGuarded(t, &Server{LoopbackBind: true})
	port := base[strings.LastIndex(base, ":"):]

	status, body := guardedRequest{
		method: http.MethodPost, path: "/v1/sessions",
		host: "evil.example", origin: "http://evil.example", contentType: "text/plain",
	}.do(t, base)
	if status != http.StatusForbidden || !strings.Contains(body, domain.CodeAuthDenied) {
		t.Fatalf("cross-site session create: %d %s", status, body)
	}

	for _, tc := range []struct {
		name string
		req  guardedRequest
		want int
	}{
		{"rebound host", guardedRequest{method: http.MethodPost, path: "/v1/sessions", host: "evil.example" + port}, http.StatusForbidden},
		{"loopback lookalike host", guardedRequest{method: http.MethodPost, path: "/v1/sessions", host: "127.0.0.1.evil.example" + port}, http.StatusForbidden},
		{"localhost lookalike host", guardedRequest{method: http.MethodPost, path: "/v1/sessions", host: "localhost.evil.example"}, http.StatusForbidden},
		{"rebound read", guardedRequest{method: http.MethodGet, path: "/v1/sessions", host: "evil.example" + port}, http.StatusForbidden},
		{"rebound health", guardedRequest{method: http.MethodGet, path: "/healthz", host: "evil.example"}, http.StatusForbidden},
		{"rebound ui", guardedRequest{method: http.MethodGet, path: "/ui", host: "evil.example"}, http.StatusForbidden},
		{"rebound approval page", guardedRequest{method: http.MethodGet, path: "/approval", host: "evil.example"}, http.StatusForbidden},
		{"foreign origin", guardedRequest{method: http.MethodPost, path: "/v1/sessions", origin: "http://evil.example"}, http.StatusForbidden},
		{"other local port origin", guardedRequest{method: http.MethodPost, path: "/v1/sessions", origin: "http://127.0.0.1:1"}, http.StatusForbidden},
		{"opaque origin", guardedRequest{method: http.MethodPost, path: "/v1/sessions", origin: "null"}, http.StatusForbidden},
		{"foreign origin read", guardedRequest{method: http.MethodGet, path: "/v1/sessions", origin: "http://evil.example"}, http.StatusForbidden},
	} {
		if status, body := tc.req.do(t, base); status != tc.want {
			t.Errorf("%s: got %d %s, want %d", tc.name, status, body, tc.want)
		}
	}
	if n := sessionCount(t, db); n != 0 {
		t.Fatalf("refused requests created %d sessions", n)
	}

	for _, tc := range []struct {
		name string
		req  guardedRequest
		want int
	}{
		{"cli without origin", guardedRequest{method: http.MethodPost, path: "/v1/sessions"}, http.StatusCreated},
		{"same origin", guardedRequest{method: http.MethodPost, path: "/v1/sessions", origin: base}, http.StatusCreated},
		{"localhost", guardedRequest{method: http.MethodPost, path: "/v1/sessions", host: "LocalHost" + port, origin: "http://localhost" + port}, http.StatusCreated},
		{"ipv6 loopback", guardedRequest{method: http.MethodPost, path: "/v1/sessions", host: "[::1]" + port, origin: "http://[::1]" + port}, http.StatusCreated},
		{"other loopback address", guardedRequest{method: http.MethodPost, path: "/v1/sessions", host: "127.0.0.2"}, http.StatusCreated},
		{"proxied same origin", guardedRequest{method: http.MethodPost, path: "/v1/sessions", host: "127.0.0.1:5173", origin: "http://127.0.0.1:5173"}, http.StatusCreated},
		{"health", guardedRequest{method: http.MethodGet, path: "/healthz"}, http.StatusOK},
		{"ui", guardedRequest{method: http.MethodGet, path: "/ui"}, http.StatusOK},
		{"approval page", guardedRequest{method: http.MethodGet, path: "/approval"}, http.StatusOK},
	} {
		if status, body := tc.req.do(t, base); status != tc.want {
			t.Errorf("%s: got %d %s, want %d", tc.name, status, body, tc.want)
		}
	}
}

func TestAllowedHostsExtendLoopbackHostCheck(t *testing.T) {
	_, base := startGuarded(t, &Server{LoopbackBind: true, AllowedHosts: []string{"harness.test", "proxy.test:8443"}})
	for host, want := range map[string]int{
		"harness.test":      http.StatusCreated,
		"Harness.Test:9000": http.StatusCreated,
		"proxy.test:8443":   http.StatusCreated,
		"proxy.test":        http.StatusForbidden,
		"proxy.test:9000":   http.StatusForbidden,
		"evil.example":      http.StatusForbidden,
	} {
		req := guardedRequest{method: http.MethodPost, path: "/v1/sessions", host: host, origin: "https://" + host}
		if status, body := req.do(t, base); status != want {
			t.Errorf("host %s: got %d %s, want %d", host, status, body, want)
		}
	}
}

// A token-protected non-loopback serve is reached under names the harness
// cannot know, so Host is unrestricted unless hosts are configured. A
// cross-origin request is refused either way.
func TestNonLoopbackHostAndOrigin(t *testing.T) {
	token := "0123456789abcdef"
	_, base := startGuarded(t, &Server{Token: token})
	for _, tc := range []struct {
		name string
		req  guardedRequest
		want int
	}{
		{"any host", guardedRequest{method: http.MethodPost, path: "/v1/sessions", host: "harness.lan:19421", token: token}, http.StatusCreated},
		{"same origin", guardedRequest{method: http.MethodPost, path: "/v1/sessions", host: "harness.lan", origin: "https://harness.lan", token: token}, http.StatusCreated},
		{"foreign origin", guardedRequest{method: http.MethodPost, path: "/v1/sessions", host: "harness.lan", origin: "https://evil.example", token: token}, http.StatusForbidden},
		{"foreign origin before auth", guardedRequest{method: http.MethodPost, path: "/v1/sessions", origin: "https://evil.example"}, http.StatusForbidden},
		{"missing token", guardedRequest{method: http.MethodPost, path: "/v1/sessions"}, http.StatusUnauthorized},
	} {
		if status, body := tc.req.do(t, base); status != tc.want {
			t.Errorf("%s: got %d %s, want %d", tc.name, status, body, tc.want)
		}
	}

	_, base = startGuarded(t, &Server{Token: token, AllowedHosts: []string{"harness.lan"}})
	for host, want := range map[string]int{"harness.lan": http.StatusCreated, "127.0.0.1": http.StatusCreated, "other.lan": http.StatusForbidden} {
		req := guardedRequest{method: http.MethodPost, path: "/v1/sessions", host: host, token: token}
		if status, body := req.do(t, base); status != want {
			t.Errorf("configured hosts, host %s: got %d %s, want %d", host, status, body, want)
		}
	}
}

func TestWriteBodyRequiresJSONContentType(t *testing.T) {
	db, base := startGuarded(t, &Server{LoopbackBind: true})
	status, body := guardedRequest{method: http.MethodPost, path: "/v1/sessions"}.do(t, base)
	if status != http.StatusCreated {
		t.Fatalf("create session: %d %s", status, body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatal(err)
	}
	runs := "/v1/sessions/" + created.ID + "/runs"
	for _, contentType := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x", "application/jsonx"} {
		status, body := guardedRequest{method: http.MethodPost, path: runs, contentType: contentType, body: `{"message":"hello"}`}.do(t, base)
		if status != http.StatusUnsupportedMediaType || !strings.Contains(body, domain.CodeInvalidArgument) {
			t.Errorf("Content-Type %q: got %d %s, want 415", contentType, status, body)
		}
	}
	if stored, err := db.ListRuns(context.Background(), created.ID); err != nil || len(stored) != 0 {
		t.Fatalf("refused bodies created runs: %d %v", len(stored), err)
	}
	for _, contentType := range []string{"application/json", "Application/JSON; charset=utf-8"} {
		status, body := guardedRequest{method: http.MethodPost, path: runs, contentType: contentType, body: `{"message":"hello"}`}.do(t, base)
		if status != http.StatusCreated {
			t.Errorf("Content-Type %q: got %d %s, want 201", contentType, status, body)
		}
	}
	// A write without a body carries no type to check.
	if status, body := (guardedRequest{method: http.MethodPost, path: "/v1/sessions", contentType: "text/plain"}).do(t, base); status != http.StatusCreated {
		t.Fatalf("bodiless write: %d %s", status, body)
	}
}

func TestHTTPServerTimeoutDefaults(t *testing.T) {
	srv := (&Server{}).NewHTTPServer("127.0.0.1:0")
	if srv.ReadHeaderTimeout != DefaultReadHeaderTimeout || srv.ReadTimeout != DefaultReadTimeout ||
		srv.WriteTimeout != DefaultWriteTimeout || srv.IdleTimeout != DefaultIdleTimeout {
		t.Fatalf("timeouts: header=%s read=%s write=%s idle=%s", srv.ReadHeaderTimeout, srv.ReadTimeout, srv.WriteTimeout, srv.IdleTimeout)
	}
}

func TestReadTimeoutClosesStalledBody(t *testing.T) {
	_, base := startGuarded(t, &Server{LoopbackBind: true, ReadTimeout: 100 * time.Millisecond})
	addr := strings.TrimPrefix(base, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	// Announce a body and never send it.
	if _, err := io.WriteString(conn, "POST /v1/sessions/ses_x/runs HTTP/1.1\r\nHost: "+addr+"\r\nContent-Type: application/json\r\nContent-Length: 64\r\n\r\n{"); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, conn); err != nil {
		t.Fatalf("server kept a stalled request open: %v", err)
	}
}

func TestWriteTimeoutAppliesToOrdinaryRoutes(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	slow := func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-time.After(400 * time.Millisecond):
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "late"})
	}
	_, base := startGuarded(t, &Server{LoopbackBind: true, WriteTimeout: 100 * time.Millisecond, Stats: slow})
	resp, err := http.Get(base + "/v1/stats")
	if err == nil {
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("response written after the write timeout: %d %s", resp.StatusCode, raw)
	}
}

type slowProvider struct {
	provider.Provider
	delay time.Duration
}

func (p slowProvider) Complete(ctx context.Context, req provider.Request) (domain.ModelResponse, error) {
	time.Sleep(p.delay)
	return p.Provider.Complete(ctx, req)
}

// A run executes inside the request that starts it, so that request must
// outlive the write timeout.
func TestRunRequestOutlivesWriteTimeout(t *testing.T) {
	m := &agent.Manager{Provider: slowProvider{Provider: &provider.Mock{}, delay: 400 * time.Millisecond}, OwnerID: "api"}
	_, base := startGuarded(t, &Server{Manager: m, LoopbackBind: true, ReadTimeout: 100 * time.Millisecond, WriteTimeout: 100 * time.Millisecond})
	status, body := guardedRequest{method: http.MethodPost, path: "/v1/sessions"}.do(t, base)
	if status != http.StatusCreated {
		t.Fatalf("create session: %d %s", status, body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatal(err)
	}
	status, body = guardedRequest{
		method: http.MethodPost, path: "/v1/sessions/" + created.ID + "/runs",
		contentType: "application/json", body: `{"message":"hello"}`,
	}.do(t, base)
	if status != http.StatusCreated || !strings.Contains(body, string(domain.RunCompleted)) {
		t.Fatalf("slow run: %d %s", status, body)
	}
}

func TestEventStreamOutlivesWriteTimeout(t *testing.T) {
	srv := &Server{
		LoopbackBind: true, ReadTimeout: 100 * time.Millisecond, WriteTimeout: 100 * time.Millisecond,
		HeartbeatInterval: 20 * time.Millisecond, PollInterval: 10 * time.Millisecond,
	}
	db, base := startGuarded(t, srv)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := db.CreateSession(ctx, storage.Session{ID: "ses_sse", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateRun(ctx, storage.Run{
		ID: "run_sse", SessionID: "ses_sse", Status: domain.RunRunning,
		Budget: domain.DefaultBudget(), Version: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, base+"/v1/runs/run_sse/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("event stream: %d", resp.StatusCode)
	}
	reader := bufio.NewReader(resp.Body)
	// Heartbeats keep arriving well past the read and write timeouts.
	started := time.Now()
	for time.Since(started) < 500*time.Millisecond {
		if _, err := reader.ReadString('\n'); err != nil {
			t.Fatalf("stream cut after %s: %v", time.Since(started), err)
		}
	}
	ev, err := trace.Persistent("run_sse", "step.late", map[string]string{"name": "late"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}
	srv.Notify("run_sse")
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("stream cut before the late event: %v", err)
		}
		if line == "event: step.late\n" {
			return
		}
	}
}
