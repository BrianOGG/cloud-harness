// Package tools is the only execution path for a tool. Callers that need a
// side effect go through Broker, which checks arguments, budget, and permission
// before the executor runs.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/security"
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
	// ExtractRequestResources optionally uses trusted invocation metadata (never model arguments).
	ExtractRequestResources func(Request) ([]string, error)
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
	// UncertainOutcome is true when Execute may have run but returned no
	// confirmed result. The agent must reconcile instead of recording failure.
	UncertainOutcome bool
}

// Request is one invocation.
type Request struct {
	// ApprovalMode comes only from the agent loop/checkpoint snapshot.
	ApprovalMode     string
	RunID            string
	SessionID        string
	InvocationID     string
	Tool             string
	Args             json.RawMessage
	IdempotencyKey   string
	DefaultAllow     []string
	ApprovalPossible bool
	SensitiveSeen    bool
	Egress           bool
	Now              time.Time
}

type requestKeyType struct{}

var requestKey requestKeyType

// Broker is the single tool entry point.
type Broker struct {
	mu              sync.RWMutex
	reg             map[string]Spec
	mcp             *mcpRegistry
	charge          Charger
	grants          GrantSource
	now             func() time.Time
	CommandEnvAllow []string
	commandLimits   CommandLimits
}

// NewBroker builds a broker. charge and grants are required so a call cannot skip them.
func NewBroker(charge Charger, grants GrantSource) *Broker {
	return &Broker{reg: map[string]Spec{}, mcp: &mcpRegistry{specs: map[string]Spec{}}, charge: charge, grants: grants, now: time.Now}
}

// SetCommandEnvAllow sets the extra environment variable names passed to run_command subprocesses.
func (b *Broker) SetCommandEnvAllow(names []string) {
	b.mu.Lock()
	b.CommandEnvAllow = append([]string(nil), names...)
	b.mu.Unlock()
}

func (b *Broker) commandEnvAllowList() []string {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	out := append([]string(nil), b.CommandEnvAllow...)
	b.mu.RUnlock()
	return out
}

// Register adds a local tool. Registering does not authorize it.
func (b *Broker) Register(spec Spec) {
	b.mu.Lock()
	if b.reg == nil {
		b.reg = map[string]Spec{}
	}
	b.reg[spec.Name] = spec
	b.mu.Unlock()
}

func (b *Broker) spec(name string) (Spec, bool) {
	b.mu.RLock()
	spec, ok := b.reg[name]
	b.mu.RUnlock()
	if !ok && b.mcp != nil && strings.HasPrefix(name, "mcp.") {
		b.mcp.mu.RLock()
		spec, ok = b.mcp.specs[name]
		b.mcp.mu.RUnlock()
	}
	return spec, ok
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
	Egress     bool
}

// Lookup returns the authorization facts for a registered tool.
func (b *Broker) Lookup(name string) (Fact, bool) {
	spec, ok := b.spec(name)
	if !ok {
		return Fact{}, false
	}
	return Fact{
		Name: spec.Name, Readonly: spec.Readonly, Idempotent: spec.Idempotent,
		Pollable: spec.Pollable, SideEffect: spec.SideEffectClass, Egress: spec.EgressScope != "",
	}, true
}

func (b *Broker) Has(name string) bool {
	_, ok := b.spec(name)
	return ok
}

// PublicSpec is the tool description sent to a model. It has no executor.
type PublicSpec struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// List returns registered tools in name order with provider-safe JSON schemas.
func (b *Broker) List() []PublicSpec {
	specs := b.Descriptors()
	for i := range specs {
		specs[i].Parameters = CleanSchema(specs[i].Parameters)
	}
	return specs
}

// Descriptors is a name-sorted snapshot of raw descriptors. Callers treat the
// schema bytes as immutable and clean only those sent to the provider.
func (b *Broker) Descriptors() []PublicSpec {
	b.mu.RLock()
	out := make([]PublicSpec, 0, len(b.reg))
	for _, spec := range b.reg {
		out = append(out, PublicSpec{Name: spec.Name, Description: spec.Description, Parameters: spec.Parameters})
	}
	if b.mcp != nil {
		b.mcp.mu.RLock()
		for name, spec := range b.mcp.specs {
			if _, shadowed := b.reg[name]; !shadowed {
				out = append(out, PublicSpec{Name: spec.Name, Description: spec.Description, Parameters: spec.Parameters})
			}
		}
		b.mcp.mu.RUnlock()
	}
	b.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

var standardJSONSchemaKeys = map[string]bool{
	"type":                 true,
	"properties":           true,
	"required":             true,
	"items":                true,
	"description":          true,
	"enum":                 true,
	"additionalProperties": true,
	"default":              true,
	"minimum":              true,
	"maximum":              true,
	"minItems":             true,
	"maxItems":             true,
	"minLength":            true,
	"maxLength":            true,
	"pattern":              true,
	"format":               true,
	"title":                true,
	"anyOf":                true,
	"oneOf":                true,
	"allOf":                true,
	"not":                  true,
	"const":                true,
}

// CleanSchema strips non-standard or internal tool metadata keys (such as readOnly,
// sideEffect, reversible) from a JSON Schema so provider requests contain only
// standard JSON Schema keywords.
func CleanSchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	cleanSchemaNode(m)
	cleaned, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return cleaned
}

