package modelconfig

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const openAIList = `{"object":"list","data":[{"id":"o3","object":"model"},{"id":"gpt-4o","object":"model"}]}`
const anthropicList = `{"data":[{"id":"claude-sonnet-4","type":"model","display_name":"Sonnet"}],"has_more":false}`
const geminiList = `{"models":[{"name":"models/gemini-2.5-pro","displayName":"Gemini Pro","supportedGenerationMethods":["generateContent","countTokens"]},{"name":"models/embedding-001","supportedGenerationMethods":["embedContent"]},{"name":"bad","supportedGenerationMethods":["generateContent"]}]}`

func TestParseCatalogs(t *testing.T) {
	for _, tc := range []struct {
		name, raw             string
		parse                 func([]byte) ([]Model, error)
		count                 int
		id, display, thinking string
		bad                   bool
	}{
		{"openai", openAIList, ParseOpenAIModels, 2, "o3", "", "supported", false},
		{"anthropic", anthropicList, ParseAnthropicModels, 1, "claude-sonnet-4", "Sonnet", "supported", false},
		{"gemini", geminiList, ParseGeminiModels, 1, "gemini-2.5-pro", "Gemini Pro", "supported", false},
		{"openai-empty", `{"data":[]}`, ParseOpenAIModels, 0, "", "", "", false},
		{"anthropic-empty", `{"data":[],"has_more":false}`, ParseAnthropicModels, 0, "", "", "", false},
		{"gemini-empty", `{"models":[]}`, ParseGeminiModels, 0, "", "", "", false},
		{"invalid", `{`, ParseOpenAIModels, 0, "", "", "", true},
		{"wrong-shape", `{"error":"rejected"}`, ParseAnthropicModels, 0, "", "", "", true},
		{"null-data", `{"data":null}`, ParseOpenAIModels, 0, "", "", "", true},
		{"null-gemini", `{"models":null}`, ParseGeminiModels, 0, "", "", "", true},
		{"oversize", strings.Repeat(" ", MaxCatalogBytes+1), ParseOpenAIModels, 0, "", "", "", true},
		{"known-unsupported", `{"data":[{"id":"gpt-4o"}]}`, ParseOpenAIModels, 1, "gpt-4o", "", "unsupported", false},
		{"declared-unsupported", `{"data":[{"id":"private","thinking":"unsupported"}]}`, ParseOpenAIModels, 1, "private", "", "unsupported", false},
		{"dedup", `{"data":[{"id":"private"},{"id":"private"},{}]}`, ParseOpenAIModels, 1, "private", "", "unknown", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.parse([]byte(tc.raw))
			if (err != nil) != tc.bad {
				t.Fatalf("error=%v", err)
			}
			if tc.bad {
				return
			}
			if got == nil || len(got) != tc.count {
				t.Fatalf("models=%+v", got)
			}
			if tc.count > 0 && (got[0].ID != tc.id || got[0].DisplayName != tc.display || got[0].Thinking != tc.thinking) {
				t.Fatalf("first=%+v", got[0])
			}
		})
	}
	rows := make([]map[string]string, 1005)
	for i := range rows {
		rows[i] = map[string]string{"id": fmt.Sprintf("model-%d", i)}
	}
	raw, _ := json.Marshal(map[string]any{"data": rows})
	got, err := ParseOpenAIModels(raw)
	if err != nil || len(got) != MaxCatalogModels {
		t.Fatalf("catalog bound len=%d err=%v", len(got), err)
	}
}

func TestDetectProtocolShapes(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{openAIList, "openai"}, {anthropicList, "anthropic"}, {geminiList, "gemini"},
		{`{"data":[]}`, "openai"}, {`{"data":[],"has_more":false}`, "anthropic"}, {`{"models":[]}`, "gemini"},
		{`{"data":[{"id":"a","object":"model","type":"model"}]}`, "anthropic"},
		{`{"data":[{}]}`, ""}, {`not-json`, ""},
	} {
		got, _, err := DetectProtocol([]byte(tc.raw))
		if got != tc.want || (err != nil) != (tc.want == "") {
			t.Fatalf("got=%s want=%s err=%v", got, tc.want, err)
		}
	}
}

func TestOfficialProtocol(t *testing.T) {
	for _, tc := range []struct{ host, want string }{
		{"generativelanguage.googleapis.com", "gemini"},
		{"GenerativeLanguage.googleapis.com", "gemini"},
		{"api.anthropic.com", "anthropic"},
		{"API.ANTHROPIC.COM", "anthropic"},
		{"api.openai.com", ""},
		{"agnes.example.com", ""},
	} {
		if got := OfficialProtocol(tc.host); got != tc.want {
			t.Fatalf("host=%s got=%s want=%s", tc.host, got, tc.want)
		}
	}
}

