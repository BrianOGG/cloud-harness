//go:build linux || darwin

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// Seatbelt is the macOS kernel sandbox. run_command applies it through
// sandbox-exec, because applying a profile in-process needs cgo. Everything
// here except the kernel itself is portable: path canonicalisation and the
// commandGuard wiring live in command_limits_darwin.go, and this file also
// builds on Linux so its fail-closed paths are tested there against stand-in
// sandbox-exec programs.
const (
	// seatbeltExecArg is argv[1] of the trampoline: this binary re-executed
	// inside the sandbox to confirm the profile is enforced before it execs
	// the real command. sandbox-exec cannot set argv[0], so the marker is the
	// first argument.
	seatbeltExecArg = "cloud-harness-exec-sandboxed"
	// seatbeltStatusFD is the pipe the trampoline reports on. Unlike the Linux
	// trampoline it must report success too: sandbox-exec can fail before the
	// trampoline runs, and that also closes the pipe without data.
	seatbeltStatusFD = 3
	seatbeltStarted  = "ok\n"
	// seatbeltDataVolume is where macOS mounts the writable volume; every
	// user path is also reachable below it.
	seatbeltDataVolume = "/System/Volumes/Data"
	seatbeltProbeLimit = 10 * time.Second
)

// seatbeltPlan is the canonical form of a commandSandboxSpec: Seatbelt matches
// rules against the kernel's own name for a file, so every path here is that
// name rather than the spelling the harness was given.
type seatbeltPlan struct {
	// Deny lists the paths the command gets no file access to.
	Deny []string `json:"deny"`
	// Missing lists the Deny entries that do not exist and could not be
	// created; the command cannot create them either.
	Missing []string `json:"missing,omitempty"`
	// Preserve is the run worktree that stays reachable inside a denied
	// control directory.
	Preserve string `json:"preserve,omitempty"`
	// Traverse lists the denied directories above Preserve. They keep
	// file-read-metadata so the worktree path can still be resolved.
	Traverse []string `json:"traverse,omitempty"`
	// Pinned lists the directories above a Deny entry. Seatbelt rules follow
	// the current path of a file, so renaming one of them would move the
	// control files out from under the rules.
	Pinned []string `json:"pinned,omitempty"`
}

// seatbeltMasks counts the runs that rely on a directory created only to be
// masked, so one command finishing does not remove it under another.
var seatbeltMasks = struct {
	sync.Mutex
	refs map[string]int
}{refs: map[string]int{}}

// holdSeatbeltMask makes sure path exists as a directory, creating it and any
// missing parents. On a case-insensitive volume a rule for a path that does
// not exist yet would not cover a differently-cased spelling of it, so the
// name is fixed before the command runs. It returns the directories to release
// afterwards. A directory the harness may not create is an error, not a reason
// to go without: the command runs as the same user and could make the parent
// writable first. Only a read-only volume is left alone.
func holdSeatbeltMask(path string) ([]string, error) {
	seatbeltMasks.Lock()
	defer seatbeltMasks.Unlock()
	var chain []string
	for dir := path; ; dir = filepath.Dir(dir) {
		_, err := os.Lstat(dir)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err == nil && seatbeltMasks.refs[dir] == 0 {
			break
		}
		chain = append(chain, dir)
		if filepath.Dir(dir) == dir {
			break
		}
	}
	var held []string
	for i := len(chain) - 1; i >= 0; i-- {
		dir := chain[i]
		if seatbeltMasks.refs[dir] == 0 {
			if err := os.Mkdir(dir, 0o700); err != nil {
				if errors.Is(err, syscall.EROFS) {
					return held, nil
				}
				return held, fmt.Errorf("create control mask %s: %w", dir, err)
			}
		}
		seatbeltMasks.refs[dir]++
		held = append([]string{dir}, held...)
	}
	return held, nil
}

func releaseSeatbeltMasks(held []string) {
	seatbeltMasks.Lock()
	defer seatbeltMasks.Unlock()
	for _, dir := range held {
		if seatbeltMasks.refs[dir]--; seatbeltMasks.refs[dir] <= 0 {
			delete(seatbeltMasks.refs, dir)
			_ = os.Remove(dir)
		}
	}
}

