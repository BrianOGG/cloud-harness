package snapshot

import (
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/storage"
)

func TestReviewPairsUsesOnlyCompleteExecutionAttempts(t *testing.T) {
	snap := func(run, label string) storage.WorkspaceSnapshot {
		return storage.WorkspaceSnapshot{ID: run + "/" + label, Label: run + "/" + label, RunID: run, WorkspaceID: "ws", WorkspacePath: "/workspace"}
	}
	oldPre, oldPost := snap("run_a", "batch-1/pre"), snap("run_a", "batch-1/post")
	a, b := snap("run_a", "batch-1/exec_one/pre"), snap("run_a", "batch-1/exec_one/post")
	c, d := snap("run_a", "batch-1/exec_retry/pre"), snap("run_a", "batch-1/exec_retry/post")
	x, y := snap("run_b", "batch-1/exec_one/pre"), snap("run_b", "batch-1/exec_one/post")
	type records = []storage.WorkspaceSnapshot
	for _, tc := range []struct {
		name    string
		history records
		want    records // consecutive before/after endpoints
		partial bool
	}{
		{"legacy single post may follow approval", records{oldPre, oldPost}, nil, true},
		{"legacy multiple posts", records{oldPre, oldPost, oldPost}, nil, true},
		{"resumed legacy batch", records{oldPre, c, d, oldPost}, records{c, d}, false},
		{"upgrade after a legacy execution", records{oldPre, oldPost, c, d, oldPost}, records{c, d}, true},
		{"independent attempts with same batch", records{oldPre, a, b, oldPost, x, y, c, d, oldPost}, records{a, b, c, d}, false},
		{"missing post then retry", records{a, c, d}, records{c, d}, true},
		{"missing retry pre", records{a, b, d}, records{a, b}, true},
		{"different attempts never pair", records{a, d}, nil, true},
		{"duplicate post", records{a, b, b}, nil, true},
		{"reversed", records{b, a}, nil, true},
		{"nested foreign", records{a, x, y, b}, nil, true},
		{"enclosing foreign", records{x, a, b, y}, nil, true},
		{"foreign still executing", records{x, a, b}, nil, true},
		{"foreign missing post before closed checkpoint", records{x, snap("run_b", "batch-1/post"), a, b}, records{a, b}, false},
		{"foreign legacy start in interval", records{a, snap("run_b", "batch-9/pre"), b}, nil, true},
		{"nested legacy foreign", records{a, snap("run_b", "batch-8/pre"), snap("run_b", "batch-8/post"), b}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pairs, partial := ReviewPairs(tc.history, "run_a", "ws", "/workspace")
			if partial != tc.partial || 2*len(pairs) != len(tc.want) {
				t.Fatalf("pairs=%v incomplete=%v", pairs, partial)
			}
			for i, pair := range pairs {
				if pair.Number != 1 || pair.Before != tc.want[2*i] || pair.After != tc.want[2*i+1] {
					t.Fatalf("unexpected endpoints: %+v", pair)
				}
			}
		})
	}
}
