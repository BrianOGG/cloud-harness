package trace

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

func TestAssembleSessionRunStepToolAndChild(t *testing.T) {
	run := storage.Run{ID: "run_p", SessionID: "ses", Status: domain.RunCompleted}
	responses := []storage.StoredResponse{{ID: "rsp_1", RunID: "run_p", Action: domain.ActionExecute}}
	batches := []storage.StoredBatch{{
		ID: "bat_1", ModelResponseID: "rsp_1", Status: domain.BatchComplete, Action: domain.ActionExecute,
		Invocations: []domain.Invocation{{
			ID: "inv_1", ProviderCallID: "c1", Name: "read_file", Status: domain.InvSucceeded, Ordinal: 0,
		}},
	}}
	child := Node{ID: "run_c", ParentID: "run_p", SessionID: "ses", Status: "COMPLETED"}
	view := Assemble(run, responses, batches, []Node{child})
	if view.SessionID != "ses" || view.Run.ID != "run_p" || len(view.Run.Steps) != 1 {
		t.Fatalf("%+v", view)
	}
	step := view.Run.Steps[0]
	if step.ID != "rsp_1" || len(step.Batches) != 1 || step.Batches[0].Calls[0].ProviderCallID != "c1" {
		t.Fatalf("step %+v", step)
	}
	if len(view.Run.Children) != 1 || view.Run.Children[0].ID != "run_c" {
		t.Fatalf("children %+v", view.Run.Children)
	}
}

func TestAssembleIncludesPairedArgumentsAndResultsWithoutOpaque(t *testing.T) {
	run := storage.Run{ID: "run", SessionID: "session", Status: domain.RunCompleted}
	responses := []storage.StoredResponse{{
		ID: "response", RunID: run.ID, Action: domain.ActionExecute,
		Opaque: []byte("private-provider-continuation-fixture"),
	}, {ID: "response-pending", RunID: run.ID, Action: domain.ActionExecute}}
	batches := []storage.StoredBatch{{
		ID: "batch", ModelResponseID: "response", RunID: run.ID, Status: domain.BatchComplete, Action: domain.ActionExecute,
		Invocations: []domain.Invocation{
			{ID: "success", ProviderCallID: "read", Name: "read_file", Status: domain.InvSucceeded, Ordinal: 0,
				Arguments: json.RawMessage(`{"path":"fixture.txt"}`),
				Result:    &domain.CallResult{OK: true, Data: json.RawMessage(`{"content":"fixture output"}`)}},
			{ID: "denied", ProviderCallID: "command", Name: "run_command", Status: domain.InvDenied, Ordinal: 1,
				Arguments: json.RawMessage(`{"command":"uname -a","cwd":"."}`),
				Result:    &domain.CallResult{OK: false, Error: domain.E(domain.CodeAuthDenied, "approval denied").WithRef("command")}},
		},
	}, {
		ID: "pending-batch", ModelResponseID: "response-pending", RunID: run.ID, Status: domain.BatchOpen, Action: domain.ActionExecute,
		Invocations: []domain.Invocation{{ID: "pending", ProviderCallID: "not-started", Name: "read_file", Status: domain.InvPending, Ordinal: 0}},
	}}
	view := Assemble(run, responses, batches, nil)
	calls := view.Run.Steps[0].Batches[0].Calls
	if len(calls) != 2 || string(calls[0].Arguments) != string(batches[0].Invocations[0].Arguments) || calls[0].Result == nil || !calls[0].Result.OK || string(calls[0].Result.Data) != `{"content":"fixture output"}` {
		t.Fatalf("success call lost persisted arguments/result: %+v", calls)
	}
	if string(calls[1].Arguments) != string(batches[0].Invocations[1].Arguments) || calls[1].Result == nil || calls[1].Result.OK || calls[1].Result.Error == nil || calls[1].Result.Error.Code != domain.CodeAuthDenied || calls[1].Result.Error.Message != "approval denied" {
		t.Fatalf("denial call lost paired error: %+v", calls[1])
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"private-provider-continuation-fixture", `"opaque"`, `"continuation"`, `"snapshot"`} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("trace exposed private provider state %q", forbidden)
		}
	}
	var public struct {
		Run struct {
			Steps []struct {
				Batches []struct {
					Calls []map[string]json.RawMessage `json:"calls"`
				} `json:"batches"`
			} `json:"steps"`
		} `json:"run"`
	}
	if err := json.Unmarshal(raw, &public); err != nil {
		t.Fatal(err)
	}
	pending := public.Run.Steps[1].Batches[0].Calls[0]
	if _, exists := pending["result"]; exists {
		t.Fatal("trace invented a result for a pending invocation")
	}
}
