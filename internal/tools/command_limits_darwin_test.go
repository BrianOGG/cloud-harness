//go:build darwin

package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const swapHelperEnv = "CLOUD_HARNESS_TEST_SWAP"

// TestSeatbeltSwapHelper is not a test: commands under test re-run this binary
// with swapHelperEnv set to "a:b" to swap two directories atomically, which no
// stock tool does.
func TestSeatbeltSwapHelper(t *testing.T) {
	a, b, ok := strings.Cut(os.Getenv(swapHelperEnv), ":")
	if !ok {
		return
	}
	if err := unix.RenamexNp(a, b, unix.RENAME_SWAP); err != nil {
		fmt.Println("swap refused:", err)
		os.Exit(1)
	}
	fmt.Println("swap done")
	os.Exit(0)
}

func needSeatbelt(t *testing.T) {
	t.Helper()
	if !commandSandboxAvailable() {
		if os.Getenv("CLOUD_HARNESS_REQUIRE_SANDBOX") != "" {
			t.Fatal("Seatbelt cannot be applied here and CLOUD_HARNESS_REQUIRE_SANDBOX is set")
		}
		t.Skip("Seatbelt cannot be applied here (sandbox-exec missing, or the tests already run inside a sandbox)")
	}
}

// tempDir is t.TempDir under the name the kernel uses for it.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := canonicalPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func wantFile(t *testing.T, path, body string) {
	t.Helper()
	if got, err := os.ReadFile(path); err != nil || string(got) != body {
		t.Errorf("%s changed: %q %v", path, got, err)
	}
}

func wantEmptyDir(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if entries, err := os.ReadDir(path); err != nil || len(entries) != 0 {
			t.Errorf("%s is not an empty directory after the command: %v %v", path, entries, err)
		}
	}
}

func wantAbsent(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("%s exists after the command: %v", path, err)
		}
	}
}

func TestCanonicalPath(t *testing.T) {
	dir := t.TempDir()
	got, err := canonicalPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := filepath.EvalSymlinks(dir); got != want {
		t.Fatalf("canonicalPath(%q) = %q, want %q", dir, got, want)
	}
	mixed := filepath.Join(got, "MixedCase")
	if err := os.Mkdir(mixed, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(got, "mixedcase")); err == nil {
		// Case-insensitive volume: every spelling names the same directory.
		if spelled, err := canonicalPath(filepath.Join(got, "mixedcase")); err != nil || spelled != mixed {
			t.Fatalf("canonicalPath did not restore the stored spelling: %q %v", spelled, err)
		}
	}
	link := filepath.Join(got, "link")
	if err := os.Symlink(mixed, link); err != nil {
		t.Fatal(err)
	}
	if resolved, err := canonicalPath(link); err != nil || resolved != mixed {
		t.Fatalf("canonicalPath did not follow the symlink: %q %v", resolved, err)
	}
	if _, err := canonicalPath(filepath.Join(got, "absent")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing path: %v", err)
	}
}

func TestRunCommandDeniesControlDirectory(t *testing.T) {
	needSeatbelt(t)
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(t.TempDir(), "settings.json"))
	limitHelpers(t, map[string]string{"probe-control": `set +e
ls .cloud-harness
cat .cloud-harness/secret
cat .CLOUD-HARNESS/secret
cat "/System/Volumes/Data$(pwd -P)/.cloud-harness/secret"
ln -s .cloud-harness alias
cat alias/secret
printf x > .cloud-harness/new
printf x >> .cloud-harness/secret
ln .cloud-harness/secret hardlink
cp -c .cloud-harness/secret cloned
cp .cloud-harness/secret copied
mv .cloud-harness moved
rm -rf .cloud-harness
printf WORKSPACE_OK > plain.txt
cat plain.txt
`})
	root := tempDir(t)
	secret := filepath.Join(root, ".cloud-harness", "secret")
	writeFile(t, secret, "CONTROL_SECRET")

	_, out := runLimited(t, context.Background(), root, CommandLimits{}, "probe-control")
	combined := out.Stdout + out.Stderr
	if strings.Contains(combined, "CONTROL_SECRET") {
		t.Fatalf("command read a control file:\n%s", combined)
	}
	if !strings.Contains(out.Stderr, "Operation not permitted") {
		t.Fatalf("control directory was not denied by the sandbox:\n%s", combined)
	}
	if !strings.Contains(out.Stdout, "WORKSPACE_OK") {
		t.Fatalf("command could not use the rest of the workspace:\n%s", combined)
	}
	wantFile(t, secret, "CONTROL_SECRET")
	for _, name := range []string{"hardlink", "cloned", "copied", "moved"} {
		if body, err := os.ReadFile(filepath.Join(root, name)); err == nil && len(body) > 0 {
			t.Errorf("%s holds control data: %q", name, body)
		}
	}
	wantAbsent(t, filepath.Join(root, ".cloud-harness", "new"), filepath.Join(root, "moved"), filepath.Join(root, "hardlink"))
}

