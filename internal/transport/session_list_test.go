package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/agent"
	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/provider"
	"github.com/cloud-harness/cloud-harness/internal/storage"
	"github.com/cloud-harness/cloud-harness/internal/storage/sqlite"
)

func TestListSessionsAggregatesAndSorts(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	base := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	createTestSession(t, db, "ses_empty", base.Add(-time.Hour), base.Add(-time.Hour))
	createTestSession(t, db, "ses_two", base.Add(-time.Hour), base.Add(-20*time.Minute))
	createTestSession(t, db, "ses_mixed", base.Add(-time.Hour), base)

	createTestRun(t, db, "run_two_old", "ses_two", domain.RunRunning, base.Add(-10*time.Minute))
	createTestRun(t, db, "run_two_latest", "ses_two", domain.RunFailed, base.Add(-5*time.Minute))
	createTestRun(t, db, "run_mixed_waiting", "ses_mixed", domain.RunWaiting, base.Add(-4*time.Minute))
	createTestRun(t, db, "run_mixed_completed", "ses_mixed", domain.RunCompleted, base.Add(-2*time.Minute))

	m := &agent.Manager{Store: db, Provider: &provider.Mock{}, OwnerID: "api"}
	h := (&Server{Manager: m}).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list sessions %d: %s", rec.Code, rec.Body)
	}
	var got []sessionView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode sessions: %v (%s)", err, rec.Body)
	}
	if len(got) != 3 {
		t.Fatalf("got %d sessions: %+v", len(got), got)
	}
	if got[0].ID != "ses_mixed" || got[1].ID != "ses_two" || got[2].ID != "ses_empty" {
		t.Fatalf("sessions not sorted by updated_at descending: %+v", got)
	}
	if got[0].RunCount != 2 || got[0].LatestRunStatus != domain.RunCompleted || got[0].LatestRunAt == nil || !got[0].LatestRunAt.Equal(base.Add(-2*time.Minute)) {
		t.Fatalf("incorrect mixed session aggregate: %+v", got[0])
	}
	if got[1].RunCount != 2 || got[1].LatestRunStatus != domain.RunFailed || got[1].LatestRunAt == nil || !got[1].LatestRunAt.Equal(base.Add(-5*time.Minute)) {
		t.Fatalf("incorrect two-run aggregate: %+v", got[1])
	}
	if got[2].RunCount != 0 || got[2].LatestRunStatus != "" || got[2].LatestRunAt != nil {
		t.Fatalf("incorrect empty session aggregate: %+v", got[2])
	}
	var wireSessions []map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &wireSessions); err != nil {
		t.Fatal(err)
	}
	if _, ok := wireSessions[2]["latest_run_status"]; ok {
		t.Fatalf("empty session should omit latest_run_status: %s", rec.Body)
	}
	if _, ok := wireSessions[2]["latest_run_at"]; ok {
		t.Fatalf("empty session should omit latest_run_at: %s", rec.Body)
	}
}

func TestListSessionsEmptyReturnsArray(t *testing.T) {
	_, h := newServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions", nil))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("empty sessions response = %d %q, want 200 []", rec.Code, rec.Body.String())
	}
}

func TestListSessionRunsUsesRunViewAndIsolatesSessions(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	base := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	createTestSession(t, db, "ses_target", base, base)
	createTestSession(t, db, "ses_other", base, base)
	first := createTestRun(t, db, "run_target_1", "ses_target", domain.RunCompleted, base.Add(time.Minute))
	second := createTestRun(t, db, "run_target_2", "ses_target", domain.RunFailed, base.Add(2*time.Minute))
	createTestRun(t, db, "run_foreign", "ses_other", domain.RunCompleted, base.Add(90*time.Second))

	m := &agent.Manager{Store: db, Provider: &provider.Mock{}, OwnerID: "api"}
	h := (&Server{Manager: m}).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions/ses_target/runs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list runs %d: %s", rec.Code, rec.Body)
	}
	var got []view
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode runs: %v (%s)", err, rec.Body)
	}
	if len(got) != 2 || got[0].ID != first.ID || got[1].ID != second.ID {
		t.Fatalf("runs not isolated and ordered by created_at: %+v", got)
	}
	for i, want := range []storage.Run{first, second} {
		getRec := httptest.NewRecorder()
		h.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "/v1/runs/"+want.ID, nil))
		if getRec.Code != http.StatusOK {
			t.Fatalf("get run %s: %d %s", want.ID, getRec.Code, getRec.Body)
		}
		var single view
		if err := json.Unmarshal(getRec.Body.Bytes(), &single); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got[i], single) {
			t.Fatalf("list view for %s differs from GET /v1/runs/{id}: list=%+v single=%+v", want.ID, got[i], single)
		}
	}

	missing := httptest.NewRecorder()
	h.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/v1/sessions/ses_missing/runs", nil))
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), `"code":"not_found"`) {
		t.Fatalf("missing session response = %d %s", missing.Code, missing.Body)
	}
}

