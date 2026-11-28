package agent

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	workspacesnapshot "github.com/BrianOGG/cloud-harness/internal/snapshot"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

func TestConfirmedMemoryAcrossManagedWorktreesAndLegacySource(t *testing.T) {
	ctx := t.Context()
	mock := &provider.Mock{}
	m, db, root := isolationManager(t, mock)
	m.DisableSnapshots, m.DisableRepoMap = true, true
	session, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	source, err := m.RunMessage(ctx, session.ID, "a conversation")
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := m.MemoryProjectID(ctx)
	if err != nil || source.ProjectID != projectID || source.WorkspaceID == projectID {
		t.Fatalf("source IDs: %+v %v", source, err)
	}

	// A schema-014 managed worktree may already have been discarded.
	legacy := storage.Run{ID: "run_legacy", SessionID: session.ID, Status: domain.RunCompleted, Version: 1}
	legacy.WorktreePath = filepath.Join(root, ".cloud-harness", "worktrees", legacy.ID)
	legacy.WorkspaceID, err = workspacesnapshot.WorkspaceIDForPath(legacy.WorktreePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateRun(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ConfirmProjectMemory(ctx, projectID, session.ID, legacy.ID, "fact", "legacy decision"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ConfirmProjectMemory(ctx, projectID, session.ID, source.ID, "fact", "shared worktree fact"); err != nil {
		t.Fatal(err)
	}
	next, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessage(ctx, next.ID, "new isolated session")
	if err != nil || run.WorktreePath == source.WorktreePath || run.ProjectID != projectID {
		t.Fatalf("next run: %+v, %v", run, err)
	}
	msgs := mock.Requests[len(mock.Requests)-1].Messages
	for _, msg := range msgs {
		if msg.Role == "user" && strings.Contains(msg.Content, "shared worktree fact") && strings.Contains(msg.Content, "legacy decision") {
			return
		}
	}
	t.Fatalf("missing confirmed memories across worktrees: %+v", msgs)
}

func TestConfirmedMemoryContextIsBounded(t *testing.T) {
	ctx := t.Context()
	m, db, _ := testManager(t, &provider.Mock{})
	session, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := m.MemoryProjectID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	source := storage.Run{ID: "run_source", SessionID: session.ID, ProjectID: projectID, Status: domain.RunCompleted, Version: 1}
	if err := db.CreateRun(ctx, source); err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= confirmedMemoryLimit; i++ {
		if _, err := db.CreateProjectMemory(ctx, projectID, source, "fact", fmt.Sprintf("row-%02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	data, err := m.confirmedMemoryContext(ctx, projectID)
	if err != nil || strings.Count(data, "\n") != confirmedMemoryLimit || strings.Contains(data, "row-00") {
		t.Fatalf("entry limit: %v %s", err, data)
	}
	for i := 0; i < 4; i++ {
		if _, err := db.CreateProjectMemory(ctx, projectID, source, "fact", strings.Repeat("字", 500)); err != nil {
			t.Fatal(err)
		}
	}
	data, err = m.confirmedMemoryContext(ctx, projectID)
	if err != nil || len(data) > confirmedMemoryBytes || !strings.Contains(data, strings.Repeat("字", 500)) {
		t.Fatalf("byte limit: %v %d", err, len(data))
	}
}
