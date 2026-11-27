// Package transport is the HTTP API. It calls the run manager and does not
// touch tools except through that manager. The default bind is loopback.
package transport

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/agent"
	"github.com/BrianOGG/cloud-harness/internal/config"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/storage"
	"github.com/BrianOGG/cloud-harness/internal/trace"
)

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
	Models            []ModelOption
	ProviderSettings  config.CustomProviderStore
	ModelsDev         *provider.ModelsDev
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
		if store, ok := s.Manager.Store.(interface {
			OnEvent(func(runID string, seq int64))
		}); ok {
			store.OnEvent(s.hub.Committed)
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
	mux.HandleFunc("GET /v1/models", s.models)
	mux.HandleFunc("GET /v1/providers", s.customProviders)
	mux.HandleFunc("PUT /v1/providers", s.customProviders)
	mux.HandleFunc("DELETE /v1/providers/{id}", s.deleteCustomProvider)
	mux.HandleFunc("POST /v1/providers/{id}/test", s.testCustomProvider)
	mux.HandleFunc("POST /v1/providers/detect", s.detectCustomProvider)
	mux.HandleFunc("POST /v1/providers/{id}/refresh-models", s.refreshCustomProviderModels)
	mux.HandleFunc("GET /v1/sessions/{id}/settings", s.sessionSettings)
	mux.HandleFunc("PUT /v1/sessions/{id}/settings", s.sessionSettings)
	mux.HandleFunc("GET /approval", serveApprovalPage)
	mux.HandleFunc("GET /v1/sessions", s.listSessions)
	mux.HandleFunc("POST /v1/sessions", s.createSession)
	mux.HandleFunc("GET /v1/sessions/{id}/runs", s.listSessionRuns)
	mux.HandleFunc("GET /v1/sessions/{id}/preferences", s.sessionSettings)
	mux.HandleFunc("PUT /v1/sessions/{id}/preferences", s.sessionSettings)
	mux.HandleFunc("GET /v1/sessions/{id}/messages", s.listSessionMessages)
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

// Preserve old notification links as redirects into the conversation UI.
func serveApprovalPage(w http.ResponseWriter, r *http.Request) {
	target := "/ui"
	if runID := r.URL.Query().Get("run"); runID != "" {
		target += "?run=" + url.QueryEscape(runID)
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target, http.StatusSeeOther)
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
		Message         string `json:"message"`
		ModelID         string `json:"model_id"`
		ReasoningEffort string `json:"reasoning_effort"`
		ApprovalMode    string `json:"approval_mode"`
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
	settings := s.Manager.Store
	prefs, err := settings.GetSessionPreferences(r.Context(), sessionID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if body.ModelID == "" {
		if prefs.ModelID == "" {
			body.ModelID = "default"
		} else {
			body.ModelID = prefs.ModelID
		}
	}
	if body.ApprovalMode == "" {
		body.ApprovalMode = prefs.ApprovalMode
		if body.ApprovalMode == "" {
			body.ApprovalMode = "ask"
		}
	}
	if body.ApprovalMode != "ask" && body.ApprovalMode != "safe" && body.ApprovalMode != "workspace-write" && body.ApprovalMode != "full-auto" {
		writeErr(w, domain.E(domain.CodeInvalidArgument, "invalid approval mode"))
		return
	}
	if body.ReasoningEffort == "" {
		body.ReasoningEffort = prefs.ReasoningEffort
	}
	if body.ReasoningEffort == "" {
		body.ReasoningEffort = "default"
	}
	if !provider.ValidReasoningEffort(body.ReasoningEffort) {
		writeErr(w, domain.E(domain.CodeInvalidArgument, "无效的思考强度"))
		return
	}
	selected, ok := s.modelOption(body.ModelID)
	if !ok {
		writeErr(w, domain.E(domain.CodeInvalidArgument, "selected model is not configured"))
		return
	}
	if err := settings.SaveSessionPreferences(r.Context(), storage.SessionPreferences{SessionID: sessionID, ApprovalMode: body.ApprovalMode, ModelID: body.ModelID, ReasoningEffort: body.ReasoningEffort}); err != nil {
		writeErr(w, err)
		return
	}
	s.execute(w, r, http.StatusCreated, func(ctx context.Context) (*agent.Execution, error) {
		return s.Manager.StartMessage(ctx, sessionID, body.Message, agent.RunOptions{ModelID: body.ModelID, Model: selected.Model, FallbackModel: selected.FallbackModel, ProviderID: selected.ProviderID, ApprovalMode: body.ApprovalMode, ReasoningEffort: body.ReasoningEffort})
	})
}

// defaultWait is the behavior of a request that executes a run when it does
// not pass ?wait=: false answers as soon as the run is RUNNING, true answers
// once the run has stopped executing, which is what every request did before
// runs became asynchronous.
const defaultWait = false

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
	// An event stream stays open while the run executes. Tell it when that
	// is over, whether or not this request is still here.
	go func() {
		<-ex.Done()
		s.Notify(ex.Run.ID)
	}()
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
	live := &liveStream{store: s.Manager.Store, executing: s.Manager.Executing, w: w, sub: sub, runID: id, after: after}
	// A terminal run that is still executing has events to come. One that is
	// not may have ended since the replay above, with its last text and
	// events queued for this subscriber in the meantime.
	if domain.TerminalRun(run.Status) && !s.Manager.Executing(id) {
		if err := live.catchUp(r.Context()); err == nil {
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
			committed, unbounded, err := live.writeQueued(r.Context())
			if err == nil && unbounded {
				err = live.catchUp(r.Context())
			}
			if err != nil {
				return
			}
			flushSSE(w)
			if committed && live.ended(r.Context()) {
				return
			}
		case <-pollTicker.C:
			if err := live.catchUp(r.Context()); err != nil {
				return
			}
			flushSSE(w)
			if live.ended(r.Context()) {
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

// liveStream is one event-stream response after its replay: where it has
// got to in the run's stored events, and the hub queue that orders those
// events among the run's model text.
type liveStream struct {
	store storage.Store
	// executing reports whether this process is still executing the run.
	executing func(runID string) bool
	w         http.ResponseWriter
	sub       *trace.Subscription
	runID     string
	after     int64
}

func (l *liveStream) write(ev storage.Event) error {
	if err := trace.WriteSSE(l.w, ev); err != nil {
		return err
	}
	if ev.Seq > l.after {
		l.after = ev.Seq
	}
	return nil
}

// writeStored writes the stored events not yet sent, up to sequence number
// through.
func (l *liveStream) writeStored(ctx context.Context, through int64) error {
	evs, err := l.store.ListEvents(ctx, l.runID, l.after)
	if err != nil {
		return err
	}
	for _, ev := range evs {
		if ev.Seq > through {
			break
		}
		if err := l.write(ev); err != nil {
			return err
		}
	}
	return nil
}

// writeQueued writes what the hub has queued, in the order it happened. A
// commit releases the stored events up to its own sequence number and no
// further: a later event, such as the one that completes a model response,
// is written only after the text queued ahead of it. committed reports that
// the queue held a commit, and unbounded that one of them did not say how far
// the store has got, so the caller should catch up.
func (l *liveStream) writeQueued(ctx context.Context) (committed, unbounded bool, err error) {
	for _, item := range l.sub.Take() {
		switch item.Kind {
		case trace.ItemPersisted:
			committed = true
			if item.Seq == 0 {
				unbounded = true
			} else if item.Seq > l.after {
				err = l.writeStored(ctx, item.Seq)
			}
		case trace.ItemDelta:
			payload, _ := json.Marshal(map[string]string{"text": item.Text})
			err = trace.WriteSSE(l.w, storage.Event{Type: "stream.delta", Payload: payload})
		case trace.ItemReset:
			err = trace.WriteSSE(l.w, storage.Event{Type: "stream.reset"})
		}
		if err != nil {
			return committed, unbounded, err
		}
	}
	return committed, unbounded, nil
}

// catchUp writes every stored event not yet sent, including those written by
// another process, which the hub never hears of. The store is read before
// the queue is taken: the text of a response is queued before the event that
// completes it is committed, so the queue taken afterwards holds all the text
// that belongs ahead of the events that were read.
func (l *liveStream) catchUp(ctx context.Context) error {
	evs, err := l.store.ListEvents(ctx, l.runID, l.after)
	if err != nil {
		return err
	}
	if _, _, err := l.writeQueued(ctx); err != nil {
		return err
	}
	for _, ev := range evs {
		if ev.Seq <= l.after {
			continue
		}
		if err := l.write(ev); err != nil {
			return err
		}
	}
	return nil
}

// ended reports whether the stream is over: the run is terminal and this
// process has returned from executing it, or the stream can no longer be
// served. An execution appends events after it records the terminal status,
// so the status alone does not end the stream. Before ending it writes
// whatever is left.
func (l *liveStream) ended(ctx context.Context) bool {
	run, err := l.store.GetRun(ctx, l.runID)
	if err != nil {
		return true
	}
	if !domain.TerminalRun(run.Status) || (l.executing != nil && l.executing(l.runID)) {
		return false
	}
	if err := l.catchUp(ctx); err == nil {
		flushSSE(l.w)
	}
	return true
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
		Decision        string  `json:"decision"`
		Answer          string  `json:"answer"`
		ExtraModelCalls int     `json:"extra_model_calls"`
		ExtraTokens     int64   `json:"extra_tokens"`
		ExtraCostUSD    float64 `json:"extra_cost_usd"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := decode(r, &body); err != nil {
			writeErr(w, err)
			return
		}
	}
	s.resume(w, r, agent.ResumeInput{
		Decision: body.Decision, Answer: body.Answer, ExtraModelCalls: body.ExtraModelCalls,
		ExtraTokens: body.ExtraTokens, ExtraCostUSD: body.ExtraCostUSD,
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
		ExtraModelCalls int     `json:"extra_model_calls"`
		ExtraTokens     int64   `json:"extra_tokens"`
		ExtraCostUSD    float64 `json:"extra_cost_usd"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	s.resume(w, r, agent.ResumeInput{ExtraModelCalls: body.ExtraModelCalls, ExtraTokens: body.ExtraTokens, ExtraCostUSD: body.ExtraCostUSD})
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
	TurnID            string                   `json:"turn_id,omitempty"`
	ParentID          string                   `json:"parent_id,omitempty"`
	ApprovalMode      string                   `json:"approval_mode,omitempty"`
	ReasoningEffort   string                   `json:"reasoning_effort,omitempty"`
	Workspace         string                   `json:"workspace,omitempty"`
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
		ID: run.ID, SessionID: run.SessionID, TurnID: run.TurnID, ParentID: run.ParentRunID, Status: run.Status, WaitReason: run.WaitReason,
		WaitKind: run.WaitReason, Budget: run.Budget, FinalAnswer: run.FinalAnswer,
		Error: run.Error, Version: run.Version,
	}
	// Read only the public runtime choices from the persisted checkpoint. The
	// session preference is for a future turn and must never stand in for this
	// run's snapshot, including after a restart or while approval is pending.
	if s.Manager != nil && s.Manager.Store != nil {
		var snapshot struct {
			ApprovalMode    string `json:"approval_mode"`
			ReasoningEffort string `json:"reasoning_effort"`
			Policy          *struct {
				Workspace string `json:"workspace"`
			} `json:"policy"`
		}
		if raw, err := s.Manager.Store.LatestCheckpoint(ctx, run.ID); err == nil && json.Unmarshal(raw, &snapshot) == nil {
			switch snapshot.ApprovalMode {
			case "ask", "safe", "workspace-write", "full-auto":
				v.ApprovalMode = snapshot.ApprovalMode
				v.ReasoningEffort = snapshot.ReasoningEffort
			}
			if snapshot.Policy != nil {
				v.Workspace = snapshot.Policy.Workspace
			}
		}
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
