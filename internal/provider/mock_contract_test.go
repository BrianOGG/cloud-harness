package provider

import (
	"bytes"
	"context"
	"testing"

	"github.com/cloud-harness/cloud-harness/internal/domain"
)

func TestMockContinuationRoundTripKeepsOpaqueBytes(t *testing.T) {
	sig := []byte("SIG-MOCK-OPAQUE-1")
	payload := []byte(`{"reasoning":"SECRET-MOCK-PAYLOAD"}`)
	scripted := domain.ModelResponse{
		Provider: "mock",
		Complete: true,
		Items: []domain.ResponseItem{
			{Ordinal: 0, Kind: domain.ItemText, Text: "visible"},
			{Ordinal: 1, Kind: domain.ItemOpaque, Opaque: &domain.OpaqueItem{Provider: "mock", Kind: "reasoning", Payload: payload, Signature: sig}},
			{Ordinal: 2, Kind: domain.ItemToolCall, ToolCall: &domain.ToolCall{ProviderCallID: "c1", Name: "read_file", Arguments: []byte(`{"path":"a"}`)}},
		},
	}
	mock := &Mock{Responses: []domain.ModelResponse{scripted}}
	first, err := mock.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "go"}}})
	if err != nil {
		t.Fatal(err)
	}
	cont, degraded, err := domain.PrepareContinuation("mock", first)
	if err != nil || degraded {
		t.Fatalf("degraded=%v err=%v", degraded, err)
	}
	if _, err := mock.Complete(context.Background(), Request{
		Messages:             []Message{{Role: "user", Content: "go"}},
		ContinuationProvider: "mock",
		Continuation:         cont,
	}); err != nil {
		t.Fatal(err)
	}
	got := mock.Requests[1].Continuation
	if len(got) != 3 || got[1].Kind != domain.ItemOpaque {
		t.Fatalf("continuation %#v", got)
	}
	if !bytes.Equal(got[1].Opaque.Signature, sig) || !bytes.Equal(got[1].Opaque.Payload, payload) {
		t.Fatal("mock continuation changed opaque bytes")
	}
	if got[2].ToolCall.ProviderCallID != "c1" || got[0].Text != "visible" {
		t.Fatal("order changed")
	}
	// Mutating the recorded copy must not change the scripted source.
	got[1].Opaque.Signature[0] = 'X'
	if scripted.Items[1].Opaque.Signature[0] == 'X' {
		t.Fatal("request log aliases the scripted signature")
	}
}
