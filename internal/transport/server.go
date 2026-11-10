// Package transport is the HTTP API. It calls the run manager and does not
// touch tools except through that manager. The default bind is loopback.
package transport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/agent"
	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/storage"
	"github.com/cloud-harness/cloud-harness/internal/trace"
)

// Server serves the HTTP API. The default bind is loopback.
type Server struct {
	Manager *agent.Manager
	Now     func() time.Time
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("POST /v1/sessions", s.createSession)
	mux.HandleFunc("POST /v1/sessions/{id}/runs", s.createRun)
	mux.HandleFunc("GET /v1/runs/{id}", s.getRun)
	mux.HandleFunc("GET /v1/runs/{id}/events", s.events)
	mux.HandleFunc("GET /v1/runs/{id}/trace", s.traceRun)
	mux.HandleFunc("POST /v1/runs/{id}/responses", s.respond)
	mux.HandleFunc("POST /v1/runs/{id}/budget", s.budget)
	mux.HandleFunc("POST /v1/runs/{id}/cancel", s.cancel)
	return mux
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	name := ""
	if s.Manager != nil && s.Manager.Provider != nil {
		name = s.Manager.Provider.Name()
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "provider": name})
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	ses, err := s.Manager.CreateSession(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": ses.ID})
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
	writeJSON(w, http.StatusCreated, runView(run))
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.Manager.Store.GetRun(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runView(run))
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Manager.Store.GetRun(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	after := int64(0)
	if h := r.Header.Get("Last-Event-ID"); h != "" {
		n, err := strconv.ParseInt(h, 10, 64)
		if err != nil {
			writeErr(w, domain.E(domain.CodeInvalidArgument, "Last-Event-ID must be an integer"))
			return
		}
		after = n
	}
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
	}
	// A stream delta is not stored. This one is only emitted when the caller
	// asks, so tests can see that it has no id and never lands in the log.
	if r.URL.Query().Get("preview") == "1" {
		_ = trace.WriteSSE(w, storage.Event{Type: "stream.delta", Payload: []byte(`{"text":"preview"}`), Persistent: false})
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
	run, err := s.Manager.Resume(r.Context(), r.PathValue("id"), agent.ResumeInput{
		Decision: body.Decision, Answer: body.Answer, ExtraModelCalls: body.ExtraModelCalls,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runView(run))
}

func (s *Server) budget(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ExtraModelCalls int `json:"extra_model_calls"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	run, err := s.Manager.Resume(r.Context(), r.PathValue("id"), agent.ResumeInput{ExtraModelCalls: body.ExtraModelCalls})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runView(run))
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	run, err := s.Manager.Cancel(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runView(run))
}

type view struct {
	ID          string            `json:"id"`
	SessionID   string            `json:"session_id"`
	Status      domain.RunStatus  `json:"status"`
	WaitReason  domain.WaitReason `json:"wait_reason,omitempty"`
	Budget      domain.Budget     `json:"budget"`
	FinalAnswer string            `json:"final_answer,omitempty"`
	Error       *domain.Error     `json:"error,omitempty"`
	Version     int64             `json:"version"`
}

func runView(run storage.Run) view {
	return view{
		ID: run.ID, SessionID: run.SessionID, Status: run.Status, WaitReason: run.WaitReason,
		Budget: run.Budget, FinalAnswer: run.FinalAnswer, Error: run.Error, Version: run.Version,
	}
}

func decode(r *http.Request, dest any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		return domain.E(domain.CodeInvalidArgument, "request body: "+err.Error())
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
