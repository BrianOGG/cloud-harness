package context

import (
	"bytes"
	"testing"
)

func TestPrefixStableAcrossUserMessages(t *testing.T) {
	const system = "You are Cloud Harness."
	const tools = "tools: none"
	a := Assemble(system, tools, "goal: ship M0", nil, "first")
	b := Assemble(system, tools, "goal: ship M0", nil, "second")
	ab, err := PrefixBytes(a)
	if err != nil {
		t.Fatal(err)
	}
	bb, err := PrefixBytes(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ab, bb) {
		t.Fatalf("prefix changed\n%s\n%s", ab, bb)
	}
	if a.DynamicSuffix[len(a.DynamicSuffix)-1].Content == b.DynamicSuffix[len(b.DynamicSuffix)-1].Content {
		t.Fatal("user message was not dynamic")
	}
	if a.StaticPrefix[0].Content != system || a.StaticPrefix[1].Content != tools {
		t.Fatalf("prefix %#v", a.StaticPrefix)
	}
}
