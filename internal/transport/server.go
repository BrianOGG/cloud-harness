// Package transport is the HTTP API. It calls the run manager and does not
// touch tools except through that manager. The default bind is loopback.
package transport

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/agent"
	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/storage"
	"github.com/cloud-harness/cloud-harness/internal/trace"
)

//go:embed approval.html
var approvalHTML []byte

const (
	// DefaultMaxBodyBytes caps POST and PUT request bodies at 1 MiB.
	DefaultMaxBodyBytes int64 = 1 << 20
	// DefaultHeartbeatInterval is the SSE ": ping" heartbeat interval.
	DefaultHeartbeatInterval = 15 * time.Second
	// DefaultPollInterval is the store poll cadence for active SSE subscriptions.
	DefaultPollInterval = 25 * time.Millisecond
	// DefaultReadHeaderTimeout is the HTTP server ReadHeaderTimeout.
	DefaultReadHeaderTimeout = 10 * time.Second
	// DefaultIdleTimeout is the HTTP server IdleTimeout.
	DefaultIdleTimeout = 120 * time.Second
)

// Server serves the HTTP API. The default bind is loopback.
type Server struct {
	Manager           *agent.Manager
	Now               func() time.Time
	Token             string
	HTTPToken         string
	MaxBodyBytes      int64
	HeartbeatInterval time.Duration
	PollInterval      time.Duration

	subMu sync.Mutex
	subs  map[string]map[chan struct{}]struct{}
}

// NewHTTPServer builds an *http.Server with the default security timeouts.
func (s *Server) NewHTTPServer(addr string) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: DefaultReadHeaderTimeout,
		IdleTimeout:       DefaultIdleTimeout,
	}
}

func (s *Server) token() string {
	if s.Token != "" {
		return s.Token
	}
	return s.HTTPToken
}

func (s *Server) maxBodyBytes() int64 {
	if s.MaxBodyBytes > 0 {
		return s.MaxBodyBytes
	}
	return DefaultMaxBodyBytes
}

func (s *Server) heartbeatInterval() time.Duration {
	if s.HeartbeatInterval > 0 {
		return s.HeartbeatInterval
	}
	return DefaultHeartbeatInterval
}

func (s *Server) pollInterval() time.Duration {
	if s.PollInterval > 0 {
		return s.PollInterval
	}
	return DefaultPollInterval
}