// planSeatbelt turns spec into canonical paths. canonical must return the
// kernel's name for an existing path, following symlinks. The returned
// directories must be passed to releaseSeatbeltMasks once the command is done.
func planSeatbelt(spec commandSandboxSpec, canonical func(string) (string, error)) (seatbeltPlan, []string, error) {
	var plan seatbeltPlan
	var held []string
	seen := map[string]bool{}
	deny := func(path string) error {
		if path == string(filepath.Separator) {
			return errors.New("refusing to mask filesystem root")
		}
		if !seen[path] {
			seen[path] = true
			plan.Deny = append(plan.Deny, path)
		}
		return nil
	}
	for _, entry := range spec.Paths {
		path := filepath.Clean(entry.Path)
		more, err := holdSeatbeltMask(path)
		held = append(held, more...)
		if err != nil {
			return plan, held, err
		}
		// The entry under its own name, and what it resolves to when it is
		// a symlink.
		named, err := canonicalEntry(path, canonical)
		if err != nil {
			return plan, held, err
		}
		if err := deny(named); err != nil {
			return plan, held, err
		}
		resolved, err := canonical(path)
		switch {
		case err == nil:
			if err := deny(resolved); err != nil {
				return plan, held, err
			}
		case !errors.Is(err, fs.ErrNotExist):
			return plan, held, err
		default:
			if _, lerr := os.Lstat(path); lerr == nil {
				// The command could create the target and have the
				// harness read it as a control file.
				return plan, held, fmt.Errorf("control path %s is a dangling symlink", path)
			}
			plan.Missing = append(plan.Missing, named)
		}
	}
	if spec.Preserve != "" {
		preserve, err := canonical(spec.Preserve)
		if err != nil {
			return plan, held, fmt.Errorf("resolve command workspace: %w", err)
		}
		plan.Preserve = preserve
		for _, path := range plan.Deny {
			if path == preserve {
				return plan, held, errors.New("refusing to mask the command workspace")
			}
			if !pathWithin(preserve, path) {
				continue
			}
			for dir := filepath.Dir(preserve); pathWithin(dir, path); dir = filepath.Dir(dir) {
				plan.Traverse = append(plan.Traverse, dir)
			}
		}
		plan.Traverse = sortedUnique(plan.Traverse)
	}
	for _, path := range plan.Deny {
		for dir := filepath.Dir(path); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
			plan.Pinned = append(plan.Pinned, dir)
		}
	}
	plan.Pinned = sortedUnique(plan.Pinned)
	return plan, held, nil
}

