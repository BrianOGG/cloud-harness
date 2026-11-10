package security

import (
	"testing"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/domain"
)

func grant(tool, scope string, until time.Time) domain.Grant {
	return domain.Grant{ID: "g", Tool: tool, Op: "call", ResourceScope: scope, State: "active", RunID: "run", ValidUntil: until}
}

func TestGrantDoesNotWidenScope(t *testing.T) {
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	g := []domain.Grant{grant("write_file", "/workspace/proj", time.Time{})}
	ok := Decide(g, Request{RunID: "run", Tool: "write_file", SideEffectClass: SideReversible, Resources: []string{"/workspace/proj/a.txt"}, Now: now})
	if !ok.Allow {
		t.Fatalf("in-scope denied: %+v", ok)
	}
	wider := Decide(g, Request{RunID: "run", Tool: "write_file", SideEffectClass: SideReversible, Resources: []string{"/workspace/other/a.txt"}, Now: now})
	if wider.Allow || wider.NeedsApproval {
		t.Fatalf("scope widened: %+v", wider)
	}
	otherTool := Decide(g, Request{RunID: "run", Tool: "read_file", Readonly: true, SideEffectClass: SideNone, Resources: []string{"/workspace/proj/a.txt"}, Now: now})
	if otherTool.Allow {
		t.Fatal("grant applied to a different tool")
	}
}

func TestExpiredUnresolvedAndDefaultReadonly(t *testing.T) {
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	expired := Decide([]domain.Grant{grant("echo", "", now.Add(-time.Minute))}, Request{RunID: "run", Tool: "echo", Readonly: true, SideEffectClass: SideNone, Now: now})
	if expired.Allow {
		t.Fatal("expired grant allowed")
	}
	unresolved := Decide(nil, Request{Tool: "read_file", UnresolvedResources: true, Now: now})
	if unresolved.Allow || unresolved.Reason == "" {
		t.Fatal(unresolved)
	}
	plain := Decide(nil, Request{Tool: "echo", Readonly: true, SideEffectClass: SideNone, DefaultAllow: []string{"echo"}, Now: now})
	if !plain.Allow {
		t.Fatal(plain)
	}
	withPath := Decide(nil, Request{Tool: "echo", Readonly: true, SideEffectClass: SideNone, DefaultAllow: []string{"echo"}, Resources: []string{"/tmp"}, Now: now})
	if withPath.Allow {
		t.Fatal("default allow ignored a resource")
	}
}
