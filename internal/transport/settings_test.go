package transport

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/config"
	"github.com/BrianOGG/cloud-harness/internal/provider"
)

func TestSettingsAPIPersistenceModelsAndCustomRun(t *testing.T) {
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	const fakeKey = "fixture-only-custom-key"
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		if r.Header.Get("Authorization") != "Bearer "+fakeKey {
			t.Error("custom provider did not receive stored fixture credential")
		}
		var body struct{ Model string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "custom-model" {
			t.Errorf("model=%q err=%v", body.Model, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{\"id\":\"fixture\",\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":\"custom reply\"}}]}")
	}))
	defer upstream.Close()
	m, _ := newServer(t)
	m.SkipPlanner = true
	m.DisableSnapshots = true
	settings := config.CustomProviderStore{Directory: t.TempDir()}
	m.ProviderResolver = func(id string) (provider.Provider, error) {
		p, err := settings.Get(id)
		if err != nil {
			return nil, err
		}
		return &provider.OpenAICompatible{BaseURL: p.BaseURL, Model: p.Model, APIKey: p.APIKey, Client: upstream.Client()}, nil
	}
	h := (&Server{Manager: m, Token: approvalTestToken, ProviderSettings: settings, Models: []ModelOption{{ID: "default", Name: "Default", Model: "default-model"}, {ID: "profile:fast", Name: "Fast", Model: "fast-model"}}}).Handler()
	request := func(method, path string, value any, want int) *httptest.ResponseRecorder {
		t.Helper()
		var body io.Reader
		if value != nil {
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			body = strings.NewReader(string(raw))
		}
		req := httptest.NewRequest(method, path, body)
		req.Header.Set("Authorization", "Bearer "+approvalTestToken)
		if value != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s %s status=%d want=%d", method, path, rec.Code, want)
		}
		if strings.Contains(rec.Body.String(), fakeKey) || strings.Contains(rec.Body.String(), "\"api_key\":") {
			t.Fatal("API echoed full key")
		}
		return rec
	}
	session, err := m.CreateSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/sessions/" + session.ID + "/preferences"
	rec := request("GET", path, nil, 200)
	if !strings.Contains(rec.Body.String(), "\"model_id\":\"default\"") || !strings.Contains(rec.Body.String(), "\"approval_mode\":\"ask\"") {
		t.Fatal("default preferences missing")
	}
	request("PUT", path, map[string]string{"approval_mode": "safe", "model_id": "profile:fast"}, 200)
	prefs, err := m.Store.GetSessionPreferences(t.Context(), session.ID)
	if err != nil || prefs.ModelID != "profile:fast" || prefs.ApprovalMode != "safe" {
		t.Fatal("preferences were not persisted")
	}
	request("PUT", path, map[string]string{"approval_mode": "unsafe", "model_id": "default"}, 400)
	request("PUT", path, map[string]string{"approval_mode": "safe", "model_id": "not-configured"}, 400)
	request("PUT", "/v1/sessions/missing/preferences", map[string]string{"approval_mode": "safe", "model_id": "default"}, 404)
	request("GET", "/v1/sessions/missing/preferences", nil, 404)
	value := map[string]string{"id": "fixture", "name": "Fixture", "model": "custom-model", "base_url": upstream.URL, "api_key": fakeKey}
	request("PUT", "/v1/providers", value, 200)
	delete(value, "api_key")
	value["name"] = "Renamed"
	request("PUT", "/v1/providers", value, 200)
	rec = request("GET", "/v1/providers", nil, 200)
	if !strings.Contains(rec.Body.String(), "\"has_api_key\":true") {
		t.Fatal("public key-presence flag missing")
	}
	rec = request("GET", "/v1/models", nil, 200)
	var options []ModelOption
	if err := json.Unmarshal(rec.Body.Bytes(), &options); err != nil || len(options) != 3 || options[2].ID != "custom:fixture" {
		t.Fatal("default/profile/custom catalog mismatch")
	}
	// Explicit selection applies to this run and becomes the next run's default.
	request("POST", "/v1/sessions/"+session.ID+"/runs?wait=1", map[string]string{"message": "hello", "model_id": "custom:fixture", "approval_mode": "workspace-write"}, 201)
	request("POST", "/v1/sessions/"+session.ID+"/runs?wait=1", map[string]string{"message": "again"}, 201)
	if upstreamCalls != 2 {
		t.Fatalf("custom upstream calls=%d", upstreamCalls)
	}
	prefs, err = m.Store.GetSessionPreferences(t.Context(), session.ID)
	if err != nil || prefs.ModelID != "custom:fixture" || prefs.ApprovalMode != "workspace-write" {
		t.Fatal("createRun preference selection was not saved")
	}
	request("POST", "/v1/sessions/"+session.ID+"/runs?wait=1", map[string]string{"message": "bad", "model_id": "missing"}, 400)
}

func TestSettingsEndpointsRemainGuarded(t *testing.T) {
	h := (&Server{Token: approvalTestToken}).Handler()
	for _, path := range []string{"/v1/models", "/v1/providers", "/v1/sessions/fixture/preferences"} {
		for _, method := range []string{"GET", "PUT"} {
			req := httptest.NewRequest(method, path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != 401 {
				t.Errorf("%s %s escaped token check: %d", method, path, rec.Code)
			}
		}
	}
}
