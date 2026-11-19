package context

import (
	"bytes"
	"encoding/json"
	"reflect"
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

func TestProjectRulesBetweenStaticPrefixAndHistory(t *testing.T) {
	recent := []Message{{Role: "assistant", Content: "earlier answer"}}
	base := Assemble("system", "tools", "memory", recent, "user")
	withRules := WithProjectRules(base, "Project rules\n<!-- AGENTS.md: global -->\nglobal rule\n<!-- AGENTS.md: AGENTS.md -->\nroot rule")
	if len(withRules.StaticPrefix) != 3 || withRules.StaticPrefix[2].Role != "system" {
		t.Fatalf("rules are not a separate system section: %+v", withRules)
	}
	if !reflect.DeepEqual(base.StaticPrefix, withRules.StaticPrefix[:2]) || !reflect.DeepEqual(base.DynamicSuffix, withRules.DynamicSuffix) {
		t.Fatal("project rules changed the static instructions or session history")
	}
	compacted := Compact(append(append([]Message(nil), withRules.StaticPrefix...), withRules.DynamicSuffix...), CompactOptions{
		PrefixCount: len(withRules.StaticPrefix), KeepIndex: 5, TokenLimit: 1,
	})
	if PrefixHash(compacted[:3]) != PrefixHash(withRules.StaticPrefix) {
		t.Fatal("compaction rewrote frozen project rules")
	}
}

func TestRepoMapStartsDynamicSuffixAfterRules(t *testing.T) {
	base := Assemble("system", "tools", "memory", []Message{{Role: "assistant", Content: "earlier"}}, "user")
	base = WithProjectRules(base, "project rules")
	withMap := WithRepoMap(base, "src/main.go\tlines=3\n")
	if len(withMap.StaticPrefix) != 3 || withMap.StaticPrefix[2].Content != "project rules" {
		t.Fatalf("repo map changed the static prefix: %+v", withMap.StaticPrefix)
	}
	if len(withMap.DynamicSuffix) != 4 || withMap.DynamicSuffix[0].Content != "Repo-Map:\nsrc/main.go\tlines=3\n" || withMap.DynamicSuffix[1].Content != "memory" {
		t.Fatalf("repo map is not before dynamic context: %+v", withMap.DynamicSuffix)
	}
	if len(base.DynamicSuffix) != 3 || base.DynamicSuffix[0].Content != "memory" {
		t.Fatalf("repo map mutated the source assembly: %+v", base.DynamicSuffix)
	}
}

func TestEmptyRepoMapLeavesAssemblyUnchanged(t *testing.T) {
	base := Assemble("system", "tools", "memory", nil, "user")
	got := WithRepoMap(base, "")
	if !reflect.DeepEqual(got, base) {
		t.Fatalf("empty repo map changed assembly: got=%+v want=%+v", got, base)
	}
}

func TestNoProjectRulesByteForByteRegression(t *testing.T) {
	// Literal pre-M11-3 wire shape: a missing rules file must add no section,
	// whitespace, or metadata, including when memory and history are populated.
	const want = `{"static_prefix":[{"role":"system","content":"system"},{"role":"system","content":"tools"}],"dynamic_suffix":[{"role":"system","content":"memory"},{"role":"assistant","content":"earlier"},{"role":"user","content":"user"}]}`
	asm := WithProjectRules(Assemble("system", "tools", "memory", []Message{{Role: "assistant", Content: "earlier"}}, "user"), "")
	got, err := json.Marshal(asm)
	if err != nil || string(got) != want {
		t.Fatalf("empty rules changed assembly bytes:\n%s\nerror=%v", got, err)
	}
	const wantEmpty = `{"static_prefix":[{"role":"system","content":"system"}],"dynamic_suffix":[{"role":"user","content":"user"}]}`
	got, err = json.Marshal(WithProjectRules(Assemble("system", "", "", nil, "user"), ""))
	if err != nil || string(got) != wantEmpty {
		t.Fatalf("empty assembly changed bytes:\n%s\nerror=%v", got, err)
	}
}
