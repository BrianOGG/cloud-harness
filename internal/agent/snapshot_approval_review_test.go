package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	workspacesnapshot "github.com/BrianOGG/cloud-harness/internal/snapshot"
	"github.com/BrianOGG/cloud-harness/internal/storage"
	"github.com/BrianOGG/cloud-harness/internal/storage/sqlite"
	"github.com/BrianOGG/cloud-harness/internal/tools"
)

func TestSnapshotReviewExcludesEditsAcrossApprovalWaits(t *testing.T) {
	m, db, root := testManager(t, &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(snapshotWrite("a", "a.txt", "run-a"), snapshotWrite("b", "b.txt", "run-b")),
		textOnly("done"),
	}})
	run := runSnapshotMessage(t, m)
	// Reload persisted state on another manager for each approval.
	for _, target := range []string{"a.txt", "b.txt"} {
		if run.Status != domain.RunWaiting {
			t.Fatalf("expected approval wait, got %s", run.Status)
		}
		for _, path := range []string{target, "human-only.txt"} {
			if err := os.WriteFile(filepath.Join(root, path), []byte("human-"+target), 0600); err != nil {
				t.Fatal(err)
			}
		}
		resumer := &Manager{Store: db, Provider: m.Provider, Broker: m.Broker, Workspace: root, OwnerID: "resume-" + target}
		var err error
		run, err = resumer.Resume(t.Context(), run.ID, ResumeInput{Decision: "approve"})
		if err != nil {
			t.Fatal(err)
		}
	}
	if run.Status != domain.RunCompleted {
		t.Fatalf("run ended %s", run.Status)
	}
	s, records := workspaceRecords(t, db, root)
	pairs, incomplete := workspacesnapshot.ReviewPairs(records, run.ID, run.WorkspaceID, root)
	if incomplete || len(pairs) != 2 {
		t.Fatalf("want two complete execution intervals, got %d (incomplete=%v)", len(pairs), incomplete)
	}
	for i, target := range []string{"a.txt", "b.txt"} {
		pair := pairs[i]
		files, err := s.ChangedPaths(t.Context(), pair.Before, pair.After)
		if err != nil || len(files) != 1 || files[0].Path != target {
			t.Fatalf("interval %d: paths %+v, err %v", i, files, err)
		}
		if _, err := s.RestoreReviewedFile(t.Context(), pair, target, func(fn func() error) error { return fn() }); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(root, target))
		if err != nil || string(got) != "human-"+target {
			t.Fatalf("restored %s to %q, err %v", target, got, err)
		}
	}
}

type missingDispatchPost struct {
	*sqlite.DB
	skipped bool
}

func (s *missingDispatchPost) CreateWorkspaceSnapshot(ctx context.Context, rec storage.WorkspaceSnapshot) (storage.WorkspaceSnapshot, error) {
	if !s.skipped && strings.Contains(rec.Label, "/exec_") && strings.HasSuffix(rec.Label, "/post") {
		s.skipped = true
		return storage.WorkspaceSnapshot{}, errors.New("lost first dispatch post")
	}
	return s.DB.CreateWorkspaceSnapshot(ctx, rec)
}

func TestSnapshotReviewRetryAfterPartialWrite(t *testing.T) {
	m, db, root := testManager(t, &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(snapshotWrite("first", "target", "done"), snapshotWrite("retry", "target", "done")), textOnly("done"),
	}})
	m.Store = &missingDispatchPost{DB: db}
	calls := 0
	m.Broker.Register(tools.Spec{
		Name: "write_file", SideEffectClass: "reversible",
		ExtractResources: func(json.RawMessage) ([]string, error) { return []string{root}, nil },
		Execute: func(context.Context, json.RawMessage) (json.RawMessage, error) {
			calls++
			content := "done"
			if calls == 1 {
				content = "partial"
			}
			if err := os.WriteFile(filepath.Join(root, "target"), []byte(content), 0600); err != nil {
				return nil, err
			}
			if calls == 1 {
				return nil, errors.New("first write failed")
			}
			return json.RawMessage(`{}`), nil
		},
	})
	allowSnapshotWrite(m, root)
	run := runSnapshotMessage(t, m)
	s, records := workspaceRecords(t, db, root)
	pairs, incomplete := workspacesnapshot.ReviewPairs(records, run.ID, run.WorkspaceID, root)
	if run.Status != domain.RunCompleted || calls != 2 || !incomplete || len(pairs) != 1 {
		t.Fatalf("run=%s calls=%d pairs=%+v incomplete=%v", run.Status, calls, pairs, incomplete)
	}
	patch, _, err := s.FileDiff(t.Context(), pairs[0], "target")
	if err != nil || !strings.Contains(patch, "-partial") || !strings.Contains(patch, "+done") {
		t.Fatalf("retry diff: %q, err %v", patch, err)
	}
}
