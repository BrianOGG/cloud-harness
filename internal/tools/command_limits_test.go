package tools

import (
	"strings"
	"testing"
)

func u64(v uint64) *uint64 { return &v }

func TestCommandLimitsEffective(t *testing.T) {
	got := CommandLimits{}.Effective()
	want := map[string]uint64{
		"memory_bytes":        DefaultCommandMemoryBytes,
		"address_space_bytes": 0,
		"cpu_seconds":         DefaultCommandCPUSeconds,
		"file_size_bytes":     DefaultCommandFileSizeBytes,
		"open_files":          DefaultCommandOpenFiles,
		"processes":           DefaultCommandProcesses,
	}
	if len(got) != len(want) {
		t.Fatalf("default limits = %v, want %v", got, want)
	}
	for name, value := range want {
		if got[name] != value {
			t.Fatalf("default %s = %d, want %d", name, got[name], value)
		}
	}

	got = CommandLimits{MemoryBytes: u64(1 << 30), CPUSeconds: u64(0), AddressSpaceBytes: u64(2 << 30)}.Effective()
	if got["memory_bytes"] != 1<<30 || got["cpu_seconds"] != 0 || got["address_space_bytes"] != 2<<30 {
		t.Fatalf("overrides were not applied: %v", got)
	}
	if got["open_files"] != DefaultCommandOpenFiles {
		t.Fatalf("an unset field lost its default: %v", got)
	}

	if got := (CommandLimits{Disabled: true, CPUSeconds: u64(5)}).Effective(); len(got) != 0 {
		t.Fatalf("disabled limits = %v, want none", got)
	}
}

func TestForkWorkspaceKeepsCommandLimits(t *testing.T) {
	b := NewBroker(&memCharge{}, memGrants{})
	if err := RegisterLocal(b, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	b.SetCommandLimits(CommandLimits{CPUSeconds: u64(7)})
	fork, err := b.ForkWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := fork.CommandLimits().Effective()["cpu_seconds"]; got != 7 {
		t.Fatalf("forked broker cpu_seconds = %d, want 7", got)
	}
}

func TestClassifyLimitOutput(t *testing.T) {
	limits := CommandLimits{}.Effective()
	cases := []struct {
		name     string
		exit     int
		stderr   string
		limits   map[string]uint64
		resource string
	}{
		{"success is never a hit", 0, "fork: Resource temporarily unavailable", limits, ""},
		{"ordinary failure", 1, "FAIL: TestSomething\nexit status 1", limits, ""},
		{"shell reports SIGXCPU", 152, "", limits, "cpu_seconds"},
		{"shell reports SIGXFSZ", 153, "", limits, "file_size_bytes"},
		{"write failed", 1, "dd: error writing 'big': File too large", limits, "file_size_bytes"},
		{"descriptor exhaustion", 2, "open /tmp/x: too many open files", limits, "open_files"},
		{"fork failed", 2, "sh: 1: Cannot fork", limits, "processes"},
		{"go thread creation", 2, "runtime: failed to create new OS thread", limits, "processes"},
		{"allocation failed", 1, "fatal error: runtime: out of memory", limits, "memory_bytes"},
		{"sanitizer shadow memory", 1, "==1==ERROR: AddressSanitizer failed to allocate 0xdfff0001000 (15392894357504) bytes at address 2008fff7000 (errno: 12)", limits, "memory_bytes"},
		{"thread sanitizer shadow memory", 66, "FATAL: ThreadSanitizer can not mmap the shadow memory", limits, "memory_bytes"},
		{"java heap", 1, "java.lang.OutOfMemoryError: Java heap space", limits, "memory_bytes"},
		{"address space when memory is unlimited", 1, "std::bad_alloc", map[string]uint64{"address_space_bytes": 1 << 30}, "address_space_bytes"},
		{"limit not applied", 153, "File too large", map[string]uint64{"cpu_seconds": 5}, ""},
		{"limits disabled", 2, "too many open files", map[string]uint64{}, ""},
	}
	for _, tc := range cases {
		hit := classifyLimitOutput(tc.exit, tc.stderr, tc.limits)
		if tc.resource == "" {
			if hit != nil {
				t.Fatalf("%s: unexpected hit %+v", tc.name, hit)
			}
			continue
		}
		if hit == nil || hit.Resource != tc.resource {
			t.Fatalf("%s: hit = %+v, want %s", tc.name, hit, tc.resource)
		}
		if hit.Certain || hit.Limit != tc.limits[tc.resource] {
			t.Fatalf("%s: hit = %+v, want an uncertain hit at %d", tc.name, hit, tc.limits[tc.resource])
		}
		if !strings.HasPrefix(hit.Message, "possible resource limit: ") || !strings.Contains(hit.Message, "command_limits."+tc.resource) {
			t.Fatalf("%s: message does not name the limit: %q", tc.name, hit.Message)
		}
	}

	// Only the tail of a long error output is searched.
	old := "too many open files\n" + strings.Repeat("x", limitOutputTail)
	if hit := classifyLimitOutput(1, old, limits); hit != nil {
		t.Fatalf("matched output outside the tail: %+v", hit)
	}
}

func TestClassifyLimitCrash(t *testing.T) {
	limits := map[string]uint64{"memory_bytes": 64 << 20}
	if hit := classifyLimitCrash("segmentation fault", 4<<20, limits); hit != nil {
		t.Fatalf("a small process crash was blamed on the memory limit: %+v", hit)
	}
	hit := classifyLimitCrash("segmentation fault", 48<<20, limits)
	if hit == nil || hit.Resource != "memory_bytes" || hit.Certain || hit.Limit != 64<<20 {
		t.Fatalf("hit = %+v, want an uncertain memory_bytes hit", hit)
	}
	hit = classifyLimitCrash("aborted", 48<<20, map[string]uint64{"address_space_bytes": 64 << 20})
	if hit == nil || hit.Resource != "address_space_bytes" {
		t.Fatalf("hit = %+v, want address_space_bytes", hit)
	}
	if hit := classifyLimitCrash("aborted", 1<<40, map[string]uint64{"cpu_seconds": 5}); hit != nil {
		t.Fatalf("crash blamed on a memory limit that is not set: %+v", hit)
	}
}

func TestKilledAtLimitMessage(t *testing.T) {
	hit := killedAtLimit("cpu_seconds", 600)
	if !hit.Certain || hit.Resource != "cpu_seconds" || hit.Limit != 600 {
		t.Fatalf("hit = %+v", hit)
	}
	want := "resource limit exceeded: the command was killed at the CPU time limit of 600 seconds per process (command_limits.cpu_seconds)"
	if hit.Message != want {
		t.Fatalf("message = %q, want %q", hit.Message, want)
	}
}
