//go:build linux || darwin

package tools

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

// evalCanonical stands in for the kernel's path lookup in tests that do not
// need macOS: symlinks resolved, spelling as given.
func evalCanonical(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}

// fakeSandboxExec writes a stand-in for sandbox-exec and returns its path.
func fakeSandboxExec(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sandbox-exec")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSeatbeltProfile(t *testing.T) {
	got, err := seatbeltProfile(seatbeltPlan{
		Deny:   []string{"/Users/me/proj/.cloud-harness", `/Users/me/Library/Application Support/cloud "x"\harness`},
		Pinned: []string{"/Users", "/Users/me", "/Users/me/proj"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `(version 1)
(allow default)
(deny file* (subpath "/Users/me/proj/.cloud-harness"))
(deny file* (subpath "/Users/me/Library/Application Support/cloud \"x\"\\harness"))
(deny file-write-unlink (literal "/Users") (literal "/Users/me") (literal "/Users/me/proj"))
`
	if got != want {
		t.Fatalf("profile:\n%s\nwant:\n%s", got, want)
	}
}

func TestSeatbeltProfilePreservesWorktree(t *testing.T) {
	got, err := seatbeltProfile(seatbeltPlan{
		Deny:     []string{"/w/.cloud-harness", "/w/.cloud-harness/worktrees/run1/.cloud-harness", "/cfg"},
		Preserve: "/w/.cloud-harness/worktrees/run1",
		Traverse: []string{"/w/.cloud-harness", "/w/.cloud-harness/worktrees"},
		Pinned:   []string{"/w"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `(version 1)
(allow default)
(deny file* (require-all (subpath "/w/.cloud-harness") (require-not (subpath "/w/.cloud-harness/worktrees/run1"))))
(deny file* (subpath "/w/.cloud-harness/worktrees/run1/.cloud-harness"))
(deny file* (subpath "/cfg"))
(allow file-read-metadata (literal "/w/.cloud-harness") (literal "/w/.cloud-harness/worktrees"))
(deny file-write-unlink (literal "/w"))
`
	if got != want {
		t.Fatalf("profile:\n%s\nwant:\n%s", got, want)
	}
}

func TestSeatbeltProfileRefusesPathsItCannotQuote(t *testing.T) {
	for _, path := range []string{"relative/.cloud-harness", "/w/line\nbreak", "/w/\x7f", "/w/\xff"} {
		if _, err := seatbeltProfile(seatbeltPlan{Deny: []string{path}}); err == nil {
			t.Errorf("%q: expected the profile to be refused", path)
		}
		if _, err := seatbeltProfile(seatbeltPlan{Deny: []string{"/w/a"}, Pinned: []string{path}}); err == nil {
			t.Errorf("%q: expected the pinned path to be refused", path)
		}
	}
}

func TestPlanSeatbeltCreatesAndKeepsMissingMasks(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	control := filepath.Join(root, ".cloud-harness")
	config := filepath.Join(root, "missing", "config")
	spec := commandSandboxSpec{Enabled: true, Paths: []commandSandboxPath{{Path: control}, {Path: config}}}

	plan, err := planSeatbelt(spec, evalCanonical)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Deny, []string{control, config}) || len(plan.Missing) != 0 {
		t.Fatalf("plan = %+v", plan)
	}
	// They stay after the command: a process it leaves running must not find
	// the name free again.
	for _, dir := range []string{control, config} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s was not created before the command: %v %v", dir, info, err)
		}
	}
	pinned := map[string]bool{}
	for _, dir := range plan.Pinned {
		pinned[dir] = true
	}
	for _, dir := range []string{root, filepath.Dir(root), filepath.Join(root, "missing")} {
		if !pinned[dir] {
			t.Errorf("%s is above a control path but may be renamed: %v", dir, plan.Pinned)
		}
	}
	if pinned[control] || pinned["/"] {
		t.Errorf("pinned = %v", plan.Pinned)
	}

	again, err := planSeatbelt(spec, evalCanonical)
	if err != nil || !reflect.DeepEqual(again, plan) {
		t.Fatalf("second plan = %+v, err = %v", again, err)
	}
}

func TestPlanSeatbeltRefusesMaskItCannotCreate(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The command runs as the same user and could chmod this directory back,
	// then create the control directory under another spelling.
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	spec := commandSandboxSpec{Paths: []commandSandboxPath{{Path: filepath.Join(root, "made", ".cloud-harness")}, {Path: filepath.Join(locked, "config")}}}
	_, err = planSeatbelt(spec, evalCanonical)
	if err == nil || !strings.Contains(err.Error(), "create control mask") || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("expected a control directory that cannot be created to stop the plan, got %v", err)
	}
}

func TestPlanSeatbeltKeepsExistingControlDirectory(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	control := filepath.Join(root, ".cloud-harness")
	if err := os.Mkdir(control, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := planSeatbelt(commandSandboxSpec{Paths: []commandSandboxPath{{Path: control}}}, evalCanonical); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(control); err != nil {
		t.Fatalf("existing control directory was removed: %v", err)
	}
}

func TestPlanSeatbeltDeniesSymlinkAndTarget(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "elsewhere")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	control := filepath.Join(root, ".cloud-harness")
	if err := os.Symlink(target, control); err != nil {
		t.Fatal(err)
	}
	plan, err := planSeatbelt(commandSandboxSpec{Paths: []commandSandboxPath{{Path: control}}}, evalCanonical)
	if err != nil || !reflect.DeepEqual(plan.Deny, []string{control, target}) {
		t.Fatalf("deny = %v, err = %v", plan.Deny, err)
	}

	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	_, err = planSeatbelt(commandSandboxSpec{Paths: []commandSandboxPath{{Path: control}}}, evalCanonical)
	if err == nil || !strings.Contains(err.Error(), "dangling symlink") {
		t.Fatalf("expected a dangling control symlink to be refused, got %v", err)
	}
}

func TestPlanSeatbeltWorktree(t *testing.T) {
	orig, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(t.TempDir(), "settings.json"))
	control := filepath.Join(orig, ".cloud-harness")
	worktree := filepath.Join(control, "worktrees", "run1")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	spec, err := commandSandboxSpecFor(worktree)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planSeatbelt(spec, evalCanonical)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Preserve != worktree {
		t.Fatalf("preserve = %q, want %q", plan.Preserve, worktree)
	}
	if want := []string{control, filepath.Join(control, "worktrees")}; !reflect.DeepEqual(plan.Traverse, want) {
		t.Fatalf("traverse = %v, want %v", plan.Traverse, want)
	}
	if plan.Deny[0] != control || plan.Deny[1] != filepath.Join(worktree, ".cloud-harness") {
		t.Fatalf("deny = %v", plan.Deny)
	}
	// On a case-insensitive volume the worktree's own control directory
	// must exist before the command can create a differently-cased one.
	if _, err := os.Stat(filepath.Join(worktree, ".cloud-harness")); err != nil {
		t.Fatalf("worktree control directory was not created: %v", err)
	}

	if _, err := planSeatbelt(commandSandboxSpec{Paths: []commandSandboxPath{{Path: worktree}}, Preserve: worktree}, evalCanonical); err == nil {
		t.Fatal("expected a mask on the command workspace itself to be refused")
	}
}

func TestPlanSeatbeltRefusesRootAndLookupFailures(t *testing.T) {
	if _, err := planSeatbelt(commandSandboxSpec{Paths: []commandSandboxPath{{Path: "/"}}}, evalCanonical); err == nil {
		t.Fatal("expected the filesystem root to be refused")
	}
	root := t.TempDir()
	broken := func(string) (string, error) { return "", syscall.EIO }
	_, err := planSeatbelt(commandSandboxSpec{Paths: []commandSandboxPath{{Path: filepath.Join(root, ".cloud-harness")}}}, broken)
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("expected a failed path lookup to stop the plan, got %v", err)
	}
}

