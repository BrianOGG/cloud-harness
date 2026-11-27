package provider

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

func TestOpenAIChatUsageBreakdownAndFieldPresence(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want domain.Usage
	}{
		{name: "details omitted", raw: "{\"prompt_tokens\":12,\"completion_tokens\":6}", want: domain.Usage{InputTokens: 12, OutputTokens: 6}},
		{name: "details reported as zero", raw: "{\"prompt_tokens\":12,\"completion_tokens\":6,\"prompt_tokens_details\":{\"cached_tokens\":0},\"completion_tokens_details\":{\"reasoning_tokens\":0}}", want: domain.Usage{InputTokens: 12, OutputTokens: 6, CacheReported: true, ReasoningReported: true}},
		{name: "all zero", raw: "{\"prompt_tokens\":0,\"completion_tokens\":0,\"prompt_tokens_details\":{\"cached_tokens\":0},\"completion_tokens_details\":{\"reasoning_tokens\":0}}", want: domain.Usage{CacheReported: true, ReasoningReported: true}},
		{name: "details reported with counts", raw: "{\"prompt_tokens\":120,\"completion_tokens\":45,\"prompt_tokens_details\":{\"cached_tokens\":30},\"completion_tokens_details\":{\"reasoning_tokens\":11}}", want: domain.Usage{InputTokens: 120, OutputTokens: 45, CachedInputTokens: 30, ReasoningTokens: 11, CacheReported: true, ReasoningReported: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var wire chatUsage
			if err := json.Unmarshal([]byte(tc.raw), &wire); err != nil {
				t.Fatal(err)
			}
			if got := wire.domain(); got != tc.want {
				t.Fatalf("usage=%+v want=%+v", got, tc.want)
			}
			if tc.want.CacheReported || tc.want.ReasoningReported {
				resp := (&assembler{usage: tc.want, finish: "stop"}).response()
				if len(resp.Items) == 0 || resp.Items[len(resp.Items)-1].Kind != domain.ItemUsage {
					t.Fatalf("reported zero usage was dropped: %+v", resp.Items)
				}
			}
		})
	}
}

func TestOpenAIResponsesUsageBreakdown(t *testing.T) {
	resp, err := ParseOpenAIResponses([]byte("{\"id\":\"resp_usage\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":120,\"input_tokens_details\":{\"cached_tokens\":30},\"output_tokens\":45,\"output_tokens_details\":{\"reasoning_tokens\":11},\"total_tokens\":165}}"))
	if err != nil {
		t.Fatal(err)
	}
	got := *resp.Items[0].Usage
	want := domain.Usage{InputTokens: 120, OutputTokens: 45, CachedInputTokens: 30, ReasoningTokens: 11, CacheReported: true, ReasoningReported: true}
	if got != want {
		t.Fatalf("usage=%+v want=%+v", got, want)
	}
}

func TestAnthropicUsageIncludesCacheCreationAndReadInInput(t *testing.T) {
	resp, err := ParseAnthropicMessage([]byte("{\"id\":\"msg_usage\",\"content\":[],\"stop_reason\":\"end_turn\",\"usage\":{\"input_tokens\":5,\"cache_creation_input_tokens\":4,\"cache_read_input_tokens\":3,\"output_tokens\":9}}"))
	if err != nil {
		t.Fatal(err)
	}
	got := *resp.Items[0].Usage
	want := domain.Usage{InputTokens: 12, OutputTokens: 9, CachedInputTokens: 3, CacheCreationInputTokens: 4, CacheReported: true}
	if got != want {
		t.Fatalf("usage=%+v want=%+v", got, want)
	}
}

func TestParseGeminiUsage(t *testing.T) {
	got, err := ParseGeminiUsage([]byte("{\"usageMetadata\":{\"promptTokenCount\":120,\"candidatesTokenCount\":45,\"cachedContentTokenCount\":30,\"thoughtsTokenCount\":11}}"))
	if err != nil {
		t.Fatal(err)
	}
	want := domain.Usage{InputTokens: 120, OutputTokens: 56, CachedInputTokens: 30, ReasoningTokens: 11, CacheReported: true, ReasoningReported: true}
	if got != want {
		t.Fatalf("usage=%+v want=%+v", got, want)
	}

	missing, err := ParseGeminiUsage([]byte("{\"usageMetadata\":{\"promptTokenCount\":8,\"candidatesTokenCount\":2}}"))
	if err != nil || missing.CacheReported || missing.ReasoningReported {
		t.Fatalf("missing optional fields=%+v err=%v", missing, err)
	}
	zero, err := ParseGeminiUsage([]byte("{\"usageMetadata\":{\"promptTokenCount\":8,\"candidatesTokenCount\":2,\"cachedContentTokenCount\":0,\"thoughtsTokenCount\":0}}"))
	if err != nil || !zero.CacheReported || !zero.ReasoningReported {
		t.Fatalf("zero optional fields=%+v err=%v", zero, err)
	}
	if _, err := ParseGeminiUsage([]byte("{")); err == nil {
		t.Fatal("invalid JSON was accepted")
	}
	if !reflect.DeepEqual(missing, domain.Usage{InputTokens: 8, OutputTokens: 2}) {
		t.Fatalf("missing-field usage=%+v", missing)
	}
}

func TestOptionalUsageFieldsSurviveZeroCounts(t *testing.T) {
	for _, tc := range []struct {
		name      string
		parse     func([]byte) (domain.ModelResponse, error)
		missing   string
		zero      string
		reasoning bool
	}{
		{"responses", ParseOpenAIResponses, `{"output":[],"usage":{"input_tokens":1,"output_tokens":1}}`, `{"output":[],"usage":{"input_tokens":0,"output_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}`, true},
		{"anthropic", ParseAnthropicMessage, `{"content":[],"usage":{"input_tokens":1,"output_tokens":1}}`, `{"content":[],"usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			missing, err := tc.parse([]byte(tc.missing))
			if err != nil || len(missing.Items) != 1 || missing.Items[0].Usage.CacheReported || missing.Items[0].Usage.ReasoningReported {
				t.Fatalf("missing fields response=%+v err=%v", missing, err)
			}
			zero, err := tc.parse([]byte(tc.zero))
			if err != nil || len(zero.Items) != 1 || !zero.Items[0].Usage.CacheReported || zero.Items[0].Usage.ReasoningReported != tc.reasoning {
				t.Fatalf("zero fields response=%+v err=%v", zero, err)
			}
		})
	}
}

func TestAttachUsagePreservesBreakdownAndFlags(t *testing.T) {
	base := domain.Usage{InputTokens: 20, OutputTokens: 8, CacheCreationInputTokens: 3, CacheReported: true}
	resp := domain.ModelResponse{Items: []domain.ResponseItem{{Kind: domain.ItemUsage, Usage: &base}}}
	got := attachUsage(resp, domain.Usage{InputTokens: 10, OutputTokens: 4, CachedInputTokens: 2, CacheCreationInputTokens: 1, ReasoningTokens: 5, ReasoningReported: true})
	want := domain.Usage{InputTokens: 30, OutputTokens: 12, CachedInputTokens: 2, CacheCreationInputTokens: 4, ReasoningTokens: 5, CacheReported: true, ReasoningReported: true}
	if *got.Items[0].Usage != want || base.InputTokens != 20 {
		t.Fatalf("combined usage=%+v original=%+v", got.Items[0].Usage, base)
	}
}
