package security

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

func TestPropertyGrantScopeDoesNotExpand(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 7))
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	grant := domain.Grant{Tool: "read_file", Op: "call", State: "active", RunID: "run", ResourceScope: "/ws/project"}
	for n := 0; n < 40; n++ {
		suffix := string(rune('a' + rng.IntN(26)))
		outside := []string{
			"/ws/project" + suffix,
			"/ws/other/" + suffix,
			"/etc/" + suffix,
		}
		for _, path := range outside {
			got := Decide([]domain.Grant{grant}, Request{
				RunID: "run", Tool: "read_file", Readonly: true, SideEffectClass: SideNone,
				Resources: []string{path}, Now: now,
			})
			if got.Allow || got.NeedsApproval {
				t.Fatalf("scope expanded to %s: %+v", path, got)
			}
		}
		inside := Decide([]domain.Grant{grant}, Request{
			RunID: "run", Tool: "read_file", Readonly: true, SideEffectClass: SideNone,
			Resources: []string{"/ws/project/notes.txt"}, Now: now,
		})
		if !inside.Allow {
			t.Fatalf("in-scope path denied: %+v", inside)
		}
	}
}
