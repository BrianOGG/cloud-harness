//go:build darwin

package tools

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// seatbeltExecPath is fixed rather than looked up on PATH: the directory is
// protected by the system, so the command cannot be replaced there.
const seatbeltExecPath = "/usr/bin/sandbox-exec"

func init() {
	if len(os.Args) > 1 && os.Args[1] == seatbeltExecArg {
		execSeatbelt(os.Args[2:])
	}
}

// seatbeltPathBuf receives F_GETPATH results. fcntl takes the buffer as an
// integer, so it must be memory the runtime never moves.
var seatbeltPathBuf struct {
	sync.Mutex
	path [unix.PathMax]byte
}

// canonicalPath returns the kernel's name for an existing path: symlinks
// resolved and every element spelled as it is stored on disk. Seatbelt matches
// rules against this name, so a rule built from another spelling of the same
// directory would not apply.
func canonicalPath(path string) (string, error) {
	fd, err := unix.Open(path, unix.O_EVTONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer func() { _ = unix.Close(fd) }()
	seatbeltPathBuf.Lock()
	defer seatbeltPathBuf.Unlock()
	buf := seatbeltPathBuf.path[:]
	clear(buf)
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETPATH, int(uintptr(unsafe.Pointer(&buf[0])))); err != nil {
		return "", &os.PathError{Op: "fcntl", Path: path, Err: err}
	}
	end := bytes.IndexByte(buf, 0)
	if end <= 0 {
		return "", &os.PathError{Op: "fcntl", Path: path, Err: unix.ENAMETOOLONG}
	}
	return string(buf[:end]), nil
}

var commandSandboxProbe = struct {
	sync.Once
	ok bool
}{}

func commandSandboxAvailable() bool {
	commandSandboxProbe.Do(func() {
		commandSandboxProbe.ok = probeSeatbelt(seatbeltExecPath, canonicalPath) == nil
	})
	return commandSandboxProbe.ok
}

// warmCommandSandbox resolves the sandbox probe during tool registration so a
// later approved command does not pay for it inside a request deadline.
func warmCommandSandbox() {
	_ = commandSandboxAvailable()
}

// commandGuard runs one run_command process under a Seatbelt profile that
// denies harness control files. Resource limits are enforced on Linux only.
// Where Seatbelt cannot be applied at all, commands that need no approval run
// as before and approved dangerous commands are not started.
type commandGuard struct {
	cmd     *exec.Cmd
	failErr error
	sandbox *commandSandboxSpec
}

func guardCommand(cmd *exec.Cmd, _ CommandLimits, workspace string, requireSandbox bool) *commandGuard {
	return guardCommandWith(cmd, workspace, requireSandbox, commandSandboxAvailable)
}

func guardCommandWith(cmd *exec.Cmd, workspace string, requireSandbox bool, available func() bool) *commandGuard {
	g := &commandGuard{cmd: cmd}
	if cmd.Err != nil {
		return g
	}
	if !available() {
		if requireSandbox {
			g.failErr = errors.New("command sandbox unavailable on this system; dangerous command was not started")
		}
		return g
	}
	spec, err := commandSandboxSpecFor(workspace)
	if err != nil {
		g.failErr = err
		return g
	}
	g.sandbox = &spec
	return g
}

// Run starts the command and waits for it. A command whose sandbox could not
// be established is not started.
func (g *commandGuard) Run() error {
	if g.failErr != nil {
		return g.failErr
	}
	if g.sandbox == nil {
		return g.cmd.Run()
	}
	plan, held, err := planSeatbelt(*g.sandbox, canonicalPath)
	defer releaseSeatbeltMasks(held)
	if err != nil {
		return sandboxNotApplied(err.Error())
	}
	return runSeatbelt(g.cmd, seatbeltExecPath, plan, g.cmd.Path, g.cmd.Args)
}

func (g *commandGuard) limitHit(error, string) *limitHit { return nil }