func cleanSchemaNode(m map[string]any) {
	for k := range m {
		if !standardJSONSchemaKeys[k] {
			delete(m, k)
		}
	}
	if props, ok := m["properties"].(map[string]any); ok {
		for propName, propVal := range props {
			propMap, ok := propVal.(map[string]any)
			if !ok {
				delete(props, propName)
				continue
			}
			cleanSchemaNode(propMap)
		}
	}
	if items, ok := m["items"].(map[string]any); ok {
		cleanSchemaNode(items)
	} else if itemsArr, ok := m["items"].([]any); ok {
		for _, item := range itemsArr {
			if itemMap, ok := item.(map[string]any); ok {
				cleanSchemaNode(itemMap)
			}
		}
	}
	if addProps, ok := m["additionalProperties"].(map[string]any); ok {
		cleanSchemaNode(addProps)
	}
	for _, comb := range []string{"anyOf", "oneOf", "allOf"} {
		if arr, ok := m[comb].([]any); ok {
			for _, elem := range arr {
				if elemMap, ok := elem.(map[string]any); ok {
					cleanSchemaNode(elemMap)
				}
			}
		}
	}
	if notMap, ok := m["not"].(map[string]any); ok {
		cleanSchemaNode(notMap)
	}
}

// ExtractResources resolves the resource scopes for a tool invocation's arguments.
func (b *Broker) ExtractResources(tool string, args json.RawMessage) ([]string, error) {
	return b.ExtractRequestResources(Request{Tool: tool, Args: args})
}

// ExtractRequestResources keeps preflight, approval and dispatch resource scopes
// consistent when a tool needs trusted run metadata to resolve its arguments.
func (b *Broker) ExtractRequestResources(req Request) ([]string, error) {
	spec, ok := b.spec(req.Tool)
	if !ok {
		return nil, domain.E(domain.CodeToolError, "unknown tool "+req.Tool)
	}
	return spec.resources(req)
}

func (spec Spec) resources(req Request) ([]string, error) {
	if spec.ExtractRequestResources != nil {
		return spec.ExtractRequestResources(req)
	}
	if spec.ExtractResources == nil {
		return nil, nil
	}
	return spec.ExtractResources(req.Args)
}

// Probe asks the tool whether the side effect is already visible.
// It does not charge budget and does not call Execute.
func (b *Broker) Probe(ctx context.Context, req Request) (Probe, error) {
	spec, ok := b.spec(req.Tool)
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
	spec, ok := b.spec(req.Tool)
	if !ok {
		return Spec{}, security.Decision{}, domain.E(domain.CodeToolError, "unknown tool "+req.Tool)
	}
	if err := validateArgs(spec, req.Args); err != nil {
		return Spec{}, security.Decision{}, err
	}
	resources, resourceErr := spec.resources(req)
	now := req.Now
	if now.IsZero() {
		now = b.now()
	}
	grants, err := b.grants.ListGrants(ctx, req.RunID, req.SessionID, now)
	if err != nil {
		return Spec{}, security.Decision{}, err
	}
	egress := req.Egress || spec.EgressScope != "" || (!spec.Readonly && strings.HasPrefix(spec.Name, "mcp."))
	decision := security.Decide(grants, security.Request{
		RunID:               req.RunID,
		SessionID:           req.SessionID,
		InvocationID:        req.InvocationID,
		Tool:                req.Tool,
		SideEffectClass:     spec.SideEffectClass,
		Readonly:            spec.Readonly,
		Resources:           resources,
		UnresolvedResources: resourceErr != nil,
		DefaultAllow:        req.DefaultAllow,
		ApprovalPossible:    req.ApprovalPossible,
		SensitiveSeen:       req.SensitiveSeen,
		Egress:              egress,
		Now:                 now,
	})
	return spec, decision, nil
}

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
	execCtx := context.WithValue(ctx, requestKey, req)
	data, err := spec.Execute(execCtx, req.Args)
	if err != nil {
		return Result{Dispatched: true, Data: data, Error: asDomain(err), UncertainOutcome: errors.Is(err, domain.ErrUncertainOutcome)}, nil
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
		if !ok || v == nil || (v == "" && key != "new_string") {
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
