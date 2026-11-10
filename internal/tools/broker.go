// Package tools is the only execution path for a tool. Callers that need a
// side effect go through Broker, which checks arguments, budget, and permission
// before the executor runs.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/security"
)

// Spec describes one local tool. Execute is stored by the registry and is not
// a second public entry point; Broker is what runs it.
type Spec struct {
	Name               string
	Description        string
	Required           []string
	Timeout            time.Duration
	Readonly           bool
	Idempotent         bool
	SideEffectClass    string
	Pollable           bool
	DataClassification string
	EgressScope        string
	ExtractResources   func(args json.RawMessage) ([]string, error)
	// StatusProbe reads external state and must not perform the side effect.
	// Known is false when the probe cannot prove the call landed.
	StatusProbe func(ctx context.Context, args json.RawMessage) (Probe, error)
	Execute     func(ctx context.Context, args json.RawMessage) (json.RawMessage, error)
	Parameters  json.RawMessage
}

// Charger is the budget ledger. The production implementation is the SQLite store.
type Charger interface {
	Charge(ctx context.Context, runID string, kind domain.ChargeKind, n int) error
}

// GrantSource loads the grants that are allowed to authorize a call.
type GrantSource interface {
	ListGrants(ctx context.Context, runID, sessionID string, now time.Time) ([]domain.Grant, error)
}

// Probe is a status check that does not execute the tool.
// Known is true only when the external fact proves the call already landed.
type Probe struct {
	Known bool
	OK    bool
	Data  json.RawMessage
}

// Result is the broker's structured outcome. Dispatched is true only when Execute ran.
type Result struct {
	OK            bool
	Data          json.RawMessage
	Error         *domain.Error
	Dispatched    bool
	NeedsApproval bool
}

// Request is one invocation.
type Request struct {
	RunID            string
	SessionID        string
	Tool             string
	Args             json.RawMessage
	IdempotencyKey   string
	DefaultAllow     []string
	ApprovalPossible bool
	SensitiveSeen    bool
	Egress           bool
	Now              time.Time
}

// Broker is the single tool entry point.
type Broker struct {
	reg    map[string]Spec
	charge Charger
	grants GrantSource
	now    func() time.Time
}

// NewBroker builds a broker. charge and grants are required so a call cannot skip them.
func NewBroker(charge Charger, grants GrantSource) *Broker {
	return &Broker{reg: map[string]Spec{}, charge: charge, grants: grants, now: time.Now}
}

// Register adds a local tool. Registering does not authorize it.
func (b *Broker) Register(spec Spec) {
	b.reg[spec.Name] = spec
}

// Lookup returns a copy of the spec without exposing Execute to other packages
// beyond the func field on the value. Callers outside this package should use Invoke.
// Fact is the authorization slice of a registered tool.
type Fact struct {
	Name       string
	Readonly   bool
	Idempotent bool
	Pollable   bool
	SideEffect string
}

// Lookup returns the authorization facts for a registered tool.
func (b *Broker) Lookup(name string) (Fact, bool) {
	spec, ok := b.reg[name]
	if !ok {
		return Fact{}, false
	}
	return Fact{
		Name: spec.Name, Readonly: spec.Readonly, Idempotent: spec.Idempotent,
		Pollable: spec.Pollable, SideEffect: spec.SideEffectClass,
	}, true
}

func (b *Broker) Has(name string) bool {
	_, ok := b.reg[name]
	return ok
}

// PublicSpec is the tool description sent to a model. It has no executor.
type PublicSpec struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// List returns registered tools in name order.
func (b *Broker) List() []PublicSpec {
	names := make([]string, 0, len(b.reg))
	for name := range b.reg {
		names = append(names, name)
	}
	sortStrings(names)
	out := make([]PublicSpec, 0, len(names))
	for _, name := range names {
		spec := b.reg[name]
		out = append(out, PublicSpec{Name: spec.Name, Description: spec.Description, Parameters: spec.Parameters})
	}
	return out
}

// Probe asks the tool whether the side effect is already visible.
// It does not charge budget and does not call Execute.
func (b *Broker) Probe(ctx context.Context, req Request) (Probe, error) {
	spec, ok := b.reg[req.Tool]
	if !ok || spec.StatusProbe == nil {
		return Probe{}, nil
	}
	return spec.StatusProbe(ctx, req.Args)
}

// Authorize is the permission decision without a charge or an execution.
func (b *Broker) Authorize(ctx context.Context, req Request) (Result, error) {
	_, decision, err := b.decide(ctx, req)
	if err != nil {
		var de *domain.Error
		if errors.As(err, &de) && (de.Code == domain.CodeToolError || de.Code == domain.CodeInvalidArgument) {
			return Result{Error: de}, nil
		}
		return Result{}, err
	}
	if decision.NeedsApproval {
		return Result{NeedsApproval: true, Error: domain.E(domain.CodeAuthDenied, decision.Reason)}, nil
	}
	if !decision.Allow {
		return Result{Error: domain.E(domain.CodeAuthDenied, decision.Reason)}, nil
	}
	return Result{OK: true}, nil
}

