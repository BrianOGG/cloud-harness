package snapshot

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/storage/sqlite"
)

func TestSnapshotRestoreKeepsWorkspaceGitAndUntrackedFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatal(err)
	}
	workspaceGit(t, workspace, "init", "--quiet")
	workspaceGit(t, workspace, "config", "user.email", "snapshot-test@example.invalid")
	workspaceGit(t, workspace, "config", "user.name", "Snapshot Test")
	writeWorkspaceFile(t, workspace, ".gitattributes", []byte("filtered.txt text eol=lf\n"))
	writeWorkspaceFile(t, workspace, ".gitignore", []byte("ignored.txt\n"))
	writeWorkspaceFile(t, workspace, "keep.txt", []byte("first snapshot\x00bytes\n"))
	writeWorkspaceFile(t, workspace, "filtered.txt", []byte("first line\r\nsecond line\r\n"))
	writeWorkspaceFile(t, workspace, "removed.txt", []byte("must be restored\n"))
	writeWorkspaceFile(t, workspace, "ignored.txt", []byte("ignored by workspace rules\n"))
	workspaceGit(t, workspace, "add", "keep.txt", "removed.txt")
	writeWorkspaceFile(t, workspace, "preexisting-unstaged.txt", []byte("user data\n"))

	gitStateBefore := map[string][32]byte{
		"HEAD":  fileHash(t, filepath.Join(workspace, ".git", "HEAD")),
		"index": fileHash(t, filepath.Join(workspace, ".git", "index")),
	}
	assertWorkspaceGitUnchanged := func() {
		t.Helper()
		for name, before := range gitStateBefore {
			if after := fileHash(t, filepath.Join(workspace, ".git", name)); after != before {
				t.Fatalf("workspace .git/%s changed: before %x after %x", name, before, after)
			}
		}
	}
	db, err := sqlite.Open(ctx, filepath.Join(root, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	snap, err := Open(workspace, db)
	if err != nil {
		t.Fatal(err)
	}
	assertWorkspaceGitUnchanged()
	if got, want := snap.WorkspaceID(), workspaceIDFor(workspace); got != want {
		t.Fatalf("workspace ID %q, want %q", got, want)
	}
	first, err := snap.Snapshot(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	assertWorkspaceGitUnchanged()
	firstFiles := map[string][]byte{
		".gitattributes":           []byte("filtered.txt text eol=lf\n"),
		".gitignore":               []byte("ignored.txt\n"),
		"filtered.txt":             []byte("first line\r\nsecond line\r\n"),
		"keep.txt":                 []byte("first snapshot\x00bytes\n"),
		"removed.txt":              []byte("must be restored\n"),
		"preexisting-unstaged.txt": []byte("user data\n"),
	}
	if first.FileCount != int64(len(firstFiles)) || first.TotalBytes != int64(len(firstFiles[".gitattributes"])+len(firstFiles[".gitignore"])+len(firstFiles["filtered.txt"])+len(firstFiles["keep.txt"])+len(firstFiles["removed.txt"])+len(firstFiles["preexisting-unstaged.txt"])) {
		t.Fatalf("first snapshot counts: %+v", first)
	}
	writeWorkspaceFile(t, workspace, "keep.txt", []byte("second snapshot\n"))
	if err := os.Remove(filepath.Join(workspace, "removed.txt")); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, workspace, "added.txt", []byte("second-only\n"))
	second, err := snap.Snapshot(ctx, "second")
	if err != nil {
		t.Fatal(err)
	}
	assertWorkspaceGitUnchanged()
	writeWorkspaceFile(t, workspace, "never-snapshotted.txt", []byte("keep this file\n"))
	if err := snap.Restore(ctx, first); err != nil {
		t.Fatal(err)
	}
	assertWorkspaceGitUnchanged()
	for path, want := range firstFiles {
		got, err := os.ReadFile(filepath.Join(workspace, path))
		if err != nil || string(got) != string(want) {
			t.Fatalf("%s contents %q, error %v; want %q", path, got, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(workspace, "added.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second-only file still exists after restore: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(workspace, "ignored.txt")); err != nil || string(got) != "ignored by workspace rules\n" {
		t.Fatalf("gitignored file changed: %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(workspace, "never-snapshotted.txt")); err != nil || string(got) != "keep this file\n" {
		t.Fatalf("never-snapshotted file changed: %q, %v", got, err)
	}
	assertWorkspaceGitUnchanged()
	shadowDir := filepath.Join(workspace, ".cloud-harness", "snapshots", snap.WorkspaceID())
	exclude, err := os.ReadFile(filepath.Join(shadowDir, "info", "exclude"))
	if err != nil || !strings.Contains(string(exclude), ".cloud-harness/") {
		t.Fatalf("shadow exclude missing self-ignore rule: %q, %v", exclude, err)
	}
	listed, err := snap.List(ctx)
	if err != nil || len(listed) != 2 || listed[0].ID != first.ID || listed[1].ID != second.ID {
		t.Fatalf("snapshot list %+v, error %v", listed, err)
	}
	diff, err := snap.Diff(ctx, first, second)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Changes) != 3 || !strings.Contains(diff.Stat, "added.txt") || !strings.Contains(diff.Stat, "keep.txt") || !strings.Contains(diff.Stat, "removed.txt") {
		t.Fatalf("diff summary %+v", diff)
	}
}

func TestSnapshotUnavailableWhenGitIsNotOnPath(t *testing.T) {
	workspace := t.TempDir()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	t.Setenv("PATH", "")
	snap, err := Open(workspace, db)
	if err != nil {
		t.Fatal(err)
	}
	if status := snap.Status(); status.State != StateDisabled || status.Reason == "" {
		t.Fatalf("status %+v, want disabled with reason", status)
	}
	_, err = snap.Snapshot(context.Background(), "unavailable")
	var unavailable *UnavailableError
	if !errors.Is(err, ErrUnavailable) || !errors.As(err, &unavailable) {
		t.Fatalf("snapshot error %T %v, want typed unavailable error", err, err)
	}
}

func TestDiffCurrentPreviewsLiveChangesWithoutSnapshotRecord(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	snapper, err := Open(workspace, db)
	if err != nil {
		t.Fatal(err)
	}
	id, err := WorkspaceIDForPath(workspace)
	if err != nil || id != snapper.WorkspaceID() {
		t.Fatalf("path identity=%q, snapshot identity=%q, err=%v", id, snapper.WorkspaceID(), err)
	}
	writeWorkspaceFile(t, workspace, "A", []byte("old"))
	writeWorkspaceFile(t, workspace, "C", []byte("restore me"))
	target, err := snapper.Snapshot(ctx, "target")
	if err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, workspace, "A", []byte("live"))
	writeWorkspaceFile(t, workspace, "B", []byte("delete me"))
	if err := os.Remove(filepath.Join(workspace, "C")); err != nil {
		t.Fatal(err)
	}
	diff, err := snapper.DiffCurrent(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "M", "B": "D", "C": "A"}
	if len(diff.Changes) != len(want) {
		t.Fatalf("diff=%+v", diff)
	}
	for _, change := range diff.Changes {
		if want[change.Path] != change.Status {
			t.Fatalf("unexpected change: %+v", change)
		}
	}
	records, err := snapper.List(ctx)
	if err != nil || len(records) != 1 || records[0].ID != target.ID {
		t.Fatalf("preview persisted snapshot: %+v, err=%v", records, err)
	}
	for file, expected := range map[string]string{"A": "live", "B": "delete me"} {
		got, err := os.ReadFile(filepath.Join(workspace, file))
		if err != nil || string(got) != expected {
			t.Fatalf("preview changed %s: %q, err=%v", file, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(workspace, "C")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preview restored C: %v", err)
	}
	target.WorkspaceID = "other"
	if _, err := snapper.DiffCurrent(ctx, target); err == nil {
		t.Fatal("preview accepted a different workspace")
	}
}

func fileHash(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}

func writeWorkspaceFile(t *testing.T, workspace, name string, data []byte) {
	t.Helper()
	path := filepath.Join(workspace, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func workspaceGit(t *testing.T, workspace string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = workspace
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}
