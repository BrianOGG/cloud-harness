package snapshot

import (
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/storage"
)

func TestReviewPairsPartialForeignOverlap(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	var records []storage.WorkspaceSnapshot
	for i, label := range []string{"b/batch-1/exec_x/pre", "a/batch-1/exec_x/pre", "a/batch-1/exec_x/post", "a/batch-2/exec_y/pre", "a/batch-2/exec_y/post"} {
		records = append(records, storage.WorkspaceSnapshot{ID: label, Label: label, RunID: label[:1], WorkspaceID: "ws", CreatedAt: start.Add(time.Duration(i) * time.Second)})
	}
	bounds := func(string) (time.Time, time.Time) { return start, records[1].CreatedAt }
	pairs, partial := ReviewPairs(records, "a", "ws", "/workspace", bounds)
	if !partial || len(pairs) != 1 || pairs[0].Number != 2 {
		t.Fatalf("overlap should affect only the first pair: %v, partial=%t", pairs, partial)
	}
	records[3].CreatedAt = time.Time{} // No timestamp evidence of separation.
	pairs, partial = ReviewPairs(records, "a", "ws", "/workspace", bounds)
	if !partial || len(pairs) != 0 {
		t.Fatalf("incomplete timeline should stay conservative: %v, partial=%t", pairs, partial)
	}
}

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
			pairs, partial := ReviewPairs(tc.history, "run_a", "ws", "/workspace", nil)
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
