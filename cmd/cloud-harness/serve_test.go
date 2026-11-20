package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The parent must allow more than serveShutdownTimeout: after HTTP draining,
// serve still closes connections and SQLite, exits, and Wait joins the stdout /
// stderr copies. Keep 2s of headroom above the 1s drain (under CPU load normal
// SIGINT-to-Wait was <100ms), independent of the 20s startup budget.
const serveStopGrace = serveShutdownTimeout + 2*time.Second

func TestServeRejectsExternalBeforeListening(t *testing.T) {
	for _, tc := range []struct {
		name   string
		flag   bool
		env    string
		config bool
	}{
		{name: "no_token"},
		{name: "legacy_flag", flag: true},
		{name: "legacy_insecure_env", env: "CLOUD_HARNESS_HTTP_ALLOW_INSECURE"},
		{name: "legacy_external_env", env: "CLOUD_HARNESS_ALLOW_EXTERNAL"},
		{name: "legacy_config", config: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setDiagnosticEnv(t, "", "")
			t.Setenv("CLOUD_HARNESS_HTTP_ALLOW_INSECURE", "")
			t.Setenv("CLOUD_HARNESS_ALLOW_EXTERNAL", "")
			if tc.env != "" {
				t.Setenv(tc.env, "1")
			}
			root := t.TempDir()
			if tc.config {
				path := filepath.Join(root, "config.json")
				if err := os.WriteFile(path, []byte(`{"http_allow_insecure":true}`), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("CLOUD_HARNESS_CONFIG", path)
			}
			// Occupy the address: validation must win before any bind error and
			// before opening the store or printing a listening announcement.
			reserved, err := net.Listen("tcp", "0.0.0.0:0")
			if err != nil {
				t.Fatal(err)
			}
			defer reserved.Close()
			_, port, err := net.SplitHostPort(reserved.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			addr := net.JoinHostPort("0.0.0.0", port)
			dbPath := filepath.Join(root, "must-not-exist.db")
			args := []string{"serve", "--addr", addr, "--db", dbPath, "--workspace", root}
			if tc.flag {
				args = append(args, "--http-allow-insecure")
			}
			var out, stderr bytes.Buffer
			code := Main(args, &out, &stderr)
			if code != 1 || !strings.Contains(stderr.String(), "non-loopback listen requires an HTTP token") || !strings.Contains(stderr.String(), "--http-token") {
				t.Fatalf("serve exit=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
			}
			if out.Len() != 0 || strings.Contains(stderr.String(), "address already in use") {
				t.Fatalf("serve reached listener startup: stdout=%s stderr=%s", out.String(), stderr.String())
			}
			if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
				t.Fatalf("serve initialized the store before validation: %v", err)
			}
			if err := reserved.Close(); err != nil {
				t.Fatal(err)
			}
			available, err := net.Listen("tcp", addr)
			if err != nil {
				t.Fatalf("rejected serve left a listener on %s: %v", addr, err)
			}
			_ = available.Close()
		})
	}
}

func TestServeHTTPBindBaseline(t *testing.T) {
	if os.Getenv("CLOUD_HARNESS_M1032_SERVE_HELPER") == "1" {
		for i, arg := range os.Args {
			if arg == "--" {
				os.Exit(Main(os.Args[i+1:], os.Stdout, os.Stderr))
			}
		}
		os.Exit(2)
	}
	for _, tc := range []struct {
		name        string
		host        string
		tokenSource string
	}{
		{name: "loopback_without_token", host: "127.0.0.1"},
		{name: "external_flag_token", host: "0.0.0.0", tokenSource: "flag"},
		{name: "external_env_token", host: "0.0.0.0", tokenSource: "env"},
		{name: "external_config_token", host: "0.0.0.0", tokenSource: "config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setDiagnosticEnv(t, "", "")
			t.Setenv("CLOUD_HARNESS_HTTP_ALLOW_INSECURE", "")
			t.Setenv("CLOUD_HARNESS_ALLOW_EXTERNAL", "")
			root := t.TempDir()
			reserved, err := net.Listen("tcp", net.JoinHostPort(tc.host, "0"))
			if err != nil {
				t.Fatal(err)
			}
			_, port, err := net.SplitHostPort(reserved.Addr().String())
			_ = reserved.Close()
			if err != nil {
				t.Fatal(err)
			}
			addr := net.JoinHostPort(tc.host, port)
			token := ""
			args := []string{"-test.run=^TestServeHTTPBindBaseline$", "--", "serve", "--addr", addr, "--provider", "mock", "--workspace", root, "--db", filepath.Join(root, "h.db")}
			if tc.tokenSource != "" {
				token = "serve-baseline-token-0123456789"
				switch tc.tokenSource {
				case "flag":
					args = append(args, "--http-token", token)
				case "env":
					t.Setenv("CLOUD_HARNESS_HTTP_TOKEN", token)
				case "config":
					path := filepath.Join(root, "config.json")
					if err := os.WriteFile(path, []byte(`{"http_token":"`+token+`"}`), 0o600); err != nil {
						t.Fatal(err)
					}
					t.Setenv("CLOUD_HARNESS_CONFIG", path)
				}
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			startupCtx, cancelStartup := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancelStartup()
			// Startup cancellation must not kill the child during graceful stop.
			// stop owns SIGINT, its separate grace, and the final Kill / Wait.
			cmd := exec.Command(executable, args...)
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
				timer := time.NewTimer(serveStopGrace)
				defer timer.Stop()
				select {
				case <-done:
				case <-timer.C:
					_ = cmd.Process.Kill()
					<-done
				}
			}
			defer stop()
			base := "http://" + net.JoinHostPort("127.0.0.1", port)
			transport := http.DefaultTransport.(*http.Transport).Clone()
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: time.Second}
			for {
				req, err := http.NewRequestWithContext(startupCtx, http.MethodGet, base+"/healthz", nil)
				if err != nil {
					t.Fatal(err)
				}
				resp, err := client.Do(req)
				if err == nil {
					_ = resp.Body.Close()
					if resp.StatusCode != http.StatusOK {
						t.Fatalf("health without token: %d", resp.StatusCode)
					}
					break
				}
				select {
				case <-done:
					t.Fatalf("serve exited before listening: %v stdout=%s stderr=%s", processErr, out.String(), stderr.String())
				case <-startupCtx.Done():
					stop()
					t.Fatalf("serve did not become healthy: stdout=%s stderr=%s", out.String(), stderr.String())
				case <-time.After(10 * time.Millisecond):
				}
			}
			cancelStartup()
			req, err := http.NewRequest(http.MethodGet, base+"/v1/approvals", nil)
			if err != nil {
				t.Fatal(err)
			}
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("queue on accepted bind: %d", resp.StatusCode)
			}
			stop()
			if processErr != nil {
				t.Fatalf("serve shutdown: %v stderr=%s", processErr, stderr.String())
			}
			if !strings.Contains(out.String(), "listening on http://") || (token != "" && (strings.Contains(out.String(), token) || strings.Contains(stderr.String(), token))) {
				t.Fatal("missing startup announcement or leaked token")
			}
		})
	}
}