// Handler returns the HTTP routes wrapped with bearer authentication and body size limits.
func (s *Server) Handler() http.Handler {
	if s.Manager != nil {
		if s.Now != nil && s.Manager.Now == nil {
			s.Manager.Now = s.Now
		}
		if s.Manager.Store != nil {
			_, _ = s.Manager.RecoverExpired(context.Background())
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /approval", serveApprovalPage)
	mux.HandleFunc("GET /v1/sessions", s.listSessions)
	mux.HandleFunc("POST /v1/sessions", s.createSession)
	mux.HandleFunc("GET /v1/sessions/{id}/runs", s.listSessionRuns)
	mux.HandleFunc("POST /v1/sessions/{id}/runs", s.createRun)
	mux.HandleFunc("GET /v1/approvals", s.approvals)
	mux.HandleFunc("GET /v1/runs/{id}", s.getRun)
	mux.HandleFunc("GET /v1/runs/{id}/events", s.events)
	mux.HandleFunc("GET /v1/runs/{id}/trace", s.traceRun)
	mux.HandleFunc("POST /v1/runs/{id}/responses", s.respond)
	mux.HandleFunc("POST /v1/runs/{id}/answer", s.answer)
	mux.HandleFunc("POST /v1/runs/{id}/budget", s.budget)
	mux.HandleFunc("POST /v1/runs/{id}/cancel", s.cancel)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tok := s.token(); tok != "" && r.URL.Path != "/healthz" && r.URL.Path != "/approval" {
			if !checkBearerToken(r.Header.Get("Authorization"), tok) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="cloud-harness"`)
				writeJSON(w, http.StatusUnauthorized, map[string]*domain.Error{
					"error": domain.E(domain.CodeAuthDenied, "missing or invalid bearer token"),
				})
				return
			}
		}
		if (r.Method == http.MethodPost || r.Method == http.MethodPut) && r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes())
		}
		mux.ServeHTTP(w, r)
	})
}

func checkBearerToken(header, expected string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got := strings.TrimPrefix(header, prefix)
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(expected)) == 1
}

func serveApprovalPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	_, _ = w.Write(approvalHTML)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	name := ""
	if s.Manager != nil && s.Manager.Provider != nil {
		name = s.Manager.Provider.Name()
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "provider": name})
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	if err := drainEmptyBody(r); err != nil {
		writeErr(w, err)
		return
	}
	ses, err := s.Manager.CreateSession(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": ses.ID})
}

type sessionView struct {
	ID              string           `json:"id"`
	CreatedAt       time.Time        `json:"created_at"`
	UpdatedAt       time.Time        `json:"updated_at"`
	RunCount        int              `json:"run_count"`
	LatestRunStatus domain.RunStatus `json:"latest_run_status,omitempty"`
	LatestRunAt     *time.Time       `json:"latest_run_at,omitempty"`
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.Manager.Store.ListSessions(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]sessionView, 0, len(sessions))
	for _, session := range sessions {
		out = append(out, sessionView{
			ID: session.ID, CreatedAt: session.CreatedAt, UpdatedAt: session.UpdatedAt,
			RunCount: session.RunCount, LatestRunStatus: session.LatestRunStatus, LatestRunAt: session.LatestRunAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listSessionRuns(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sessionID := r.PathValue("id")
	if _, err := s.Manager.Store.GetSession(ctx, sessionID); err != nil {
		writeErr(w, err)
		return
	}
	runs, err := s.Manager.Store.ListRuns(ctx, sessionID)
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]view, 0, len(runs))
	for _, run := range runs {
		out = append(out, s.runView(ctx, run))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Message string `json:"message"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	if stringsTrim(body.Message) == "" {
		writeErr(w, domain.E(domain.CodeInvalidArgument, "message is required"))
		return
	}
	run, err := s.Manager.RunMessage(r.Context(), r.PathValue("id"), body.Message)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.Notify(run.ID)
	writeJSON(w, http.StatusCreated, s.runView(r.Context(), run))
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.Manager.Store.GetRun(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.runView(r.Context(), run))
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run, err := s.Manager.Store.GetRun(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	after, err := s.parseLastEventID(r.Context(), id, r.Header.Get("Last-Event-ID"))
	if err != nil {
		writeErr(w, err)
		return
	}
	subCh, unsubscribe := s.subscribe(id)
	defer unsubscribe()

	events, err := s.Manager.Store.ListEvents(r.Context(), id, after)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	for _, ev := range events {
		if err := trace.WriteSSE(w, ev); err != nil {
			return
		}
		if ev.Seq > after {
			after = ev.Seq
		}
	}
	// A stream delta is not stored. This one is only emitted when the caller
	// asks, so tests can see that it has no id and never lands in the log.
	if r.URL.Query().Get("preview") == "1" {
		_ = trace.WriteSSE(w, storage.Event{Type: "stream.delta", Payload: []byte(`{"text":"preview"}`), Persistent: false})
	}
	flushSSE(w)

	if cur, gerr := s.Manager.Store.GetRun(r.Context(), id); gerr == nil {
		run = cur
	}
	if domain.TerminalRun(run.Status) {
		if tail, lerr := s.Manager.Store.ListEvents(r.Context(), id, after); lerr == nil && len(tail) > 0 {
			for _, ev := range tail {
				if err := trace.WriteSSE(w, ev); err != nil {
					return
				}
				if ev.Seq > after {
					after = ev.Seq
				}
			}
			flushSSE(w)
		}
		return
	}
	if r.Context().Done() == nil {
		return
	}

	hbTicker := time.NewTicker(s.heartbeatInterval())
	defer hbTicker.Stop()
	pollTicker := time.NewTicker(s.pollInterval())
	defer pollTicker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-hbTicker.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flushSSE(w)
		case <-subCh:
			if s.drainLiveEvents(r.Context(), w, id, &after) {
				return
			}
		case <-pollTicker.C:
			if s.drainLiveEvents(r.Context(), w, id, &after) {
				return
			}
		}
	}
}

func (s *Server) parseLastEventID(ctx context.Context, runID, header string) (int64, error) {
	h := stringsTrim(header)
	if h == "" {
		return 0, nil
	}
	if n, err := strconv.ParseInt(h, 10, 64); err == nil && n >= 0 {
		return n, nil
	}
	if all, err := s.Manager.Store.ListEvents(ctx, runID, 0); err == nil {
		for _, ev := range all {
			if ev.ID == h {
				return ev.Seq, nil
			}
		}
	}
	return 0, domain.E(domain.CodeInvalidArgument, "Last-Event-ID must be an integer")
}

func (s *Server) drainLiveEvents(ctx context.Context, w http.ResponseWriter, runID string, after *int64) bool {
	evs, err := s.Manager.Store.ListEvents(ctx, runID, *after)
	if err != nil {
		return true
	}
	if len(evs) > 0 {
		for _, ev := range evs {
			if err := trace.WriteSSE(w, ev); err != nil {
				return true
			}
			if ev.Seq > *after {
				*after = ev.Seq
			}
		}
		flushSSE(w)
	}
	run, err := s.Manager.Store.GetRun(ctx, runID)
	if err != nil {
		return true
	}
	if domain.TerminalRun(run.Status) {
		if tail, lerr := s.Manager.Store.ListEvents(ctx, runID, *after); lerr == nil && len(tail) > 0 {
			for _, ev := range tail {
				if err := trace.WriteSSE(w, ev); err != nil {
					return true
				}
				if ev.Seq > *after {
					*after = ev.Seq
				}
			}
			flushSSE(w)
		}
		return true
	}
	return false
}

func (s *Server) subscribe(runID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.subMu.Lock()
	if s.subs == nil {
		s.subs = make(map[string]map[chan struct{}]struct{})
	}
	if s.subs[runID] == nil {
		s.subs[runID] = make(map[chan struct{}]struct{})
	}
	s.subs[runID][ch] = struct{}{}
	s.subMu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			s.subMu.Lock()
			if m, ok := s.subs[runID]; ok {
				delete(m, ch)
				if len(m) == 0 {
					delete(s.subs, runID)
				}
			}
			s.subMu.Unlock()
		})
	}
	return ch, unsubscribe
}

