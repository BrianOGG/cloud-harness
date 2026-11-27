package provider

import (
	"encoding/json"
	"github.com/BrianOGG/cloud-harness/internal/modelconfig"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestReasoningEffortMappingAndWire(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic", "gemini"} {
		for _, effort := range []string{"default", "off", "low", "medium", "high"} {
			t.Run(protocol+"/"+effort, func(t *testing.T) {
				called := false
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					called = true
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					switch protocol {
					case "openai":
						want := ""
						if effort != "default" && effort != "off" {
							want = effort
						}
						if got, _ := body["reasoning_effort"].(string); got != want {
							t.Errorf("effort=%q want=%q", got, want)
						}
						if _, ok := body["extra_body"]; ok {
							t.Error("unexpected Gemini fields")
						}
					case "anthropic":
						budget := map[string]int{"low": 4000, "medium": 10000, "high": 24000}[effort]
						if budget == 0 {
							if _, ok := body["thinking"]; ok {
								t.Error("default/off must omit thinking")
							}
						} else {
							thinking, ok := body["thinking"].(map[string]any)
							if !ok || thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(budget) {
								t.Errorf("thinking=%v", body["thinking"])
							}
							if body["max_tokens"].(float64) < float64(budget+8000) {
								t.Error("max_tokens below thinking budget")
							}
						}
						if _, ok := body["temperature"]; ok {
							t.Error("temperature conflicts with thinking")
						}
						if _, ok := body["tool_choice"]; ok {
							t.Error("forced tool choice conflicts with thinking")
						}
					case "gemini":
						if _, ok := body["reasoning_effort"]; ok {
							t.Error("Gemini cannot also send reasoning_effort")
						}
						if effort == "default" {
							if _, ok := body["extra_body"]; ok {
								t.Error("default must preserve service defaults")
							}
						} else {
							budget := map[string]int{"off": 0, "low": 1024, "medium": 8192, "high": 24576}[effort]
							want := map[string]any{"google": map[string]any{"thinking_config": map[string]any{"thinking_budget": float64(budget), "include_thoughts": false}}}
							if !reflect.DeepEqual(body["extra_body"], want) {
								t.Errorf("Gemini config=%v", body["extra_body"])
							}
						}
					}
					w.Header().Set("Content-Type", "application/json")
					if protocol == "anthropic" {
						_, _ = io.WriteString(w, `{"id":"msg","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
					} else {
						_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
					}
				}))
				defer upstream.Close()
				var p Provider = &OpenAICompatible{BaseURL: upstream.URL, APIKey: "reasoning-wire-fixture", Model: "private-model", Protocol: protocol}
				if protocol == "anthropic" {
					p = &Anthropic{BaseURL: upstream.URL, APIKey: "reasoning-wire-fixture", Model: "private-model"}
				}
				if _, err := p.Complete(t.Context(), Request{ReasoningEffort: effort, Messages: []Message{{Role: "user", Content: "hello"}}}); err != nil {
					t.Fatal(err)
				}
				if !called {
					t.Fatal("provider not called")
				}
			})
		}
	}
}

func TestReasoningParameterRejectionRetriesOnceAndCaches(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic", "gemini"} {
		for _, stream := range []bool{false, true} {
			t.Run(protocol+map[bool]string{false: "/json", true: "/stream"}[stream], func(t *testing.T) {
				const key = "reasoning-error-fixture"
				parameter := map[string]string{"openai": "reasoning_effort", "anthropic": "thinking", "gemini": "extra_body"}[protocol]
				var calls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					n := calls.Add(1)
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					_, present := body[parameter]
					if present != (n == 1) {
						t.Errorf("call %d parameter present=%v", n, present)
					}
					if n == 1 {
						w.WriteHeader(400)
						_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "Unrecognized request argument supplied: " + map[string]string{"openai": "reasoning_effort", "anthropic": "thinking", "gemini": "thinking_config"}[protocol] + " " + key}})
						return
					}
					w.Header().Set("Content-Type", "application/json")
					if protocol == "anthropic" {
						_, _ = io.WriteString(w, `{"id":"msg","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
					} else {
						_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
					}
				}))
				defer upstream.Close()
				construct := func() Provider {
					if protocol == "anthropic" {
						return &Anthropic{BaseURL: upstream.URL, APIKey: key, Model: "private-model"}
					}
					return &OpenAICompatible{BaseURL: upstream.URL, APIKey: key, Model: "private-model", Protocol: protocol}
				}
				var events []ReasoningFallbackEvent
				req := Request{ReasoningEffort: "high", singleAttempt: true, Messages: []Message{{Role: "user", Content: "hello"}}, OnReasoningFallback: func(ev ReasoningFallbackEvent) { events = append(events, ev) }}
				if stream {
					req.OnDelta = func(string) {}
				}
				for range 2 {
					if _, err := construct().Complete(t.Context(), req); err != nil {
						t.Fatal(err)
					}
				}
				if calls.Load() != 3 || len(events) != 1 {
					t.Fatalf("calls=%d events=%d", calls.Load(), len(events))
				}
				raw, _ := json.Marshal(events)
				if strings.Contains(string(raw), key) || strings.Contains(string(raw), "Unrecognized") {
					t.Fatal("upstream body leaked in fallback event")
				}
				if ReasoningSupport(protocol, upstream.URL, "private-model", "") != "unsupported" {
					t.Fatal("rejection was not cached")
				}
			})
		}
	}
}