func (b *Broker) decide(ctx context.Context, req Request) (Spec, security.Decision, error) {
	if b.charge == nil || b.grants == nil {
		return Spec{}, security.Decision{}, domain.E(domain.CodeConflict, "broker is missing budget or permission")
	}
	spec, ok := b.reg[req.Tool]
	if !ok {
		return Spec{}, security.Decision{}, domain.E(domain.CodeToolError, "unknown tool "+req.Tool)
	}
	if err := validateArgs(spec, req.Args); err != nil {
		return Spec{}, security.Decision{}, err
	}
	var resources []string
	unresolved := false
	if spec.ExtractResources != nil {
		got, err := spec.ExtractResources(req.Args)
		if err != nil {
			unresolved = true
		} else {
			resources = got
		}
	}
	now := req.Now
	if now.IsZero() {
		now = b.now()
	}
	grants, err := b.grants.ListGrants(ctx, req.RunID, req.SessionID, now)
	if err != nil {
		return Spec{}, security.Decision{}, err
	}
	decision := security.Decide(grants, security.Request{
		RunID:               req.RunID,
		SessionID:           req.SessionID,
		Tool:                req.Tool,
		SideEffectClass:     spec.SideEffectClass,
		Readonly:            spec.Readonly,
		Resources:           resources,
		UnresolvedResources: unresolved,
		DefaultAllow:        req.DefaultAllow,
		ApprovalPossible:    req.ApprovalPossible,
		SensitiveSeen:       req.SensitiveSeen,
		Egress:              req.Egress,
		Now:                 now,
	})
	return spec, decision, nil
}

func sortStrings(in []string) { sort.Strings(in) }

// Invoke validates, authorizes, charges, and only then executes.
// A denied or invalid call does not charge and does not execute.
// Budget is checked after permission so a rejection does not burn the ledger,
// and execution still cannot happen unless both succeeded.
func (b *Broker) Invoke(ctx context.Context, req Request) (Result, error) {
	spec, decision, err := b.decide(ctx, req)
	if err != nil {
		var de *domain.Error
		if errors.As(err, &de) && (de.Code == domain.CodeToolError || de.Code == domain.CodeInvalidArgument) {
			return Result{Error: de}, nil
		}
		return Result{}, err
	}
	if !decision.Allow {
		if decision.NeedsApproval {
			return Result{NeedsApproval: true, Error: domain.E(domain.CodeAuthDenied, decision.Reason)}, nil
		}
		return Result{Error: domain.E(domain.CodeAuthDenied, decision.Reason)}, nil
	}
	if req.IdempotencyKey != "" {
		if store, ok := b.charge.(idempotencyStore); ok {
			cached, found, err := store.LookupIdempotency(ctx, req.RunID, req.Tool, req.IdempotencyKey)
			if err != nil {
				return Result{}, err
			}
			if found {
				return Result{OK: true, Data: cached}, nil
			}
		}
	}
	if err := b.charge.Charge(ctx, req.RunID, domain.ChargeTool, 1); err != nil {
		return Result{Error: asDomain(err)}, nil
	}
	if spec.Execute == nil {
		return Result{Error: domain.E(domain.CodeToolError, "tool has no executor")}, nil
	}
	data, err := spec.Execute(ctx, req.Args)
	if err != nil {
		return Result{Dispatched: true, Error: asDomain(err)}, nil
	}
	if req.IdempotencyKey != "" {
		if store, ok := b.charge.(idempotencyStore); ok && len(data) > 0 {
			when := req.Now
			if when.IsZero() {
				when = time.Now()
			}
			_ = store.SaveIdempotency(ctx, req.RunID, req.Tool, req.IdempotencyKey, data, when)
		}
	}
	return Result{OK: true, Data: data, Dispatched: true}, nil
}

type idempotencyStore interface {
	LookupIdempotency(ctx context.Context, runID, toolName, key string) (json.RawMessage, bool, error)
	SaveIdempotency(ctx context.Context, runID, toolName, key string, result json.RawMessage, now time.Time) error
}

func validateArgs(spec Spec, raw json.RawMessage) *domain.Error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return domain.E(domain.CodeInvalidArgument, "tool arguments must be a JSON object")
	}
	for _, key := range spec.Required {
		v, ok := m[key]
		if !ok || v == nil || v == "" {
			return domain.E(domain.CodeInvalidArgument, "missing argument "+key)
		}
	}
	return nil
}

func asDomain(err error) *domain.Error {
	if err == nil {
		return nil
	}
	if de, ok := err.(*domain.Error); ok {
		return de
	}
	if domain.IsCode(err, domain.CodeBudget) {
		return domain.E(domain.CodeBudget, err.Error())
	}
	return domain.E(domain.CodeToolError, err.Error())
}
