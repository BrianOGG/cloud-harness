package trace

import (
	"testing"

	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/storage"
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
