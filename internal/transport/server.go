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

	"github.com/BrianOGG/cloud-harness/internal/agent"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/storage"
	"github.com/BrianOGG/cloud-harness/internal/trace"
)

//go:embed approval.html
var approvalHTML []byte

const (
	// DefaultMaxBodyBytes caps POST and PUT request bodies at 1 MiB.
	DefaultMaxBodyBytes int64 = 1 << 20
	// DefaultHeartbeatInterval is the SSE ": ping" heartbeat interval.
	DefaultHeartbeatInterval = 15 * time.Second
	// DefaultPollInterval is how often an SSE subscription re-reads the store
	// without being notified. Events written by this process wake subscribers
	// at once; the poll only picks up writes made by another process.
	DefaultPollInterval = 2 * time.Second
	// DefaultReadHeaderTimeout is the HTTP server ReadHeaderTimeout.
	DefaultReadHeaderTimeout = 10 * time.Second
	// DefaultIdleTimeout is the HTTP server IdleTimeout.
	DefaultIdleTimeout = 120 * time.Second
	// DefaultReadTimeout bounds reading one request, including its body.
	DefaultReadTimeout = 30 * time.Second
	// DefaultWriteTimeout bounds handling one request and writing its response.
	// Event streams and requests that execute a run are exempt.
	DefaultWriteTimeout = 60 * time.Second
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
	Stats             http.HandlerFunc
	Doctor            http.HandlerFunc
	// LoopbackBind is set when the listener is bound to a loopback address.
	// The Host header must then name a loopback address or an AllowedHosts entry.
	LoopbackBind bool
	// AllowedHosts lists extra accepted Host values, as "host" or "host:port".
	// A non-empty list enables the Host check on any bind.
	AllowedHosts []string
	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	streamOnce sync.Once
	hub        *trace.Hub
}

// stream returns the hub that carries run activity to SSE subscribers. The
// first call connects it to the manager's model text and to the store's
// event commits.
func (s *Server) stream() *trace.Hub {
	s.streamOnce.Do(func() {
		s.hub = trace.NewHub()
		if s.Manager == nil {
			return
		}
		if s.Manager.Stream == nil {
			s.Manager.Stream = s.hub
		}
		if store, ok := s.Manager.Store.(interface{ OnEvent(func(runID string)) }); ok {
			store.OnEvent(s.hub.Notify)
		}
	})
	return s.hub
}

// NewHTTPServer builds an *http.Server with the default security timeouts.
func (s *Server) NewHTTPServer(addr string) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: DefaultReadHeaderTimeout,
		ReadTimeout:       s.readTimeout(),
		WriteTimeout:      s.writeTimeout(),
		IdleTimeout:       DefaultIdleTimeout,
	}
}

func (s *Server) readTimeout() time.Duration {
	if s.ReadTimeout > 0 {
		return s.ReadTimeout
	}
	return DefaultReadTimeout
}

