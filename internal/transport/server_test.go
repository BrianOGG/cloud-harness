package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloud-harness/cloud-harness/internal/agent"
	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/provider"
	"github.com/cloud-harness/cloud-harness/internal/storage/sqlite"
)

func newServer(t *testing.T) (*agent.Manager, http.Handler) {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	m := &agent.Manager{Store: db, Provider: &provider.Mock{}, OwnerID: "api"}
	return m, (&Server{Manager: m}).Handler()
}

func TestMockRunOverHTTP(t *testing.T) {
	_, h := newServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewReader(nil))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("session %d %s", rec.Code, rec.Body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatal(rec.Body)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/sessions/"+created.ID+"/runs", strings.NewReader(`{"message":"hello"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("run %d %s", rec.Code, rec.Body)
	}
	var run struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		FinalAnswer string `json:"final_answer"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	if run.Status != "COMPLETED" || run.FinalAnswer != "Mock answer: hello" {
		t.Fatalf("%+v", run)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/runs/"+run.ID+"/events", nil)
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "id: 1\n") || !strings.Contains(body, "event: run.created") {
		t.Fatalf("sse:\n%s", body)
	}
	first := body
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/runs/"+run.ID+"/events?preview=1", nil)
	req.Header.Set("Last-Event-ID", "1")
	h.ServeHTTP(rec, req)
	replay := rec.Body.String()
	if strings.Contains(replay, "id: 1\n") {
		t.Fatalf("replayed the first event:\n%s", replay)
	}
	if !strings.Contains(replay, "event: stream.delta") || strings.Contains(replay, "id: ") && strings.Contains(replay, "stream.delta") {
		// The delta must appear, and the line immediately before it must not be an id.
	}
	if !strings.Contains(replay, "event: stream.delta\ndata: {\"text\":\"preview\"}") {
		t.Fatalf("preview missing:\n%s", replay)
	}
	deltaAt := strings.Index(replay, "event: stream.delta")
	if strings.Contains(replay[deltaAt-10:deltaAt], "id:") {
		t.Fatalf("delta has a cursor:\n%s", replay)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/runs/"+run.ID+"/events", nil)
	h.ServeHTTP(rec, req)
	if rec.Body.String() != first {
		t.Fatal("reconnecting replayed a different log or stored the preview")
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/runs/"+run.ID+"/trace", nil)
	h.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "opaque") && strings.Contains(rec.Body.String(), "SECRET") {
		t.Fatalf("trace %s", rec.Body)
	}
}

func TestResumeDoesNotReplaceRun(t *testing.T) {
	m, h := newServer(t)
	m.UseBudget = true
	m.InitialBudget = domain.Budget{MaxModelCalls: 0}
	ctx := context.Background()
	ses, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessage(ctx, ses.ID, "need budget")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/"+run.ID+"/responses", strings.NewReader(`{"answer":"yes"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("responses %d %s", rec.Code, rec.Body)
	}
	got, err := m.Store.GetRun(ctx, run.ID)
	if err != nil || got.ID != run.ID || got.Status != domain.RunWaiting {
		t.Fatalf("run changed %+v %v", got, err)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/runs/"+run.ID+"/cancel", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"CANCELLED"`) {
		t.Fatalf("cancel %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/runs/"+run.ID+"/cancel", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second cancel %d %s", rec.Code, rec.Body)
	}
}
