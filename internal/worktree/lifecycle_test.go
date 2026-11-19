package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLifecycleInventoryAndCleanMerge(t *testing.T) {
	ctx := context.Background()
	root := testRepo(t)
	a, err := Create(ctx, root, "run_a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Create(ctx, root, "run_b")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := List(ctx, root)
	if err != nil || len(entries) != 2 || entries[0].BaseSHA != a.BaseSHA || !entries[0].Exists || !entries[1].Registered {
		t.Fatalf("inventory: %+v %v", entries, err)
	}
	write(t, a.Path, "file.txt", "agent\ntwo\n")
	calls := []string{}
	recorded := false
	err = Merge(ctx, root, a, MergeOptions{Test: func(_ context.Context, path string) error {
		calls = append(calls, path)
		if read(t, path, "file.txt") != "agent\ntwo\n" {
			t.Error("gate saw wrong content")
		}
		return nil
	}, Record: func(context.Context) error { recorded = true; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if !recorded || !reflect.DeepEqual(calls, []string{a.Path, root}) || read(t, root, "file.txt") != "agent\ntwo\n" {
		t.Fatalf("merge: calls=%v recorded=%v", calls, recorded)
	}
	assertRemoved(t, root, a)
	if _, exists, err := Lookup(ctx, root, b.RunID); err != nil || !exists {
		t.Fatalf("other worktree removed: %v", err)
	}
	if err := Remove(ctx, root, a); err != nil {
		t.Fatalf("idempotent removal: %v", err)
	}
}

func TestMergeConflictFailureAndManualRetry(t *testing.T) {
	ctx := context.Background()
	root := testRepo(t)
	info, err := Create(ctx, root, "run_conflict")
	if err != nil {
		t.Fatal(err)
	}
	write(t, info.Path, "file.txt", "agent\ntwo\n")
	write(t, root, "file.txt", "main\ntwo\n")
	testGit(t, root, "commit", "-qam", "main edit")
	before := testGit(t, root, "rev-parse", "HEAD")
	attempts := 0
	opts := MergeOptions{Test: func(context.Context, string) error { return nil }, Resolve: func(context.Context, Info, []string, string) error { attempts++; return nil }}
	err = Merge(ctx, root, info, opts)
	var conflict *ConflictError
	if !errors.As(err, &conflict) || !reflect.DeepEqual(conflict.Files, []string{"file.txt"}) || attempts != ConflictResolutionAttempts {
		t.Fatalf("conflict: attempts=%d err=%v", attempts, err)
	}
	if testGit(t, root, "rev-parse", "HEAD") != before || read(t, root, "file.txt") != "main\ntwo\n" || !merging(ctx, info.Path) {
		t.Fatal("conflict touched main or lost agent merge")
	}
	if testGit(t, info.Path, "rev-parse", "MERGE_HEAD") != before {
		t.Fatal("main was not merged into agent first")
	}
	write(t, info.Path, "file.txt", "both\ntwo\n")
	testGit(t, info.Path, "add", "file.txt")
	if err := Merge(ctx, root, info, opts); err != nil {
		t.Fatal(err)
	}
	if read(t, root, "file.txt") != "both\ntwo\n" || attempts != ConflictResolutionAttempts {
		t.Fatal("manual retry did not preserve resolution")
	}
	assertRemoved(t, root, info)
}

func TestMergeAgentResolvesConflictAndRunsTests(t *testing.T) {
	ctx := context.Background()
	root := testRepo(t)
	info, err := Create(ctx, root, "run_repair")
	if err != nil {
		t.Fatal(err)
	}
	write(t, info.Path, "file.txt", "agent\ntwo\n")
	write(t, root, "file.txt", "main\ntwo\n")
	testGit(t, root, "commit", "-qam", "main edit")
	attempts, gates := 0, 0
	err = Merge(ctx, root, info, MergeOptions{Test: func(context.Context, string) error { gates++; return nil }, Resolve: func(_ context.Context, got Info, files []string, feedback string) error {
		attempts++
		if got != info || len(files) != 1 {
			t.Fatal("bad resolver input")
		}
		write(t, info.Path, "file.txt", "both\ntwo\n")
		return nil
	}})
	if err != nil || attempts != 1 || gates != 2 {
		t.Fatalf("repair attempts=%d gates=%d err=%v", attempts, gates, err)
	}
}

func TestMergeAbortLeavesMainUnchanged(t *testing.T) {
	ctx := context.Background()
	root := testRepo(t)
	info, err := Create(ctx, root, "run_abort")
	if err != nil {
		t.Fatal(err)
	}
	write(t, info.Path, "file.txt", "agent\ntwo\n")
	before := testGit(t, root, "rev-parse", "HEAD")
	// A real pre-merge-commit failure leaves MERGE_HEAD in main. The coordinator
	// must invoke merge --abort, rather than merely returning Git's error.
	hook := filepath.Join(root, ".git", "hooks", "pre-merge-commit")
	if err := os.MkdirAll(filepath.Dir(hook), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	err = Merge(ctx, root, info, MergeOptions{Test: func(context.Context, string) error { return nil }})
	if err == nil || !strings.Contains(err.Error(), "merge into main failed") {
		t.Fatalf("expected main merge failure: %v", err)
	}
	if merging(ctx, root) || testGit(t, root, "rev-parse", "HEAD") != before || testGit(t, root, "status", "--porcelain") != "" || read(t, root, "file.txt") != "one\ntwo\n" {
		t.Fatal("merge --abort failed to restore main")
	}
	if _, exists, err := Lookup(ctx, root, info.RunID); err != nil || !exists {
		t.Fatal("failed merge lost worktree")
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	if err := Merge(ctx, root, info, MergeOptions{Test: func(context.Context, string) error { return nil }}); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestMergeRollbackTestAndRecordFailure(t *testing.T) {
	for _, failure := range []string{"test", "record", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			root := testRepo(t)
			info, err := Create(ctx, root, "run_rollback")
			if err != nil {
				t.Fatal(err)
			}
			write(t, info.Path, "file.txt", "agent\ntwo\n")
			before := testGit(t, root, "rev-parse", "HEAD")
			recorded := false
			opts := MergeOptions{Test: func(_ context.Context, path string) error {
				if path == root && failure != "record" {
					if failure == "cancel" {
						cancel()
					}
					return errors.New("test gate failed")
				}
				return nil
			}, Record: func(context.Context) error { recorded = true; return errors.New("DB unavailable") }}
			err = Merge(ctx, root, info, opts)
			if err == nil || !strings.Contains(err.Error(), "rolled back to "+before) || recorded != (failure == "record") {
				t.Fatalf("rollback: %v recorded=%v", err, recorded)
			}
			if testGit(t, root, "rev-parse", "HEAD") != before || read(t, root, "file.txt") != "one\ntwo\n" || testGit(t, root, "status", "--porcelain") != "" {
				t.Fatal("main not restored")
			}
			if _, exists, err := Lookup(context.Background(), root, info.RunID); err != nil || !exists {
				t.Fatal("rollback lost agent branch")
			}
		})
	}
}

func TestMergeRejectsDetachedAndDirtyMain(t *testing.T) {
	for _, state := range []string{"detached", "dirty"} {
		t.Run(state, func(t *testing.T) {
			root := testRepo(t)
			info, err := Create(context.Background(), root, "run_bad_main")
			if err != nil {
				t.Fatal(err)
			}
			if state == "detached" {
				testGit(t, root, "checkout", "--detach", "-q")
			} else {
				write(t, root, "file.txt", "dirty\n")
			}
			if err := Merge(context.Background(), root, info, MergeOptions{Test: func(context.Context, string) error { t.Fatal("test gate should not run"); return nil }}); err == nil {
				t.Fatal("invalid main accepted")
			}
		})
	}
}

func assertRemoved(t *testing.T, root string, info Info) {
	t.Helper()
	if _, err := os.Stat(info.Path); !os.IsNotExist(err) {
		t.Fatalf("worktree remains: %v", err)
	}
	if _, err := git(context.Background(), root, nil, "show-ref", "--verify", "refs/heads/"+info.Branch); err == nil {
		t.Fatal("branch remains")
	}
	if _, err := os.Stat(info.Path + ".json"); !os.IsNotExist(err) {
		t.Fatal("metadata remains")
	}
}

func TestRetriedMergeSharesRepairBudgetWithNewMain(t *testing.T) {
	ctx := context.Background()
	root := testRepo(t)
	info, err := Create(ctx, root, "run_budget")
	if err != nil {
		t.Fatal(err)
	}
	write(t, info.Path, "file.txt", "agent\ntwo\n")
	write(t, root, "file.txt", "main A\ntwo\n")
	testGit(t, root, "commit", "-qam", "main A")
	if err := Merge(ctx, root, info, MergeOptions{Test: func(context.Context, string) error { return nil }}); err == nil {
		t.Fatal("expected retained conflict")
	}
	write(t, root, "file.txt", "main B\ntwo\n")
	testGit(t, root, "commit", "-qam", "main B")
	before := testGit(t, root, "rev-parse", "HEAD")
	attempts := 0
	err = Merge(ctx, root, info, MergeOptions{Test: func(context.Context, string) error { return nil }, Resolve: func(context.Context, Info, []string, string) error {
		attempts++
		if attempts == 1 {
			return errors.New("try again")
		}
		write(t, info.Path, "file.txt", "combined A\ntwo\n")
		return nil
	}})
	var conflict *ConflictError
	if !errors.As(err, &conflict) || attempts != ConflictResolutionAttempts {
		t.Fatalf("repair limit not shared: attempts=%d err=%v", attempts, err)
	}
	if testGit(t, root, "rev-parse", "HEAD") != before || !merging(ctx, info.Path) || testGit(t, info.Path, "rev-parse", "MERGE_HEAD") != before {
		t.Fatal("new conflict not retained against current main")
	}
}
