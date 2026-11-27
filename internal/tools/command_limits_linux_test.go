//go:build linux

package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"golang.org/x/sys/unix"
)

// softLimits parses the soft limit column of /proc/<pid>/limits.
func softLimits(t *testing.T, text string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		if len(line) < 46 || !strings.HasPrefix(line, "Max ") {
			continue
		}
		out[strings.TrimSpace(line[:26])] = strings.TrimSpace(line[26:46])
	}
	if len(out) == 0 {
		t.Fatalf("no limits in %q", text)
	}
	return out
}

// wantSoft is the soft limit a command gets for value: limits only lower
// what this process already has.
func wantSoft(t *testing.T, resource int, value uint64) string {
	t.Helper()
	var inherited syscall.Rlimit
	if err := syscall.Getrlimit(resource, &inherited); err != nil {
		t.Fatal(err)
	}
	if value == 0 || value > inherited.Cur {
		value = inherited.Cur
	}
	if value == unix.RLIM_INFINITY {
		return "unlimited"
	}
	return strconv.FormatUint(value, 10)
}

func TestRunCommandAppliesLimitsToDescendants(t *testing.T) {
	// The script's shell is the command and cat is its child.
	limitHelpers(t, map[string]string{"show-limits": "cat /proc/self/limits"})
	root := t.TempDir()

	res, out := runLimited(t, context.Background(), root, CommandLimits{}, "show-limits")
	if !res.OK || out.ExitCode != 0 || out.ResourceLimit != nil {
		t.Fatalf("default limits broke a normal command: %+v %+v", res, out)
	}
	got := softLimits(t, out.Stdout)
	for name, want := range map[string]string{
		"Max data size":      wantSoft(t, unix.RLIMIT_DATA, DefaultCommandMemoryBytes),
		"Max address space":  wantSoft(t, unix.RLIMIT_AS, 0),
		"Max cpu time":       wantSoft(t, unix.RLIMIT_CPU, DefaultCommandCPUSeconds),
		"Max file size":      wantSoft(t, unix.RLIMIT_FSIZE, DefaultCommandFileSizeBytes),
		"Max open files":     wantSoft(t, unix.RLIMIT_NOFILE, DefaultCommandOpenFiles),
		"Max processes":      wantSoft(t, unix.RLIMIT_NPROC, DefaultCommandProcesses),
		"Max core file size": "0",
	} {
		if got[name] != want {
			t.Fatalf("default %s = %q, want %q\n%s", name, got[name], want, out.Stdout)
		}
	}

	custom := CommandLimits{
		MemoryBytes: u64(1 << 30), AddressSpaceBytes: u64(2 << 30), CPUSeconds: u64(30),
		FileSizeBytes: u64(1 << 20), OpenFiles: u64(64), Processes: u64(0),
	}
	_, out = runLimited(t, context.Background(), root, custom, "show-limits")
	got = softLimits(t, out.Stdout)
	for name, want := range map[string]string{
		"Max data size":     wantSoft(t, unix.RLIMIT_DATA, 1<<30),
		"Max address space": wantSoft(t, unix.RLIMIT_AS, 2<<30),
		"Max cpu time":      wantSoft(t, unix.RLIMIT_CPU, 30),
		"Max file size":     wantSoft(t, unix.RLIMIT_FSIZE, 1<<20),
		"Max open files":    wantSoft(t, unix.RLIMIT_NOFILE, 64),
		"Max processes":     wantSoft(t, unix.RLIMIT_NPROC, 0),
	} {
		if got[name] != want {
			t.Fatalf("configured %s = %q, want %q\n%s", name, got[name], want, out.Stdout)
		}
	}
}

func TestRunCommandLimitsDisabled(t *testing.T) {
	limitHelpers(t, map[string]string{"show-limits": "cat /proc/self/limits"})
	res, out := runLimited(t, context.Background(), t.TempDir(), CommandLimits{Disabled: true, CPUSeconds: u64(1), OpenFiles: u64(64)}, "show-limits")
	if !res.OK || out.ExitCode != 0 {
		t.Fatalf("command failed with limits disabled: %+v %+v", res, out)
	}
	got := softLimits(t, out.Stdout)
	for name, resource := range map[string]int{
		"Max data size": unix.RLIMIT_DATA,
		"Max cpu time":  unix.RLIMIT_CPU,
		"Max file size": unix.RLIMIT_FSIZE,
		"Max processes": unix.RLIMIT_NPROC,
	} {
		if want := wantSoft(t, resource, 0); got[name] != want {
			t.Fatalf("disabled %s = %q, want the inherited %q", name, got[name], want)
		}
	}
	if got["Max open files"] == "64" {
		t.Fatalf("open files limit was applied while disabled\n%s", out.Stdout)
	}
}

