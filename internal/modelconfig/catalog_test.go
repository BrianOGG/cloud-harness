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