func TestSeatbeltResult(t *testing.T) {
	exit := errors.New("exit status 3")
	if err := seatbeltResult(seatbeltStarted, "/bin/x", exit); err != exit {
		t.Fatalf("started command: %v", err)
	}
	if err := seatbeltResult(seatbeltStarted, "/bin/x", nil); err != nil {
		t.Fatalf("started command: %v", err)
	}
	var pathErr *fs.PathError
	if err := seatbeltResult(seatbeltStarted+"exec\n2", "/bin/x", exit); !errors.As(err, &pathErr) || pathErr.Err != syscall.ENOENT || pathErr.Path != "/bin/x" {
		t.Fatalf("exec failure: %v", err)
	}
	for _, status := range []string{"", "sandbox\nthe sandbox does not deny /x", "garbage", seatbeltStarted + "garbage"} {
		err := seatbeltResult(status, "/bin/x", nil)
		if err == nil || !strings.Contains(err.Error(), "the command was not started") {
			t.Errorf("status %q: %v", status, err)
		}
	}
	if err := seatbeltResult("", "/bin/x", exit); !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("sandbox-exec failure lost its cause: %v", err)
	}
}

// seatbeltTarget is a command that leaves a marker when it runs.
func seatbeltTarget(t *testing.T) (target string, argv []string, marker string) {
	t.Helper()
	dir := t.TempDir()
	marker = filepath.Join(dir, "started")
	target = filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("#!/bin/sh\necho ran > "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return target, []string{target}, marker
}

func TestSeatbeltDoesNotStartCommandWhenProfileIsNotEnforced(t *testing.T) {
	// This sandbox-exec runs the command without any sandbox, as a broken
	// or replaced one would.
	transparent := fakeSandboxExec(t, `shift 2; exec "$@"`)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	control := filepath.Join(root, ".cloud-harness")
	if err := os.Mkdir(control, 0o755); err != nil {
		t.Fatal(err)
	}
	target, argv, marker := seatbeltTarget(t)

	err = runSeatbelt(exec.Command(target), transparent, seatbeltPlan{Deny: []string{control}}, target, argv)
	if err == nil || !strings.Contains(err.Error(), "the sandbox does not deny "+control) {
		t.Fatalf("expected the command to be refused, got %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("command ran without a sandbox: %v", err)
	}
	if err := probeSeatbelt(transparent, evalCanonical); err == nil {
		t.Fatal("probe accepted a sandbox that enforces nothing")
	}
}

func TestSeatbeltDoesNotStartCommandWhenSandboxExecFails(t *testing.T) {
	failing := fakeSandboxExec(t, `echo "sandbox-exec: sandbox_apply: Operation not permitted" >&2; exit 71`)
	target, argv, marker := seatbeltTarget(t)
	var stderr strings.Builder
	cmd := exec.Command(target)
	cmd.Stderr = &stderr

	err := runSeatbelt(cmd, failing, seatbeltPlan{Deny: []string{"/nonexistent/.cloud-harness"}}, target, argv)
	if err == nil || !strings.Contains(err.Error(), "sandbox-exec did not start the command: exit status 71") {
		t.Fatalf("expected the command to be refused, got %v", err)
	}
	if !strings.Contains(stderr.String(), "sandbox_apply") {
		t.Fatalf("sandbox-exec's own error was lost: %q", stderr.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("command ran without a sandbox: %v", err)
	}
	if err := probeSeatbelt(failing, evalCanonical); err == nil {
		t.Fatal("probe accepted a sandbox-exec that fails")
	}
	if err := probeSeatbelt(filepath.Join(t.TempDir(), "absent"), evalCanonical); err == nil {
		t.Fatal("probe accepted a missing sandbox-exec")
	}
}

func TestSeatbeltRefusesPlanItCannotExpress(t *testing.T) {
	target, argv, marker := seatbeltTarget(t)
	err := runSeatbelt(exec.Command(target), fakeSandboxExec(t, `shift 2; exec "$@"`), seatbeltPlan{Deny: []string{"/w/line\nbreak"}}, target, argv)
	if err == nil || !strings.Contains(err.Error(), "cannot express path") {
		t.Fatalf("expected the command to be refused, got %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("command ran without a sandbox: %v", err)
	}
}

func TestVerifySeatbeltAcceptsOnlyDeniedPaths(t *testing.T) {
	dir := t.TempDir()
	if err := verifySeatbelt(seatbeltPlan{Deny: []string{dir}}); err == nil || !strings.Contains(err.Error(), "does not deny") {
		t.Fatalf("readable path passed verification: %v", err)
	}
	absent := filepath.Join(dir, "absent")
	if err := verifySeatbelt(seatbeltPlan{Deny: []string{absent}}); err == nil {
		t.Fatal("a path that should exist passed verification while absent")
	}
	if err := verifySeatbelt(seatbeltPlan{Deny: []string{absent}, Missing: []string{absent}}); err != nil {
		t.Fatalf("a path known to be missing failed verification: %v", err)
	}
	if err := verifySeatbelt(seatbeltPlan{Preserve: absent}); err == nil {
		t.Fatal("an unreachable workspace passed verification")
	}
}