func TestRunCommandDeniesConfigDirectory(t *testing.T) {
	needSeatbelt(t)
	config := filepath.Join(tempDir(t), "Cloud Harness")
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(config, "settings.json"))
	trust, policy := filepath.Join(config, "trust.json"), filepath.Join(config, "policy.json")
	writeFile(t, trust, "TRUST_SECRET")
	writeFile(t, policy, "POLICY_SECRET")
	limitHelpers(t, map[string]string{"probe-config": fmt.Sprintf(`set +e
cfg=%q
ls "$cfg"
cat "$cfg/trust.json" "$cfg/policy.json"
printf x > "$cfg/trust.json"
printf x >> "$cfg/policy.json"
printf x > "$cfg/new.json"
ln "$cfg/trust.json" trust-link
cp -c "$cfg/policy.json" policy-clone
rm -f "$cfg/trust.json"
mv "$cfg" "$cfg.moved"
`, config)})
	root := tempDir(t)

	_, out := runLimited(t, context.Background(), root, CommandLimits{}, "probe-config")
	combined := out.Stdout + out.Stderr
	if strings.Contains(combined, "_SECRET") || strings.Contains(out.Stdout, "trust.json") {
		t.Fatalf("command read the configuration directory:\n%s", combined)
	}
	if !strings.Contains(out.Stderr, "Operation not permitted") {
		t.Fatalf("configuration directory was not denied by the sandbox:\n%s", combined)
	}
	wantFile(t, trust, "TRUST_SECRET")
	wantFile(t, policy, "POLICY_SECRET")
	wantAbsent(t, filepath.Join(config, "new.json"), config+".moved", filepath.Join(root, "trust-link"))
	if body, err := os.ReadFile(filepath.Join(root, "policy-clone")); err == nil && len(body) > 0 {
		t.Errorf("policy was cloned out of the configuration directory: %q", body)
	}
}

func TestRunCommandCannotCreateMissingControlFiles(t *testing.T) {
	needSeatbelt(t)
	config := filepath.Join(tempDir(t), "missing", "config")
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(config, "settings.json"))
	limitHelpers(t, map[string]string{"make-control": fmt.Sprintf(`set +e
cfg=%q
mkdir -p .cloud-harness/hooks
printf x > .cloud-harness/policy.json
printf x > .Cloud-Harness/policy.json
mkdir -p "$cfg"
printf x > "$cfg/trust.json"
`, config)})
	root := tempDir(t)

	_, out := runLimited(t, context.Background(), root, CommandLimits{}, "make-control")
	if !strings.Contains(out.Stderr, "Operation not permitted") {
		t.Fatalf("command was not denied:\n%s", out.Stdout+out.Stderr)
	}
	// The harness created both directories and they are still empty. On a
	// case-insensitive volume this also covers every other spelling.
	wantEmptyDir(t, filepath.Join(root, ".cloud-harness"), config)
}

