package transport

import (
	"bytes"
	"encoding/json"
	"github.com/BrianOGG/cloud-harness/internal/agent"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/config"
	"github.com/BrianOGG/cloud-harness/internal/modelconfig"
	"github.com/BrianOGG/cloud-harness/internal/provider"
)

func modelConfigRequest(t *testing.T, h http.Handler, method, path string, value any, want int, key string) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if value != nil {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(raw)
	}
	r := httptest.NewRequest(method, path, body)
	r.Header.Set("Authorization", "Bearer "+approvalTestToken)
	if value != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("%s %s status=%d want=%d response=%s", method, path, w.Code, want, provider.Redact(w.Body.String(), key))
	}
	if key != "" && strings.Contains(w.Body.String(), key) || strings.Contains(w.Body.String(), `"api_key":`) {
		t.Fatal("response exposed provider key")
	}
	return w
}

func TestProviderDetectAndRefreshCachePrivateAndReadOnly(t *testing.T) {
	const key = "api-model-catalog-fixture"
	var failure atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/models" || r.URL.RawQuery != "" {
			t.Error("unexpected discovery side effect")
		}
		if failure.Load() {
			w.WriteHeader(401)
			_, _ = io.WriteString(w, key)
			return
		}
		if r.Header.Get("x-api-key") == key {
			if r.Header.Get("anthropic-version") != "2023-06-01" {
				t.Error("missing API version")
			}
			_, _ = io.WriteString(w, `{"data":[{"id":"sonnet","type":"model","display_name":"Sonnet"}],"has_more":false}`)
			return
		}
		w.WriteHeader(401)
		_, _ = io.WriteString(w, key)
	}))
	defer upstream.Close()
	settings := config.CustomProviderStore{Directory: t.TempDir()}
	p := config.CustomProvider{ID: "saved", Name: "Saved", BaseURL: upstream.URL + "/v1", Model: "sonnet", APIKey: key, Protocol: "anthropic", Models: []string{"another"}}
	if _, err := settings.Save(p); err != nil {
		t.Fatal(err)
	}
	h := (&Server{Token: approvalTestToken, ProviderSettings: settings}).Handler()
	var logs bytes.Buffer
	oldLog := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldLog)
	file := filepath.Join(settings.Directory, "compatible-providers.json")
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	rec := modelConfigRequest(t, h, "POST", "/v1/providers/detect", map[string]string{"base_url": p.BaseURL, "provider_id": "saved"}, 200, key)
	var result modelconfig.Detection
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || result.Protocol != "anthropic" || len(result.Models) != 1 {
		t.Fatal("wrong detected catalog")
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(before, after) {
		t.Fatal("detection wrote saved settings")
	}
	rec = modelConfigRequest(t, h, "POST", "/v1/providers/saved/refresh-models", nil, 200, key)
	var info config.CustomProviderInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil || len(info.Catalog) != 1 || info.CatalogFetchedAt == nil || !reflect.DeepEqual(info.Models, []string{"sonnet", "another"}) {
		t.Fatal("refresh did not preserve enabled models")
	}
	cached, _ := os.ReadFile(file)
	failure.Store(true)
	rec = modelConfigRequest(t, h, "POST", "/v1/providers/saved/refresh-models", nil, 502, key)
	if !strings.Contains(rec.Body.String(), `"upstream_status":401`) {
		t.Fatal("upstream status missing")
	}
	after, _ = os.ReadFile(file)
	if !bytes.Equal(cached, after) {
		t.Fatal("failed refresh cleared previous cache")
	}
	for _, url := range []string{"http://example.invalid", "https://user:secret@example.invalid", "https://example.invalid?key=secret", "https://example.invalid#fragment"} {
		modelConfigRequest(t, h, "POST", "/v1/providers/detect", map[string]string{"base_url": url, "api_key": key}, 400, key)
	}
	modelConfigRequest(t, h, "POST", "/v1/providers/detect", map[string]string{"base_url": p.BaseURL, "protocol": "invalid", "api_key": key}, 400, key)
	if strings.Contains(logs.String(), key) {
		t.Fatal("provider key appeared in logs")
	}
	for _, path := range []string{"/v1/providers/detect", "/v1/providers/saved/refresh-models"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", path, nil))
		if w.Code != 401 {
			t.Fatal("discovery escaped authentication")
		}
	}
}

