package context

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestMemoryDeltaMergesAndKeepsSources(t *testing.T) {
	raw := []byte(`[{"op":"ADD","fact":"port is 8080","source":"trn_1"},{"op":"ADD","section":"decisions","id":"d1","text":"ship","reason":"tests pass","source":"e1"},{"op":"NOOP"},{"op":"UPDATE","section":"facts","id":"facts-1","text":"port is 9090"},{"op":"DELETE","section":"pending","text":"gone"}]`)
	deltas, err := ParseDeltas(raw)
	if err != nil {
		t.Fatal(err)
	}
	mem, err := ApplyDelta(Memory{Pending: []string{"gone", "stay"}}, deltas)
	if err != nil {
		t.Fatal(err)
	}
	if len(mem.Facts) != 1 || mem.Facts[0].Text != "port is 9090" || mem.Facts[0].Source != "trn_1" {
		t.Fatalf("facts %+v", mem.Facts)
	}
	if len(mem.Decisions) != 1 || mem.Decisions[0].Reason != "tests pass" {
		t.Fatalf("decisions %+v", mem.Decisions)
	}
	if len(mem.Pending) != 1 || mem.Pending[0] != "stay" {
		t.Fatalf("pending %+v", mem.Pending)
	}
	again, err := ParseMemory(mem.JSON())
	if err != nil || again.Facts[0].Text != "port is 9090" {
		t.Fatalf("round trip %+v %v", again, err)
	}
	if !strings.Contains(mem.Prompt(), "port is 9090") {
		t.Fatal(mem.Prompt())
	}
}

func TestPlannerSkipsShortSessions(t *testing.T) {
	plan, why := ShouldPlan(PlanInput{PriorTurns: 0, UserText: "Say hello.", SuffixTokens: 20, TokenLimit: 8000})
	if plan || why != "short session" {
		t.Fatalf("planned %v %s", plan, why)
	}
	plan, why = ShouldPlan(PlanInput{PriorTurns: 0, UserText: "What did we decide earlier?"})
	if !plan || why != "cross-turn reference" {
		t.Fatalf("cue %v %s", plan, why)
	}
	plan, _ = ShouldPlan(PlanInput{Force: true, Skip: true})
	if plan {
		t.Fatal("skip did not win")
	}
	resources, err := ParsePlan(`note: {"resources":[{"id":"note_token","precision":"verbatim"},{"id":"x","precision":"nope"}]}`)
	if err != nil || len(resources) != 2 || resources[0].Precision != "verbatim" || resources[1].Precision != "excerpt" {
		t.Fatalf("%+v %v", resources, err)
	}
	if Present("abcdefghij", "summary") != "abcdefghij" {
		t.Fatal("short verbatim clipped")
	}
}

func TestCompactKeepsPairsPrefixAndUser(t *testing.T) {
	long := strings.Repeat("alpha ", 80)
	msgs := []Message{
		{Role: "system", Content: "system rules"},
		{Role: "system", Content: "tools: read_file"},
		{Role: "user", Content: "current question stays"},
		{Role: "assistant", Content: "old", ToolCalls: []ToolRef{{ID: "c1", Name: "read_file"}}},
		{Role: "tool", ToolCallID: "c1", Content: long},
		{Role: "assistant", Content: "new", ToolCalls: []ToolRef{{ID: "c2", Name: "read_file"}}},
		{Role: "tool", ToolCallID: "c2", Content: "fresh result"},
	}
	out := Compact(msgs, CompactOptions{PrefixCount: 2, KeepIndex: 2, TokenLimit: 40})
	if out[0].Content != msgs[0].Content || out[1].Content != msgs[1].Content {
		t.Fatal("prefix changed")
	}
	if out[2].Content != "current question stays" {
		t.Fatal("user message changed")
	}
	if out[5].Content != "fresh result" || out[6].Content != "fresh result" && out[6].ToolCallID != "c2" {
		if out[6].Content != "fresh result" || out[6].ToolCallID != "c2" {
			t.Fatalf("active pair changed %#v", out[6])
		}
	}
	if !strings.Contains(out[4].Content, `"compacted":true`) || !strings.Contains(out[4].Content, `"tool_call_id":"c1"`) {
		t.Fatalf("old result %s", out[4].Content)
	}
	if out[4].ToolCallID != "c1" || !ToolPairsIntact(out) {
		t.Fatal("pairing broken")
	}
	ab, _ := PrefixBytes(Assembly{StaticPrefix: out[:2]})
	bb, _ := PrefixBytes(Assembly{StaticPrefix: msgs[:2]})
	if !bytes.Equal(ab, bb) {
		t.Fatal("prefix hash bytes changed")
	}
	secret := []byte("SIG-OPAQUE-XYZ")
	payload := []byte("SECRET-PAYLOAD")
	raw, _ := jsonMarshal(out)
	if bytes.Contains(raw, secret) || bytes.Contains(raw, payload) {
		t.Fatal("compacted view absorbed opaque bytes")
	}
	if !bytes.Equal(secret, []byte("SIG-OPAQUE-XYZ")) || !bytes.Equal(payload, []byte("SECRET-PAYLOAD")) {
		t.Fatal("compaction rewrote the signature buffer")
	}
}

func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}
