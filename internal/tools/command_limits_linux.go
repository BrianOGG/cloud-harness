//go:build linux

package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/security"
	"golang.org/x/sys/unix"
)

const (
	// limitExecArg0 is argv[0] of the trampoline: this binary re-executed to
	// set rlimits on itself and then exec the real command. Setting them in
	// the child, before the command exists, leaves no window in which the
	// command runs unlimited.
	limitExecArg0    = "cloud-harness-exec-limits"
	sandboxProbeArg0 = "cloud-harness-sandbox-probe"
	limitExecSelf    = "/proc/self/exe"
	// limitStatusFD is the close-on-exec pipe the trampoline reports failures
	// on. The parent reads EOF without data once the command has started.
	limitStatusFD = 3
	// cpuKillGrace is the gap between SIGXCPU and the kernel's SIGKILL.
	cpuKillGrace = 5
)

type rlimit struct {
	name     string
	config   string
	resource int
	cur, max uint64
}

// Memory limits come last so the trampoline itself runs unlimited for as long
// as possible.
var rlimitOrder = []struct {
	name     string
	config   string
	resource int
}{
	{"fsize", limitFileSize, unix.RLIMIT_FSIZE},
	{"nofile", limitOpenFiles, unix.RLIMIT_NOFILE},
	{"nproc", limitProcesses, unix.RLIMIT_NPROC},
	{"cpu", limitCPU, unix.RLIMIT_CPU},
	{"as", limitAddressSpace, unix.RLIMIT_AS},
	{"data", limitMemory, unix.RLIMIT_DATA},
}

func init() {
	if len(os.Args) > 0 && os.Args[0] == limitExecArg0 {
		execLimited(os.Args[1:])
	}
	if len(os.Args) > 0 && os.Args[0] == sandboxProbeArg0 {
		os.Exit(probeCommandSandbox())
	}
}

// execLimited is the trampoline body: args are the encoded limits, the
// encoded command sandbox, the target path, and the target argv. It never
// returns.
func execLimited(args []string) {
	fail := func(kind, detail string) {
		_, _ = syscall.Write(limitStatusFD, []byte(kind+"\n"+detail))
		os.Exit(126)
	}
	syscall.CloseOnExec(limitStatusFD)
	if len(args) < 4 {
		fail("sandbox", "malformed trampoline arguments")
	}
	limits, err := decodeRlimits(args[0])
	if err != nil {
		fail("limits", err.Error())
	}
	var sandbox commandSandboxSpec
	if err := json.Unmarshal([]byte(args[1]), &sandbox); err != nil {
		fail("sandbox", "decode sandbox: "+err.Error())
	}
	if sandbox.Enabled {
		if err := applyCommandSandbox(sandbox); err != nil {
			fail("sandbox", err.Error())
		}
	}
	env := os.Environ()
	for _, lim := range limits {
		if err := syscall.Setrlimit(lim.resource, &syscall.Rlimit{Cur: lim.cur, Max: lim.max}); err != nil {
			fail("limits", "setrlimit "+lim.name+": "+err.Error())
		}
	}
	if sandbox.Enabled {
		if err := dropCommandPrivileges(); err != nil {
			fail("sandbox", err.Error())
		}
	}
	err = syscall.Exec(args[2], args[3:], env)
	var errno syscall.Errno
	if errors.As(err, &errno) {
		fail("exec", strconv.Itoa(int(errno)))
	}
	fail("limits", "exec: "+err.Error())
}

func encodeRlimits(limits []rlimit) string {
	parts := make([]string, 0, len(limits))
	for _, lim := range limits {
		parts = append(parts, fmt.Sprintf("%s=%d:%d", lim.name, lim.cur, lim.max))
	}
	return strings.Join(parts, ",")
}