func TestRunCommandKilledAtCPULimit(t *testing.T) {
	limitHelpers(t, map[string]string{"spin": "while :; do :; done"})
	start := time.Now()
	res, out := runLimited(t, context.Background(), t.TempDir(), CommandLimits{CPUSeconds: u64(1)}, "spin")
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("command ran for %v under a 1 second CPU limit", elapsed)
	}
	if res.OK || res.Error == nil || res.Error.Code != domain.CodeResourceLimit {
		t.Fatalf("result = %+v, want a resource_limit error", res)
	}
	if !strings.Contains(res.Error.Message, "CPU time limit of 1 seconds") || !strings.Contains(res.Error.Message, "command_limits.cpu_seconds") {
		t.Fatalf("error does not name the limit: %q", res.Error.Message)
	}
	hit := out.ResourceLimit
	if hit == nil || hit.Resource != "cpu_seconds" || hit.Limit != 1 || !hit.Certain {
		t.Fatalf("resource_limit = %+v, want a certain cpu_seconds hit", hit)
	}
	if out.ExitCode != -1 || out.Error != res.Error.Message {
		t.Fatalf("data = %+v, want exit_code -1 and the limit message", out)
	}
}

func TestRunCommandKilledAtFileSizeLimit(t *testing.T) {
	limitHelpers(t, map[string]string{
		"fill":       "exec dd if=/dev/zero of=direct.bin bs=1024 count=64",
		"fill-child": "dd if=/dev/zero of=child.bin bs=1024 count=64\nexit $?",
	})
	root := t.TempDir()
	limits := CommandLimits{FileSizeBytes: u64(4096)}

	res, out := runLimited(t, context.Background(), root, limits, "fill")
	if res.OK || res.Error == nil || res.Error.Code != domain.CodeResourceLimit {
		t.Fatalf("result = %+v, want a resource_limit error", res)
	}
	if !strings.Contains(res.Error.Message, "file size limit of 4096 bytes") {
		t.Fatalf("error does not name the limit: %q", res.Error.Message)
	}
	if hit := out.ResourceLimit; hit == nil || hit.Resource != "file_size_bytes" || !hit.Certain {
		t.Fatalf("resource_limit = %+v, want a certain file_size_bytes hit", hit)
	}

	// A child of the command hits the limit and the command exits on its own.
	res, out = runLimited(t, context.Background(), root, limits, "fill-child")
	if !res.OK || out.ExitCode != 128+int(syscall.SIGXFSZ) {
		t.Fatalf("result = %+v %+v, want exit status %d", res, out, 128+int(syscall.SIGXFSZ))
	}
	if hit := out.ResourceLimit; hit == nil || hit.Resource != "file_size_bytes" || hit.Certain {
		t.Fatalf("resource_limit = %+v, want an uncertain file_size_bytes hit", hit)
	}

	for _, name := range []string{"direct.bin", "child.bin"} {
		info, err := os.Stat(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > 4096 {
			t.Fatalf("%s grew to %d bytes past the 4096 byte limit", name, info.Size())
		}
	}
}

func TestRunCommandMemoryLimit(t *testing.T) {
	// Ask a standard utility to allocate a buffer beyond RLIMIT_DATA. Shells
	// report expansion exhaustion differently (dash exits 2 before the limit
	// signal), so use dd's stable allocation failure output for classification.
	limitHelpers(t, map[string]string{"grow": "dd if=/dev/zero of=/dev/null bs=67108864 count=1"})
	for name, limits := range map[string]CommandLimits{
		"memory_bytes": {MemoryBytes: u64(32 << 20)},
		// The default memory limit stays on and is the looser of the two.
		"address_space_bytes": {AddressSpaceBytes: u64(64 << 20)},
	} {
		_, out := runLimited(t, context.Background(), t.TempDir(), limits, "grow")
		if out.ExitCode == 0 {
			t.Fatalf("%s: unbounded allocation succeeded: %+v", name, out)
		}
		if hit := out.ResourceLimit; hit == nil || hit.Resource != name || !strings.Contains(hit.Message, "command_limits."+name) {
			t.Fatalf("%s: resource_limit = %+v (data=%+v)", name, hit, out)
		}
	}
}

func TestRunCommandOpenFilesLimit(t *testing.T) {
	// Every level of the recursion keeps one more descriptor open.
	limitHelpers(t, map[string]string{"leak-fds": "f() { f </dev/null; }\nf"})
	res, out := runLimited(t, context.Background(), t.TempDir(), CommandLimits{OpenFiles: u64(32)}, "leak-fds")
	if !res.OK || out.ExitCode == 0 {
		t.Fatalf("descriptor leak was not stopped: %+v %+v", res, out)
	}
	if hit := out.ResourceLimit; hit == nil || hit.Resource != "open_files" || hit.Limit != 32 || hit.Certain {
		t.Fatalf("resource_limit = %+v (data=%+v)", hit, out)
	}
}

func TestRunCommandProcessLimit(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("RLIMIT_NPROC is not enforced for root")
	}
	limitHelpers(t, map[string]string{"spawn": "ls\nls"})
	_, out := runLimited(t, context.Background(), t.TempDir(), CommandLimits{Processes: u64(1)}, "spawn")
	if out.ExitCode == 0 {
		t.Fatalf("command forked past a limit of 1 process: %+v", out)
	}
	if hit := out.ResourceLimit; hit == nil || hit.Resource != "processes" || hit.Limit != 1 {
		t.Fatalf("resource_limit = %+v (data=%+v)", hit, out)
	}
}

