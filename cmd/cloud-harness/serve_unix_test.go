//go:build unix

package main

import (
	"bytes"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// serve used to register only SIGINT, so SIGTERM killed the process without
// draining HTTP or closing the store. It also checks that serve wires the
// loopback Host check and --http-allowed-hosts into the handler.
func TestServeGuardsHostAndStopsOnSIGTERM(t *testing.T) {
	setDiagnosticEnv(t, "", "")
	root := t.TempDir()
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
	cmd := exec.Command(executable, "-test.run=^TestServeHTTPBindBaseline$", "--", "serve", "--addr", addr,
		"--http-allowed-hosts", "harness.test", "--provider", "mock", "--workspace", root, "--db", filepath.Join(root, "h.db"))
	cmd.Env = append(os.Environ(), "CLOUD_HARNESS_M1032_SERVE_HELPER=1")
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	exited := false
	defer func() {
		if !exited {
			_ = cmd.Process.Kill()
			<-done
		}
	}()

	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := client.Get("http://" + addr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		select {
		case err := <-done:
			exited = true
			t.Fatalf("serve exited before listening: %v stderr=%s", err, stderr.String())
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve did not become healthy: stderr=%s", stderr.String())
		}
	}
	for _, tc := range []struct {
		host, origin, contentType string
		want                      int
	}{
		{"evil.example", "http://evil.example", "text/plain", http.StatusForbidden},
		{addr, "http://evil.example", "text/plain", http.StatusForbidden},
		{"harness.test", "https://harness.test", "", http.StatusCreated},
		{addr, "http://" + addr, "", http.StatusCreated},
	} {
		req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/sessions", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = tc.host
		req.Header.Set("Origin", tc.origin)
		if tc.contentType != "" {
			req.Header.Set("Content-Type", tc.contentType)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("Host %s Origin %s: got %d, want %d", tc.host, tc.origin, resp.StatusCode, tc.want)
		}
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		exited = true
		// A process killed by the signal reports it here instead of exit status 0.
		if err != nil {
			t.Fatalf("serve did not shut down cleanly on SIGTERM: %v stderr=%s", err, stderr.String())
		}
	case <-time.After(serveStopGrace):
		t.Fatalf("serve still running %s after SIGTERM", serveStopGrace)
	}
	if !strings.Contains(out.String(), `"http_allowed_hosts":["harness.test"]`) {
		t.Fatalf("startup announcement omits allowed hosts: %s", out.String())
	}
}