// TestRunCommandSurvivorCannotCreateControlFiles covers a process the command
// leaves running: it keeps its sandbox, and the control directories created
// for the command must still be there when it acts.
func TestRunCommandSurvivorCannotCreateControlFiles(t *testing.T) {
	needSeatbelt(t)
	config := filepath.Join(tempDir(t), "missing", "config")
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(config, "settings.json"))
	root := tempDir(t)
	other := filepath.Join(filepath.Dir(config), "Config")
	release, done := filepath.Join(root, "survivor.go"), filepath.Join(root, "survivor.done")
	limitHelpers(t, map[string]string{"leave-survivor": fmt.Sprintf(`(
	while [ ! -e %q ]; do sleep 0.1; done
	mkdir -p .Cloud-Harness %q
	printf PLANTED > .Cloud-Harness/policy.json
	printf PLANTED > .cloud-harness/policy.json
	printf PLANTED > %q
	printf PLANTED > %q
	printf done > %q
) </dev/null >/dev/null 2>&1 &
echo STARTED
`, release, other, filepath.Join(other, "trust.json"), filepath.Join(config, "trust.json"), done)})

	_, out := runLimited(t, context.Background(), root, CommandLimits{}, "leave-survivor")
	if !strings.Contains(out.Stdout, "STARTED") {
		t.Fatalf("command did not run: %+v", out)
	}
	// The command has returned; only now does the survivor act.
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if _, err := os.Stat(done); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("survivor did not finish")
		}
	}
	// Read the way the harness would, whatever the volume's case rules.
	wantAbsent(t, filepath.Join(root, ".cloud-harness", "policy.json"), filepath.Join(config, "trust.json"))
	wantEmptyDir(t, filepath.Join(root, ".cloud-harness"), config)
}