func TestUnsupportedReasoningOmittedAndOther400NotRetried(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic", "gemini"} {
		for _, declared := range []bool{false, true} {
			t.Run(protocol+map[bool]string{false: "/known", true: "/declared"}[declared], func(t *testing.T) {
				calls := 0
				model := map[string]string{"openai": "gpt-4o", "anthropic": "claude-3-5-sonnet", "gemini": "gemini-2.0-flash"}[protocol]
				if declared {
					model = "private"
				}
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					for _, field := range []string{"reasoning_effort", "thinking", "extra_body"} {
						if _, ok := body[field]; ok {
							t.Errorf("unsupported sent %s", field)
						}
					}
					w.WriteHeader(400)
					_, _ = io.WriteString(w, `{"error":{"message":"invalid messages"}}`)
				}))
				defer srv.Close()
				thinking := map[string]string{}
				if declared {
					thinking[model] = "unsupported"
				}
				var p Provider = &OpenAICompatible{BaseURL: srv.URL, APIKey: "unsupported-fixture", Model: model, Protocol: protocol, Thinking: thinking}
				if protocol == "anthropic" {
					p = &Anthropic{BaseURL: srv.URL, APIKey: "unsupported-fixture", Model: model, Thinking: thinking}
				}
				if _, err := p.Complete(t.Context(), Request{ReasoningEffort: "high", Messages: []Message{{Role: "user", Content: "hello"}}}); err == nil {
					t.Fatal("non-parameter error suppressed")
				}
				if calls != 1 {
					t.Fatalf("unrelated 400 retried %d times", calls)
				}
			})
		}
	}
	for _, effort := range []string{"invalid", "HIGH", "none"} {
		if ValidReasoningEffort(effort) {
			t.Fatal("invalid effort accepted")
		}
	}
}

func TestCatalogReasoningWire(t *testing.T) {
	for _, tc := range []struct {
		name, kind, effort, want string
		values                   []string
		toggle                   *bool
	}{
		{name: "Agnes off", kind: "toggle", effort: "off", toggle: boolPointer(false)},
		{name: "Agnes default", kind: "toggle", effort: "default", toggle: boolPointer(true)},
		{name: "Agnes legacy high", kind: "toggle", effort: "high", toggle: boolPointer(true)},
		{name: "Step 3.5 low", kind: "effort", effort: "low", want: "low", values: []string{"low", "high"}},
		{name: "Step 3.5 missing medium", kind: "effort", effort: "medium", values: []string{"low", "high"}},
		{name: "Step 3.5 missing off", kind: "effort", effort: "off", values: []string{"low", "high"}},
		{name: "Step 5 medium", kind: "effort", effort: "medium", want: "medium", values: []string{"low", "medium", "high"}},
		{name: "Step 5 high", kind: "effort", effort: "high", want: "high", values: []string{"low", "medium", "high"}},
		{name: "minimal alias", kind: "effort", effort: "low", want: "minimal", values: []string{"minimal", "max", "none"}},
		{name: "off alias", kind: "effort", effort: "off", want: "none", values: []string{"minimal", "max", "none"}},
		{name: "max is not high", kind: "effort", effort: "high", values: []string{"minimal", "max", "none"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var body map[string]json.RawMessage
				if json.NewDecoder(r.Body).Decode(&body) != nil {
					t.Error("invalid request")
				}
				var effort string
				_ = json.Unmarshal(body["reasoning_effort"], &effort)
				if effort != tc.want {
					t.Errorf("wire effort=%s want=%s", effort, tc.want)
				}
				if tc.toggle == nil {
					if body["chat_template_kwargs"] != nil {
						t.Error("unexpected toggle")
					}
				} else {
					var toggle struct {
						EnableThinking *bool `json:"enable_thinking"`
					}
					if json.Unmarshal(body["chat_template_kwargs"], &toggle) != nil || toggle.EnableThinking == nil || *toggle.EnableThinking != *tc.toggle {
						t.Error("toggle did not carry the explicit boolean")
					}
				}
				_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
			}))
			defer upstream.Close()
			m := modelconfig.Model{ID: "catalog-model", Thinking: "supported", ReasoningType: tc.kind, ReasoningValues: tc.values}
			p := &OpenAICompatible{BaseURL: upstream.URL, APIKey: "catalog-wire-fixture", Model: m.ID, ModelMetadata: map[string]modelconfig.Model{m.ID: m}}
			if _, err := p.Complete(t.Context(), Request{ReasoningEffort: tc.effort, Messages: []Message{{Role: "user", Content: "hello"}}}); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}
