//go:build linux

package tools

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// limitExecArg0 is argv[0] of the trampoline: this binary re-executed to
	// set rlimits on itself and then exec the real command. Setting them in
	// the child, before the command exists, leaves no window in which the
	// command runs unlimited.
	limitExecArg0 = "cloud-harness-exec-limits"
	limitExecSelf = "/proc/self/exe"
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
}

// execLimited is the trampoline body: args are the encoded limits, the target
// path, and the target argv. It never returns.
func execLimited(args []string) {
	fail := func(kind, detail string) {
		_, _ = syscall.Write(limitStatusFD, []byte(kind+"\n"+detail))
		os.Exit(126)
	}
	syscall.CloseOnExec(limitStatusFD)
	if len(args) < 3 {
		fail("limits", "malformed trampoline arguments")
	}
	limits, err := decodeRlimits(args[0])
	if err != nil {
		fail("limits", err.Error())
	}
	env := os.Environ()
	for _, lim := range limits {
		if err := syscall.Setrlimit(lim.resource, &syscall.Rlimit{Cur: lim.cur, Max: lim.max}); err != nil {
			fail("limits", "setrlimit "+lim.name+": "+err.Error())
		}
	}
	err = syscall.Exec(args[1], args[2:], env)
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

// commandGuard runs one run_command process in its own process group, under
// resource limits when they are enabled.
type commandGuard struct {
	cmd     *exec.Cmd
	target  string
	applied map[string]uint64
	planErr error
	statusR *os.File
	statusW *os.File
}

func guardCommand(cmd *exec.Cmd, limits CommandLimits) *commandGuard {
	g := &commandGuard{cmd: cmd, target: cmd.Path, applied: map[string]uint64{}}
	applySysProc(cmd)
	cmd.Cancel = func() error { return killGroup(cmd) }
	if cmd.Err != nil {
		return g
	}
	plan, err := planRlimits(limits)
	if err != nil {
		g.planErr = err
		return g
	}
	if len(plan) == 0 {
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
	cmd.Args = append([]string{limitExecArg0, encodeRlimits(plan), cmd.Path}, cmd.Args...)
	cmd.Path = limitExecSelf
	cmd.ExtraFiles = []*os.File{g.statusW}
	return g
}

// Run starts the command and waits for it. A command whose limits could not
// be established is not started.
func (g *commandGuard) Run() error {
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
		return limitsNotApplied(err.Error())
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
	return limitsNotApplied(detail)
}

func limitsNotApplied(detail string) error {
	return errors.New("resource limits could not be applied, so the command was not started: " + detail)
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