func decodeRlimits(spec string) ([]rlimit, error) {
	if spec == "" {
		return nil, nil
	}
	var out []rlimit
	for _, part := range strings.Split(spec, ",") {
		name, value, ok := strings.Cut(part, "=")
		curText, maxText, ok2 := strings.Cut(value, ":")
		cur, err1 := strconv.ParseUint(curText, 10, 64)
		max, err2 := strconv.ParseUint(maxText, 10, 64)
		if !ok || !ok2 || err1 != nil || err2 != nil {
			return nil, fmt.Errorf("malformed limit %q", part)
		}
		resource := -1
		if name == "core" {
			resource = unix.RLIMIT_CORE
		}
		for _, known := range rlimitOrder {
			if known.name == name {
				resource = known.resource
			}
		}
		if resource < 0 {
			return nil, fmt.Errorf("unknown limit %q", name)
		}
		out = append(out, rlimit{name: name, resource: resource, cur: cur, max: max})
	}
	return out, nil
}

// planRlimits turns the configured limits into the values to set. They only
// ever lower the limits this process already runs under.
func planRlimits(limits CommandLimits) ([]rlimit, error) {
	want := limits.Effective()
	if len(want) == 0 {
		return nil, nil
	}
	plan := []rlimit{{name: "core", resource: unix.RLIMIT_CORE}}
	for _, known := range rlimitOrder {
		value := want[known.config]
		if value == 0 {
			continue
		}
		var inherited syscall.Rlimit
		if err := syscall.Getrlimit(known.resource, &inherited); err != nil {
			return nil, fmt.Errorf("getrlimit %s: %w", known.name, err)
		}
		lim := rlimit{name: known.name, config: known.config, resource: known.resource}
		lim.cur, lim.max = min(value, inherited.Cur), min(value, inherited.Max)
		if known.resource == unix.RLIMIT_CPU && value <= math.MaxUint64-cpuKillGrace {
			lim.max = min(value+cpuKillGrace, inherited.Max)
		}
		plan = append(plan, lim)
	}
	return plan, nil
}

type commandSandboxPath struct {
	Path   string `json:"path"`
	Create bool   `json:"create,omitempty"`
}

type commandSandboxSpec struct {
	Enabled bool                 `json:"enabled"`
	Paths   []commandSandboxPath `json:"paths,omitempty"`
}

// commandControlPaths returns the control directories for this workspace and
// the harness configuration directory. The command sees them as empty tmpfs
// mounts, so reads and writes cannot reach hooks, policy, snapshots, the run
// database, or the trust store.
func commandControlPaths(root string) []commandSandboxPath {
	path := filepath.Join(filepath.Clean(root), security.ControlDir)
	paths := []commandSandboxPath{controlSandboxPath(path)}
	if config, ok := commandConfigPath(root); ok {
		paths = append(paths, controlSandboxPath(config))
	}
	return paths
}

func controlSandboxPath(path string) commandSandboxPath {
	_, err := os.Lstat(path)
	return commandSandboxPath{Path: path, Create: os.IsNotExist(err)}
}

func commandConfigPath(root string) (string, bool) {
	dir, err := security.ConfigDirectory()
	if err != nil {
		return "", false
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", false
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(absDir, absRoot)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		// Masking the configuration directory would also hide the workspace.
		return "", false
	}
	return absDir, true
}

func applyCommandSandbox(spec commandSandboxSpec) error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mounts private: %w", err)
	}
	seen := map[string]bool{}
	for _, entry := range spec.Paths {
		path := filepath.Clean(entry.Path)
		if entry.Create {
			if err := os.MkdirAll(path, 0o755); err != nil {
				// A read-only or permission-denied parent means the command
				// cannot create the control directory either, so there is
				// nothing to mask.
				if errors.Is(err, syscall.EROFS) || os.IsPermission(err) {
					continue
				}
				return fmt.Errorf("create control mask %s: %w", path, err)
			}
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			resolved = path
		}
		resolved = filepath.Clean(resolved)
		if resolved == string(os.PathSeparator) {
			return fmt.Errorf("refusing to mask filesystem root")
		}
		if seen[resolved] {
			continue
		}
		seen[resolved] = true
		info, err := os.Stat(resolved)
		if err != nil || !info.IsDir() {
			continue
		}
		if err := unix.Mount("tmpfs", resolved, "tmpfs", 0, "mode=0755"); err != nil {
			return fmt.Errorf("mask %s: %w", resolved, err)
		}
	}
	return nil
}

