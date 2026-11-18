// Package security decides whether a tool call is authorized.
// It does not execute tools. The broker is the only caller that acts on a decision.
package security

import (
	"path"
	"strings"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/domain"
)

const (
	SideNone         = "none"
	SideReversible   = "reversible"
	SideIrreversible = "irreversible"
)

// Request is one broker call. Resources are paths or ids already extracted
// from arguments. UnresolvedResources means extraction failed; the decision is deny.
type Request struct {
	RunID               string
	SessionID           string
	InvocationID        string
	Tool                string
	SideEffectClass     string
	Readonly            bool
	Resources           []string
	UnresolvedResources bool
	DefaultAllow        []string
	ApprovalPossible    bool
	SensitiveSeen       bool
	Egress              bool
	Now                 time.Time
}

// Decision is the permission result. Allow and NeedsApproval are mutually exclusive.
type Decision struct {
	Allow         bool
	NeedsApproval bool
	Reason        string
}

// Decide applies scoped grants. A grant for the same tool name with a different
// resource scope does not match. Expired and revoked grants are ignored.
func Decide(grants []domain.Grant, req Request) Decision {
	if req.Now.IsZero() {
		req.Now = time.Now()
	}
	if req.UnresolvedResources {
		return Decision{Reason: "resource scope could not be resolved"}
	}
	if req.SensitiveSeen && req.Egress {
		for _, g := range grants {
			if g.State == "active" && g.Tool == req.Tool && g.Op == "egress" && (g.RunID == "" || g.RunID == req.RunID) {
				return Decision{Allow: true, Reason: "explicit egress grant"}
			}
		}
		return Decision{Reason: "sensitive data cannot be sent externally"}
	}
	for _, g := range grants {
		if !grantMatches(g, req) {
			continue
		}
		return Decision{Allow: true, Reason: "matching grant"}
	}
	if req.Readonly && req.SideEffectClass == SideNone && listed(req.DefaultAllow, req.Tool) && len(req.Resources) == 0 {
		return Decision{Allow: true, Reason: "readonly tool with no resource scope"}
	}
	if req.ApprovalPossible && (req.SideEffectClass == SideReversible || req.SideEffectClass == SideIrreversible) {
		return Decision{NeedsApproval: true, Reason: "side effect requires approval"}
	}
	return Decision{Reason: "no matching grant"}
}

func grantMatches(g domain.Grant, req Request) bool {
	if g.State != "" && g.State != "active" {
		return false
	}
	if g.Tool != req.Tool {
		return false
	}
	if g.Op != "" && g.Op != "call" {
		return false
	}
	if !g.ValidUntil.IsZero() && !g.ValidUntil.After(req.Now) {
		return false
	}
	if g.RunID != "" && g.RunID != req.RunID {
		return false
	}
	if g.SessionID != "" && g.SessionID != req.SessionID {
		return false
	}
	if g.InvocationID != "" {
		if req.InvocationID == "" || g.InvocationID != req.InvocationID || g.RunID == "" {
			return false
		}
		return exactResourcesAllowed(g.ResourceScope, req.Resources, req.Tool)
	}
	return resourcesAllowed(g.ResourceScope, req.Resources)
}

func exactResourcesAllowed(scope string, resources []string, tool string) bool {
	if len(resources) == 0 {
		return scope == ""
	}
	if scope == "" || len(resources) != 1 {
		return false
	}
	if tool == "run_command" {
		return scope == resources[0]
	}
	return clean(scope) == clean(resources[0])
}

func resourcesAllowed(scope string, resources []string) bool {
	if len(resources) == 0 {
		return scope == ""
	}
	if scope == "" {
		return false
	}
	scope = clean(scope)
	for _, r := range resources {
		r = clean(r)
		if r != scope && !strings.HasPrefix(r, scope+"/") {
			return false
		}
	}
	return true
}

func clean(p string) string {
	p = strings.ReplaceAll(p, `\`, `/`)
	if p == "" {
		return ""
	}
	return path.Clean(p)
}

func listed(items []string, want string) bool {
	for _, it := range items {
		if it == want {
			return true
		}
	}
	return false
}
