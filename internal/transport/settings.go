package transport

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/config"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

type ModelOption struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Model         string `json:"model"`
	FallbackModel string `json:"fallback_model,omitempty"`
	Provider      string `json:"provider"`
	ProviderID    string `json:"provider_id,omitempty"`
}

func (s *Server) sessionSettings(w http.ResponseWriter, r *http.Request) {
	store := s.Manager.Store
	id := r.PathValue("id")
	if _, err := store.GetSession(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	if r.Method == http.MethodGet {
		p, err := store.GetSessionPreferences(r.Context(), id)
		if err != nil {
			writeErr(w, err)
			return
		}
		if p.ModelID == "" {
			p.ModelID = "default"
		}
		writeJSON(w, http.StatusOK, map[string]string{"approval_mode": p.ApprovalMode, "model_id": p.ModelID})
		return
	}
	var body struct {
		ApprovalMode string `json:"approval_mode"`
		ModelID      string `json:"model_id"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	if body.ApprovalMode != "ask" && body.ApprovalMode != "safe" && body.ApprovalMode != "workspace-write" && body.ApprovalMode != "full-auto" {
		writeErr(w, domain.E(domain.CodeInvalidArgument, "invalid approval mode"))
		return
	}
	if body.ModelID == "" {
		body.ModelID = "default"
	}
	if _, ok := s.modelOption(body.ModelID); !ok {
		writeErr(w, domain.E(domain.CodeInvalidArgument, "selected model is not configured"))
		return
	}
	p := storage.SessionPreferences{SessionID: id, ApprovalMode: body.ApprovalMode, ModelID: body.ModelID}
	if err := store.SaveSessionPreferences(r.Context(), p); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"approval_mode": p.ApprovalMode, "model_id": p.ModelID})
}

func (s *Server) configuredModels() []ModelOption {
	if len(s.Models) > 0 {
		return append([]ModelOption(nil), s.Models...)
	}
	return []ModelOption{{ID: "default", Name: "默认模型"}}
}

func (s *Server) models(w http.ResponseWriter, _ *http.Request) {
	items := s.configuredModels()
	if s.ProviderSettings.Directory != "" {
		providers, err := s.ProviderSettings.List()
		if err != nil {
			writeErr(w, domain.E("internal", "provider settings could not be read"))
			return
		}
		for _, p := range providers {
			items = append(items, ModelOption{ID: "custom:" + p.ID, Name: p.Name + " · " + p.Model, Model: p.Model, Provider: "openai", ProviderID: p.ID})
		}
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) modelOption(id string) (ModelOption, bool) {
	for _, item := range s.configuredModels() {
		if item.ID == id {
			return item, true
		}
	}
	if strings.HasPrefix(id, "custom:") && s.ProviderSettings.Directory != "" {
		custom, err := s.ProviderSettings.Get(strings.TrimPrefix(id, "custom:"))
		if err == nil {
			return ModelOption{ID: id, Name: custom.Name + " · " + custom.Model, Model: custom.Model, Provider: "openai", ProviderID: custom.ID}, true
		}
	}
	return ModelOption{}, false
}

func (s *Server) customProviders(w http.ResponseWriter, r *http.Request) {
	if s.ProviderSettings.Directory == "" {
		writeErr(w, domain.E("internal", "custom provider settings are unavailable"))
		return
	}
	if r.Method == http.MethodGet {
		rows, err := s.ProviderSettings.List()
		if err != nil {
			writeErr(w, domain.E("internal", "provider settings could not be read"))
			return
		}
		writeJSON(w, http.StatusOK, rows)
		return
	}
	var input config.CustomProvider
	if err := decode(r, &input); err != nil {
		writeErr(w, err)
		return
	}
	result, err := s.ProviderSettings.Save(input)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) deleteCustomProvider(w http.ResponseWriter, r *http.Request) {
	if s.ProviderSettings.Directory == "" {
		writeErr(w, domain.E("internal", "custom provider settings are unavailable"))
		return
	}
	if err := s.ProviderSettings.Delete(r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Test the saved connection with a read-only model catalog request. Never run
// inference or forward an upstream body/error that may contain credentials.
func (s *Server) testCustomProvider(w http.ResponseWriter, r *http.Request) {
	if s.ProviderSettings.Directory == "" {
		writeErr(w, domain.E("internal", "custom provider settings are unavailable"))
		return
	}
	p, err := s.ProviderSettings.Get(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, p.BaseURL+"/models", nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "连接检测失败，请检查服务地址"})
		return
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "连接检测失败，请检查服务地址、网络与凭据"})
		return
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "模型列表请求失败，请检查服务地址、凭据或服务是否支持 /models", "upstream_status": response.StatusCode})
		return
	}
	var catalog struct {
		Data *[]struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	const maxCatalogBytes = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxCatalogBytes+1))
	if err != nil || len(raw) > maxCatalogBytes || json.Unmarshal(raw, &catalog) != nil || catalog.Data == nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "服务未返回有效的模型列表；可继续使用手动模型 ID"})
		return
	}
	found := false
	for _, model := range *catalog.Data {
		if model.ID == p.Model {
			found = true
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "model_available": found})
}