func (s *Server) writeTimeout() time.Duration {
	if s.WriteTimeout > 0 {
		return s.WriteTimeout
	}
	return DefaultWriteTimeout
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

// Handler returns the HTTP routes wrapped with the Host and Origin checks,
// bearer authentication, and body type and size limits.
func (s *Server) Handler() http.Handler {
	s.stream()
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
	if s.Stats != nil {
		mux.HandleFunc("GET /v1/stats", s.Stats)
	}
	if s.Doctor != nil {
		mux.HandleFunc("GET /v1/doctor", s.Doctor)
	}
	mux.HandleFunc("GET /v1/runs/{id}", s.getRun)
	mux.HandleFunc("GET /v1/runs/{id}/events", s.events)
	mux.HandleFunc("GET /v1/runs/{id}/trace", s.traceRun)
	mux.HandleFunc("POST /v1/runs/{id}/responses", s.respond)
	mux.HandleFunc("POST /v1/runs/{id}/answer", s.answer)
	mux.HandleFunc("POST /v1/runs/{id}/budget", s.budget)
	mux.HandleFunc("POST /v1/runs/{id}/cancel", s.cancel)

	return s.guard(withUI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tok := s.token(); tok != "" && r.URL.Path != "/healthz" && r.URL.Path != "/approval" {
			if !checkBearerToken(r.Header.Get("Authorization"), tok) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="cloud-harness"`)
				writeJSON(w, http.StatusUnauthorized, map[string]*domain.Error{
					"error": domain.E(domain.CodeAuthDenied, "missing or invalid bearer token"),
				})
				return
			}
		}
		if !requireJSONBody(r) {
			writeJSON(w, http.StatusUnsupportedMediaType, map[string]*domain.Error{
				"error": domain.E(domain.CodeInvalidArgument, "request body requires Content-Type: application/json"),
			})
			return
		}
		if (r.Method == http.MethodPost || r.Method == http.MethodPut) && r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes())
		}
		mux.ServeHTTP(w, r)
	})))
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
	sessionID := r.PathValue("id")
	s.execute(w, r, http.StatusCreated, func(ctx context.Context) (*agent.Execution, error) {
		return s.Manager.StartMessage(ctx, sessionID, body.Message, agent.RunOptions{})
	})
}

// defaultWait is the behavior of a request that executes a run when it does
// not pass ?wait=: true answers once the run has stopped executing, false
// answers as soon as the run is RUNNING.
const defaultWait = true

func waitRequested(r *http.Request) (bool, error) {
	raw := r.URL.Query().Get("wait")
	if raw == "" {
		return defaultWait, nil
	}
	switch raw {
	case "1", "true":
		return true, nil
	case "0", "false":
		return false, nil
	}
	return false, domain.E(domain.CodeInvalidArgument, "wait must be 0 or 1")
}

// execute starts a run in the background and answers with its view. The run
// does not belong to the request: a client that disconnects leaves it running,
// and only POST /v1/runs/{id}/cancel stops it. With wait the answer is held
// until the run has stopped executing.
func (s *Server) execute(w http.ResponseWriter, r *http.Request, status int, start func(context.Context) (*agent.Execution, error)) {
	wait, err := waitRequested(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	rearm := func() {}
	if wait {
		rearm = s.holdOpen(w)
	}
	ex, err := start(r.Context())
	if err != nil {
		rearm()
		writeErr(w, err)
		return
	}
	run := ex.Run
	if wait {
		run, err = ex.Wait(r.Context())
		if r.Context().Err() != nil {
			return
		}
		rearm()
		if err != nil {
			writeErr(w, err)
			return
		}
	}
	s.Notify(run.ID)
	writeJSON(w, status, s.runView(r.Context(), run))
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
	w = &streamWriter{ResponseWriter: w, rearm: s.holdOpen(w)}
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
	sub := s.stream().Subscribe(id)
	defer sub.Close()

	events, err := s.Manager.Store.ListEvents(r.Context(), id, after)
	if err != nil {
		writeErr(w, err)
		return
	}
	sub.Replayed()
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
		case <-sub.Wake():
			for _, item := range sub.Take() {
				switch item.Kind {
				case trace.ItemPersisted:
					if s.drainLiveEvents(r.Context(), w, id, &after) {
						return
					}
				case trace.ItemDelta:
					payload, _ := json.Marshal(map[string]string{"text": item.Text})
					if err := trace.WriteSSE(w, storage.Event{Type: "stream.delta", Payload: payload}); err != nil {
						return
					}
				case trace.ItemReset:
					if err := trace.WriteSSE(w, storage.Event{Type: "stream.reset"}); err != nil {
						return
					}
				}
			}
			flushSSE(w)
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

// Notify wakes active SSE subscribers for runID so they check the event store.
func (s *Server) Notify(runID string) { s.stream().Notify(runID) }

// SubscriberCount reports the number of active SSE subscribers for runID.
func (s *Server) SubscriberCount(runID string) int { return s.stream().Subscribers(runID) }

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
	s.resume(w, r, agent.ResumeInput{
		Decision: body.Decision, Answer: body.Answer, ExtraModelCalls: body.ExtraModelCalls,
	})
}

func (s *Server) resume(w http.ResponseWriter, r *http.Request, in agent.ResumeInput) {
	id := r.PathValue("id")
	s.execute(w, r, http.StatusOK, func(ctx context.Context) (*agent.Execution, error) {
		return s.Manager.StartResume(ctx, id, in)
	})
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
	s.resume(w, r, agent.ResumeInput{Answer: ans})
}

func (s *Server) budget(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ExtraModelCalls int `json:"extra_model_calls"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	s.resume(w, r, agent.ResumeInput{ExtraModelCalls: body.ExtraModelCalls})
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	if err := drainEmptyBody(r); err != nil {
		writeErr(w, err)
		return
	}
	id := r.PathValue("id")
	rearm := s.holdOpen(w)
	run, err := s.Manager.Cancel(r.Context(), id)
	rearm()
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
	ParentID          string                   `json:"parent_id,omitempty"`
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
		ID: run.ID, SessionID: run.SessionID, ParentID: run.ParentRunID, Status: run.Status, WaitReason: run.WaitReason,
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
