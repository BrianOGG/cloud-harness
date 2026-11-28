package transport

import (
	"net/http"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

func (s *Server) projectMemories(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	projectID, err := s.Manager.MemoryProjectID(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	store := s.Manager.Store
	switch r.Method {
	case http.MethodGet:
		items, err := store.ListProjectMemories(ctx, projectID, 0)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, items)
	case http.MethodPost:
		var body struct {
			Kind      string `json:"kind"`
			Content   string `json:"content"`
			SessionID string `json:"source_session_id"`
			RunID     string `json:"source_run_id"`
		}
		if err := decode(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		if body.SessionID == "" || body.RunID == "" {
			writeErr(w, domain.E(domain.CodeInvalidArgument, "请选择来源对话和运行"))
			return
		}
		item, err := s.Manager.ConfirmProjectMemory(ctx, projectID, body.SessionID, body.RunID, body.Kind, body.Content)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, item)
	case http.MethodPut:
		var body struct {
			Kind    string `json:"kind"`
			Content string `json:"content"`
		}
		if err := decode(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		if err := store.UpdateProjectMemory(ctx, projectID, r.PathValue("id"), body.Kind, body.Content); err != nil {
			writeErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		if err := store.DeleteProjectMemory(ctx, projectID, r.PathValue("id")); err != nil {
			writeErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
