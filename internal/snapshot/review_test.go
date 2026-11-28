package snapshot

import (
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/storage"
)

func TestReviewPairsHandlesApprovalSegmentsAndForeignWindows(t *testing.T) {
	snap := func(run, label string) storage.WorkspaceSnapshot {
		return storage.WorkspaceSnapshot{ID: label, Label: run + "/" + label, RunID: run, WorkspaceID: "ws", WorkspacePath: "/workspace"}
	}
	before := snap("run_a", "batch-1/pre")
	post := snap("run_a", "batch-1/post")
	last := post
	last.ID = "resumed"
	foreign := snap("run_b", "batch-9/pre")
	foreignPost := snap("run_b", "batch-9/post")
	for _, tc := range []struct {
		name     string
		history  []storage.WorkspaceSnapshot
		complete bool
	}{
		{"resumed", []storage.WorkspaceSnapshot{foreign, foreignPost, before, post, last}, true},
		{"nested foreign", []storage.WorkspaceSnapshot{before, foreign, foreignPost, post, last}, false},
		{"enclosing foreign", []storage.WorkspaceSnapshot{foreign, before, post, last, foreignPost}, false},
		{"partial", []storage.WorkspaceSnapshot{post, last}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pairs, partial := ReviewPairs(tc.history, "run_a", "ws", "/workspace")
			if partial == tc.complete || tc.complete && (len(pairs) != 1 || pairs[0].After.ID != "resumed") || !tc.complete && len(pairs) != 0 {
				t.Fatalf("pairs=%v incomplete=%v", pairs, partial)
			}
		})
	}
}
