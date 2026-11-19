package agent

import (
	"encoding/json"
	"testing"

	"github.com/cloud-harness/cloud-harness/internal/config"
	"github.com/cloud-harness/cloud-harness/internal/storage"
)

func TestSummarizeOracleEvents(t *testing.T) {
	events := []storage.Event{
		{Type: "oracle.asked", Payload: json.RawMessage(`{"model":"model-a","input_tokens":1000000,"output_tokens":500000}`)},
		{Type: "model.completed", Payload: json.RawMessage(`not an oracle event`)},
		{Type: "oracle.asked", Payload: json.RawMessage(`{"model":"model-b","input_tokens":9000000,"output_tokens":9000000}`)},
		{Type: "oracle.asked", Payload: json.RawMessage(`{"model":"model-a","input_tokens":2000000,"output_tokens":0}`)},
		{Type: "oracle.failed", Payload: json.RawMessage(`{"reason":"budget"}`)},
		{Type: "oracle.failed", Payload: json.RawMessage(`{"reason":"upstream_error"}`)},
	}
	prices := map[string]config.ModelPrice{
		"model-a": {Input: 0.25, Output: 0.5},
	}

	summary, err := SummarizeOracleEvents(events, prices)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Consultations != 3 || summary.CostUSD != 1 || summary.BudgetFailures != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	if got := summary.ByModel["model-a"]; got.ModelCalls != 2 || got.CostUSD != 1 {
		t.Fatalf("model-a summary = %+v", got)
	}
	if got := summary.ByModel["model-b"]; got.ModelCalls != 1 || got.CostUSD != 0 {
		t.Fatalf("unpriced model summary = %+v", got)
	}
}

func TestSummarizeOracleEventsEmpty(t *testing.T) {
	summary, err := SummarizeOracleEvents(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Consultations != 0 || summary.CostUSD != 0 || summary.BudgetFailures != 0 || len(summary.ByModel) != 0 {
		t.Fatalf("empty summary = %+v", summary)
	}
}

func TestSummarizeOracleEventsRejectsInvalidPayload(t *testing.T) {
	for _, payload := range []string{
		`not json`,
		`{"model":"","input_tokens":0,"output_tokens":0}`,
		`{"model":"model-a","input_tokens":10}`,
		`{"model":"model-a","input_tokens":-1,"output_tokens":0}`,
		`{"model":"model-a","input_tokens":0,"output_tokens":"many"}`,
	} {
		t.Run(payload, func(t *testing.T) {
			_, err := SummarizeOracleEvents([]storage.Event{{Type: "oracle.asked", Payload: json.RawMessage(payload)}}, nil)
			if err == nil {
				t.Fatal("invalid oracle.asked payload was accepted")
			}
		})
	}
}
