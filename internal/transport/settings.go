package transport

import (
	"errors"
	"net/http"
	"strings"

	"github.com/BrianOGG/cloud-harness/internal/config"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/modelconfig"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

type ModelOption struct {
	ProviderName  string `json:"provider_name,omitempty"`
	LegacyID      string `json:"legacy_id,omitempty"`
	Thinking      string `json:"thinking,omitempty"`
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
		writeJSON(w, http.StatusOK, map[string]string{"approval_mode": p.ApprovalMode, "model_id": p.ModelID, "reasoning_effort": p.ReasoningEffort})
		return
	}
	var body struct {
		ApprovalMode    string `json:"approval_mode"`
		ModelID         string `json:"model_id"`
		ReasoningEffort string `json:"reasoning_effort"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	if body.ApprovalMode != "ask" && body.ApprovalMode != "safe" && body.ApprovalMode != "workspace-write" && body.ApprovalMode != "full-auto" {
		writeErr(w, domain.E(domain.CodeInvalidArgument, "invalid approval mode"))
		return
	}
	if !provider.ValidReasoningEffort(body.ReasoningEffort) {
		writeErr(w, domain.E(domain.CodeInvalidArgument, "无效的思考强度"))
		return
	}
	if body.ReasoningEffort == "" {
		body.ReasoningEffort = "default"
	}
	if body.ModelID == "" {
		body.ModelID = "default"
	}
	if _, ok := s.modelOption(body.ModelID); !ok {
		writeErr(w, domain.E(domain.CodeInvalidArgument, "selected model is not configured"))
		return
	}
	p := storage.SessionPreferences{SessionID: id, ApprovalMode: body.ApprovalMode, ModelID: body.ModelID, ReasoningEffort: body.ReasoningEffort}
	if err := store.SaveSessionPreferences(r.Context(), p); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"approval_mode": p.ApprovalMode, "model_id": p.ModelID, "reasoning_effort": p.ReasoningEffort})
}

func (s *Server) configuredModels() []ModelOption {
	if len(s.Models) > 0 {
		items := append([]ModelOption(nil), s.Models...)
		for i := range items {
			if items[i].Thinking == "" {
				items[i].Thinking = modelconfig.ThinkingSupport(items[i].Provider, items[i].Model)
				if s.Manager != nil {
					items[i].Thinking = provider.ModelReasoningSupport(s.Manager.Provider, items[i].Model)
				}
			}
		}
		return items
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
			for _, model := range p.Models {
				items = append(items, customModelOption(p, model))
			}
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
		custom, model, err := s.ProviderSettings.ResolveModelID(id)
		if err == nil {
			option := customModelOption(customProviderInfo(custom), model)
			option.ID = id
			return option, true
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

func customProviderInfo(p config.CustomProvider) config.CustomProviderInfo {
	return config.CustomProviderInfo{ID: p.ID, Name: p.Name, Model: p.Model, Protocol: p.Protocol, Catalog: p.Catalog, BaseURL: p.BaseURL}
}
func customModelOption(p config.CustomProviderInfo, model string) ModelOption {
	declared := "unknown"
	for _, entry := range p.Catalog {
		if entry.ID == model {
			declared = entry.Thinking
			break
		}
	}
	option := ModelOption{ID: "custom:" + p.ID + "/" + model, Name: p.Name + " · " + model, Model: model, Provider: p.Protocol, ProviderID: p.ID, ProviderName: p.Name, Thinking: provider.ReasoningSupport(p.Protocol, "custom:"+p.ID, model, declared)}
	if model == p.Model {
		option.LegacyID = "custom:" + p.ID
	}
	return option
}

// detectCustomProvider never writes settings or sends an inference request.
func (s *Server) detectCustomProvider(w http.ResponseWriter, r *http.Request) {
	var input struct {
		BaseURL    string `json:"base_url"`
		APIKey     string `json:"api_key"`
		ProviderID string `json:"provider_id"`
		Protocol   string `json:"protocol"`
	}
	if err := decode(r, &input); err != nil {
		writeErr(w, err)
		return
	}
	base, err := modelconfig.NormalizeBaseURL(input.BaseURL)
	if err != nil {
		writeErr(w, domain.E(domain.CodeInvalidArgument, err.Error()))
		return
	}
	if input.Protocol != "" && !modelconfig.ValidProtocol(input.Protocol) {
		writeErr(w, domain.E(domain.CodeInvalidArgument, "无效的模型服务协议"))
		return
	}
	if input.APIKey == "" && input.ProviderID != "" {
		if s.ProviderSettings.Directory == "" {
			writeErr(w, domain.E("internal", "custom provider settings are unavailable"))
			return
		}
		p, err := s.ProviderSettings.Get(input.ProviderID)
		if err != nil {
			writeErr(w, err)
			return
		}
		input.APIKey = p.APIKey
	}
	result, err := provider.FetchModelCatalog(r.Context(), base, input.APIKey, input.Protocol)
	if err != nil {
		writeCatalogError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func writeCatalogError(w http.ResponseWriter, err error) {
	status := 0
	var catalogError *modelconfig.CatalogError
	if errors.As(err, &catalogError) {
		status = catalogError.UpstreamStatus
	}
	// Fixed text only: never forward upstream bodies, URLs or error messages.
	writeJSON(w, http.StatusBadGateway, map[string]any{"error": "模型列表获取失败；请检查地址、凭据和协议，也可手动填写模型 ID", "upstream_status": status})
}
func (s *Server) refreshCustomProviderModels(w http.ResponseWriter, r *http.Request) {
	if s.ProviderSettings.Directory == "" {
		writeErr(w, domain.E("internal", "custom provider settings are unavailable"))
		return
	}
	p, err := s.ProviderSettings.Get(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	result, err := provider.FetchModelCatalog(r.Context(), p.BaseURL, p.APIKey, p.Protocol)
	if err != nil {
		writeCatalogError(w, err)
		return
	}
	info, err := s.ProviderSettings.UpdateCatalog(p, result)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// Retain the existing read-only connection check for older clients.
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
	result, err := provider.FetchModelCatalog(r.Context(), p.BaseURL, p.APIKey, p.Protocol)
	if err != nil {
		writeCatalogError(w, err)
		return
	}
	found := false
	for _, model := range result.Models {
		if model.ID == p.Model {
			found = true
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "model_available": found})
}
