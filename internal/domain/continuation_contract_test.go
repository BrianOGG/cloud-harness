package domain

import (
	"bytes"
	"encoding/json"
	"testing"
)

func opaqueResponse() ModelResponse {
	return ModelResponse{
		Provider:           "anthropic",
		ProviderResponseID: "msg_1",
		Complete:           true,
		Items: []ResponseItem{
			{Ordinal: 0, Kind: ItemText, Text: "thinking out loud is visible text"},
			{Ordinal: 1, Kind: ItemOpaque, Opaque: &OpaqueItem{
				Provider:  "anthropic",
				Kind:      "thinking",
				Payload:   []byte(`{"thinking":"SECRET-PAYLOAD-7f3a","signature":"should-not-be-rewritten"}`),
				Signature: []byte("SIG-OPAQUE-XYZ-001"),
			}},
			{Ordinal: 2, Kind: ItemToolCall, ToolCall: &ToolCall{
				ProviderCallID: "toolu_1",
				Name:           "read_file",
				Arguments:      json.RawMessage(`{"path":"main.go"}`),
			}},
			{Ordinal: 3, Kind: ItemOpaque, Opaque: &OpaqueItem{
				Provider:  "anthropic",
				Kind:      "redacted_thinking",
				Payload:   []byte("REDACTED-BLOCK-22"),
				Signature: []byte("SIG-OPAQUE-XYZ-002"),
			}},
			{Ordinal: 4, Kind: ItemUsage, Usage: &Usage{InputTokens: 10, OutputTokens: 4, CachedInputTokens: 3}},
		},
	}
}

func TestOpaqueReplayPreservesOrderAndSignatureBytes(t *testing.T) {
	resp := opaqueResponse()
	replayed, err := Replay(resp.Provider, resp.Items)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed) != len(resp.Items) {
		t.Fatalf("len %d", len(replayed))
	}
	for i := range resp.Items {
		if replayed[i].Kind != resp.Items[i].Kind || replayed[i].Ordinal != i {
			t.Fatalf("item %d reordered: %+v", i, replayed[i])
		}
		if resp.Items[i].Opaque != nil {
			if !bytes.Equal(replayed[i].Opaque.Payload, resp.Items[i].Opaque.Payload) {
				t.Fatal("payload changed")
			}
			if !bytes.Equal(replayed[i].Opaque.Signature, resp.Items[i].Opaque.Signature) {
				t.Fatal("signature changed")
			}
		}
	}
	replayed[1].Opaque.Signature[0] = 'X'
	replayed[1].Opaque.Payload[0] = 'X'
	if resp.Items[1].Opaque.Signature[0] == 'X' || resp.Items[1].Opaque.Payload[0] == 'X' {
		t.Fatal("replay aliases stored opaque bytes")
	}
}

func TestTraceAndIndexExcludeOpaque(t *testing.T) {
	resp := opaqueResponse()
	view := TraceView(resp.Items)
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if ContainsOpaqueBytes(raw, resp.Items) || bytes.Contains(raw, []byte("SECRET-PAYLOAD")) || bytes.Contains(raw, []byte("SIG-OPAQUE")) || bytes.Contains(raw, []byte("REDACTED-BLOCK")) {
		t.Fatalf("trace leaked opaque: %s", raw)
	}
	for _, it := range view {
		if it.Kind == ItemOpaque {
			t.Fatal("opaque item in trace view")
		}
	}
	if !bytes.Contains(raw, []byte("thinking out loud")) {
		t.Fatal("visible text dropped")
	}
	indexed := IndexableText(resp.Items)
	if indexed != "thinking out loud is visible text" {
		t.Fatalf("index text %q", indexed)
	}
	if ContainsOpaqueBytes([]byte(indexed), resp.Items) {
		t.Fatal("index contains opaque")
	}
	visible, blob, err := SplitForStorage(resp)
	if err != nil {
		t.Fatal(err)
	}
	visRaw, _ := json.Marshal(visible)
	if ContainsOpaqueBytes(visRaw, resp.Items) || bytes.Contains(visRaw, []byte("SECRET-PAYLOAD")) || bytes.Contains(visRaw, []byte("SIG-OPAQUE")) || bytes.Contains(visRaw, []byte("REDACTED-BLOCK")) {
		t.Fatalf("visible rows contain opaque: %s", visRaw)
	}
	var ob OpaqueBlob
	if err := json.Unmarshal(blob, &ob); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ob.Items[0].Payload, resp.Items[1].Opaque.Payload) || !bytes.Equal(ob.Items[0].Signature, resp.Items[1].Opaque.Signature) {
		t.Fatal("blob lost opaque bytes")
	}
	if !bytes.Equal(ob.Items[1].Payload, resp.Items[3].Opaque.Payload) {
		t.Fatal("blob reordered opaque items")
	}
	merged, err := MergeFromStorage(resp.Provider, visible, blob)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Replay(resp.Provider, merged)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again[1].Opaque.Signature, resp.Items[1].Opaque.Signature) || !bytes.Equal(again[3].Opaque.Payload, resp.Items[3].Opaque.Payload) {
		t.Fatal("round trip changed opaque bytes or order")
	}
	if again[2].ToolCall.ProviderCallID != "toolu_1" {
		t.Fatal("tool call moved")
	}
}

func TestContinuationDoesNotCrossProviders(t *testing.T) {
	resp := opaqueResponse()
	if err := ContinuationCompatible(resp.Provider, "openai"); !IsCode(err, CodeProviderMismatch) {
		t.Fatalf("got %v", err)
	}
	items, degraded, err := PrepareContinuation("openai", resp)
	if err != nil {
		t.Fatal(err)
	}
	if !degraded {
		t.Fatal("expected degradation")
	}
	raw, _ := json.Marshal(items)
	if ContainsOpaqueBytes(raw, resp.Items) {
		t.Fatal("degraded continuation still carries opaque bytes")
	}
	same, degraded, err := PrepareContinuation("anthropic", resp)
	if err != nil || degraded {
		t.Fatalf("same provider degraded=%v err=%v", degraded, err)
	}
	if !bytes.Equal(same[1].Opaque.Signature, resp.Items[1].Opaque.Signature) {
		t.Fatal("same-provider replay dropped signature")
	}
	if _, err := Replay("openai", resp.Items); !IsCode(err, CodeProviderMismatch) {
		t.Fatalf("replay across providers: %v", err)
	}
}

func TestToolResultPairingHelper(t *testing.T) {
	resp := ModelResponse{
		Provider: "mock", Complete: true,
		Items: []ResponseItem{
			toolItem(0, "c1", "read_file", `{"path":"a"}`),
			toolItem(1, "c2", "read_file", `{"path":"b"}`),
		},
	}
	batch, err := PlanBatch("run", resp, seqID())
	if err != nil {
		t.Fatal(err)
	}
	if err := Drive(&batch, nil, func(inv Invocation) (json.RawMessage, error) {
		return json.RawMessage(`{"id":"` + inv.ProviderCallID + `"}`), nil
	}); err != nil {
		t.Fatal(err)
	}
	results := []ToolResultView{}
	for _, inv := range batch.Invocations {
		results = append(results, ToolResultView{ProviderCallID: inv.ProviderCallID, OK: inv.Result.OK, Data: inv.Result.Data})
	}
	if err := PairToolResults(resp.ToolCalls(), results); err != nil {
		t.Fatal(err)
	}
	results[0].ProviderCallID = "other"
	if err := PairToolResults(resp.ToolCalls(), results); err == nil {
		t.Fatal("mismatched call id was accepted")
	}
}
