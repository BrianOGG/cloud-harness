package hooks

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"go/build"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// GoSnapshot compares bytes rather than mtimes or git status. This detects
// untracked files and repeated edits to an already dirty file on every call.
type GoSnapshot map[string][sha256.Size]byte

func (e *Engine) GoCheckEnabled() bool {
	return e != nil && e.Config != nil && (e.Config.GoCheck == nil || *e.Config.GoCheck)
}

func (e *Engine) SnapshotGo(ctx context.Context) (GoSnapshot, error) {
	if !e.GoCheckEnabled() {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	files := make(GoSnapshot)
	root, err := filepath.EvalSymlinks(e.Config.Workspace)
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == ".cloud-harness" {
				return filepath.SkipDir
			}
			return nil
		}
		// Never follow a symlink into a different workspace or credential file.
		if filepath.Ext(path) != ".go" || !d.Type().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, err = io.Copy(hash, contextReader{ctx: ctx, reader: f})
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		var sum [sha256.Size]byte
		copy(sum[:], hash.Sum(nil))
		files[rel] = sum
		return nil
	})
	return files, err
}

// CheckGo runs only for packages containing changed Go files. Diagnostics are
// returned for appending to the original tool result, so the next model call
// can fix the change. It never formats files or changes go.mod/go.sum.
func (e *Engine) CheckGo(ctx context.Context, input Input, before GoSnapshot, scanErr error, record Recorder) (Outcome, error) {
	if !e.GoCheckEnabled() {
		return Outcome{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	redact := newRedactor()
	after, err := e.SnapshotGo(ctx)
	if scanErr == nil {
		scanErr = err
	}
	if scanErr != nil {
		event := Event{Point: PostToolUse, Type: "go_check", InvocationID: input.InvocationID, ExitCode: 1, Warning: true, Reason: "Go self-check could not inspect workspace"}
		if errors.Is(scanErr, context.DeadlineExceeded) || errors.Is(scanErr, context.Canceled) {
			event.ExitCode, event.Blocked, event.Warning = 2, true, false
			event.TimedOut = errors.Is(scanErr, context.DeadlineExceeded)
			event.Reason = "Go self-check inspection timed out or was cancelled"
		}
		_, event.Stderr = safeOutput(redact, "", scanErr.Error())
		if record != nil {
			if err := record(ctx, event); err != nil {
				return Outcome{}, err
			}
		}
		return Outcome{Message: event.Message(), Blocked: event.Blocked}, nil
	}
	packages := make(map[string][]string)
	for path, hash := range after {
		if previous, exists := before[path]; !exists || previous != hash {
			dir := filepath.Dir(path)
			packages[dir] = append(packages[dir], filepath.Base(path))
		}
	}
	for path := range before {
		if _, exists := after[path]; !exists {
			dir := filepath.Dir(path)
			if _, exists := packages[dir]; !exists {
				packages[dir] = nil
			}
		}
	}
	if len(packages) == 0 {
		return Outcome{}, nil
	}
	var dirs []string
	for dir := range packages {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	var output cappedBuffer
	exitCode := 0
	for _, dir := range dirs {
		cwd, err := workingDir(e.Config.Workspace, dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			exitCode = 1
			_, _ = fmt.Fprintf(&output, "Go self-check: %s\n", err)
			continue
		}
		files := packages[dir]
		sort.Strings(files)
		if len(files) > 0 {
			args := []string{"-l"}
			for _, file := range files {
				args = append(args, "./"+file)
			}
			res := runProcess(ctx, cwd, goEnv(), "", filepath.Join(runtime.GOROOT(), "bin", "gofmt"), args...)
			if res.ExitCode != 0 || res.Stdout != "" || res.Stderr != "" {
				exitCode = 1
				_, _ = fmt.Fprintf(&output, "gofmt -l (%s; formatting or syntax needs repair):\n%s%s\n", dir, res.Stdout, res.Stderr)
			}
		}
		if ctx.Err() != nil {
			break
		}
		res := runProcess(ctx, cwd, goEnv(), "", filepath.Join(runtime.GOROOT(), "bin", "go"), "vet", ".")
		if res.ExitCode != 0 {
			exitCode = 1
			_, _ = fmt.Fprintf(&output, "go vet (%s):\n%s%s\n", dir, res.Stdout, res.Stderr)
		}
	}
	event := Event{Point: PostToolUse, Type: "go_check", InvocationID: input.InvocationID, ExitCode: exitCode, Warning: exitCode != 0}
	_, event.Stderr = safeOutput(redact, "", string(output.data))
	if ctx.Err() != nil {
		event.ExitCode, event.Blocked, event.Warning = 2, true, false
		event.TimedOut = ctx.Err() == context.DeadlineExceeded
		event.Reason = "Go self-check timed out or was cancelled; inspect the changed package before continuing"
	} else if event.Warning {
		event.Reason = "Go self-check found problems; repair the changed package in this task"
	}
	if record != nil {
		if err := record(ctx, event); err != nil {
			return Outcome{}, err
		}
	}
	if event.Warning || event.Blocked {
		return Outcome{Message: event.Message(), Blocked: event.Blocked}, nil
	}
	return Outcome{}, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func goEnv() []string {
	env := commandEnv()
	cache := os.Getenv("GOCACHE")
	if cache == "" {
		if dir, err := os.UserCacheDir(); err == nil {
			cache = filepath.Join(dir, "go-build")
		} else {
			cache = filepath.Join(os.TempDir(), "cloud-harness-go-build")
		}
	}
	// No proxy, credentials, user Go config, downloads, or writable module mode.
	return append(env, "GOCACHE="+cache, "GOPATH="+build.Default.GOPATH,
		"GOENV=off", "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off",
		"GOFLAGS=-mod=readonly", "GOTELEMETRY=off",
		"GOROOT="+strings.TrimSpace(runtime.GOROOT()))
}