func TestLooksLikeAndEndpointHint(t *testing.T) {
	agnesOpenAI := `{"object":"list","data":[{"id":"agnes-3.0-flash","object":"model","supported_endpoint_types":["openai"]}]}`
	agnesAnthropic := `{"data":[{"id":"agnes-3.0-flash","type":"model","display_name":"agnes-3.0-flash"}],"has_more":false,"first_id":"a","last_id":"z"}`
	hintAnthropic := `{"object":"list","data":[{"id":"only-claude","object":"model","supported_endpoint_types":["anthropic"]}]}`
	if !LooksLikeOpenAI([]byte(agnesOpenAI)) || LooksLikeAnthropic([]byte(agnesOpenAI)) {
		t.Fatal("agnes openai body should only look like OpenAI")
	}
	if !LooksLikeAnthropic([]byte(agnesAnthropic)) || LooksLikeOpenAI([]byte(agnesAnthropic)) {
		t.Fatal("agnes anthropic body should only look like Anthropic")
	}
	if OpenAIEndpointHint([]byte(agnesOpenAI)) != "" {
		t.Fatal("openai endpoint types should not override")
	}
	if OpenAIEndpointHint([]byte(hintAnthropic)) != "anthropic" {
		t.Fatal("anthropic-only endpoint types should hint anthropic")
	}
	if OpenAIEndpointHint([]byte(`{"data":[{"id":"x","supported_endpoint_types":["anthropic","gemini"]}]}`)) != "" {
		t.Fatal("mixed non-openai types should not hint")
	}
}

func TestSelectAutoProtocolPrefersOpenAI(t *testing.T) {
	openaiModels := []Model{{ID: "agnes-3.0-flash"}}
	anthropicModels := []Model{{ID: "agnes-3.0-flash", DisplayName: "agnes-3.0-flash"}}
	geminiModels := []Model{{ID: "gemini-2.5-pro"}}
	agnesOpenAI := []byte(`{"object":"list","data":[{"id":"agnes-3.0-flash","object":"model","supported_endpoint_types":["openai"]}]}`)
	protocol, alts, models := SelectAutoProtocol(map[string][]Model{"openai": openaiModels, "anthropic": anthropicModels}, agnesOpenAI)
	if protocol != "openai" || len(alts) != 1 || alts[0] != "anthropic" || models[0].ID != "agnes-3.0-flash" {
		t.Fatalf("dual gateway: protocol=%s alts=%v models=%v", protocol, alts, models)
	}
	protocol, alts, models = SelectAutoProtocol(map[string][]Model{"anthropic": anthropicModels}, nil)
	if protocol != "anthropic" || alts != nil || models[0].DisplayName == "" {
		t.Fatalf("anthropic only: protocol=%s alts=%v", protocol, alts)
	}
	hintBody := []byte(`{"object":"list","data":[{"id":"only-claude","object":"model","supported_endpoint_types":["anthropic"]}]}`)
	protocol, alts, models = SelectAutoProtocol(map[string][]Model{"openai": {{ID: "only-claude"}}}, hintBody)
	if protocol != "anthropic" || models[0].ID != "only-claude" {
		t.Fatalf("hint: protocol=%s alts=%v models=%v", protocol, alts, models)
	}
	protocol, alts, models = SelectAutoProtocol(map[string][]Model{"openai": openaiModels, "anthropic": anthropicModels, "gemini": geminiModels}, nil)
	if protocol != "openai" || len(alts) != 2 || alts[0] != "anthropic" || alts[1] != "gemini" {
		t.Fatalf("all: protocol=%s alts=%v", protocol, alts)
	}
	protocol, alts, models = SelectAutoProtocol(nil, nil)
	if protocol != "" || alts != nil || models != nil {
		t.Fatalf("empty: protocol=%s", protocol)
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	for _, tc := range []struct {
		url string
		ok  bool
	}{
		{"https://api.example/v1/", true}, {"http://localhost:1234/v1", true}, {"http://127.0.0.1:1234", true}, {"http://[::1]:1234", true},
		{"http://example.com", false}, {"https://user:secret@example.com", false}, {"https://example.com/?key=secret", false}, {"https://example.com/#fragment", false}, {"https://example.com/?", false}, {"https://example.com/#", false}, {"bad", false},
	} {
		_, err := NormalizeBaseURL(tc.url)
		if (err == nil) != tc.ok {
			t.Fatalf("URL validation mismatch")
		}
	}
}
