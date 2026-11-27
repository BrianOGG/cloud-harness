package transport

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	rec = request("PUT", path, map[string]string{"approval_mode": "full-auto", "model_id": "profile:fast"}, 200)
	if !strings.Contains(rec.Body.String(), `"approval_mode":"full-auto"`) {
		t.Fatal("full-auto preference response missing")
	}
	rec = request("GET", path, nil, 200)
	if !strings.Contains(rec.Body.String(), `"approval_mode":"full-auto"`) {
		t.Fatal("full-auto preference did not round-trip")
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
	for _, mode := range []string{"unsafe", "unknown", "FULL-AUTO"} {
		request("PUT", path, map[string]string{"approval_mode": mode, "model_id": "default"}, 400)
		request("POST", "/v1/sessions/"+session.ID+"/runs?wait=1", map[string]string{"message": "bad mode", "approval_mode": mode}, 400)
	}
	// Both explicit createRun selection and inherited preferences must accept
	// full-auto and expose the checkpoint value in the run view.
	for _, body := range []map[string]string{
		{"message": "full-auto", "approval_mode": "full-auto"},
		{"message": "inherit full-auto"},
	} {
		rec = request("POST", "/v1/sessions/"+session.ID+"/runs?wait=1", body, 201)
		var run view
		if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil || run.ApprovalMode != "full-auto" {
			t.Fatalf("full-auto run snapshot=%q err=%v", run.ApprovalMode, err)
		}
	}
	prefs, err = m.Store.GetSessionPreferences(t.Context(), session.ID)
	if err != nil || prefs.ModelID != "custom:fixture" || prefs.ApprovalMode != "full-auto" || upstreamCalls != 4 {
		t.Fatalf("full-auto run preferences=%+v calls=%d err=%v", prefs, upstreamCalls, err)
	}
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

func TestProviderDeleteKeepsPreferencesAndRemovesModel(t *testing.T) {
	m, _ := newServer(t)
	settings := config.CustomProviderStore{Directory: t.TempDir()}
	h := (&Server{Manager: m, Token: approvalTestToken, ProviderSettings: settings}).Handler()
	request := func(method, path, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+approvalTestToken)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s status=%d want=%d", method, rec.Code, want)
		}
		return rec
	}
	session, err := m.CreateSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	p := config.CustomProvider{ID: "fixture", Name: "Fixture", BaseURL: "https://example.invalid/v1", Model: "fixture", APIKey: "fixture-only-delete"}
	raw, _ := json.Marshal(p)
	request("PUT", "/v1/providers", string(raw), 200)
	prefs := map[string]string{"model_id": "custom:fixture", "approval_mode": "safe"}
	raw, _ = json.Marshal(prefs)
	request("PUT", "/v1/sessions/"+session.ID+"/preferences", string(raw), 200)
	request("DELETE", "/v1/providers/fixture", "", 204)
	request("DELETE", "/v1/providers/fixture", "", 404)
	request("POST", "/v1/providers/fixture/test", "", 404)
	if strings.Contains(request("GET", "/v1/models", "", 200).Body.String(), "custom:fixture") {
		t.Fatal("deleted model remains selectable")
	}
	saved, err := m.Store.GetSessionPreferences(t.Context(), session.ID)
	if err != nil || saved.ModelID != "custom:fixture" || saved.ApprovalMode != "safe" {
		t.Fatal("delete changed session preferences")
	}
	raw, _ = json.Marshal(map[string]string{"message": "fixture", "model_id": "custom:fixture", "approval_mode": "safe"})
	request("POST", "/v1/sessions/"+session.ID+"/runs?wait=0", string(raw), 400)
	for _, item := range []struct{ method, path string }{{"DELETE", "/v1/providers/fixture"}, {"POST", "/v1/providers/fixture/test"}} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(item.method, item.path, nil))
		if rec.Code != 401 {
			t.Fatal("provider operation escaped bearer protection")
		}
	}
}

func TestProviderConnectionCheckIsReadOnlyBoundedAndRedacted(t *testing.T) {
	const fakeKey = "fixture-only-connection-check"
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { redirected.Add(1); w.WriteHeader(200) }))
	defer target.Close()
	for _, scenario := range []string{"found", "missing", "unauthorized", "redirect", "invalid", "oversize", "empty"} {
		t.Run(scenario, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/v1/models" {
					t.Error("check must only GET models")
				}
				if r.Header.Get("Authorization") != "Bearer "+fakeKey {
					t.Error("saved fixture key not used")
				}
				switch scenario {
				case "unauthorized":
					w.WriteHeader(401)
					_, _ = io.WriteString(w, fakeKey)
				case "redirect":
					http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
				case "invalid":
					_, _ = io.WriteString(w, fakeKey)
				case "oversize":
					_, _ = io.WriteString(w, strings.Repeat(" ", (1<<20)+1))
				default:
					ids := []map[string]string{}
					if scenario == "found" {
						ids = append(ids, map[string]string{"id": "fixture"})
					}
					if scenario == "missing" {
						ids = append(ids, map[string]string{"id": "other"})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": ids})
				}
			}))
			defer upstream.Close()
			settings := config.CustomProviderStore{Directory: t.TempDir()}
			if _, err := settings.Save(config.CustomProvider{ID: "fixture", Name: "Fixture", BaseURL: upstream.URL + "/v1", Model: "fixture", APIKey: fakeKey}); err != nil {
				t.Fatal(err)
			}
			h := (&Server{Token: approvalTestToken, ProviderSettings: settings}).Handler()
			req := httptest.NewRequest("POST", "/v1/providers/fixture/test", nil)
			req.Header.Set("Authorization", "Bearer "+approvalTestToken)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			want := 502
			if scenario == "found" || scenario == "missing" || scenario == "empty" {
				want = 200
			}
			if rec.Code != want {
				t.Fatalf("status=%d want=%d", rec.Code, want)
			}
			if strings.Contains(rec.Body.String(), fakeKey) {
				t.Fatal("upstream credential leaked")
			}
			if want == 200 {
				var result map[string]bool
				if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || !result["ok"] || result["model_available"] != (scenario == "found") {
					t.Fatal("wrong connection result")
				}
			}
		})
	}
	if redirected.Load() != 0 {
		t.Fatal("connection test followed a redirect")
	}
}
