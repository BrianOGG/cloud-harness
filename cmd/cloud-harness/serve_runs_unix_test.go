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
	inModel := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"thinking\"}}]}\n\n")
		w.(http.Flusher).Flush()
		inModel <- struct{}{}
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
	done := make(chan struct{})
	var processErr error
	go func() {
		processErr = cmd.Wait()
		close(done)
	}()
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
		case <-done:
			exited = true
			t.Fatalf("serve exited before listening: %v stderr=%s", processErr, stderr.String())
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
		defer func() { _ = resp.Body.Close() }()
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

	// The upstream has started a response, but its content is unclassified
	// until completion and must not be published as a visible delta.
	streamCtx, stopStream := context.WithTimeout(context.Background(), 20*time.Second)
	defer stopStream()
	select {
	case <-inModel:
	case <-streamCtx.Done():
		t.Fatal("run did not enter the upstream model call")
	}
	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, base+"/v1/runs/"+run.ID+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Body.Close() }()
	lines := bufio.NewScanner(stream.Body)
	sawCreated := false
	for !sawCreated && lines.Scan() {
		sawCreated = lines.Text() == "event: run.created"
	}
	if !sawCreated {
		t.Fatalf("no run.created before the stream ended: %v stderr=%s", lines.Err(), stderr.String())
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		exited = true
		if processErr != nil {
			t.Fatalf("serve did not shut down cleanly on SIGTERM: %v stderr=%s", processErr, stderr.String())
		}
	case <-time.After(serveRunStopTimeout + serveStopGrace):
		t.Fatal("serve still running after SIGTERM")
	}
	// The open subscription was told how the run ended before serve exited.
	sawCancelled := false
	for lines.Scan() {
		sawCancelled = sawCancelled || lines.Text() == "event: run.cancelled"
		if lines.Text() == "event: stream.delta" {
			t.Error("unclassified content was sent to the subscriber")
		}
	}
	if !sawCancelled {
		t.Error("the subscriber was not sent run.cancelled")
	}

	db, err := sqlite.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
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
