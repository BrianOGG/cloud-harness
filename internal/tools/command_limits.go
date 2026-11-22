package tools

import (
	"fmt"
	"runtime"
	"strings"
	"time"
)

// Default run_command resource limits. Each is a per-process kernel rlimit that
// child processes inherit; none of them is a budget for the whole process tree.
const (
	// DefaultCommandMemoryBytes caps private writable memory (RLIMIT_DATA).
	// RLIMIT_AS is not the default because Chromium reserves more address
	// space than that and fails under it. C and C++ binaries built with
	// AddressSanitizer or ThreadSanitizer map writable shadow memory far
	// above either limit and need memory_bytes set to 0.
	DefaultCommandMemoryBytes   uint64 = 8 << 30
	DefaultCommandFileSizeBytes uint64 = 4 << 30
	DefaultCommandOpenFiles     uint64 = 16384
	// DefaultCommandProcesses counts every process and thread of the user, not
	// only this command's.
	DefaultCommandProcesses uint64 = 16384
	minCommandCPUSeconds    uint64 = 600
)

// DefaultCommandCPUSeconds is what one process can use by keeping every CPU
// busy for the whole wall-clock timeout, so it never ends a command before
// the timeout does. It stops processes that outlive their command.
var DefaultCommandCPUSeconds = max(minCommandCPUSeconds, uint64(defaultCommandTimeout/time.Second)*uint64(runtime.NumCPU()))

// CommandLimits configures the resource limits of run_command processes.
// A nil field uses the default and an explicit 0 leaves that resource
// unlimited. Limits are enforced on Linux only.
type CommandLimits struct {
	Disabled          bool    `json:"disabled,omitempty"`
	MemoryBytes       *uint64 `json:"memory_bytes,omitempty"`
	AddressSpaceBytes *uint64 `json:"address_space_bytes,omitempty"`
	CPUSeconds        *uint64 `json:"cpu_seconds,omitempty"`
	FileSizeBytes     *uint64 `json:"file_size_bytes,omitempty"`
	OpenFiles         *uint64 `json:"open_files,omitempty"`
	Processes         *uint64 `json:"processes,omitempty"`
}

// Config field names, used to tell the model and the operator which setting
// a failure points at.
const (
	limitMemory       = "memory_bytes"
	limitAddressSpace = "address_space_bytes"
	limitCPU          = "cpu_seconds"
	limitFileSize     = "file_size_bytes"
	limitOpenFiles    = "open_files"
	limitProcesses    = "processes"
)

// Effective returns the configured value of every limit with defaults
// applied, keyed by config field name. Zero means unlimited. It is empty when
// limits are disabled.
func (l CommandLimits) Effective() map[string]uint64 {
	if l.Disabled {
		return map[string]uint64{}
	}
	pick := func(v *uint64, def uint64) uint64 {
		if v == nil {
			return def
		}
		return *v
	}
	return map[string]uint64{
		limitMemory:       pick(l.MemoryBytes, DefaultCommandMemoryBytes),
		limitAddressSpace: pick(l.AddressSpaceBytes, 0),
		limitCPU:          pick(l.CPUSeconds, DefaultCommandCPUSeconds),
		limitFileSize:     pick(l.FileSizeBytes, DefaultCommandFileSizeBytes),
		limitOpenFiles:    pick(l.OpenFiles, DefaultCommandOpenFiles),
		limitProcesses:    pick(l.Processes, DefaultCommandProcesses),
	}
}

// SetCommandLimits sets the resource limits applied to run_command subprocesses.
func (b *Broker) SetCommandLimits(limits CommandLimits) {
	b.mu.Lock()
	b.commandLimits = limits
	b.mu.Unlock()
}