func TestModelsNewAndLegacySelectionsAndReasoningSettings(t *testing.T) {
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	const key = "settings-reasoning-fixture"
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["reasoning_effort"] != "high" {
			t.Error("saved reasoning effort not forwarded")
		}
		if body["model"] != "env-model" && body["model"] != "one" && body["model"] != "two/variant" {
			t.Error("wrong selected model")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer upstream.Close()
	m, _ := newServer(t)
	m.SkipPlanner = true
	m.DisableSnapshots = true
	m.ModelRouting = &config.ModelRouting{}
	m.Provider = &provider.OpenAICompatible{BaseURL: upstream.URL, APIKey: key, Model: "env-model"}
	settings := config.CustomProviderStore{Directory: t.TempDir()}
	_, err := settings.Save(config.CustomProvider{ID: "multi", Name: "Multiple", BaseURL: upstream.URL, Model: "one", Models: []string{"two/variant"}, APIKey: key, Protocol: "openai", Catalog: []modelconfig.Model{{ID: "one", Thinking: "unknown"}, {ID: "two/variant", Thinking: "unknown"}}})
	if err != nil {
		t.Fatal(err)
	}
	m.ProviderResolver = func(id string) (provider.Provider, error) {
		p, err := settings.Get(id)
		return &provider.OpenAICompatible{BaseURL: p.BaseURL, APIKey: p.APIKey, Model: p.Model}, err
	}
	s := &Server{Manager: m, Token: approvalTestToken, ProviderSettings: settings, Models: []ModelOption{{ID: "default", Name: "Env", Model: "env-model", Provider: "openai"}}}
	h := s.Handler()
	rec := modelConfigRequest(t, h, "GET", "/v1/models", nil, 200, key)
	var models []ModelOption
	if err := json.Unmarshal(rec.Body.Bytes(), &models); err != nil || len(models) != 3 || models[1].ID != "custom:multi/one" || models[1].LegacyID != "custom:multi" || models[2].ID != "custom:multi/two/variant" || models[2].Provider != "openai" {
		t.Fatal("enabled model options incorrect")
	}
	session, err := m.CreateSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/sessions/" + session.ID + "/settings"
	rec = modelConfigRequest(t, h, "GET", path, nil, 200, key)
	if !strings.Contains(rec.Body.String(), `"reasoning_effort":"default"`) {
		t.Fatal("missing default reasoning effort")
	}
	for _, effort := range []string{"default", "off", "low", "medium", "high"} {
		rec = modelConfigRequest(t, h, "PUT", path, map[string]string{"model_id": "default", "approval_mode": "ask", "reasoning_effort": effort}, 200, key)
		if !strings.Contains(rec.Body.String(), `"reasoning_effort":"`+effort+`"`) {
			t.Fatal("reasoning settings did not round trip")
		}
	}
	modelConfigRequest(t, h, "PUT", path, map[string]string{"approval_mode": "ask", "model_id": "default", "reasoning_effort": "invalid"}, 400, key)
	before := modelConfigRequest(t, h, "GET", path, nil, 200, key).Body.String()
	modelConfigRequest(t, h, "POST", "/v1/sessions/"+session.ID+"/runs?wait=invalid", map[string]string{"message": "hello", "reasoning_effort": "low", "model_id": "custom:multi"}, 400, key)
	if after := modelConfigRequest(t, h, "GET", path, nil, 200, key).Body.String(); before != after {
		t.Fatal("rejected request changed session preferences")
	}
	for _, id := range []string{"default", "custom:multi", "custom:multi/two/variant"} {
		rec = modelConfigRequest(t, h, "POST", "/v1/sessions/"+session.ID+"/runs?wait=1", map[string]string{"message": "hello", "model_id": id}, 201, key)
		var run view
		if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil || run.ReasoningEffort != "high" {
			t.Fatal("run snapshot missing reasoning effort")
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("model calls=%d", calls.Load())
	}
	modelConfigRequest(t, h, "POST", "/v1/sessions/"+session.ID+"/runs", map[string]string{"message": "hello", "reasoning_effort": "invalid"}, 400, key)
	for _, id := range []string{"custom:multi/disabled", "custom:missing"} {
		if _, ok := s.modelOption(id); ok {
			t.Fatal("accepted unavailable model")
		}
	}
}

func TestDefaultReasoningFallbackTraceAndMenu(t *testing.T) {
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	const key = "default-reasoning-event-fixture"
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, present := body["reasoning_effort"]
		if present != (n == 1) {
			t.Error("default model did not learn unsupported reasoning parameter")
		}
		if n == 1 {
			w.WriteHeader(400)
			_, _ = io.WriteString(w, "Unrecognized request argument supplied: reasoning_effort "+key)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer upstream.Close()
	m, _ := newServer(t)
	m.SkipPlanner = true
	m.DisableSnapshots = true
	m.ModelRouting = &config.ModelRouting{}
	m.Provider = &provider.OpenAICompatible{BaseURL: upstream.URL, APIKey: key, Model: "agnes-3.0-flash"}
	h := (&Server{Manager: m, Token: approvalTestToken, Models: []ModelOption{{ID: "default", Name: "Agnes", Model: "agnes-3.0-flash", Provider: "openai"}}}).Handler()
	session, err := m.CreateSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		rec := modelConfigRequest(t, h, "POST", "/v1/sessions/"+session.ID+"/runs?wait=1", map[string]string{"message": "hello", "reasoning_effort": "high"}, 201, key)
		var run view
		_ = json.Unmarshal(rec.Body.Bytes(), &run)
		events, err := m.Store.ListEvents(t.Context(), run.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		found := 0
		for _, event := range events {
			if strings.Contains(string(event.Payload), key) {
				t.Fatal("key in persistent event")
			}
			if event.Type == provider.ReasoningFallbackEventType {
				found++
				if strings.Contains(string(event.Payload), "Unrecognized") {
					t.Fatal("raw error in fallback trace")
				}
			}
		}
		if calls.Load() == 2 && found != 1 {
			t.Fatal("negotiation was not traced")
		}
	}
	rec := modelConfigRequest(t, h, "GET", "/v1/models", nil, 200, key)
	var options []ModelOption
	_ = json.Unmarshal(rec.Body.Bytes(), &options)
	if len(options) != 1 || options[0].Thinking != "unsupported" {
		t.Fatal("default menu did not reflect learned capability")
	}
	if calls.Load() != 3 {
		t.Fatal("unexpected negotiation retry count")
	}
}

func TestModelsDevMetadataAcrossSettingsAndModelMenu(t *testing.T) {
	const key = "modelsdev-integration-fixture-key"
	store := config.CustomProviderStore{Directory: t.TempDir()}
	fixture, err := os.ReadFile("../provider/testdata/modelsdev.json")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"fetched_at": time.Now(), "providers": json.RawMessage(fixture)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Directory, "models-dev-cache.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	catalog := provider.NewModelsDev(store.Directory)
	primary := &provider.OpenAICompatible{BaseURL: "https://apihub.agnes-ai.com/v1", Model: "agnes-3.0-flash"}
	provider.SetModelCatalog(primary, catalog)
	s := &Server{Token: approvalTestToken, ProviderSettings: store, ModelsDev: catalog, Manager: &agent.Manager{Provider: primary}, Models: []ModelOption{{ID: "default", Name: "默认 · agnes-3.0-flash", Model: "agnes-3.0-flash", Provider: "openai"}}}
	h := s.Handler()
	input := config.CustomProvider{ID: "step", Name: "StepFun", BaseURL: "https://api.stepfun.com/v1", APIKey: key, Model: "step-3.5-flash", Models: []string{"step-5-preview", "stepaudio-2.5-tts"}, Catalog: []modelconfig.Model{{ID: "step-3.5-flash"}, {ID: "step-5-preview"}, {ID: "stepaudio-2.5-tts"}}}
	rec := modelConfigRequest(t, h, "PUT", "/v1/providers", input, 200, key)
	var saved config.CustomProviderInfo
	if json.Unmarshal(rec.Body.Bytes(), &saved) != nil || len(saved.Catalog) != 3 || saved.Catalog[0].ReasoningType != "effort" || !reflect.DeepEqual(saved.Catalog[0].ReasoningValues, []string{"low", "high"}) {
		t.Fatal("saved catalog missing metadata")
	}
	persisted, err := store.Get("step")
	if err != nil || persisted.Catalog[0].ReasoningType != "effort" {
		t.Fatal("new optional fields failed strict on-disk reload")
	}
	rec = modelConfigRequest(t, h, "GET", "/v1/models", nil, 200, key)
	var options []ModelOption
	if json.Unmarshal(rec.Body.Bytes(), &options) != nil || len(options) != 4 {
		t.Fatal("wrong model menu")
	}
	if options[0].ReasoningType != "toggle" || options[0].Thinking != "supported" || options[0].DisplayName != "Agnes 3.0 Flash" {
		t.Fatalf("default metadata=%+v", options[0])
	}
	if options[1].Name != "StepFun · Step 3.5 Flash" || options[1].ReasoningType != "effort" || options[3].Category != "other" || options[3].Thinking != "unsupported" {
		t.Fatalf("custom menu=%+v", options)
	}
	// Old records get the same enriched view without writing or exposing credentials.
	persisted.Catalog = nil
	if _, err := store.Save(persisted); err != nil {
		t.Fatal(err)
	}
	rec = modelConfigRequest(t, h, "GET", "/v1/providers", nil, 200, key)
	var infos []config.CustomProviderInfo
	if json.Unmarshal(rec.Body.Bytes(), &infos) != nil || len(infos) != 1 || infos[0].Catalog[0].DisplayName != "Step 3.5 Flash" {
		t.Fatal("legacy provider listing not enriched")
	}
	// A separate upstream list still gets enriched during detect and refresh.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			t.Error("fixture provider credential missing")
		}
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"stepfun/step-5-preview"},{"id":"stepaudio-2.5-tts"}]}`)
	}))
	defer upstream.Close()
	rec = modelConfigRequest(t, h, "POST", "/v1/providers/detect", map[string]string{"base_url": upstream.URL, "api_key": key, "protocol": "openai"}, 200, key)
	var detected modelconfig.Detection
	if json.Unmarshal(rec.Body.Bytes(), &detected) != nil || detected.Models[0].DisplayName != "Step 5 Preview" || len(detected.Models[0].ReasoningValues) != 3 || detected.Models[1].Category != "other" {
		t.Fatal("detected catalog not enriched")
	}
	persisted.BaseURL = upstream.URL
	persisted.Protocol = "openai"
	if _, err := store.Save(persisted); err != nil {
		t.Fatal(err)
	}
	rec = modelConfigRequest(t, h, "POST", "/v1/providers/step/refresh-models", nil, 200, key)
	if json.Unmarshal(rec.Body.Bytes(), &saved) != nil || saved.Catalog[0].ReasoningType != "effort" || saved.Catalog[1].Category != "other" {
		t.Fatal("refreshed catalog not enriched")
	}
}
func TestModelsDevUnavailableDoesNotBlockManualSettings(t *testing.T) {
	store := config.CustomProviderStore{Directory: t.TempDir()}
	// Nil enrichment is the no-cache/no-network fallback and never fails settings.
	h := (&Server{Token: approvalTestToken, ProviderSettings: store}).Handler()
	input := config.CustomProvider{ID: "manual", Name: "Manual", BaseURL: "https://private.invalid/v1", APIKey: "offline-fixture-key", Model: "my-manual-model", Catalog: []modelconfig.Model{{ID: "step-router-v1"}}}
	rec := modelConfigRequest(t, h, "PUT", "/v1/providers", input, 200, input.APIKey)
	var info config.CustomProviderInfo
	if json.Unmarshal(rec.Body.Bytes(), &info) != nil || info.Catalog[0].Category != "other" || info.Catalog[1].Thinking != "unknown" || info.Catalog[1].Category != "chat" {
		t.Fatal("manual save offline fallback failed")
	}
	modelConfigRequest(t, h, "GET", "/v1/providers", nil, 200, input.APIKey)
	modelConfigRequest(t, h, "GET", "/v1/models", nil, 200, input.APIKey)
}
