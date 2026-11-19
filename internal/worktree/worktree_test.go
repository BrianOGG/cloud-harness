package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func testGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	out, err := git(context.Background(), root, nil, args...)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func testRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	root := t.TempDir()
	testGit(t, root, "init", "-q", "--template=")
	testGit(t, root, "config", "user.name", "Worktree Test")
	testGit(t, root, "config", "user.email", "worktree@example.invalid")
	testGit(t, root, "config", "commit.gpgsign", "false")
	testGit(t, root, "config", "core.autocrlf", "false")
	write(t, root, "file.txt", "one\ntwo\n")
	write(t, root, "delete.txt", "remove\n")
	testGit(t, root, "add", ".")
	testGit(t, root, "commit", "-qm", "base")
	return root
}

func write(t *testing.T, root, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, root, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestCreateLookupIsolationAndPendingSummary(t *testing.T) {
	ctx := context.Background()
	root := testRepo(t)
	base := testGit(t, root, "rev-parse", "HEAD")
	write(t, root, "file.txt", "dirty main stays here\n")
	mainStatus := testGit(t, root, "status", "--porcelain=v1", "--untracked-files=all")
	if _, exists, err := Lookup(ctx, root, "run_test"); err != nil || exists {
		t.Fatalf("missing lookup: exists=%v err=%v", exists, err)
	}
	info, err := Create(ctx, root, "run_test")
	if err != nil {
		t.Fatal(err)
	}
	if info.BaseSHA != base || info.Branch != "cloud-harness/run_test" || info.Path != filepath.Join(root, ".cloud-harness", "worktrees", "run_test") {
		t.Fatalf("info: %+v", info)
	}
	if testGit(t, info.Path, "branch", "--show-current") != info.Branch || read(t, info.Path, "file.txt") != "one\ntwo\n" || testGit(t, info.Path, "rev-parse", "HEAD") != base {
		t.Fatal("worktree did not check out the dispatch HEAD")
	}
	loaded, exists, err := Lookup(ctx, root, info.RunID)
	if err != nil || !exists || loaded != info {
		t.Fatalf("lookup: %+v %v %v", loaded, exists, err)
	}
	write(t, info.Path, "file.txt", "changed\ntwo\n")
	testGit(t, info.Path, "add", "file.txt")
	indexBefore := testGit(t, info.Path, "write-tree")
	write(t, info.Path, "file.txt", "changed again\ntwo\n")
	write(t, info.Path, "new file.txt", "new\n")
	if err := os.Remove(filepath.Join(info.Path, "delete.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(info.Path, ".cloud-harness"), 0755); err != nil {
		t.Fatal(err)
	}
	write(t, info.Path, ".cloud-harness/secret-state", "do not include\n")
	before := testGit(t, info.Path, "status", "--porcelain=v1", "--untracked-files=all")
	summary, err := Summarize(ctx, info)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(summary.Files, []string{"delete.txt", "file.txt", "new file.txt"}) || !strings.Contains(summary.Stat, "3 files changed, 2 insertions(+), 2 deletions(-)") || !strings.Contains(summary.Diff, "+changed again") || !strings.Contains(summary.Diff, "+new") || summary.Truncated {
		t.Fatalf("summary: %+v", summary)
	}
	if testGit(t, info.Path, "write-tree") != indexBefore || testGit(t, info.Path, "status", "--porcelain=v1", "--untracked-files=all") != before {
		t.Fatal("summary changed the real index or workspace status")
	}
	if read(t, root, "file.txt") != "dirty main stays here\n" || read(t, root, "delete.txt") != "remove\n" || testGit(t, root, "status", "--porcelain=v1", "--untracked-files=all") != mainStatus {
		t.Fatal("worktree operations changed the main workspace")
	}
	if _, err := Create(ctx, root, "run_second"); err != nil {
		t.Fatal(err)
	}
	if strings.Count(read(t, root, ".git/info/exclude"), ".cloud-harness/\n") != 1 {
		t.Fatal("exclude entry was not idempotent")
	}
}

func TestCommittedSummaryMatchesTripleDotAndBaseStaysFixed(t *testing.T) {
	ctx := context.Background()
	root := testRepo(t)
	first := testGit(t, root, "rev-parse", "HEAD")
	write(t, root, "file.txt", "new dispatch head\n")
	testGit(t, root, "commit", "-qam", "advance main")
	base := testGit(t, root, "rev-parse", "HEAD")
	info, err := Create(ctx, root, "run_commit")
	if err != nil {
		t.Fatal(err)
	}
	if info.BaseSHA != base || info.BaseSHA == first {
		t.Fatal("base was not captured at dispatch")
	}
	write(t, info.Path, "file.txt", "committed result\n")
	testGit(t, info.Path, "commit", "-qam", "worktree edit")
	summary, err := Summarize(ctx, info)
	if err != nil {
		t.Fatal(err)
	}
	expected := testGit(t, info.Path, "diff", "--no-color", base+"...HEAD")
	if strings.TrimSpace(summary.Diff) != expected {
		t.Fatalf("diff mismatch: %s != %s", summary.Diff, expected)
	}
	loaded, exists, err := Lookup(ctx, root, info.RunID)
	if err != nil || !exists || loaded.BaseSHA != base {
		t.Fatalf("base lost after commit: %+v %v %v", loaded, exists, err)
	}
}

func TestSummaryCapsPatchButKeepsFilesAndStat(t *testing.T) {
	root := testRepo(t)
	info, err := Create(context.Background(), root, "run_large")
	if err != nil {
		t.Fatal(err)
	}
	write(t, info.Path, "large.txt", strings.Repeat("长行的文本\n", MaxDiffBytes))
	summary, err := Summarize(context.Background(), info)
	if err != nil {
		t.Fatal(err)
	}
	if !summary.Truncated || len(summary.Diff) > MaxDiffBytes || !utf8.ValidString(summary.Diff) || !reflect.DeepEqual(summary.Files, []string{"large.txt"}) || !strings.Contains(summary.Stat, "65536 insertions(+)") {
		t.Fatalf("bad truncation: files=%v stat=%s bytes=%d truncated=%v", summary.Files, summary.Stat, len(summary.Diff), summary.Truncated)
	}
	// JSON-safe replacement of invalid source bytes can itself exceed the
	// output cap, even when the raw Git patch was shorter than the cap.
	write(t, info.Path, ".gitattributes", "large.txt diff\n")
	write(t, info.Path, "large.txt", strings.Repeat("\xffx", MaxDiffBytes/3))
	summary, err = Summarize(context.Background(), info)
	if err != nil || !summary.Truncated || len(summary.Diff) > MaxDiffBytes || !utf8.ValidString(summary.Diff) {
		t.Fatalf("invalid UTF-8 cap: bytes=%d truncated=%v err=%v", len(summary.Diff), summary.Truncated, err)
	}
}

func TestRejectInvalidAndNestedWorkspacesWithoutCreatingState(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if _, err := Create(ctx, root, "run_plain"); err == nil || !strings.Contains(err.Error(), ".git") {
		t.Fatalf("non-Git workspace: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".cloud-harness")); !os.IsNotExist(err) {
		t.Fatalf("non-Git workspace created state: %v", err)
	}
	root = testRepo(t)
	if _, err := Create(ctx, root, "../escape"); err == nil {
		t.Fatal("invalid run id accepted")
	}
	info, err := Create(ctx, root, "run_outer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Create(ctx, info.Path, "run_nested"); err == nil || !strings.Contains(err.Error(), "already a linked") {
		t.Fatalf("nested worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(info.Path, ".cloud-harness")); !os.IsNotExist(err) {
		t.Fatalf("nested validation created state: %v", err)
	}
	if _, err := Create(ctx, root, info.RunID); err == nil || !strings.Contains(err.Error(), "git worktree add") || !strings.Contains(err.Error(), "stderr:") || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("command diagnostics missing: %v", err)
	}
}

func TestIgnoreInheritedGitRouting(t *testing.T) {
	root := testRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "wrong.git"))
	t.Setenv("GIT_WORK_TREE", t.TempDir())
	t.Setenv("GIT_INDEX_FILE", filepath.Join(t.TempDir(), "wrong-index"))
	info, err := Create(context.Background(), root, "run_env")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Summarize(context.Background(), info); err != nil {
		t.Fatal(err)
	}
}