func boolPointer(v bool) *bool { return &v }

func TestToggleRejectionStillRetriesOnceAndCaches(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "stream"}[stream], func(t *testing.T) {
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var body map[string]json.RawMessage
				_ = json.NewDecoder(r.Body).Decode(&body)
				if (body["chat_template_kwargs"] != nil) != (calls == 1) {
					t.Errorf("call %d toggle presence unexpected", calls)
				}
				if calls == 1 {
					w.WriteHeader(400)
					_, _ = io.WriteString(w, `{"error":{"message":"unsupported chat_template_kwargs.enable_thinking"}}`)
					return
				}
				_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
			}))
			defer upstream.Close()
			m := modelconfig.Model{ID: "agnes-3.0-flash", Thinking: "supported", ReasoningType: "toggle"}
			p := &OpenAICompatible{BaseURL: upstream.URL, APIKey: "toggle-rejection-fixture", Model: m.ID, ModelMetadata: map[string]modelconfig.Model{m.ID: m}}
			events := 0
			req := Request{ReasoningEffort: "off", Messages: []Message{{Role: "user", Content: "hello"}}, OnReasoningFallback: func(ReasoningFallbackEvent) { events++ }}
			if stream {
				req.OnDelta = func(string) {}
			}
			for range 2 {
				if _, err := p.Complete(t.Context(), req); err != nil {
					t.Fatal(err)
				}
			}
			if calls != 3 || events != 1 || ModelMetadata(p, m.ID).Thinking != "unsupported" {
				t.Fatalf("calls=%d events=%d", calls, events)
			}
		})
	}
	if got := ReasoningSupport("openai", "authoritative-fixture", "gpt-4-future", "supported"); got != "supported" {
		t.Fatal("name heuristic overrode catalog")
	}
}

func TestCatalogAnthropicToggleAndMissingEffortWire(t *testing.T) {
	for _, tc := range []struct {
		kind, effort, wantType string
		maxTokens              int
	}{
		{"toggle", "off", "disabled", 8192}, {"toggle", "default", "enabled", 12000}, {"toggle", "high", "enabled", 12000}, {"effort", "medium", "", 8192},
	} {
		t.Run(tc.kind+"/"+tc.effort, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				thinking, _ := body["thinking"].(map[string]any)
				if tc.wantType == "" {
					if thinking != nil {
						t.Error("unavailable effort sent thinking")
					}
				} else if thinking["type"] != tc.wantType {
					t.Error("wrong toggle type")
				}
				if tc.wantType == "disabled" {
					if _, exists := thinking["budget_tokens"]; exists {
						t.Error("disabled thinking included a budget")
					}
				}
				if body["max_tokens"] != float64(tc.maxTokens) {
					t.Errorf("max_tokens=%v", body["max_tokens"])
				}
				_, _ = io.WriteString(w, `{"id":"msg","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
			}))
			defer upstream.Close()
			m := modelconfig.Model{ID: "catalog-native", Thinking: "supported", ReasoningType: tc.kind, ReasoningValues: []string{"low", "high"}}
			p := &Anthropic{BaseURL: upstream.URL, APIKey: "native-toggle-fixture", Model: m.ID, ModelMetadata: map[string]modelconfig.Model{m.ID: m}}
			if _, err := p.Complete(t.Context(), Request{ReasoningEffort: tc.effort, Messages: []Message{{Role: "user", Content: "hello"}}}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