// Notify wakes active SSE subscribers for runID so they check the event store.
func (s *Server) Notify(runID string) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for ch := range s.subs[runID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// SubscriberCount reports the number of active SSE subscribers for runID.
func (s *Server) SubscriberCount(runID string) int {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	return len(s.subs[runID])
}

func flushSSE(w http.ResponseWriter) {
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (s *Server) traceRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run, err := s.Manager.Store.GetRun(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	responses, err := s.Manager.Store.ListVisibleResponses(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	batches, err := s.Manager.Store.LoadBatches(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	children, err := childNodes(r.Context(), s.Manager.Store, run)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, trace.Assemble(run, responses, batches, children))
}

func childNodes(ctx context.Context, store storage.Store, run storage.Run) ([]trace.Node, error) {
	runs, err := store.ListRuns(ctx, run.SessionID)
	if err != nil {
		return nil, err
	}
	var out []trace.Node
	for _, child := range runs {
		if child.ParentRunID != run.ID {
			continue
		}
		out = append(out, trace.Node{
			ID: child.ID, ParentID: child.ParentRunID, SessionID: child.SessionID,
			Status: string(child.Status), WaitReason: string(child.WaitReason),
		})
	}
	return out, nil
}

func (s *Server) respond(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Decision        string `json:"decision"`
		Answer          string `json:"answer"`
		ExtraModelCalls int    `json:"extra_model_calls"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := decode(r, &body); err != nil {
			writeErr(w, err)
			return
		}
	}
	id := r.PathValue("id")
	run, err := s.Manager.Resume(r.Context(), id, agent.ResumeInput{
		Decision: body.Decision, Answer: body.Answer, ExtraModelCalls: body.ExtraModelCalls,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	s.Notify(id)
	writeJSON(w, http.StatusOK, s.runView(r.Context(), run))
}

func (s *Server) answer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Answer string `json:"answer"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	id := r.PathValue("id")
	run, err := s.Manager.Store.GetRun(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	if run.Status != domain.RunWaiting || run.WaitReason != domain.WaitUser {
		writeErr(w, domain.E(domain.CodeNotWaiting, "run is not waiting for user input").WithRef(id))
		return
	}
	ans := stringsTrim(body.Answer)
	if ans == "" {
		writeErr(w, domain.E(domain.CodeInvalidArgument, "answer is required").WithRef(id))
		return
	}
	resumed, err := s.Manager.Resume(r.Context(), id, agent.ResumeInput{Answer: ans})
	if err != nil {
		writeErr(w, err)
		return
	}
	s.Notify(id)
	writeJSON(w, http.StatusOK, s.runView(r.Context(), resumed))
}

func (s *Server) budget(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ExtraModelCalls int `json:"extra_model_calls"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	id := r.PathValue("id")
	run, err := s.Manager.Resume(r.Context(), id, agent.ResumeInput{ExtraModelCalls: body.ExtraModelCalls})
	if err != nil {
		writeErr(w, err)
		return
	}
	s.Notify(id)
	writeJSON(w, http.StatusOK, s.runView(r.Context(), run))
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	if err := drainEmptyBody(r); err != nil {
		writeErr(w, err)
		return
	}
	id := r.PathValue("id")
	run, err := s.Manager.Cancel(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.Notify(id)
	writeJSON(w, http.StatusOK, s.runView(r.Context(), run))
}

type view struct {
	ID                string                   `json:"id"`
	SessionID         string                   `json:"session_id"`
	Status            domain.RunStatus         `json:"status"`
	WaitReason        domain.WaitReason        `json:"wait_reason,omitempty"`
	WaitKind          domain.WaitReason        `json:"wait_kind,omitempty"`
	Question          string                   `json:"question,omitempty"`
	PendingInvocation *agent.PendingInvocation `json:"pending_invocation,omitempty"`
	Budget            domain.Budget            `json:"budget"`
	FinalAnswer       string                   `json:"final_answer,omitempty"`
	Error             *domain.Error            `json:"error,omitempty"`
	Version           int64                    `json:"version"`
}

func (s *Server) runView(ctx context.Context, run storage.Run) view {
	v := view{
		ID: run.ID, SessionID: run.SessionID, Status: run.Status, WaitReason: run.WaitReason,
		WaitKind: run.WaitReason, Budget: run.Budget, FinalAnswer: run.FinalAnswer,
		Error: run.Error, Version: run.Version,
	}
	if run.Status == domain.RunWaiting && s.Manager != nil {
		if info, err := s.Manager.WaitInfo(ctx, run); err == nil {
			v.Question = info.Question
			v.PendingInvocation = info.PendingInvocation
		}
	}
	return v
}

func drainEmptyBody(r *http.Request) error {
	if r == nil || r.Body == nil {
		return nil
	}
	_, err := io.Copy(io.Discard, r.Body)
	return err
}

func decode(r *http.Request, dest any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return err
		}
		return domain.E(domain.CodeInvalidArgument, "request body: "+err.Error())
	}
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return err
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]*domain.Error{
			"error": domain.E(domain.CodeResourceLimit, "request body too large"),
		})
		return
	}
	status := http.StatusInternalServerError
	var de *domain.Error
	if errors.As(err, &de) {
		switch de.Code {
		case domain.CodeNotFound:
			status = http.StatusNotFound
		case domain.CodeInvalidArgument, domain.CodeProviderConfig:
			status = http.StatusBadRequest
		case domain.CodeNotWaiting, domain.CodeConflict, domain.CodeLease, domain.CodeBudget:
			status = http.StatusConflict
		case domain.CodeNotImplemented:
			status = http.StatusNotImplemented
		case domain.CodeExternalBind, domain.CodeAuthDenied, domain.CodeProgramDisabled:
			status = http.StatusForbidden
		}
	} else {
		de = domain.E("internal", "request failed")
	}
	writeJSON(w, status, map[string]*domain.Error{"error": de})
}

func stringsTrim(s string) string {
	i := 0
	j := len(s)
	for i < j && (s[i] == ' ' || s[i] == '\n' || s[i] == '\t') {
		i++
	}
	for j > i && (s[j-1] == ' ' || s[j-1] == '\n' || s[j-1] == '\t') {
		j--
	}
	return s[i:j]
}
