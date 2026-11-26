package provider

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

func TestChatContentReasoningIsPrivate(t *testing.T) {
	cases := []struct {
		name, content, structured, want string
	}{
		{"agnes orphan close", "可以回答问题和草稿\n</think>\n\n可以回答问题、最终答复", "", "可以回答问题、最终答复"},
		{"paired", "<think>private draft</think>\n\nFinal answer", "", "Final answer"},
		{"structured", "Final answer", "private structured draft", "Final answer"},
		{"both", "<think>private inline draft</think>Final answer", "private structured draft", "Final answer"},
		{"unclosed", "<think>private unfinished draft", "", ""},
		{"nested", "<THINK>private <think>nested</think> draft</THINK>Final answer", "", "Final answer"},
		{"multiple blocks", "<think>private one</think>Final <think>private two</think>answer", "", "Final answer"},
		{"plain preserved", "  Final answer\n\n", "", "  Final answer\n\n"},
	}
	for _, tc := range cases {
		for _, wire := range []string{"json", "sse"} {
			t.Run(tc.name+"/"+wire, func(t *testing.T) {
				var deltas strings.Builder
				var resp domain.ModelResponse
				var err error
				if wire == "json" {
					body, marshalErr := json.Marshal(map[string]any{"id": "fixture", "choices": []any{map[string]any{
						"finish_reason": "stop", "message": map[string]any{"content": tc.content, "reasoning_content": tc.structured},
					}}})
					if marshalErr != nil {
						t.Fatal(marshalErr)
					}
					resp, err = parseChatJSON(body, func(d string) { deltas.WriteString(d) })
				} else {
					var body strings.Builder
					// A separate SSE event per rune splits every possible tag boundary.
					for _, r := range tc.content {
						data, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": string(r)}}}})
						fmt.Fprintf(&body, "data: %s\n\n", data)
					}
					data, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"reasoning_content": tc.structured}, "finish_reason": "stop"}}})
					fmt.Fprintf(&body, "data: %s\n\ndata: [DONE]\n\n", data)
					resp, err = parseSSE(strings.NewReader(body.String()), nil, func(d string) { deltas.WriteString(d) })
				}
				if err != nil {
					t.Fatal(err)
				}
				if !resp.Complete || resp.Text() != tc.want || deltas.String() != tc.want {
					t.Fatalf("complete=%v visible=%q delta=%q, want=%q", resp.Complete, resp.Text(), deltas.String(), tc.want)
				}
				projection, _ := json.Marshal(domain.TraceView(resp.Items))
				if strings.Contains(string(projection), "private") || strings.Contains(domain.IndexableText(resp.Items), "private") {
					t.Fatal("private reasoning leaked through a public projection")
				}
				var reasoning strings.Builder
				for i, item := range resp.Items {
					if item.Ordinal != i {
						t.Fatal("non-contiguous ordinals")
					}
					if item.Kind == domain.ItemOpaque {
						if item.Opaque.Kind != "reasoning_content" || item.Opaque.Provider != "openai" {
							t.Fatal("reasoning did not use the existing opaque path")
						}
						reasoning.Write(item.Opaque.Payload)
					}
				}
				if tc.structured != "" && !strings.Contains(reasoning.String(), tc.structured) {
					t.Fatal("structured reasoning was lost")
				}
				if strings.Contains(tc.content, "private") && !strings.Contains(reasoning.String(), "private") {
					t.Fatal("inline reasoning was not retained privately")
				}
			})
		}
	}
}

func TestChatDoesNotPublishAmbiguousPrefix(t *testing.T) {
	a := newAssembler()
	a.onDelta = func(d string) { t.Errorf("published unclassified content: %q", d) }
	for _, text := range []string{"private draft", "</thi", "nk>"} {
		data, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": text}}}})
		if err := a.consumeData(data); err != nil {
			t.Fatal(err)
		}
	}
	if resp := a.response(); resp.Text() != "" || resp.Complete {
		t.Fatal("cut-off reasoning was visible or marked complete")
	}
}