// canonicalEntry names path without following a symlink in its last element:
// the canonical form of the nearest existing parent, plus the rest as given.
func canonicalEntry(path string, canonical func(string) (string, error)) (string, error) {
	rest := ""
	for dir := path; ; {
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no existing directory above %s", path)
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		resolved, err := canonical(parent)
		if err == nil {
			return filepath.Join(resolved, rest), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		dir = parent
	}
}

func sortedUnique(in []string) []string {
	sort.Strings(in)
	out := in[:0]
	for i, value := range in {
		if i == 0 || value != in[i-1] {
			out = append(out, value)
		}
	}
	return out
}

// seatbeltProfile renders plan as a Seatbelt profile. Everything stays allowed
// except file operations on the control files, so a command behaves as it does
// unsandboxed until it touches them.
func seatbeltProfile(plan seatbeltPlan) (string, error) {
	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n")
	preserve := ""
	if plan.Preserve != "" {
		quoted, err := seatbeltString(plan.Preserve)
		if err != nil {
			return "", err
		}
		preserve = quoted
	}
	for _, path := range plan.Deny {
		quoted, err := seatbeltString(path)
		if err != nil {
			return "", err
		}
		if preserve != "" && pathWithin(plan.Preserve, path) {
			fmt.Fprintf(&b, "(deny file* (require-all (subpath %s) (require-not (subpath %s))))\n", quoted, preserve)
			continue
		}
		fmt.Fprintf(&b, "(deny file* (subpath %s))\n", quoted)
	}
	// Later rules win, so these two follow the denials they refine.
	if err := seatbeltLiterals(&b, "(allow file-read-metadata", plan.Traverse); err != nil {
		return "", err
	}
	if err := seatbeltLiterals(&b, "(deny file-write-unlink", plan.Pinned); err != nil {
		return "", err
	}
	return b.String(), nil
}

func seatbeltLiterals(b *strings.Builder, rule string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	b.WriteString(rule)
	for _, path := range paths {
		quoted, err := seatbeltString(path)
		if err != nil {
			return err
		}
		b.WriteString(" (literal " + quoted + ")")
	}
	b.WriteString(")\n")
	return nil
}

// seatbeltString quotes path for a profile. Paths the profile language cannot
// carry unambiguously are refused rather than approximated.
func seatbeltString(path string) (string, error) {
	if !filepath.IsAbs(path) || !utf8.ValidString(path) {
		return "", fmt.Errorf("command sandbox cannot express path %q", path)
	}
	for i := 0; i < len(path); i++ {
		if path[i] < 0x20 || path[i] == 0x7f {
			return "", fmt.Errorf("command sandbox cannot express path %q", path)
		}
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(path) + `"`, nil
}

// runSeatbelt runs target with argv under plan, through cmd, and waits for
// it. cmd supplies the directory, environment, and streams. With no target it
// only confirms that the sandbox is enforced. The command is not started
// unless the trampoline has seen the profile deny every path in plan.
func runSeatbelt(cmd *exec.Cmd, sandboxExec string, plan seatbeltPlan, target string, argv []string) error {
	profile, err := seatbeltProfile(plan)
	if err != nil {
		return sandboxNotApplied(err.Error())
	}
	self, err := os.Executable()
	if err != nil {
		return sandboxNotApplied(err.Error())
	}
	check, err := json.Marshal(plan)
	if err != nil {
		return sandboxNotApplied(err.Error())
	}
	statusR, statusW, err := os.Pipe()
	if err != nil {
		return sandboxNotApplied(err.Error())
	}
	defer func() { _ = statusR.Close() }()
	cmd.Path = sandboxExec
	cmd.Args = []string{sandboxExec, "-p", profile, self, seatbeltExecArg, string(check)}
	if target != "" {
		cmd.Args = append(append(cmd.Args, target), argv...)
	}
	cmd.ExtraFiles = []*os.File{statusW}
	err = cmd.Start()
	_ = statusW.Close()
	if err != nil {
		return sandboxNotApplied(err.Error())
	}
	status, _ := io.ReadAll(io.LimitReader(statusR, 4096))
	return seatbeltResult(string(status), target, cmd.Wait())
}

// seatbeltResult maps what the trampoline reported to the command's result.
func seatbeltResult(status, target string, waitErr error) error {
	rest, started := strings.CutPrefix(status, seatbeltStarted)
	switch {
	case started && rest == "":
		return waitErr
	case started:
		kind, detail, _ := strings.Cut(rest, "\n")
		if errno, err := strconv.Atoi(detail); kind == "exec" && err == nil {
			return &os.PathError{Op: "fork/exec", Path: target, Err: syscall.Errno(errno)}
		}
		return sandboxNotApplied("unexpected trampoline status")
	case status == "":
		detail := "sandbox-exec did not start the command"
		if waitErr != nil {
			detail += ": " + waitErr.Error()
		}
		return sandboxNotApplied(detail)
	}
	if kind, detail, _ := strings.Cut(status, "\n"); kind == "sandbox" {
		return sandboxNotApplied(detail)
	}
	return sandboxNotApplied("unexpected trampoline status")
}

// execSeatbelt is the trampoline body: args are the encoded plan, then the
// target path and its argv. It never returns.
func execSeatbelt(args []string) {
	report := func(text string) { _, _ = syscall.Write(seatbeltStatusFD, []byte(text)) }
	fail := func(detail string) {
		report("sandbox\n" + detail)
		os.Exit(126)
	}
	syscall.CloseOnExec(seatbeltStatusFD)
	if len(args) != 1 && len(args) < 3 {
		fail("malformed trampoline arguments")
	}
	var plan seatbeltPlan
	if err := json.Unmarshal([]byte(args[0]), &plan); err != nil {
		fail("decode sandbox: " + err.Error())
	}
	if err := verifySeatbelt(plan); err != nil {
		fail(err.Error())
	}
	report(seatbeltStarted)
	if len(args) == 1 {
		os.Exit(0)
	}
	err := syscall.Exec(args[1], args[2:], os.Environ())
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		errno = syscall.ENOEXEC
	}
	report("exec\n" + strconv.Itoa(int(errno)))
	os.Exit(126)
}

// verifySeatbelt runs inside the sandbox and confirms the kernel refuses each
// denied path, under its own name and under the data volume. It catches a
// profile that was not applied and a rule that does not match the name the
// kernel uses for the file.
func verifySeatbelt(plan seatbeltPlan) error {
	missing := map[string]bool{}
	for _, path := range plan.Missing {
		missing[path] = true
	}
	for _, path := range plan.Deny {
		if err := confirmDenied(path, missing[path]); err != nil {
			return err
		}
		if !pathWithin(path, seatbeltDataVolume) {
			if err := confirmDenied(seatbeltDataVolume+path, true); err != nil {
				return err
			}
		}
	}
	if plan.Preserve != "" {
		fd, err := unix.Open(plan.Preserve, unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("the sandbox hides the command workspace %s: %v", plan.Preserve, err)
		}
		_ = unix.Close(fd)
	}
	return nil
}

func confirmDenied(path string, mayBeMissing bool) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	switch {
	case err == nil:
		_ = unix.Close(fd)
		return fmt.Errorf("the sandbox does not deny %s", path)
	case errors.Is(err, unix.EPERM):
		return nil
	case mayBeMissing && (errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR)):
		return nil
	}
	return fmt.Errorf("could not confirm the sandbox denies %s: %v", path, err)
}

// probeSeatbelt reports whether sandbox-exec can apply a profile here and the
// kernel enforces it. It fails inside another sandbox, which may not nest.
func probeSeatbelt(sandboxExec string, canonical func(string) (string, error)) error {
	if _, err := os.Stat(sandboxExec); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "cloud-harness-sandbox-probe-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(dir) }()
	resolved, err := canonical(dir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), seatbeltProbeLimit)
	defer cancel()
	return runSeatbelt(exec.CommandContext(ctx, sandboxExec), sandboxExec, seatbeltPlan{Deny: []string{resolved}}, "", nil)
}
