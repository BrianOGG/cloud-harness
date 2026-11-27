package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/modelconfig"
)

const openAIList = `{"object":"list","data":[{"id":"o3","object":"model"},{"id":"gpt-4o","object":"model"}]}`
const anthropicList = `{"data":[{"id":"claude-sonnet-4","type":"model","display_name":"Sonnet"}],"has_more":false}`
const geminiList = `{"models":[{"name":"models/gemini-2.5-pro","displayName":"Gemini Pro","supportedGenerationMethods":["generateContent","countTokens"]},{"name":"models/embedding-001","supportedGenerationMethods":["embedContent"]},{"name":"bad","supportedGenerationMethods":["generateContent"]}]}`

func TestFetchCatalogAuthenticationProtocolPriorityAndManual(t *testing.T) {
	const key = "catalog-fixture-key"
	for _, tc := range []struct {
		name         string
		allowed      []string
		manual, want string
		alternatives []string
	}{
		{"openai", []string{"openai"}, "", "openai", nil},
		{"anthropic", []string{"anthropic"}, "", "anthropic", nil},
		{"gemini", []string{"gemini"}, "", "gemini", nil},
		{"conflict", []string{"openai", "anthropic"}, "", "openai", []string{"anthropic"}},
		{"all", []string{"openai", "anthropic", "gemini"}, "", "openai", []string{"anthropic", "gemini"}},
		{"tie", []string{"openai", "generic"}, "", "openai", nil},
		{"manual", []string{"openai", "anthropic", "gemini"}, "openai", "openai", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "GET" || r.URL.RawQuery != "" {
					t.Error("discovery must only GET without query credentials")
				}
				candidate := ""
				switch {
				case r.Header.Get("Authorization") == "Bearer "+key:
					candidate = "openai"
				case r.Header.Get("x-api-key") == key:
					candidate = "anthropic"
				case r.Header.Get("x-goog-api-key") == key:
					candidate = "gemini"
				}
				if candidate == "anthropic" && (r.Header.Get("anthropic-version") != "2023-06-01" || r.URL.Path != "/v1/models") {
					t.Error("invalid Anthropic endpoint/headers")
				}
				if candidate != "anthropic" && r.URL.Path != "/v1/models" {
					t.Error("invalid catalog endpoint")
				}
				allowed := false
				generic := false
				for _, item := range tc.allowed {
					if item == candidate {
						allowed = true
					}
					if item == "generic" {
						generic = true
					}
				}
				if generic {
					allowed = true
				}
				if !allowed {
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = io.WriteString(w, key)
					return
				}
				raw := openAIList
				if !generic {
					switch candidate {
					case "anthropic":
						raw = anthropicList
					case "gemini":
						raw = geminiList
					}
				}
				_, _ = io.WriteString(w, raw)
			}))
			defer srv.Close()
			got, err := FetchModelCatalog(t.Context(), srv.URL+"/v1", key, tc.manual)
			if err != nil || got.Protocol != tc.want || got.FetchedAt.IsZero() || !equalStrings(got.Alternatives, tc.alternatives) {
				t.Fatalf("result=%+v err=%v want alts=%v", got, err, tc.alternatives)
			}
			wantCalls := int32(3)
			if tc.manual != "" {
				wantCalls = 1
			}
			if calls.Load() != wantCalls {
				t.Fatalf("calls=%d", calls.Load())
			}
		})
	}
}

func TestFetchCatalogErrorsBoundedRedactedAndNoRedirect(t *testing.T) {
	const key = "catalog-error-fixture"
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1); _, _ = io.WriteString(w, openAIList) }))
	defer target.Close()
	for _, scenario := range []string{"401", "redirect", "invalid", "oversize", "key-in-body", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch scenario {
				case "401":
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = io.WriteString(w, key)
				case "redirect":
					http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
				case "invalid":
					_, _ = io.WriteString(w, "not JSON "+key)
				case "oversize":
					_, _ = io.WriteString(w, strings.Repeat(" ", modelconfig.MaxCatalogBytes+1))
				case "key-in-body":
					_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": key}}})
				case "cancelled":
					<-r.Context().Done()
				}
			}))
			defer upstream.Close()
			ctx := t.Context()
			if scenario == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 30*time.Millisecond)
				defer cancel()
			}
			_, err := FetchModelCatalog(ctx, upstream.URL, key, "openai")
			if err == nil || strings.Contains(err.Error(), key) {
				t.Fatal("unsafe discovery error")
			}
			if scenario == "401" {
				catalogErr, ok := err.(*modelconfig.CatalogError)
				if !ok || catalogErr.UpstreamStatus != 401 {
					t.Fatal("missing upstream status")
				}
			}
		})
	}
	if redirected.Load() != 0 {
		t.Fatal("followed credential-bearing redirect")
	}
}

const agnesOpenAIList = `{"object":"list","success":true,"data":[{"id":"agnes-3.0-flash","object":"model","created":1626777600,"owned_by":"custom","supported_endpoint_types":["openai"]}]}`
const agnesAnthropicList = `{"data":[{"id":"agnes-3.0-flash","created_at":"2021-07-20T10:40:00Z","display_name":"agnes-3.0-flash","type":"model"}],"has_more":false,"first_id":"agnes-3.0-flash","last_id":"agnes-3.0-flash"}`
const openaiHintAnthropic = `{"object":"list","data":[{"id":"only-claude","object":"model","supported_endpoint_types":["anthropic"]}]}`

func TestFetchDualProtocolGatewayPrefersOpenAI(t *testing.T) {
	const key = "dual-gateway-fixture"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			t.Error("discovery must not put credentials in the query")
		}
		switch {
		case r.Header.Get("Authorization") == "Bearer "+key:
			_, _ = io.WriteString(w, agnesOpenAIList)
		case r.Header.Get("x-api-key") == key && r.Header.Get("anthropic-version") == "2023-06-01":
			_, _ = io.WriteString(w, agnesAnthropicList)
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer srv.Close()
	got, err := FetchModelCatalog(t.Context(), srv.URL+"/v1", key, "")
	if err != nil || got.Protocol != "openai" || len(got.Models) != 1 || got.Models[0].ID != "agnes-3.0-flash" || !equalStrings(got.Alternatives, []string{"anthropic"}) {
		t.Fatalf("auto detect=%+v err=%v", got, err)
	}
	manual, err := FetchModelCatalog(t.Context(), srv.URL+"/v1", key, "anthropic")
	if err != nil || manual.Protocol != "anthropic" || len(manual.Alternatives) != 0 || manual.Models[0].ID != "agnes-3.0-flash" {
		t.Fatalf("manual anthropic=%+v err=%v", manual, err)
	}
}

func TestFetchOpenAIEndpointHintSelectsAnthropic(t *testing.T) {
	const key = "endpoint-hint-fixture"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer "+key {
			_, _ = io.WriteString(w, openaiHintAnthropic)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	got, err := FetchModelCatalog(t.Context(), srv.URL+"/v1", key, "")
	if err != nil || got.Protocol != "anthropic" || got.Models[0].ID != "only-claude" {
		t.Fatalf("hint=%+v err=%v", got, err)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
