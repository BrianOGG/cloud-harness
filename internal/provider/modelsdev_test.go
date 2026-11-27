package provider

import (
	"encoding/json"
	"github.com/BrianOGG/cloud-harness/internal/modelconfig"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type catalogRoundTrip func(*http.Request) (*http.Response, error)

func (f catalogRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fixtureModelsDev(t *testing.T, handler http.HandlerFunc) *ModelsDev {
	t.Helper()
	fixture, err := os.ReadFile("testdata/modelsdev.json")
	if err != nil {
		t.Fatal(err)
	}
	if handler == nil {
		handler = func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(fixture) }
	}
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	target, _ := url.Parse(upstream.URL)
	c := NewModelsDev(t.TempDir())
	c.client.Transport = catalogRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.String() != modelsDevURL || r.Host != "models.dev" || r.Body != nil {
			t.Error("catalog request must be a bare GET of the fixed public URL")
		}
		for name, values := range r.Header {
			if name != "Accept" && name != "User-Agent" {
				t.Errorf("unexpected public catalog header: %s", name)
			}
			for _, value := range values {
				if strings.Contains(value, "private") || strings.Contains(value, "fixture-key") {
					t.Error("private config leaked")
				}
			}
		}
		req := r.Clone(r.Context())
		u := *r.URL
		u.Scheme, u.Host = target.Scheme, target.Host
		req.URL = &u
		return http.DefaultTransport.RoundTrip(req)
	})
	return c
}

