//go:build unix

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/storage/sqlite"
)

// A run created over HTTP no longer ends with its request, so SIGTERM has to
// stop it: serve cancels it, records CANCELLED, and exits cleanly. Without
// that the store would close on a run left RUNNING with nothing executing it.
func TestServeCancelsExecutingRunOnSIGTERM(t *testing.T) {
	// The upstream sends one text delta and then stalls, so the run is in the
	// middle of a model response when the signal arrives.
	stalled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"thinking\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-stalled:
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	defer close(stalled)

	setDiagnosticEnv(t, "", "test-key")
	t.Setenv("OPENAI_BASE_URL", upstream.URL)
	t.Setenv("OPENAI_MODEL", "test-model")
	root := t.TempDir()
	dbPath := filepath.Join(root, "h.db")
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
		"--provider", "openai", "--workspace", root, "--db", dbPath)
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

	base := "http://" + addr
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := client.Get(base + "/healthz")
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

	post := func(path, body string, into any) {
		t.Helper()
		resp, err := client.Post(base+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("POST %s: status %d stderr=%s", path, resp.StatusCode, stderr.String())
		}
		if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
			t.Fatal(err)
		}
	}
	var session struct {
		ID string `json:"id"`
	}
	post("/v1/sessions", "", &session)
	var run struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	post("/v1/sessions/"+session.ID+"/runs?wait=0", `{"message":"hello"}`, &run)
	if run.Status != string(domain.RunRunning) {
		t.Fatalf("run is %s right after creation", run.Status)
	}

	// Wait for the delta, which proves the run is inside the model call.
	streamCtx, stopStream := context.WithTimeout(context.Background(), 20*time.Second)
	defer stopStream()
	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, base+"/v1/runs/"+run.ID+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	lines := bufio.NewScanner(stream.Body)
	sawDelta := false
	for !sawDelta && lines.Scan() {
		sawDelta = lines.Text() == "event: stream.delta"
	}
	if !sawDelta {
		t.Fatalf("no stream.delta before the stream ended: %v stderr=%s", lines.Err(), stderr.String())
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		exited = true
		if err != nil {
			t.Fatalf("serve did not shut down cleanly on SIGTERM: %v stderr=%s", err, stderr.String())
		}
	case <-time.After(serveRunStopTimeout + serveStopGrace):
		t.Fatal("serve still running after SIGTERM")
	}
	// The open subscription was told how the run ended before serve exited.
	sawCancelled := false
	for lines.Scan() {
		sawCancelled = sawCancelled || lines.Text() == "event: run.cancelled"
	}
	if !sawCancelled {
		t.Error("the subscriber was not sent run.cancelled")
	}

	db, err := sqlite.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stored, err := db.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != domain.RunCancelled {
		t.Fatalf("run is %s after SIGTERM, want CANCELLED", stored.Status)
	}
	events, err := db.ListEvents(context.Background(), run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	cancelled := false
	for _, ev := range events {
		if ev.Type == "stream.delta" {
			t.Error("a delta was persisted")
		}
		cancelled = cancelled || ev.Type == "run.cancelled"
	}
	if !cancelled {
		t.Errorf("no run.cancelled event was recorded: %d events", len(events))
	}
}
