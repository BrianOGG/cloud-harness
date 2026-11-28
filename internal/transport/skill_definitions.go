package transport

import (
	"errors"
	"io/fs"
	"net/http"

	contextpkg "github.com/BrianOGG/cloud-harness/internal/context"
	"github.com/BrianOGG/cloud-harness/internal/domain"
)

func (s *Server) skillDefinitions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.Manager == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]*domain.Error{"error": domain.E("unavailable", "技能管理暂不可用")})
		return
	}
	workspace := s.Manager.Workspace
	scope, name := r.PathValue("scope"), r.PathValue("name")
	var result any
	var err error
	status := http.StatusOK
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if scope == "" {
			result, err = contextpkg.ListScopedSkills(workspace)
		} else {
			result, err = contextpkg.OpenSkillDocument(workspace, scope, name)
		}
	case http.MethodPost:
		var body struct {
			Content string `json:"content"`
		}
		if err = decode(r, &body); err == nil {
			result, err = contextpkg.SaveSkillDocument(workspace, scope, "", body.Content, "")
			status = http.StatusCreated
		}
	case http.MethodPut:
		var body struct {
			Content  string `json:"content"`
			Revision string `json:"revision"`
		}
		if err = decode(r, &body); err == nil {
			result, err = contextpkg.SaveSkillDocument(workspace, scope, name, body.Content, body.Revision)
		}
	case http.MethodDelete:
		r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes())
		var body struct {
			Revision string `json:"revision"`
		}
		if err = decode(r, &body); err == nil {
			err = contextpkg.DeleteSkillDocument(workspace, scope, name, body.Revision)
			status = http.StatusNoContent
		}
	}
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			err = domain.E(domain.CodeNotFound, "技能文件或工作区不存在")
		}
		writeErr(w, err)
		return
	}
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	writeJSON(w, status, result)
}