func commandLastCapability() int {
	raw, err := os.ReadFile("/proc/sys/kernel/cap_last_cap")
	if err != nil {
		return int(unix.CAP_LAST_CAP)
	}
	last, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || last < 0 {
		return int(unix.CAP_LAST_CAP)
	}
	return last
}

func dropCommandPrivileges() error {
	last := commandLastCapability()
	for cap := 0; cap <= last; cap++ {
		if cap == unix.CAP_SETPCAP {
			continue
		}
		if err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(cap), 0, 0, 0); err != nil {
			return fmt.Errorf("drop capability %d: %w", cap, err)
		}
	}
	if last >= int(unix.CAP_SETPCAP) {
		if err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(unix.CAP_SETPCAP), 0, 0, 0); err != nil {
			return fmt.Errorf("drop capability %d: %w", unix.CAP_SETPCAP, err)
		}
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no_new_privs: %w", err)
	}
	return nil
}

var commandSandboxProbe = struct {
	sync.Once
	ok bool
}{}

func commandUsernsAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getuid(), Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getgid(), Size: 1},
		},
		GidMappingsEnableSetgroups: false,
	}
}

func commandSandboxAvailable() bool {
	commandSandboxProbe.Do(func() {
		cmd := exec.Command(limitExecSelf)
		cmd.Args = []string{sandboxProbeArg0}
		cmd.SysProcAttr = commandUsernsAttr()
		commandSandboxProbe.ok = cmd.Run() == nil
	})
	return commandSandboxProbe.ok
}

// warmCommandSandbox resolves the sandbox probe during tool registration so a
// later approved command does not pay for it inside a request deadline.
func warmCommandSandbox() {
	_ = commandSandboxAvailable()
}