func TestListSessionRunsEmptyReturnsArray(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "empty-runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	createTestSession(t, db, "ses_empty_runs", now, now)
	m := &agent.Manager{Store: db, Provider: &provider.Mock{}, OwnerID: "api"}
	h := (&Server{Manager: m}).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions/ses_empty_runs/runs", nil))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("empty session runs response = %d %q, want 200 []", rec.Code, rec.Body.String())
	}
}

func TestSessionListEndpointsUseBearerAuth(t *testing.T) {
	m, _ := newServer(t)
	session, err := m.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	token := "session-list-token-12345"
	h := (&Server{Manager: m, Token: token}).Handler()
	for _, path := range []string{"/v1/sessions", "/v1/sessions/" + session.ID + "/runs"} {
		for _, tc := range []struct {
			name   string
			header string
			status int
		}{
			{name: "missing", status: http.StatusUnauthorized},
			{name: "wrong", header: "Bearer wrong-session-list-token", status: http.StatusUnauthorized},
			{name: "correct", header: "Bearer " + token, status: http.StatusOK},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, path, nil)
				if tc.header != "" {
					req.Header.Set("Authorization", tc.header)
				}
				h.ServeHTTP(rec, req)
				if rec.Code != tc.status {
					t.Fatalf("status %d, want %d: %s", rec.Code, tc.status, rec.Body)
				}
				if tc.status == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") != `Bearer realm="cloud-harness"` {
					t.Fatalf("missing Bearer challenge: %v", rec.Header())
				}
			})
		}
	}
}

func TestListSessionRunsReturnsMoreThanInteractiveListLimit(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "many-runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	createTestSession(t, db, "ses_many", now, now)
	for i := 0; i < 205; i++ {
		createTestRun(t, db, "run_"+strconv.Itoa(i), "ses_many", domain.RunCompleted, now.Add(time.Duration(i)*time.Second))
	}
	m := &agent.Manager{Store: db, Provider: &provider.Mock{}, OwnerID: "api"}
	h := (&Server{Manager: m}).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions/ses_many/runs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list runs %d: %s", rec.Code, rec.Body)
	}
	var got []view
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode runs: %v (%s)", err, rec.Body)
	}
	if len(got) != 205 || got[0].ID != "run_0" || got[len(got)-1].ID != "run_204" {
		t.Fatalf("got %d runs, first=%v last=%v", len(got), got[0].ID, got[len(got)-1].ID)
	}
}

func createTestSession(t *testing.T, db *sqlite.DB, id string, createdAt, updatedAt time.Time) {
	t.Helper()
	if err := db.CreateSession(context.Background(), storage.Session{ID: id, CreatedAt: createdAt, UpdatedAt: updatedAt}); err != nil {
		t.Fatal(err)
	}
}

func createTestRun(t *testing.T, db *sqlite.DB, id, sessionID string, status domain.RunStatus, createdAt time.Time) storage.Run {
	t.Helper()
	run := storage.Run{
		ID: id, SessionID: sessionID, Status: status, Budget: domain.DefaultBudget(), Version: 1,
		CreatedAt: createdAt, UpdatedAt: createdAt,
	}
	if err := db.CreateRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	return run
}