// TestRunCommandCancelKillsSandboxedCommand covers a cancelled run: the kill
// has to reach the command through sandbox-exec and the trampoline, which
// replace each other in one process.
func TestRunCommandCancelKillsSandboxedCommand(t *testing.T) {
	needSeatbelt(t)
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(t.TempDir(), "settings.json"))
	root := tempDir(t)
	pidFile := filepath.Join(root, "command.pid")
	limitHelpers(t, map[string]string{"sleep-long": fmt.Sprintf("echo $$ > %q\nexec sleep 60", pidFile)})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer cancel()
		for i := 0; i < 1000; i++ {
			if body, err := os.ReadFile(pidFile); err == nil && strings.HasSuffix(string(body), "\n") {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	started := time.Now()
	runLimited(t, ctx, root, CommandLimits{}, "sleep-long")
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Fatalf("cancelled command returned after %v", elapsed)
	}
	body, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("command did not start: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	// run_command waited for the process, so the pid is gone, not a zombie.
	if err := unix.Kill(pid, 0); !errors.Is(err, unix.ESRCH) {
		t.Fatalf("command %d survived cancellation: %v", pid, err)
	}
	wantEmptyDir(t, filepath.Join(root, ".cloud-harness"))
}

// TestRunCommandVolumeCaseSensitivity only records what the isolation tests ran on.
func TestRunCommandVolumeCaseSensitivity(t *testing.T) {
	dir := tempDir(t)
	if err := os.Mkdir(filepath.Join(dir, "CaseProbe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "caseprobe")); err == nil {
		t.Log("test volume is case-insensitive: other spellings name the same control directory")
	} else {
		t.Log("test volume is case-sensitive: other spellings name different directories")
	}
}

func TestRunCommandRefusedWhenMissingConfigDirectoryCannotBeHeld(t *testing.T) {
	needSeatbelt(t)
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	locked := filepath.Join(tempDir(t), "locked")
	if err := os.Mkdir(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(locked, "config", "settings.json"))
	// Without the directory the rule names a path that does not exist, and
	// on a case-insensitive volume this would plant a policy the harness reads.
	limitHelpers(t, map[string]string{"plant-config": fmt.Sprintf(`set +e
chmod u+w %[1]q
mkdir %[1]q/Config
printf PLANTED > %[1]q/Config/policy.json
`, locked)})
	root := tempDir(t)

	_, out := runLimited(t, context.Background(), root, CommandLimits{}, "plant-config")
	if !strings.Contains(out.Error, "the command was not started") || !strings.Contains(out.Error, "create control mask") {
		t.Fatalf("expected the command to be refused: %+v", out)
	}
	if info, err := os.Stat(locked); err != nil || info.Mode().Perm() != 0o555 {
		t.Fatalf("command ran and changed the parent directory: %v %v", info, err)
	}
	wantAbsent(t, filepath.Join(locked, "Config"), filepath.Join(locked, "config"))
}

func TestRunCommandCannotMoveDirectoriesAboveControlFiles(t *testing.T) {
	needSeatbelt(t)
	base := tempDir(t)
	root := filepath.Join(base, "ws")
	other := filepath.Join(base, "other")
	config := filepath.Join(base, "home", "config")
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(config, "settings.json"))
	writeFile(t, filepath.Join(root, ".cloud-harness", "secret"), "CONTROL_SECRET")
	writeFile(t, filepath.Join(config, "trust.json"), "TRUST_SECRET")
	writeFile(t, filepath.Join(other, "marker"), "other")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Seatbelt rules follow a file's current path, so each of these would
	// expose the control files under a name no rule covers.
	limitHelpers(t, map[string]string{"move-parents": fmt.Sprintf(`set +e
base=%q
mv "$base/ws" "$base/ws-moved"
cat "$base/ws-moved/.cloud-harness/secret"
mv "$base/home" "$base/home-moved"
cat "$base/home-moved/config/trust.json"
%s="$base/ws:$base/other" %q -test.run='^TestSeatbeltSwapHelper$'
cat "$base/other/.cloud-harness/secret"
`, base, swapHelperEnv, self)})

	_, out := runLimited(t, context.Background(), root, CommandLimits{}, "move-parents")
	combined := out.Stdout + out.Stderr
	if strings.Contains(combined, "_SECRET") {
		t.Fatalf("command read control files by moving a parent directory:\n%s", combined)
	}
	if !strings.Contains(out.Stdout, "swap refused") {
		t.Fatalf("swap helper did not run or was not refused:\n%s", combined)
	}
	wantFile(t, filepath.Join(root, ".cloud-harness", "secret"), "CONTROL_SECRET")
	wantFile(t, filepath.Join(config, "trust.json"), "TRUST_SECRET")
	wantFile(t, filepath.Join(other, "marker"), "other")
	wantAbsent(t, filepath.Join(base, "ws-moved"), filepath.Join(base, "home-moved"))
}

func TestRunCommandDeniesOriginalControlInWorktree(t *testing.T) {
	needSeatbelt(t)
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(t.TempDir(), "settings.json"))
	limitHelpers(t, map[string]string{"probe-worktree": `set +e
pwd -P
cat note.txt
printf WRITE_OK > written.txt
cat written.txt
cat ../../harness.db
printf x > ../../harness.db
ls ..
cat ../run2/note.txt
ls ../..
`})
	orig := tempDir(t)
	control := filepath.Join(orig, ".cloud-harness")
	worktree := filepath.Join(control, "worktrees", "run1")
	writeFile(t, filepath.Join(control, "harness.db"), "DB_SECRET")
	writeFile(t, filepath.Join(worktree, "note.txt"), "NOTE_OK")
	writeFile(t, filepath.Join(control, "worktrees", "run2", "note.txt"), "OTHER_RUN_SECRET")

	_, out := runLimited(t, context.Background(), worktree, CommandLimits{}, "probe-worktree")
	combined := out.Stdout + out.Stderr
	if got := strings.TrimSpace(strings.SplitN(out.Stdout, "\n", 2)[0]); got != worktree {
		t.Fatalf("command cwd = %q, want worktree %q\n%s", got, worktree, combined)
	}
	if !strings.Contains(out.Stdout, "NOTE_OK") || !strings.Contains(out.Stdout, "WRITE_OK") {
		t.Fatalf("command could not use its worktree:\n%s", combined)
	}
	if strings.Contains(combined, "_SECRET") || strings.Contains(out.Stdout, "run2") || strings.Contains(out.Stdout, "harness.db") {
		t.Fatalf("command read the original control directory:\n%s", combined)
	}
	wantFile(t, filepath.Join(control, "harness.db"), "DB_SECRET")
	wantEmptyDir(t, filepath.Join(worktree, ".cloud-harness"))
}