// probeCommandSandbox runs the same namespace, mount, and privilege-drop setup
// as a sandboxed command without exec-ing an external binary, so availability
// does not depend on /bin/true being installed.
func probeCommandSandbox() int {
	if err := applyCommandSandbox(commandSandboxSpec{Enabled: true}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := dropCommandPrivileges(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// commandGuard runs one run_command process in its own process group, under
// resource limits when they are enabled. When user namespaces are available it
// also masks harness control files; restricted kernels keep the rlimits-only
// behavior instead of failing every command.
type commandGuard struct {
	cmd     *exec.Cmd
	target  string
	applied map[string]uint64
	planErr error
	failErr error
	statusR *os.File
	statusW *os.File
	sandbox commandSandboxSpec
	created []string
}

func guardCommand(cmd *exec.Cmd, limits CommandLimits, workspace string, requireSandbox bool) *commandGuard {
	g := &commandGuard{cmd: cmd, target: cmd.Path, applied: map[string]uint64{}}
	applySysProc(cmd)
	cmd.Cancel = func() error { return killGroup(cmd) }
	if cmd.Err != nil {
		return g
	}
	if requireSandbox && !commandSandboxAvailable() {
		g.failErr = errors.New("command sandbox unavailable on this kernel; dangerous command was not started")
		return g
	}
	if commandSandboxAvailable() {
		g.sandbox.Enabled = true
		control := commandControlPaths(workspace)
		g.sandbox.Paths = control
		for _, entry := range control {
			if entry.Create {
				g.created = append(g.created, entry.Path)
			}
		}
		attr := cmd.SysProcAttr
		if attr == nil {
			attr = &syscall.SysProcAttr{}
			cmd.SysProcAttr = attr
		}
		attr.Cloneflags = syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS
		attr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}
		attr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}
		attr.GidMappingsEnableSetgroups = false
	}
	plan, err := planRlimits(limits)
	if err != nil {
		g.planErr = err
		return g
	}
	sandboxJSON, err := json.Marshal(g.sandbox)
	if err != nil {
		g.planErr = err
		return g
	}
	if g.statusR, g.statusW, err = os.Pipe(); err != nil {
		g.planErr = err
		return g
	}
	for _, lim := range plan {
		if lim.config != "" {
			g.applied[lim.config] = lim.cur
		}
	}
	cmd.Args = append([]string{limitExecArg0, encodeRlimits(plan), string(sandboxJSON), cmd.Path}, cmd.Args...)
	cmd.Path = limitExecSelf
	cmd.ExtraFiles = []*os.File{g.statusW}
	return g
}

// Run starts the command and waits for it. A command whose limits or sandbox
// could not be established is not started.
func (g *commandGuard) Run() error {
	defer g.cleanupSandbox()
	if g.failErr != nil {
		return g.failErr
	}
	if g.planErr != nil {
		return limitsNotApplied(g.planErr.Error())
	}
	if g.statusR == nil {
		return g.cmd.Run()
	}
	defer g.statusR.Close()
	err := g.cmd.Start()
	_ = g.statusW.Close()
	if err != nil {
		return sandboxNotApplied(err.Error())
	}
	status, _ := io.ReadAll(io.LimitReader(g.statusR, 4096))
	err = g.cmd.Wait()
	if len(status) == 0 {
		return err
	}
	kind, detail, _ := strings.Cut(string(status), "\n")
	if errno, convErr := strconv.Atoi(detail); kind == "exec" && convErr == nil {
		return &os.PathError{Op: "fork/exec", Path: g.target, Err: syscall.Errno(errno)}
	}
	if kind == "sandbox" {
		return sandboxNotApplied(detail)
	}
	return limitsNotApplied(detail)
}

func limitsNotApplied(detail string) error {
	return errors.New("resource limits could not be applied, so the command was not started: " + detail)
}

func sandboxNotApplied(detail string) error {
	return errors.New("command sandbox could not be applied, so the command was not started: " + detail)
}

func (g *commandGuard) cleanupSandbox() {
	for _, path := range g.created {
		_ = os.Remove(path)
	}
}

// limitHit reports which resource limit, if any, explains the failure err.
func (g *commandGuard) limitHit(err error, stderr string) *limitHit {
	var exitErr *exec.ExitError
	if len(g.applied) == 0 || !errors.As(err, &exitErr) {
		return nil
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		return nil
	}
	var maxRSS uint64
	if usage, ok := exitErr.SysUsage().(*syscall.Rusage); ok && usage.Maxrss > 0 {
		maxRSS = uint64(usage.Maxrss) << 10
	}
	if !ws.Signaled() {
		if hit := classifyLimitOutput(ws.ExitStatus(), stderr, g.applied); hit != nil {
			return hit
		}
		switch ws.ExitStatus() {
		case shellExitABRT, shellExitBUS, shellExitSEGV:
			return classifyLimitCrash(fmt.Sprintf("a crash (exit status %d)", ws.ExitStatus()), maxRSS, g.applied)
		}
		return nil
	}
	cpu, fsize := g.applied[limitCPU], g.applied[limitFileSize]
	switch ws.Signal() {
	case syscall.SIGXCPU:
		if cpu > 0 {
			return killedAtLimit(limitCPU, cpu)
		}
	case syscall.SIGXFSZ:
		if fsize > 0 {
			return killedAtLimit(limitFileSize, fsize)
		}
	case syscall.SIGKILL:
		// The hard CPU limit kills with a plain SIGKILL, which other
		// senders use too.
		used := exitErr.UserTime() + exitErr.SystemTime()
		if cpu > 0 && used >= time.Duration(cpu)*time.Second {
			return suspectedLimit(limitCPU, cpu, fmt.Sprintf("the command was killed after using %.0f seconds of CPU time", used.Seconds()))
		}
	case syscall.SIGSEGV, syscall.SIGABRT, syscall.SIGBUS:
		return classifyLimitCrash(ws.Signal().String(), maxRSS, g.applied)
	}
	return nil
}