func TestServeShutdownClosesPendingConnections(t *testing.T) {
	for _, active := range []bool{false, true} {
		name := "new_without_headers"
		if active {
			name = "active_handler"
		}
		t.Run(name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			accepted, handling, handled := make(chan struct{}), make(chan struct{}), make(chan struct{})
			srv := &http.Server{
				Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					close(handling)
					defer close(handled)
					<-r.Context().Done()
				}),
				ConnState: func(_ net.Conn, state http.ConnState) {
					if state == http.StateNew {
						close(accepted)
					}
				},
			}
			serveErr := make(chan error, 1)
			go func() { serveErr <- srv.Serve(listener) }()
			t.Cleanup(func() {
				_ = srv.Close()
				if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
					t.Errorf("Serve: %v", err)
				}
			})
			conn, err := net.DialTimeout("tcp", listener.Addr().String(), serveStopGrace)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			// Synchronize on the server's state rather than sleeping: a successful
			// Dial alone does not prove that Serve has accepted the connection.
			select {
			case <-accepted:
			case <-time.After(serveStopGrace):
				t.Fatal("server did not accept the connection")
			}
			if active {
				if _, err := io.WriteString(conn, "GET /healthz HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
					t.Fatal(err)
				}
				select {
				case <-handling:
				case <-time.After(serveStopGrace):
					t.Fatal("handler did not start")
				}
			}
			started := time.Now()
			shutdownServe(srv)
			elapsed := time.Since(started)
			if elapsed >= serveStopGrace {
				t.Fatalf("HTTP shutdown exhausted the parent's %s grace: %s", serveStopGrace, elapsed)
			}
			// Returning on the drain deadline alone is insufficient: Close must
			// also release the header reader / cancel the active handler.
			if err := conn.SetReadDeadline(time.Now().Add(serveStopGrace)); err != nil {
				t.Fatal(err)
			}
			var b [1]byte
			if n, err := conn.Read(b[:]); n != 0 || !errors.Is(err, io.EOF) {
				t.Fatalf("connection remained open after shutdown: n=%d err=%v", n, err)
			}
			if active {
				select {
				case <-handled:
				case <-time.After(serveStopGrace):
					t.Fatal("shutdown did not cancel the request context")
				}
			}
			t.Logf("pending connection=%s shutdown=%s", name, elapsed)
		})
	}
}
