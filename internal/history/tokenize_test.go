package history

import (
	"strings"
	"testing"
)

func hasToken(a Analysis, tok string) bool {
	for _, t := range a.FTSTokens {
		if t == tok {
			return true
		}
	}
	return false
}

func hasExact(a Analysis, term string, kind ExactKind) bool {
	for _, e := range a.Exact {
		if e.Term == term && e.Kind == kind {
			return true
		}
	}
	return false
}

func TestChineseBigrams(t *testing.T) {
	a := Analyze("云端执行框架负责检索历史")
	for _, tok := range []string{"云端", "执行", "框架", "负责", "检索", "历史"} {
		if !hasToken(a, tok) {
			t.Fatalf("missing bigram %s in %#v", tok, a.FTSTokens)
		}
	}
	// Overlap is required so a query for the same phrase still matches,
	// and a different two-character word does not.
	if hasToken(a, "云计算") || hasToken(a, "云计") {
		t.Fatal("unrelated bigram was emitted")
	}
	plan := PlanQuery("检索")
	if plan.ExactOnly || plan.FTSMatch != `"检索"` {
		t.Fatalf("plan %+v", plan)
	}
	phrase := PlanQuery("检索历史")
	if !strings.Contains(phrase.FTSMatch, `"检索"`) || !strings.Contains(phrase.FTSMatch, `"索历"`) || !strings.Contains(phrase.FTSMatch, `"历史"`) {
		t.Fatalf("phrase match %s", phrase.FTSMatch)
	}
}

func TestPathFileIdentAndID(t *testing.T) {
	a := Analyze("see /var/log/app/server.go and parseToolCallID plus evt_9f3a2c ticket 10086")
	if !hasExact(a, "/var/log/app/server.go", ExactPath) {
		t.Fatalf("path exact missing: %#v", a.Exact)
	}
	if !hasExact(a, "server.go", ExactFile) {
		t.Fatal("filename exact missing")
	}
	if !hasToken(a, "server") || !hasToken(a, "ToolCall") || !hasToken(a, "parseToolCallID") {
		t.Fatalf("fts tokens %#v", a.FTSTokens)
	}
	if !hasExact(a, "evt_9f3a2c", ExactID) {
		t.Fatal("event id exact missing")
	}
	if !hasToken(a, "10086") || !hasExact(a, "10086", ExactID) {
		t.Fatal("numeric id missing")
	}
	fileQ := PlanQuery("server.go")
	if !fileQ.ExactOnly || fileQ.FTSMatch != "" {
		t.Fatalf("file query leaked into fts: %+v", fileQ)
	}
	idQ := PlanQuery("evt_9f3a2c")
	if !idQ.ExactOnly {
		t.Fatal("id query should be exact")
	}
	if PlanQuery("10086").ExactOnly {
		t.Fatal("plain number is an FTS token, not a punctuation id")
	}
	if !hasToken(Analyze("parseToolCallID"), "ToolCall") {
		t.Fatal("camel join ToolCall missing")
	}
}