func TestModelsDevIDHostPathAndOfficialPriority(t *testing.T) {
	c := fixtureModelsDev(t, nil)
	c.Refresh(t.Context())
	for _, tc := range []struct {
		base, id, name, kind string
		values               []string
	}{
		{"https://api.stepfun.com/v1", "step-5-preview", "Step 5 Preview", "effort", []string{"low", "medium", "high"}},
		{"https://api.stepfun.com/v1/", "stepfun/step-5-preview", "Step 5 Preview", "effort", []string{"low", "medium", "high"}},
		{"https://private.invalid/v1", "nvidia/stepfun-ai/step-5-preview", "Step 5 Preview", "effort", []string{"low", "medium", "high"}},
		{"https://api.stepfun.com/step_plan/v1", "step-5-preview", "Plan Step 5", "effort", []string{"high"}},
		{"https://api.stepfun.com/step_plan/v1/chat/completions", "step-5-preview", "Plan Step 5", "effort", []string{"high"}},
		{"https://api.stepfun.ai/v1", "step-5-preview", "Global Step 5", "effort", []string{"low", "high"}},
		{"https://apihub.agnes-ai.com/v1", "agnes/agnes-3.0-flash", "Agnes 3.0 Flash", "toggle", nil},
		{"https://private.invalid/v1", "step-3.5-flash", "Step 3.5 Flash", "effort", []string{"low", "high"}},
		{"https://router.requesty.ai/v1", "step-5-preview", "Aggregator Step 5", "effort", []string{"none", "low", "medium", "high", "max"}},
	} {
		got := c.Enrich(tc.base, "openai", []modelconfig.Model{{ID: tc.id}})[0]
		if got.ID != tc.id || got.DisplayName != tc.name || got.Thinking != "supported" || got.ReasoningType != tc.kind || !reflect.DeepEqual(got.ReasoningValues, tc.values) {
			t.Errorf("%s %s: %+v", tc.base, tc.id, got)
		}
	}
	// Capabilities in this one deliberately conflict with a name heuristic.
	got := c.Enrich("https://api.stepfun.com/v1", "openai", []modelconfig.Model{{ID: "step-1o-turbo-vision", Thinking: "supported", ReasoningType: "effort", ReasoningValues: []string{"high"}}})[0]
	if got.Thinking != "unsupported" || got.ReasoningType != "" || got.ReasoningValues != nil {
		t.Fatalf("catalog did not override stale capabilities: %+v", got)
	}
}
func TestModelsDevModalitiesAndOfflineClassification(t *testing.T) {
	c := fixtureModelsDev(t, nil)
	c.Refresh(t.Context())
	for _, id := range []string{"step-5-preview", "step-3.5-flash", "step-3.7-flash", "step-1o-turbo-vision", "agnes-3.0-flash", "my-private-model"} {
		if got := c.Enrich("", "openai", []modelconfig.Model{{ID: id}})[0]; got.Category != "chat" {
			t.Errorf("expected chat: %+v", got)
		}
	}
	for _, id := range []string{"stepaudio-2.5-tts", "stepaudio-2.5-asr", "stepaudio-2.5-chat", "step-router-v1", "stepaudio-2.5-realtime", "whisper-1", "text-embedding-3", "image-gen", "video-gen"} {
		if got := c.Enrich("", "openai", []modelconfig.Model{{ID: id}})[0]; got.Category != "other" {
			t.Errorf("expected other: %+v", got)
		}
	}
}
func TestModelsDevCacheTTLFailuresAndNoPrivateData(t *testing.T) {
	for _, failure := range []string{"5xx", "timeout", "invalid", "wrong-shape", "redirect"} {
		t.Run(failure, func(t *testing.T) {
			c := fixtureModelsDev(t, nil)
			now := time.Now()
			c.now = func() time.Time { return now }
			c.Refresh(t.Context())
			// A new reader must be able to use the on-disk cache without a request.
			disk := NewModelsDev(c.directory)
			if got := disk.Enrich("https://private.example/v1", "openai", []modelconfig.Model{{ID: "step-5-preview"}})[0]; got.DisplayName != "Step 5 Preview" {
				t.Fatal("cache was not persisted")
			}
			var calls atomic.Int32
			redirected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(100); w.WriteHeader(500) }))
			defer redirected.Close()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch failure {
				case "5xx":
					w.WriteHeader(503)
				case "timeout":
					<-r.Context().Done()
				case "invalid":
					_, _ = io.WriteString(w, "{")
				case "wrong-shape":
					_, _ = io.WriteString(w, `{"error":{"message":"unavailable"}}`)
				case "redirect":
					http.Redirect(w, r, redirected.URL, 302)
				}
			}))
			defer upstream.Close()
			target, _ := url.Parse(upstream.URL)
			transport := c.client.Transport
			c.client.Transport = catalogRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != modelsDevURL || len(r.Header.Values("Authorization")) > 0 || r.Header.Get("Referer") != "" || r.Header.Get("X-Api-Key") != "" || r.URL.RawQuery != "" {
					t.Error("private data on catalog request")
				}
				req := r.Clone(r.Context())
				u := *r.URL
				u.Scheme, u.Host = target.Scheme, target.Host
				req.URL = &u
				return http.DefaultTransport.RoundTrip(req)
			})
			c.client.Timeout = 30 * time.Millisecond
			c.Refresh(t.Context())
			if calls.Load() != 0 {
				t.Fatal("fresh cache fetched again")
			}
			now = now.Add(25 * time.Hour)
			c.Refresh(t.Context())
			if calls.Load() != 1 {
				t.Fatalf("refresh/redirect calls=%d", calls.Load())
			}
			got := c.Enrich("https://private.example/v1", "openai", []modelconfig.Model{{ID: "step-5-preview"}})[0]
			if got.DisplayName != "Step 5 Preview" || len(got.ReasoningValues) != 3 {
				t.Fatal("failure lost stale cache")
			}
			c.Refresh(t.Context())
			if calls.Load() != 1 {
				t.Fatal("failure backoff missing")
			}
			c.client.Transport = transport
			raw, _ := os.ReadFile(c.path())
			if strings.Contains(string(raw), "private.example") {
				t.Fatal("private base URL persisted in public cache")
			}
			// No cache is equally non-fatal, including unknown manual model IDs.
			empty := fixtureModelsDev(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
			empty.Refresh(t.Context())
			fallback := empty.Enrich("https://private.example/v1", "openai", []modelconfig.Model{{ID: "manual-id"}, {ID: "stepaudio-2.5-tts"}})
			if fallback[0].Thinking != "unknown" || fallback[0].Category != "chat" || fallback[1].Category != "other" {
				t.Fatalf("fallback=%+v", fallback)
			}
		})
	}
}
func TestModelsDevMetadataJSONIsBackwardCompatible(t *testing.T) {
	var model modelconfig.Model
	if err := json.Unmarshal([]byte(`{"id":"old"}`), &model); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(model)
	if string(raw) != `{"id":"old"}` {
		t.Fatalf("optional metadata changed legacy JSON: %s", raw)
	}
}