// CommandLimits returns the resource limits applied to run_command subprocesses.
func (b *Broker) CommandLimits() CommandLimits {
	if b == nil {
		return CommandLimits{}
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.commandLimits
}

// limitHit explains a run_command failure caused by a resource limit. Certain
// is true when the kernel signal that killed the command identifies the limit;
// otherwise the exit status, resource usage, or error output only suggests it.
type limitHit struct {
	Resource string `json:"resource"`
	Limit    uint64 `json:"limit"`
	Certain  bool   `json:"certain"`
	Message  string `json:"message"`
}

func describeLimit(resource string, limit uint64) string {
	switch resource {
	case limitCPU:
		return fmt.Sprintf("CPU time limit of %d seconds per process", limit)
	case limitFileSize:
		return fmt.Sprintf("file size limit of %d bytes per file", limit)
	case limitOpenFiles:
		return fmt.Sprintf("open files limit of %d per process", limit)
	case limitProcesses:
		return fmt.Sprintf("limit of %d processes and threads for this user", limit)
	case limitAddressSpace:
		return fmt.Sprintf("address space limit of %d bytes per process", limit)
	default:
		return fmt.Sprintf("memory limit of %d bytes per process", limit)
	}
}

// killedAtLimit reports a command the kernel killed for exceeding a limit.
func killedAtLimit(resource string, limit uint64) *limitHit {
	msg := "resource limit exceeded: the command was killed at the " + describeLimit(resource, limit)
	return &limitHit{Resource: resource, Limit: limit, Certain: true, Message: msg + limitConfigRef(resource)}
}

// suspectedLimit reports a failure that evidence attributes to a limit.
func suspectedLimit(resource string, limit uint64, evidence string) *limitHit {
	msg := "possible resource limit: " + evidence + ", which points to the " + describeLimit(resource, limit)
	return &limitHit{Resource: resource, Limit: limit, Message: msg + limitConfigRef(resource)}
}

func limitConfigRef(resource string) string {
	return " (command_limits." + resource + ")"
}

const limitOutputTail = 64 << 10

// Exit statuses a shell reports when its child died from a signal.
const (
	shellExitABRT = 128 + 6
	shellExitBUS  = 128 + 7
	shellExitSEGV = 128 + 11
	shellExitXCPU = 128 + 24
	shellExitXFSZ = 128 + 25
)

var limitOutputHints = []struct {
	resource string
	phrases  []string
}{
	{limitFileSize, []string{"file too large", "file size limit exceeded", "efbig"}},
	{limitCPU, []string{"cpu time limit exceeded"}},
	{limitOpenFiles, []string{"too many open files", "emfile"}},
	{limitProcesses, []string{"resource temporarily unavailable", "cannot fork", "failed to create new os thread", "can't start new thread"}},
	{limitMemory, []string{"cannot allocate", "out of memory", "outofmemory", "memory exhausted", "bad_alloc", "memoryerror", "enomem", "failed to allocate", "shadow memory"}},
}

// classifyLimitOutput matches a failed command's exit status and error output
// against the limits that were applied. limits holds the applied values by
// config field name; a missing or zero entry was not limited.
func classifyLimitOutput(exitCode int, stderr string, limits map[string]uint64) *limitHit {
	if exitCode == 0 {
		return nil
	}
	if exitCode == shellExitXCPU && limits[limitCPU] > 0 {
		return suspectedLimit(limitCPU, limits[limitCPU], "the command exited with the status of a process killed by SIGXCPU")
	}
	if exitCode == shellExitXFSZ && limits[limitFileSize] > 0 {
		return suspectedLimit(limitFileSize, limits[limitFileSize], "the command exited with the status of a process killed by SIGXFSZ")
	}
	if len(stderr) > limitOutputTail {
		stderr = stderr[len(stderr)-limitOutputTail:]
	}
	text := strings.ToLower(stderr)
	for _, hint := range limitOutputHints {
		resource := hint.resource
		if resource == limitMemory {
			resource = tighterMemoryLimit(limits)
		}
		if limits[resource] == 0 {
			continue
		}
		for _, phrase := range hint.phrases {
			if strings.Contains(text, phrase) {
				return suspectedLimit(resource, limits[resource], fmt.Sprintf("the command failed and its error output contains %q", phrase))
			}
		}
	}
	return nil
}

// tighterMemoryLimit picks the limit an allocation failure is attributed to:
// the lower of the memory and address space limits that were applied. Address
// space is never smaller than the writable memory inside it, so the lower
// value is the one reached first.
func tighterMemoryLimit(limits map[string]uint64) string {
	memory, addressSpace := limits[limitMemory], limits[limitAddressSpace]
	if addressSpace > 0 && (memory == 0 || addressSpace < memory) {
		return limitAddressSpace
	}
	return limitMemory
}

// classifyLimitCrash attributes a crash to the memory limit when the command
// had grown to at least half of it. crash names the signal and maxRSS is the
// peak resident memory in bytes.
func classifyLimitCrash(crash string, maxRSS uint64, limits map[string]uint64) *limitHit {
	resource := tighterMemoryLimit(limits)
	limit := limits[resource]
	if limit == 0 || maxRSS < limit/2 {
		return nil
	}
	return suspectedLimit(resource, limit, fmt.Sprintf("the command died from %s after using %d bytes of memory", crash, maxRSS))
}
