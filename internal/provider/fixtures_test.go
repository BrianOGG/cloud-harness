package provider

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

func TestAnthropicThinkingPassthroughOrder(t *testing.T) {
	raw := []byte(`{
	  "id": "msg_1",
	  "content": [
	    {"type":"text","text":"Looking now"},
	    {"type":"thinking","thinking":"SECRET-THINK","signature":"SIG-ANTH-1"},
	    {"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"a.txt"}},
	    {"type":"redacted_thinking","data":"REDACTED-BLOCK","signature":"SIG-ANTH-2"}
	  ]
	}`)
	resp, err := ParseAnthropicMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 4 {
		t.Fatalf("len %d", len(resp.Items))
	}
	kinds := []domain.ItemKind{domain.ItemText, domain.ItemOpaque, domain.ItemToolCall, domain.ItemOpaque}
	for i, kind := range kinds {
		if resp.Items[i].Kind != kind || resp.Items[i].Ordinal != i {
			t.Fatalf("item %d %+v", i, resp.Items[i])
		}
	}
	if !bytes.Contains(resp.Items[1].Opaque.Payload, []byte(`"signature":"SIG-ANTH-1"`)) {
		t.Fatalf("thinking payload changed: %s", resp.Items[1].Opaque.Payload)
	}
	if string(resp.Items[1].Opaque.Signature) != "SIG-ANTH-1" || string(resp.Items[3].Opaque.Signature) != "SIG-ANTH-2" {
		t.Fatal("signatures were not preserved")
	}
	if resp.Items[2].ToolCall.ProviderCallID != "toolu_1" {
		t.Fatal("tool call moved")
	}
	replayed, err := domain.Replay("anthropic", resp.Items)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(replayed[1].Opaque.Payload, resp.Items[1].Opaque.Payload) || !bytes.Equal(replayed[3].Opaque.Signature, resp.Items[3].Opaque.Signature) {
		t.Fatal("replay changed thinking bytes or order")
	}
	view := domain.TraceView(resp.Items)
	rawView := mustJSON(t, view)
	if bytes.Contains(rawView, []byte("SECRET-THINK")) || bytes.Contains(rawView, []byte("SIG-ANTH")) || bytes.Contains(rawView, []byte("REDACTED-BLOCK")) {
		t.Fatalf("trace leaked thinking: %s", rawView)
	}
}

func TestOpenAIReasoningPassthroughOrder(t *testing.T) {
	raw := []byte(`{
	  "id": "resp_1",
	  "output": [
	    {"type":"reasoning","id":"rs_1","encrypted_content":"SECRET-REASON","summary":[]},
	    {"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"a.txt\"}"},
	    {"type":"message","content":[{"type":"output_text","text":"done"}]}
	  ]
	}`)
	resp, err := ParseOpenAIResponses(raw)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Items[0].Kind != domain.ItemOpaque || resp.Items[1].Kind != domain.ItemToolCall || resp.Items[2].Kind != domain.ItemText {
		t.Fatalf("order %+v %+v %+v", resp.Items[0].Kind, resp.Items[1].Kind, resp.Items[2].Kind)
	}
	if !bytes.Contains(resp.Items[0].Opaque.Payload, []byte("SECRET-REASON")) {
		t.Fatalf("reasoning payload %s", resp.Items[0].Opaque.Payload)
	}
	if resp.Items[1].ToolCall.ProviderCallID != "call_1" || resp.Items[2].Text != "done" {
		t.Fatal("items were reordered")
	}
	replayed, err := domain.Replay("openai-responses", resp.Items)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(replayed[0].Opaque.Payload, resp.Items[0].Opaque.Payload) {
		t.Fatal("replay changed reasoning bytes")
	}
	if bytes.Contains(mustJSON(t, domain.TraceView(resp.Items)), []byte("SECRET-REASON")) {
		t.Fatal("trace leaked reasoning")
	}
}

func TestAnthropicStopReasonControlsComplete(t *testing.T) {
	for _, tc := range []struct {
		reason   string
		complete bool
	}{
		{"", true},
		{"end_turn", true},
		{"stop_sequence", true},
		{"tool_use", true},
		{"max_tokens", false},
		{"refusal", false},
		{"pause_turn", false},
		{"future_reason", false},
	} {
		raw := []byte(`{"id":"msg_stop","content":[{"type":"text","text":"partial"}],"stop_reason":"` + tc.reason + `"}`)
		resp, err := ParseAnthropicMessage(raw)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Complete != tc.complete {
			t.Fatalf("stop_reason %q complete=%v; want %v", tc.reason, resp.Complete, tc.complete)
		}
	}
}

func TestOpenAIResponsesRefusalBecomesText(t *testing.T) {
	resp, err := ParseOpenAIResponses([]byte(`{"id":"resp_refusal","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"I can't help with that."}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text() != "I can't help with that." {
		t.Fatalf("text %q", resp.Text())
	}
	if _, err := domain.Classify(resp); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
