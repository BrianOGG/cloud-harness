package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/config"
	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/provider"
	workspacesnapshot "github.com/cloud-harness/cloud-harness/internal/snapshot"
	"github.com/cloud-harness/cloud-harness/internal/storage"
)

func TestM9MilestoneRewindAndRetain(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, "harness.db")
	if err := os.WriteFile(filepath.Join(workspace, "first.txt"), []byte("before run one\x00\n"), 0644); err != nil {
		t.Fatal(err)
	}

	mgr, db, err := openManager(ctx, config.Config{DBPath: dbPath, Provider: "mock"}, workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		m9ToolResponse("write-first", "first.txt", "run one bytes\x00\n"),
		m9TextResponse("first run done"),
		m9ToolResponse("write-second-a", "first.txt", "run two first write\x00\r\n"),
		m9ToolResponse("write-second-b", "second.txt", "run two second write\x00\n"),
		m9TextResponse("second run done"),
	}}
	mgr.Provider = mock
	mgr.Grants = []domain.Grant{{Tool: "write_file", Op: "call", ResourceScope: workspace, State: "active", Subject: "m9-e2e"}}

	// This unbound snapshot shares the workspace repository but has no run
	// association, so retention must preserve both its record and its ref.
	snapper, err := workspacesnapshot.Open(workspace, db)
	if err != nil {
		t.Fatal(err)
	}
	unassigned, err := snapper.Snapshot(ctx, "manual/keep")
	if err != nil {
		t.Fatal(err)
	}
	if unassigned.RunID != "" {
		t.Fatalf("manual snapshot unexpectedly bound to run %q", unassigned.RunID)
	}

	session, err := mgr.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	firstRun, err := mgr.RunMessage(ctx, session.ID, "write the first file")
	if err != nil || firstRun.Status != domain.RunCompleted {
		t.Fatalf("first run=%+v, err=%v", firstRun, err)
	}
	workspaceID := firstRun.WorkspaceID
	shadowDir := filepath.Join(workspace, ".cloud-harness", "snapshots", workspaceID)
	firstRecords := mustWorkspaceSnapshots(t, db, workspaceID)
	firstRunRecords := snapshotsForRun(firstRecords, firstRun.ID)
	if len(firstRunRecords) < 1 {
		t.Fatalf("first run has no automatic snapshots: %+v", firstRecords)
	}
	for _, rec := range firstRunRecords {
		assertGitObjectExists(t, workspace, shadowDir, rec.TreeOID)
	}

	// The second run's first pre snapshot is its first checkpoint. This run
	// dispatches two writes in separate batches so --all must restore both
	// files and remove the later persistent events.
	wantTargetBytes := []byte("second-run starting bytes\x00\r\n")
	if err := os.WriteFile(filepath.Join(workspace, "second.txt"), wantTargetBytes, 0644); err != nil {
		t.Fatal(err)
	}
	secondRun, err := mgr.RunMessage(ctx, session.ID, "write the second file")
	if err != nil || secondRun.Status != domain.RunCompleted {
		t.Fatalf("second run=%+v, err=%v", secondRun, err)
	}
	records := mustWorkspaceSnapshots(t, db, workspaceID)
	secondRunRecords := snapshotsForRun(records, secondRun.ID)
	if len(secondRunRecords) < 1 {
		t.Fatalf("second run has no automatic snapshots: %+v", records)
	}
	var target storage.WorkspaceSnapshot
	for _, rec := range secondRunRecords {
		if rec.Label == secondRun.ID+"/batch-1/pre" {
			target = rec
			break
		}
	}
	if target.ID == "" || target.EventSeq == nil || *target.EventSeq < 1 {
		t.Fatalf("first second-run checkpoint is incomplete: %+v", target)
	}
	var secondPost storage.WorkspaceSnapshot
	for _, rec := range secondRunRecords {
		if rec.Label == secondRun.ID+"/batch-1/post" {
			secondPost = rec
			break
		}
	}
	if secondPost.ID == "" || secondPost.TreeOID == target.TreeOID {
		t.Fatalf("second run did not capture a distinct post-write tree: target=%+v post=%+v", target, secondPost)
	}
	assertGitObjectExists(t, workspace, shadowDir, secondPost.TreeOID)
	if got, err := os.ReadFile(filepath.Join(workspace, "first.txt")); err != nil || string(got) != "run two first write\x00\r\n" {
		t.Fatalf("first file after run two=%q, err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(workspace, "second.txt")); err != nil || string(got) != "run two second write\x00\n" {
		t.Fatalf("second file after run two=%q, err=%v", got, err)
	}

	var rewindOut, rewindErr bytes.Buffer
	if code := MainWithIO([]string{"rewind", secondRun.ID, "--to", target.ID, "--all", "--yes", "--db", dbPath, "--workspace", workspace}, nil, &rewindOut, &rewindErr); code != 0 {
		t.Fatalf("rewind exited %d: stdout=%s stderr=%s", code, rewindOut.String(), rewindErr.String())
	}
	if got, err := os.ReadFile(filepath.Join(workspace, "second.txt")); err != nil || !bytes.Equal(got, wantTargetBytes) {
		t.Fatalf("rewound bytes=%q, err=%v; want %q", got, err, wantTargetBytes)
	}
	if got, err := os.ReadFile(filepath.Join(workspace, "first.txt")); err != nil || string(got) != "run one bytes\x00\n" {
		t.Fatalf("rewound first-run bytes=%q, err=%v", got, err)
	}
	secondEvents, err := db.ListEvents(ctx, secondRun.ID, 0)
	if err != nil || int64(len(secondEvents)) != *target.EventSeq {
		t.Fatalf("rewound events=%d, err=%v; target seq=%d", len(secondEvents), err, *target.EventSeq)
	}
	afterRewindRecords := mustWorkspaceSnapshots(t, db, workspaceID)
	if len(afterRewindRecords) != len(records)+1 {
		t.Fatalf("rewind snapshot rows=%d, want %d including its safety point", len(afterRewindRecords), len(records)+1)
	}

	// Add a legacy orphan whose run_id has no matching Run row. It should be
	// selected by the same retain pass as the expired run's snapshots.
	if err := os.WriteFile(filepath.Join(workspace, "orphan-only.txt"), []byte("orphan only objects\x00\n"), 0644); err != nil {
		t.Fatal(err)
	}
	orphan, err := snapper.Snapshot(ctx, "run_orphan/batch-1/pre")
	if err != nil {
		t.Fatal(err)
	}
	if orphan.RunID != "run_orphan" {
		t.Fatalf("orphan label did not bind run id: %+v", orphan)
	}
	if err := os.Remove(filepath.Join(workspace, "orphan-only.txt")); err != nil {
		t.Fatal(err)
	}

	agedRun, err := db.GetRun(ctx, secondRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	agedRun.UpdatedAt = time.Now().UTC().AddDate(0, 0, -30)
	if err := db.CompareAndSwapRun(ctx, agedRun, agedRun.Version); err != nil {
		t.Fatalf("age terminal run: %v", err)
	}

	beforeDryRunRecords := mustWorkspaceSnapshots(t, db, workspaceID)
	if len(beforeDryRunRecords) != len(afterRewindRecords)+1 {
		t.Fatalf("pre-retain snapshot rows=%d, want %d including the orphan", len(beforeDryRunRecords), len(afterRewindRecords)+1)
	}
	t.Logf("snapshot rows: before rewind=%d, after rewind=%d, before dry-run=%d", len(records), len(afterRewindRecords), len(beforeDryRunRecords))

	var dryOut, dryErr bytes.Buffer
	if code := MainWithIO([]string{"retain", "--prune-runs-days", "7", "--dry-run", "--db", dbPath, "--workspace", workspace}, nil, &dryOut, &dryErr); code != 0 {
		t.Fatalf("retain dry-run exited %d: stdout=%s stderr=%s", code, dryOut.String(), dryErr.String())
	}
	dry := decodeM9RetainReport(t, dryOut.Bytes())
	wantSnapshots := len(secondRunRecords) + 2 // rewind safety point and orphan
	if dry.RunsToDelete != 1 || dry.SnapshotsToDelete != wantSnapshots || dry.SnapshotsDeleted != 0 || dry.UnassignedRetained != 1 {
		t.Fatalf("dry-run counts=%+v, want runs=1 snapshots=%d and one unassigned retained", dry, wantSnapshots)
	}
	if dry.AffectedWorkspaces != 1 || dry.ReclaimedWorkspaces != 0 || dry.ReclamationFailures != 0 || len(dry.WorkspaceReclamation) != 1 {
		t.Fatalf("dry-run workspace report=%+v", dry)
	}
	if estimate := dry.WorkspaceReclamation[0]; estimate.WorkspaceID != workspaceID || estimate.Snapshots != wantSnapshots || estimate.Objects < 1 || estimate.Bytes < 1 || estimate.Error != "" {
		t.Fatalf("dry-run object estimate=%+v", estimate)
	}
	if _, err := db.GetRun(ctx, secondRun.ID); err != nil {
		t.Fatalf("dry-run deleted the expired run: %v", err)
	}
	afterDryRunRecords := mustWorkspaceSnapshots(t, db, workspaceID)
	if len(afterDryRunRecords) != len(beforeDryRunRecords) {
		t.Fatalf("dry-run changed snapshot rows: got %d, before %d", len(afterDryRunRecords), len(beforeDryRunRecords))
	}
	if !reflect.DeepEqual(afterDryRunRecords, beforeDryRunRecords) {
		t.Fatalf("dry-run changed snapshot records: got %+v, before %+v", afterDryRunRecords, beforeDryRunRecords)
	}
	t.Logf("dry-run preserved all %d snapshot rows and their fields", len(afterDryRunRecords))
	assertGitObjectExists(t, workspace, shadowDir, secondPost.TreeOID)

	var retainOut, retainErr bytes.Buffer
	if code := MainWithIO([]string{"retain", "--prune-runs-days", "7", "--db", dbPath, "--workspace", workspace}, nil, &retainOut, &retainErr); code != 0 {
		t.Fatalf("retain exited %d: stdout=%s stderr=%s", code, retainOut.String(), retainErr.String())
	}
	actual := decodeM9RetainReport(t, retainOut.Bytes())
	if actual.RunsDeleted != 1 || actual.SnapshotsDeleted != wantSnapshots || actual.SnapshotsToDelete != wantSnapshots || actual.UnassignedRetained != 1 {
		t.Fatalf("retain deletion counts=%+v, want runs=1 snapshots=%d and one unassigned retained", actual, wantSnapshots)
	}
	if actual.ReclaimedWorkspaces != 1 || actual.ReclamationFailures != 0 || len(actual.WorkspaceReclamation) != 1 {
		t.Fatalf("retain reclamation report=%+v; stderr=%s", actual, retainErr.String())
	}
	if _, err := db.GetRun(ctx, secondRun.ID); err == nil {
		t.Fatal("expired run still exists after retain")
	}
	if _, err := db.GetRun(ctx, firstRun.ID); err != nil {
		t.Fatalf("surviving run was deleted: %v", err)
	}
	remaining := mustWorkspaceSnapshots(t, db, workspaceID)
	if len(remaining) != len(firstRunRecords)+1 {
		t.Fatalf("remaining snapshots=%d, want surviving run %d plus unassigned record", len(remaining), len(firstRunRecords))
	}
	keptUnassigned := false
	keptFirst := make(map[string]bool)
	for _, rec := range remaining {
		if rec.ID == unassigned.ID && rec.RunID == "" {
			keptUnassigned = true
		}
		if rec.RunID == firstRun.ID {
			keptFirst[rec.ID] = true
		}
		if rec.RunID == secondRun.ID || rec.ID == orphan.ID {
			t.Errorf("deleted snapshot remains: %+v", rec)
		}
	}
	if !keptUnassigned || len(keptFirst) != len(firstRunRecords) {
		t.Fatalf("surviving snapshots: unassigned=%t run-one=%v", keptUnassigned, keptFirst)
	}
	assertGitObjectMissing(t, workspace, shadowDir, target.TreeOID)
	assertGitObjectMissing(t, workspace, shadowDir, secondPost.TreeOID)
	for _, rec := range firstRunRecords {
		assertGitObjectExists(t, workspace, shadowDir, rec.TreeOID)
	}
	assertGitObjectExists(t, workspace, shadowDir, unassigned.TreeOID)
	if got, err := os.ReadFile(filepath.Join(workspace, "second.txt")); err != nil || !bytes.Equal(got, wantTargetBytes) {
		t.Fatalf("retain changed workspace bytes=%q, err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(workspace, "first.txt")); err != nil || string(got) != "run one bytes\x00\n" {
		t.Fatalf("retain changed first file=%q, err=%v", got, err)
	}
}

func m9ToolResponse(id, path, content string) domain.ModelResponse {
	args, _ := json.Marshal(map[string]string{"path": path, "content": content})
	return domain.ModelResponse{Provider: "mock", Complete: true, Items: []domain.ResponseItem{{
		Ordinal: 0,
		Kind:    domain.ItemToolCall,
		ToolCall: &domain.ToolCall{
			ProviderCallID: id,
			Name:           "write_file",
			Arguments:      args,
		},
	}}}
}

func m9TextResponse(text string) domain.ModelResponse {
	return domain.ModelResponse{Provider: "mock", Complete: true, Items: []domain.ResponseItem{{Ordinal: 0, Kind: domain.ItemText, Text: text}}}
}

func snapshotsForRun(records []storage.WorkspaceSnapshot, runID string) []storage.WorkspaceSnapshot {
	var out []storage.WorkspaceSnapshot
	for _, rec := range records {
		if rec.RunID == runID {
			out = append(out, rec)
		}
	}
	return out
}

func mustWorkspaceSnapshots(t *testing.T, db interface {
	ListWorkspaceSnapshots(context.Context, string) ([]storage.WorkspaceSnapshot, error)
}, workspaceID string) []storage.WorkspaceSnapshot {
	t.Helper()
	records, err := db.ListWorkspaceSnapshots(context.Background(), workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	return records
}

type m9WorkspaceReport struct {
	WorkspaceID string `json:"workspace_id"`
	Snapshots   int    `json:"snapshots"`
	Objects     int64  `json:"objects"`
	Bytes       int64  `json:"bytes"`
	Error       string `json:"error"`
}

type m9RetainReport struct {
	DryRun               bool                `json:"dry_run"`
	RunsToDelete         int                 `json:"runs_to_delete"`
	RunsDeleted          int                 `json:"deleted_runs"`
	SnapshotsDeleted     int                 `json:"deleted_snapshots"`
	SnapshotsToDelete    int                 `json:"snapshots_to_delete"`
	UnassignedRetained   int                 `json:"unassigned_retained"`
	AffectedWorkspaces   int                 `json:"affected_workspaces"`
	ReclaimedWorkspaces  int                 `json:"reclaimed_workspaces"`
	ReclamationFailures  int                 `json:"reclamation_failures"`
	WorkspaceReclamation []m9WorkspaceReport `json:"workspace_reclamation"`
}

func decodeM9RetainReport(t *testing.T, raw []byte) m9RetainReport {
	t.Helper()
	var report m9RetainReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("decode retain report: %v; JSON=%s", err, raw)
	}
	return report
}

func assertGitObjectExists(t *testing.T, workspace, shadowDir, oid string) {
	t.Helper()
	cmd := exec.Command("git", "--git-dir="+shadowDir, "--work-tree="+workspace, "cat-file", "-e", oid)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Git object %s is missing: %v: %s", oid, err, output)
	}
}

func assertGitObjectMissing(t *testing.T, workspace, shadowDir, oid string) {
	t.Helper()
	cmd := exec.Command("git", "--git-dir="+shadowDir, "--work-tree="+workspace, "cat-file", "-e", oid)
	if output, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("Git object %s remains after reclamation: %s", oid, output)
	}
}
