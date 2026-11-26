package trace

import (
	"encoding/json"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

// Call is one tool invocation inside a batch. Opaque bytes are not included.
type Call struct {
	ID             string             `json:"id"`
	ProviderCallID string             `json:"provider_call_id"`
	Name           string             `json:"name"`
	Status         string             `json:"status"`
	Ordinal        int                `json:"ordinal"`
	Arguments      json.RawMessage    `json:"arguments,omitempty"`
	Result         *domain.CallResult `json:"result,omitempty"`
}

// Batch is one ordered tool batch under a step.
type Batch struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Action string `json:"action"`
	Calls  []Call `json:"calls"`
}

// Step is one model response and the batches that came from it.
type Step struct {
	ID      string  `json:"id"`
	Action  string  `json:"action"`
	Batches []Batch `json:"batches"`
}

// Node is one run. Children are sub-agent runs, not a copy of this run's tools.
type Node struct {
	ID         string `json:"id"`
	ParentID   string `json:"parent_id,omitempty"`
	SessionID  string `json:"session_id"`
	Status     string `json:"status"`
	WaitReason string `json:"wait_reason,omitempty"`
	Steps      []Step `json:"steps"`
	Children   []Node `json:"children,omitempty"`
}

// View is the Session → Run → Step → Tool / Sub-Agent / Batch reading of one run.
type View struct {
	SessionID string `json:"session_id"`
	Run       Node   `json:"run"`
}

// Assemble builds a trace view from already loaded rows. It does not read the
// database and it does not include stream deltas.
func Assemble(run storage.Run, responses []storage.StoredResponse, batches []storage.StoredBatch, children []Node) View {
	byResponse := map[string][]Batch{}
	for _, batch := range batches {
		view := Batch{ID: batch.ID, Status: string(batch.Status), Action: string(batch.Action)}
		for _, inv := range batch.Invocations {
			view.Calls = append(view.Calls, Call{
				ID: inv.ID, ProviderCallID: inv.ProviderCallID, Name: inv.Name,
				Status: string(inv.Status), Ordinal: inv.Ordinal,
				Arguments: inv.Arguments, Result: inv.Result,
			})
		}
		byResponse[batch.ModelResponseID] = append(byResponse[batch.ModelResponseID], view)
	}
	node := Node{
		ID: run.ID, ParentID: run.ParentRunID, SessionID: run.SessionID,
		Status: string(run.Status), WaitReason: string(run.WaitReason),
		Children: children,
	}
	for _, resp := range responses {
		node.Steps = append(node.Steps, Step{
			ID: resp.ID, Action: string(resp.Action), Batches: byResponse[resp.ID],
		})
	}
	return View{SessionID: run.SessionID, Run: node}
}