func TestRunCommandUnaffectedByDefaultLimits(t *testing.T) {
	limitHelpers(t, map[string]string{
		"fail3": "echo out\necho err >&2\nexit 3",
		"write": "dd if=/dev/zero of=big.bin bs=1024 count=2048 2>/dev/null",
	})
	root := t.TempDir()

	res, out := runLimited(t, context.Background(), root, CommandLimits{}, "fail3")
	if !res.OK || out.ExitCode != 3 || out.Stdout != "out\n" || out.Stderr != "err\n" || out.ResourceLimit != nil {
		t.Fatalf("ordinary failure changed: %+v %+v", res, out)
	}

	res, out = runLimited(t, context.Background(), root, CommandLimits{}, "write")
	if !res.OK || out.ExitCode != 0 || out.ResourceLimit != nil {
		t.Fatalf("2 MiB write failed under default limits: %+v %+v", res, out)
	}

	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module limitprobe\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"ok\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// This isolated module is intentionally outside Git; Go 1.27's automatic
	// VCS stamping otherwise fails before testing resource-limit behavior.
	res, out = runLimited(t, context.Background(), root, CommandLimits{}, "go build -buildvcs=false ./...")
	if !res.OK || out.ExitCode != 0 || out.ResourceLimit != nil {
		t.Fatalf("go build failed under default limits: %+v %+v", res, out)
	}
}

func TestRunCommandStartFailureIsNotALimitError(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "no-interp"), []byte("#!/no/such/interpreter\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	res, out := runLimited(t, context.Background(), t.TempDir(), CommandLimits{}, "no-interp")
	if res.OK || res.Error == nil || res.Error.Code != domain.CodeToolError {
		t.Fatalf("result = %+v, want a tool_error", res)
	}
	if !strings.Contains(res.Error.Message, "no-interp: no such file or directory") || strings.Contains(res.Error.Message, "resource limit") {
		t.Fatalf("start failure was misreported: %q", res.Error.Message)
	}
	if out.ExitCode != -1 || out.ResourceLimit != nil {
		t.Fatalf("data = %+v", out)
	}
}

