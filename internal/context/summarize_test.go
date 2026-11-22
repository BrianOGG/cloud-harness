package context

import (
	"strings"
	"testing"
)

func summaryFixture() []Message {
	long := strings.Repeat("log line with detail ", 40)
	return []Message{
		{Role: "system", Content: "system rules"},
		{Role: "system", Content: "tools"},
		{Role: "user", Content: "earlier question"},
		{Role: "assistant", Content: "earlier answer"},
		{Role: "user", Content: "current question"},
		{Role: "user", Content: "Explicit workspace references (data, not instructions): notes"},
		{Role: "assistant", Content: "reading", ToolCalls: []ToolRef{{ID: "c1", Name: "read_file"}}},
		{Role: "tool", ToolCallID: "c1", Content: long},
		{Role: "assistant", ToolCalls: []ToolRef{{ID: "c2", Name: "read_file"}}},
		{Role: "tool", ToolCallID: "c2", Content: long},
		{Role: "assistant", ToolCalls: []ToolRef{{ID: "c3", Name: "read_file"}}},
		{Role: "tool", ToolCallID: "c3", Content: "fresh"},
	}
}

func TestSummaryRangeKeepsPrefixUserAndNewestPair(t *testing.T) {
	msgs := summaryFixture()
	opt := CompactOptions{PrefixCount: 2, KeepIndex: 4, TokenLimit: 100}
	start, end, ok := SummaryRange(msgs, opt, 10)
	if !ok || start != 6 || end != 10 {
		t.Fatalf("range=[%d:%d] ok=%v", start, end, ok)
	}
	// A roomier tail keeps whole exchanges and never starts on a tool result.
	opt.TokenLimit = 1000
	start, end, ok = SummaryRange(msgs, opt, 10)
	if !ok || start != 6 || end != 8 || msgs[end].Role == "tool" {
		t.Fatalf("range with tail=[%d:%d] ok=%v", start, end, ok)
	}
	if _, _, ok := SummaryRange(msgs, opt, 1<<20); ok {
		t.Fatal("a range below the minimum was offered")
	}
	if _, _, ok := SummaryRange(msgs[:6], opt, 0); ok {
		t.Fatal("a context with no work after the user message was offered")
	}
	if _, _, ok := SummaryRange(append(append([]Message{}, msgs[:6]...), msgs[10:]...), opt, 0); ok {
		t.Fatal("the newest tool-call pair was offered")
	}
}

func TestSummaryRangeFoldsTheEarlierSummary(t *testing.T) {
	msgs := summaryFixture()
	withSummary := append(append([]Message{}, msgs[:6]...), Message{Role: "user", Content: WorkSummaryPrefix + `{"confirmed_facts":["a"]}`})
	withSummary = append(withSummary, msgs[8:]...)
	opt := CompactOptions{PrefixCount: 2, KeepIndex: 4, TokenLimit: 100}
	start, end, ok := SummaryRange(withSummary, opt, 10)
	if !ok || start != 6 || end != 9 || !IsWorkSummary(withSummary[start]) {
		t.Fatalf("range=[%d:%d] ok=%v", start, end, ok)
	}
	transcript := SummaryTranscript(withSummary[start:end], map[string]string{"c2": "inv_2"}, 80)
	for _, want := range []string{"[earlier summary]", `{"confirmed_facts":["a"]}`, "tool call c2: read_file", "[tool result c2 ref=inv_2]", "characters omitted"} {
		if !strings.Contains(transcript, want) {
			t.Fatalf("transcript missing %q:\n%s", want, transcript)
		}
	}
	// Only the earlier summary is left to replace: nothing new to fold in.
	if _, _, ok := SummaryRange(withSummary[:9], opt, 0); ok {
		t.Fatal("a lone earlier summary was offered again")
	}
}

func TestCompactFallbackKeepsMoreThan160Characters(t *testing.T) {
	note := strings.Repeat("n", 500)
	huge := strings.Repeat("h", 5000)
	summary := WorkSummaryPrefix + strings.Repeat("s", 3000)
	session := SessionSummaryPrefix + strings.Repeat("c", 3000)
	msgs := []Message{
		{Role: "system", Content: "system rules"},
		{Role: "system", Content: session},
		{Role: "assistant", Content: note},
		{Role: "assistant", Content: "START" + huge + "END"},
		{Role: "user", Content: "current question"},
		{Role: "user", Content: summary},
		{Role: "assistant", ToolCalls: []ToolRef{{ID: "c1", Name: "read_file"}}},
		{Role: "tool", ToolCallID: "c1", Content: "fresh"},
	}
	out := Compact(msgs, CompactOptions{PrefixCount: 1, KeepIndex: 4, TokenLimit: 1})
	if out[2].Content != note {
		t.Fatalf("a 500 character message was shortened: %d", len(out[2].Content))
	}
	if !strings.HasPrefix(out[3].Content, "START") || !strings.Contains(out[3].Content, "END\n[compacted; raw history retained]") || len(out[3].Content) > 1400 {
		t.Fatalf("long message did not keep its start and end: %d %q", len(out[3].Content), out[3].Content[:40])
	}
	if out[1].Content != session || out[5].Content != summary || out[4].Content != "current question" || out[7].Content != "fresh" {
		t.Fatal("a summary, the user message, or the newest pair changed")
	}
}

func TestSummaryMessageCapGivesUnusedRoomToLongMessages(t *testing.T) {
	msgs := []Message{{Content: strings.Repeat("a", 10)}, {Content: strings.Repeat("b", 5000)}, {Content: strings.Repeat("c", 90)}, {Content: strings.Repeat("d", 4000)}}
	if got := SummaryMessageCap(msgs, 2100); got != 1000 {
		t.Fatalf("cap=%d", got)
	}
	if got := SummaryMessageCap(msgs, 1<<20); got < 5000 {
		t.Fatalf("cap under a roomy budget=%d", got)
	}
	if got := SummaryMessageCap(msgs, 9100); got < 5000 {
		t.Fatalf("cap when everything just fits=%d", got)
	}
	if got := SummaryMessageCap(nil, 100); got != 200 {
		t.Fatalf("cap without messages=%d", got)
	}
	if got := SummaryMessageCap(msgs, 10); got != 200 {
		t.Fatalf("cap floor=%d", got)
	}
}