func TestRunCommandDeniesControlThroughSymlinkedWorkspace(t *testing.T) {
	needSeatbelt(t)
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(t.TempDir(), "settings.json"))
	limitHelpers(t, map[string]string{"probe-symlink": "set +e\ncat note.txt\ncat ../../harness.db\n"})
	real := tempDir(t)
	control := filepath.Join(real, ".cloud-harness")
	writeFile(t, filepath.Join(control, "harness.db"), "DB_SECRET")
	writeFile(t, filepath.Join(control, "worktrees", "run1", "note.txt"), "NOTE_OK")
	link := filepath.Join(t.TempDir(), "ws")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	_, out := runLimited(t, context.Background(), filepath.Join(link, ".cloud-harness", "worktrees", "run1"), CommandLimits{}, "probe-symlink")
	combined := out.Stdout + out.Stderr
	if !strings.Contains(out.Stdout, "NOTE_OK") {
		t.Fatalf("command could not read the preserved worktree:\n%s", combined)
	}
	if strings.Contains(combined, "DB_SECRET") {
		t.Fatalf("command read the original control database through a symlinked workspace:\n%s", combined)
	}
}

func TestRunCommandReportsExitAndStartFailures(t *testing.T) {
	needSeatbelt(t)
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(t.TempDir(), "settings.json"))
	limitHelpers(t, map[string]string{"exit-three": "echo out; echo err >&2; exit 3"})
	root := tempDir(t)
	_, out := runLimited(t, context.Background(), root, CommandLimits{}, "exit-three")
	if out.ExitCode != 3 || out.Stdout != "out\n" || out.Stderr != "err\n" {
		t.Fatalf("result = %+v", out)
	}

	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "no-interpreter"), []byte("#!/nonexistent/interpreter\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, out = runLimited(t, context.Background(), root, CommandLimits{}, "no-interpreter")
	if !strings.Contains(out.Error, "no such file or directory") || strings.Contains(out.Error, "sandbox") {
		t.Fatalf("start failure was not reported as one: %+v", out)
	}
}

func TestRunCommandFailsClosedWhenSandboxUnavailable(t *testing.T) {
	root := tempDir(t)
	marker := filepath.Join(root, "started")
	unavailable := func() bool { return false }

	guard := guardCommandWith(exec.Command("touch", marker), root, true, unavailable)
	if err := guard.Run(); err == nil || !strings.Contains(err.Error(), "dangerous command was not started") {
		t.Fatalf("expected an approved command to be refused without a sandbox, got %v", err)
	}
	wantAbsent(t, marker)

	// Commands that need no approval keep working, as on a restricted Linux
	// kernel.
	if err := guardCommandWith(exec.Command("touch", marker), root, false, unavailable).Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("safe command did not run: %v", err)
	}
}

func TestRunCommandFailsClosedWhenMaskCannotBeBuilt(t *testing.T) {
	needSeatbelt(t)
	root := tempDir(t)
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(root, "settings.json"))
	if guard := guardCommand(exec.Command("pwd"), CommandLimits{}, root, false); guard.failErr == nil || guard.Run() == nil {
		t.Fatal("expected run_command to fail closed when the configuration directory contains the workspace")
	}

	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(t.TempDir(), "settings.json"))
	control := filepath.Join(tempDir(t), ".cloud-harness")
	outside := tempDir(t)
	writeFile(t, filepath.Join(outside, "run1", "note.txt"), "note")
	writeFile(t, filepath.Join(control, "harness.db"), "DB_SECRET")
	if err := os.Symlink(outside, filepath.Join(control, "worktrees")); err != nil {
		t.Fatal(err)
	}
	if guard := guardCommand(exec.Command("pwd"), CommandLimits{}, filepath.Join(control, "worktrees", "run1"), false); guard.failErr == nil {
		t.Fatal("expected run_command to fail closed when the worktree resolves outside its control directory")
	}

	// A control path that is a dangling symlink could be filled in by the
	// command, so nothing is started.
	dangling := tempDir(t)
	if err := os.Symlink(filepath.Join(dangling, "absent"), filepath.Join(dangling, ".cloud-harness")); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dangling, "started")
	err := guardCommand(exec.Command("touch", marker), CommandLimits{}, dangling, true).Run()
	if err == nil || !strings.Contains(err.Error(), "dangling symlink") {
		t.Fatalf("expected a dangling control symlink to be refused, got %v", err)
	}
	wantAbsent(t, marker)
}
