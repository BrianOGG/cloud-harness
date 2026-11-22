// Package subagent keeps the delegation contract. The live child runner is the
// agent manager. Disabled refuses delegation and does not create a run.
package subagent

import (
	"context"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

// Request is the delegate_task contract. Fields are accepted and ignored.
type Request struct {
	ParentRunID     string
	Goal            string
	RelevantContext string
	SuccessCriteria string
	AllowedTools    []string
	Budget          domain.Budget
	OutputContract  string
}

// Runner delegates a child run. The only M0 implementation refuses.
type Runner interface {
	Delegate(ctx context.Context, req Request) error
}

// Disabled refuses delegation. It does not create a run.
type Disabled struct{}

// Delegate returns not_implemented and performs no work.
func (Disabled) Delegate(context.Context, Request) error {
	return domain.E(domain.CodeNotImplemented, "sub-agent delegation is not enabled until M4")
}