func TestRunCommandCancelKillsProcessGroup(t *testing.T) {
	limitHelpers(t, map[string]string{"linger": "sleep 300 &\necho $! > child.pid\nwait"})
	for name, limits := range map[string]CommandLimits{"limited": {}, "disabled": {Disabled: true}} {
		root := t.TempDir()
		ctx, cancel := context.WithCancel(context.Background())
		pidFile := filepath.Join(root, "child.pid")
		go func() {
			defer cancel()
			for i := 0; i < 500; i++ {
				if body, err := os.ReadFile(pidFile); err == nil && strings.HasSuffix(string(body), "\n") {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
		start := time.Now()
		res, _ := runLimited(t, ctx, root, limits, "linger")
		cancel()
		if res.OK || time.Since(start) > 30*time.Second {
			t.Fatalf("%s: canceled command result = %+v after %v", name, res, time.Since(start))
		}
		body, err := os.ReadFile(pidFile)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		alive := true
		for i := 0; i < 300 && alive; i++ {
			alive = syscall.Kill(pid, 0) == nil
			if alive {
				time.Sleep(10 * time.Millisecond)
			}
		}
		if alive {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("%s: background child %d survived the canceled command", name, pid)
		}
	}
}

func TestRlimitEncoding(t *testing.T) {
	plan, err := planRlimits(CommandLimits{CPUSeconds: u64(30), AddressSpaceBytes: u64(1 << 40)})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeRlimits(encodeRlimits(plan))
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != len(plan) {
		t.Fatalf("decoded %d limits, want %d", len(decoded), len(plan))
	}
	for i, lim := range plan {
		got := decoded[i]
		if got.name != lim.name || got.resource != lim.resource || got.cur != lim.cur || got.max != lim.max {
			t.Fatalf("limit %d decoded as %+v, want %+v", i, got, lim)
		}
		if lim.name == "cpu" && (lim.cur != 30 || lim.max != 30+cpuKillGrace) {
			var inherited syscall.Rlimit
			_ = syscall.Getrlimit(unix.RLIMIT_CPU, &inherited)
			if inherited.Cur >= 30+cpuKillGrace {
				t.Fatalf("cpu limit = %d:%d, want 30:%d", lim.cur, lim.max, 30+cpuKillGrace)
			}
		}
	}
	if plan, err := planRlimits(CommandLimits{Disabled: true}); err != nil || len(plan) != 0 {
		t.Fatalf("disabled plan = %v %v, want empty", plan, err)
	}
	for _, bad := range []string{"cpu", "cpu=1", "cpu=1:x", "bogus=1:1", "cpu=-1:1"} {
		if _, err := decodeRlimits(bad); err == nil {
			t.Fatalf("decodeRlimits(%q) succeeded", bad)
		}
	}
}

func TestRunCommandMasksControlDirectory(t *testing.T) {
	if !commandSandboxAvailable() {
		t.Skip("user namespaces unavailable")
	}
	limitHelpers(t, map[string]string{"probe-control": "set +e\nls .cloud-harness\ncat .cloud-harness/secret\nprintf x > .cloud-harness/new\n"})
	root := t.TempDir()
	control := filepath.Join(root, ".cloud-harness")
	if err := os.MkdirAll(control, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(control, "secret"), []byte("CONTROL_SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, out := runLimited(t, context.Background(), root, CommandLimits{}, "probe-control")
	combined := out.Stdout + out.Stderr
	if strings.Contains(combined, "CONTROL_SECRET") {
		t.Fatalf("command read control file:\n%s", combined)
	}
	if strings.Contains(out.Stdout, "secret") {
		t.Fatalf("control directory was not masked:\n%s", out.Stdout)
	}
	if body, err := os.ReadFile(filepath.Join(control, "secret")); err != nil || string(body) != "CONTROL_SECRET" {
		t.Fatalf("control file changed: %q %v", body, err)
	}
	if _, err := os.Stat(filepath.Join(control, "new")); !os.IsNotExist(err) {
		t.Fatalf("command wrote through the control mask: %v", err)
	}
}

func TestRunCommandMasksTrustStore(t *testing.T) {
	if !commandSandboxAvailable() {
		t.Skip("user namespaces unavailable")
	}
	configDir := t.TempDir()
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(configDir, "settings.json"))
	trustPath := filepath.Join(configDir, "trust.json")
	if err := os.WriteFile(trustPath, []byte("TRUST_SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	limitHelpers(t, map[string]string{"probe-trust": "cat " + trustPath + "\nprintf x > " + trustPath + "\n"})
	root := t.TempDir()

	_, out := runLimited(t, context.Background(), root, CommandLimits{}, "probe-trust")
	combined := out.Stdout + out.Stderr
	if strings.Contains(combined, "TRUST_SECRET") {
		t.Fatalf("command read trust store:\n%s", combined)
	}
	if body, err := os.ReadFile(trustPath); err != nil || string(body) != "TRUST_SECRET" {
		t.Fatalf("trust store changed: %q %v", body, err)
	}
}

func TestRunCommandMasksMissingControlDirectory(t *testing.T) {
	if !commandSandboxAvailable() {
		t.Skip("user namespaces unavailable")
	}
	limitHelpers(t, map[string]string{"make-control": "printf x > .cloud-harness/pwn\n"})
	root := t.TempDir()

	if _, out := runLimited(t, context.Background(), root, CommandLimits{}, "make-control"); strings.Contains(out.Stdout+out.Stderr, "CONTROL_SECRET") {
		t.Fatalf("command saw control contents:\n%s", out.Stdout+out.Stderr)
	}
	if _, err := os.Stat(filepath.Join(root, ".cloud-harness")); !os.IsNotExist(err) {
		t.Fatalf("temporary control mask was not cleaned up: %v", err)
	}
}

func TestRunCommandMasksOriginalControlInWorktree(t *testing.T) {
	if !commandSandboxAvailable() {
		t.Skip("user namespaces unavailable")
	}
	limitHelpers(t, map[string]string{"probe-worktree": "set +e\npwd\ncat ../../harness.db\nprintf x > ../../harness.db\n"})
	orig := t.TempDir()
	control := filepath.Join(orig, ".cloud-harness")
	worktree := filepath.Join(control, "worktrees", "run1")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(control, "harness.db"), []byte("DB_SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "note.txt"), []byte("note"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, out := runLimited(t, context.Background(), worktree, CommandLimits{}, "probe-worktree")
	combined := out.Stdout + out.Stderr
	if got := strings.TrimSpace(strings.SplitN(out.Stdout, "\n", 2)[0]); got != worktree {
		t.Fatalf("command cwd = %q, want worktree %q", got, worktree)
	}
	if strings.Contains(combined, "DB_SECRET") {
		t.Fatalf("command read the original control database:\n%s", combined)
	}
	if body, err := os.ReadFile(filepath.Join(control, "harness.db")); err != nil || string(body) != "DB_SECRET" {
		t.Fatalf("original control database changed: %q %v", body, err)
	}
	if _, err := os.Stat(filepath.Join(worktree, ".cloud-harness")); !os.IsNotExist(err) {
		t.Fatalf("worktree control mask leaked: %v", err)
	}
}

func TestRunCommandMasksOriginalControlInSymlinkedWorktree(t *testing.T) {
	if !commandSandboxAvailable() {
		t.Skip("user namespaces unavailable")
	}
	limitHelpers(t, map[string]string{"probe-symlink-worktree": "set +e\ncat note.txt\ncat ../../harness.db\n"})
	real := t.TempDir()
	control := filepath.Join(real, ".cloud-harness")
	worktree := filepath.Join(control, "worktrees", "run1")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(control, "harness.db"), []byte("DB_SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "note.txt"), []byte("note"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "ws")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(link, ".cloud-harness", "worktrees", "run1")

	_, out := runLimited(t, context.Background(), root, CommandLimits{}, "probe-symlink-worktree")
	combined := out.Stdout + out.Stderr
	if !strings.Contains(combined, "note") {
		t.Fatalf("command could not read the preserved worktree:\n%s", combined)
	}
	if strings.Contains(combined, "DB_SECRET") {
		t.Fatalf("command read the original control database through a symlinked workspace:\n%s", combined)
	}
	if body, err := os.ReadFile(filepath.Join(control, "harness.db")); err != nil || string(body) != "DB_SECRET" {
		t.Fatalf("original control database changed: %q %v", body, err)
	}
}

func TestRunCommandFailsClosedWhenWorktreesDirIsRelocated(t *testing.T) {
	if !commandSandboxAvailable() {
		t.Skip("user namespaces unavailable")
	}
	real := t.TempDir()
	control := filepath.Join(real, ".cloud-harness")
	if err := os.MkdirAll(control, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "run1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(control, "harness.db"), []byte("DB_SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(control, "worktrees")); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(control, "worktrees", "run1")

	guard := guardCommand(exec.CommandContext(context.Background(), "pwd"), CommandLimits{}, root, false)
	if guard.failErr == nil {
		t.Fatal("expected run_command to fail closed when the worktree resolves outside its control directory")
	}
}

func TestCommandSandboxFailsClosedWhenConfigAliasContainsWorkspace(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(base, "shared")
	if err := os.Symlink(real, shared); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(shared, "ws")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(shared, "settings.json"))

	if _, err := commandSandboxSpecFor(workspace); err == nil {
		t.Fatal("expected run_command to fail closed when a symlinked configuration directory contains the workspace")
	}
}

func TestRunCommandFailsClosedWhenConfigContainsWorkspace(t *testing.T) {
	if !commandSandboxAvailable() {
		t.Skip("user namespaces unavailable")
	}
	root := t.TempDir()
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(root, "settings.json"))

	guard := guardCommand(exec.CommandContext(context.Background(), "pwd"), CommandLimits{}, root, false)
	if guard.failErr == nil {
		t.Fatal("expected run_command to fail closed when the configuration directory contains the workspace")
	}
}
