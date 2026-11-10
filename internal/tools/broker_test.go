package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/domain"
)

type memCharge struct{ b domain.Budget }

func (m *memCharge) Charge(_ context.Context, _ string, kind domain.ChargeKind, n int) error {
	next, err := m.b.Charge(kind, n)
	if err != nil {
		return err
	}
	m.b = next
	return nil
}

type memGrants struct{ grants []domain.Grant }

func (m memGrants) ListGrants(context.Context, string, string, time.Time) ([]domain.Grant, error) {
	return m.grants, nil
}

func TestBrokerDenyAndBudgetDoNotExecute(t *testing.T) {
	charge := &memCharge{b: domain.Budget{MaxToolCalls: 1}}
	b := NewBroker(charge, memGrants{})
	calls := 0
	b.Register(Spec{
		Name: "write_file", SideEffectClass: "irreversible", Required: []string{"path"},
		ExtractResources: func(args json.RawMessage) ([]string, error) {
			var m map[string]string
			if err := json.Unmarshal(args, &m); err != nil {
				return nil, err
			}
			return []string{m["path"]}, nil
		},
		Execute: func(context.Context, json.RawMessage) (json.RawMessage, error) {
			calls++
			return json.RawMessage(`{"ok":true}`), nil
		},
	})
	res, err := b.Invoke(context.Background(), Request{RunID: "run", Tool: "write_file", Args: json.RawMessage(`{"path":"/tmp/a"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Dispatched || calls != 0 || res.Error == nil || res.Error.Code != domain.CodeAuthDenied {
		t.Fatalf("deny executed: %+v calls=%d", res, calls)
	}
	if charge.b.UsedToolCalls != 0 {
		t.Fatal("denied call consumed budget")
	}
	bad, err := b.Invoke(context.Background(), Request{RunID: "run", Tool: "write_file", Args: json.RawMessage(`{}`)})
	if err != nil || bad.Dispatched || bad.Error.Code != domain.CodeInvalidArgument {
		t.Fatalf("schema %+v %v", bad, err)
	}
	b.Register(Spec{
		Name: "echo", Readonly: true, SideEffectClass: "none",
		Execute: func(context.Context, json.RawMessage) (json.RawMessage, error) {
			calls++
			return json.RawMessage(`{"pong":true}`), nil
		},
	})
	ok, err := b.Invoke(context.Background(), Request{RunID: "run", Tool: "echo", Args: json.RawMessage(`{}`), DefaultAllow: []string{"echo"}})
	if err != nil || !ok.OK || !ok.Dispatched || calls != 1 {
		t.Fatalf("allow %+v %v calls=%d", ok, err, calls)
	}
	again, err := b.Invoke(context.Background(), Request{RunID: "run", Tool: "echo", Args: json.RawMessage(`{}`), DefaultAllow: []string{"echo"}})
	if err != nil || again.Dispatched || again.Error.Code != domain.CodeBudget || calls != 1 {
		t.Fatalf("budget %+v %v calls=%d", again, err, calls)
	}
}

type stubMCP struct{}

func (stubMCP) Protocol() string    { return "mcp" }
func (stubMCP) ServerLabel() string { return "unused.example" }
func (stubMCP) Discover(context.Context) ([]Spec, error) {
	return []Spec{{
		Name: "remote_echo", Readonly: true, SideEffectClass: "none",
		Execute: func(context.Context, json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"from":"remote"}`), nil
		},
	}}, nil
}

func TestMCPAdapterDoesNotBypassBroker(t *testing.T) {
	var adapter MCPAdapter = stubMCP{}
	if adapter.Protocol() != "mcp" {
		t.Fatal(adapter.Protocol())
	}
	specs, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b := NewBroker(&memCharge{b: domain.Budget{MaxToolCalls: 5}}, memGrants{})
	for _, spec := range specs {
		b.Register(spec)
	}
	res, err := b.Invoke(context.Background(), Request{RunID: "run", Tool: "remote_echo", Args: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Dispatched || res.OK {
		t.Fatal("adapter spec ran without a grant")
	}
}
