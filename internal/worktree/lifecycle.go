package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const ConflictResolutionAttempts = 2

// Entry is an inventory observation; it is not another lifecycle state store.
type Entry struct {
	Info
	Exists     bool      `json:"exists"`
	Registered bool      `json:"registered"`
	Error      string    `json:"error,omitempty"`
	ModifiedAt time.Time `json:"-"`
}

// List reconciles the private metadata directory with Git's registered
// worktrees, including registrations whose working directory disappeared.
func List(ctx context.Context, workspace string) ([]Entry, error) {
	root, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	if err := Validate(ctx, root); err != nil {
		return nil, err
	}
	raw, err := git(ctx, root, nil, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		// Older Git releases have porcelain output but not its NUL option.
		raw, err = git(ctx, root, nil, "-c", "core.quotePath=false", "worktree", "list", "--porcelain")
		if err != nil {
			return nil, err
		}
		raw = []byte(strings.ReplaceAll(string(raw), "\n", "\x00"))
	}
	parent := filepath.Join(root, ".cloud-harness", "worktrees")
	entries := map[string]Entry{}
	current := ""
	for _, field := range strings.Split(string(raw), "\x00") {
		if strings.HasPrefix(field, "worktree ") {
			path := strings.TrimPrefix(field, "worktree ")
			if strings.HasPrefix(path, "\"") {
				decoded, err := strconv.Unquote(path)
				if err != nil {
					return nil, fmt.Errorf("decode worktree path: %w", err)
				}
				path = decoded
			}
			current = ""
			if filepath.Dir(path) == parent && runIDPattern.MatchString(filepath.Base(path)) {
				current = filepath.Base(path)
				entries[current] = Entry{Info: Info{RunID: current, Path: path, Branch: "cloud-harness/" + current}, Registered: true}
			}
		} else if current != "" && strings.HasPrefix(field, "branch refs/heads/") {
			e := entries[current]
			e.Branch = strings.TrimPrefix(field, "branch refs/heads/")
			entries[current] = e
		}
	}
	disk, err := os.ReadDir(parent)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, item := range disk {
		id := item.Name()
		if !item.IsDir() {
			if !strings.HasSuffix(id, ".json") {
				continue
			}
			id = strings.TrimSuffix(id, ".json")
		}
		if !runIDPattern.MatchString(id) {
			continue
		}
		if _, ok := entries[id]; !ok {
			entries[id] = Entry{Info: Info{RunID: id, Path: filepath.Join(parent, id), Branch: "cloud-harness/" + id}}
		}
	}
	out := make([]Entry, 0, len(entries))
	for id, e := range entries {
		info, exists, lookupErr := Lookup(ctx, root, id)
		e.Exists = exists
		if lookupErr != nil {
			e.Error = lookupErr.Error()
		} else if exists {
			e.Info = info
		}
		for _, path := range []string{e.Path + ".json", e.Path} {
			if st, err := os.Stat(path); err == nil && st.ModTime().After(e.ModifiedAt) {
				e.ModifiedAt = st.ModTime()
			}
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RunID < out[j].RunID })
	return out, nil
}

type MergeOptions struct {
	// Test is required and runs both before and after integration into main.
	Test func(context.Context, string) error
	// Resolve may edit files only in info.Path. Git staging/commits remain here.
	Resolve func(context.Context, Info, []string, string) error
	// Record executes after both test gates, before cleanup. Failure rolls main
	// back and retains the agent branch, so the run remains pending/retryable.
	Record func(context.Context) error
}

type ConflictError struct {
	Files []string
	Cause error
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("conflict resolution stopped after at most %d attempts; conflict files: %s; worktree retained for manual resolution: %v", ConflictResolutionAttempts, strings.Join(e.Files, ", "), e.Cause)
}
func (e *ConflictError) Unwrap() error { return e.Cause }

// Merge performs Git-only integration. The lifecycle coordinator must serialize
// callers through the DB decision and cleanup as well as this Git operation.
func Merge(ctx context.Context, workspace string, info Info, opts MergeOptions) error {
	if opts.Test == nil {
		return errors.New("merge requires a test gate")
	}
	if err := validateBinding(ctx, workspace, info); err != nil {
		return err
	}
	mainRaw, err := git(ctx, workspace, nil, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return fmt.Errorf("cannot merge from detached HEAD: %w", err)
	}
	main := strings.TrimSpace(string(mainRaw))
	if main == info.Branch {
		return errors.New("workspace HEAD must be on the main workspace branch")
	}
	if merging(ctx, workspace) {
		return errors.New("main workspace already has a merge in progress")
	}
	if err := requireClean(ctx, workspace); err != nil {
		return fmt.Errorf("main workspace must be clean: %w", err)
	}
	before, err := git(ctx, workspace, nil, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	beforeSHA := strings.TrimSpace(string(before))
	// A retry may finish an older merge and then conflict with newer main.
	// Both phases share one repair budget for this invocation.
	attemptsLeft := ConflictResolutionAttempts
	if merging(ctx, info.Path) {
		if err := resolveMerge(ctx, info, opts, &attemptsLeft); err != nil {
			return err
		}
	} else if err := commitEdits(ctx, info.Path, "cloud-harness: preserve "+info.RunID); err != nil {
		return err
	}
	// A retained conflict may have been against an older main HEAD. After
	// completing it, integrate today's main as well before touching main.
	if _, err := identityGit(ctx, info.Path, "merge", "--no-edit", main); err != nil {
		files, readErr := conflicts(ctx, info.Path)
		if readErr != nil {
			return errors.Join(err, readErr)
		}
		if len(files) == 0 {
			return err
		}
	}
	if merging(ctx, info.Path) {
		if err := resolveMerge(ctx, info, opts, &attemptsLeft); err != nil {
			return err
		}
	} else if err := opts.Test(ctx, info.Path); err != nil {
		return fmt.Errorf("worktree test gate: %w", err)
	}
	if err := requireClean(ctx, info.Path); err != nil {
		return fmt.Errorf("worktree changed during tests: %w", err)
	}
	// Recheck before touching main: a human or test gate may have changed it.
	head, err := git(ctx, workspace, nil, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	branch, err := git(ctx, workspace, nil, "symbolic-ref", "--short", "HEAD")
	if err != nil || strings.TrimSpace(string(head)) != beforeSHA || strings.TrimSpace(string(branch)) != main {
		return errors.New("main workspace HEAD changed while preparing merge")
	}
	if err := requireClean(ctx, workspace); err != nil {
		return fmt.Errorf("main workspace changed while preparing merge: %w", err)
	}
	if _, err := identityGit(ctx, workspace, "merge", "--no-ff", "--no-edit", info.Branch); err != nil {
		recoverCtx, cancel := recoveryContext(ctx)
		defer cancel()
		var abortErr error
		if merging(recoverCtx, workspace) {
			_, abortErr = git(recoverCtx, workspace, nil, "merge", "--abort")
		}
		var resetErr error
		head, readErr := git(recoverCtx, workspace, nil, "rev-parse", "HEAD")
		if abortErr != nil || (readErr == nil && strings.TrimSpace(string(head)) != beforeSHA) {
			_, resetErr = git(recoverCtx, workspace, nil, "reset", "--hard", beforeSHA)
		}
		return errors.Join(fmt.Errorf("merge into main failed: %w", err), abortErr, readErr, resetErr)
	}
	rollback := func(cause error) error {
		recoverCtx, cancel := recoveryContext(ctx)
		defer cancel()
		_, resetErr := git(recoverCtx, workspace, nil, "reset", "--hard", beforeSHA)
		if resetErr != nil {
			return errors.Join(fmt.Errorf("merge failed; rollback to %s failed: %w", beforeSHA, cause), resetErr)
		}
		return fmt.Errorf("merge rolled back to %s: %w", beforeSHA, cause)
	}
	if err := opts.Test(ctx, workspace); err != nil {
		return rollback(fmt.Errorf("main test gate: %w", err))
	}
	if err := requireClean(ctx, workspace); err != nil {
		return rollback(fmt.Errorf("main changed during tests: %w", err))
	}
	if opts.Record != nil {
		if err := opts.Record(ctx); err != nil {
			return rollback(fmt.Errorf("record merged state: %w", err))
		}
	}
	if err := Remove(ctx, workspace, info); err != nil {
		return fmt.Errorf("merged successfully; cleanup pending (use discard): %w", err)
	}
	return nil
}

func resolveMerge(ctx context.Context, info Info, opts MergeOptions, attemptsLeft *int) error {
	files, err := conflicts(ctx, info.Path)
	if err != nil {
		return err
	}
	original := append([]string(nil), files...)
	// Manual resolution staged before retry needs no model call.
	if len(files) == 0 {
		if err := checkMarkers(ctx, info.Path); err != nil {
			return err
		}
		if err := opts.Test(ctx, info.Path); err != nil {
			return fmt.Errorf("resolved worktree test gate: %w", err)
		}
		_, err := identityGit(ctx, info.Path, "commit", "--no-edit")
		return err
	}
	feedback := "Resolve these merge conflicts preserving both sides' intent."
	for *attemptsLeft > 0 {
		if opts.Resolve == nil {
			break
		}
		(*attemptsLeft)--
		err = opts.Resolve(ctx, info, original, feedback)
		if err == nil {
			err = checkMarkers(ctx, info.Path)
		}
		if err == nil {
			_, err = git(ctx, info.Path, nil, "add", "-A", "--", ".")
		}
		if err == nil {
			err = opts.Test(ctx, info.Path)
		}
		if err == nil {
			_, err = identityGit(ctx, info.Path, "commit", "--no-edit")
		}
		if err == nil {
			return nil
		}
		feedback = err.Error()
		if ctx.Err() != nil {
			break
		}
	}
	if err == nil {
		err = errors.New("conflicts require manual resolution")
	}
	return &ConflictError{Files: original, Cause: err}
}

func checkMarkers(ctx context.Context, path string) error {
	// diff --check catches conflict markers even before git add clears unmerged
	// entries. Check both layers so a staged marker cannot evade the gate.
	if out, err := git(ctx, path, nil, "diff", "--check"); err != nil {
		return fmt.Errorf("conflict/whitespace check: %w; %s", err, out)
	}
	out, err := git(ctx, path, nil, "diff", "--cached", "--check")
	if err != nil {
		return fmt.Errorf("staged conflict/whitespace check: %w; %s", err, out)
	}
	return nil
}

func conflicts(ctx context.Context, path string) ([]string, error) {
	raw, err := git(ctx, path, nil, "diff", "--name-only", "--diff-filter=U", "-z")
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00"), nil
}
func merging(ctx context.Context, path string) bool {
	_, err := git(ctx, path, nil, "rev-parse", "--verify", "MERGE_HEAD")
	return err == nil
}
func requireClean(ctx context.Context, path string) error {
	raw, err := git(ctx, path, nil, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return err
	}
	if len(raw) > 0 {
		return fmt.Errorf("uncommitted files: %s", strings.TrimSpace(string(raw)))
	}
	return nil
}
func commitEdits(ctx context.Context, path, message string) error {
	raw, err := git(ctx, path, nil, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil || len(raw) == 0 {
		return err
	}
	if _, err = git(ctx, path, nil, "add", "-A", "--", "."); err != nil {
		return err
	}
	_, err = identityGit(ctx, path, "commit", "-m", message)
	return err
}
func identityGit(ctx context.Context, path string, args ...string) ([]byte, error) {
	return git(ctx, path, nil, append([]string{"-c", "user.name=Cloud Harness", "-c", "user.email=cloud-harness@localhost", "-c", "commit.gpgsign=false"}, args...)...)
}
func recoveryContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
}
func validateBinding(ctx context.Context, workspace string, info Info) error {
	if info.Branch != "cloud-harness/"+info.RunID {
		return errors.New("worktree branch no longer matches run binding")
	}
	actual, exists, err := Lookup(ctx, workspace, info.RunID)
	if err != nil {
		return err
	}
	if !exists || actual != info {
		return errors.New("worktree is missing or its binding changed")
	}
	return nil
}

// Remove destroys only this run's registered worktree, branch and original
// metadata. Callers must check run activity and authorize unmerged deletion.
// Missing paths/branches are allowed so interrupted cleanup can be retried.
func Remove(ctx context.Context, workspace string, info Info) error {
	root, err := filepath.Abs(workspace)
	if err != nil {
		return err
	}
	if !runIDPattern.MatchString(info.RunID) || info.Path != filepath.Join(root, ".cloud-harness", "worktrees", info.RunID) || info.Branch != "cloud-harness/"+info.RunID {
		return errors.New("refusing to remove an invalid worktree binding")
	}
	inventory, err := List(ctx, root)
	if err != nil {
		return err
	}
	for _, e := range inventory {
		if e.RunID == info.RunID {
			if e.Error != "" || e.Branch != info.Branch {
				return errors.New("refusing to remove a changed or unreadable worktree binding")
			}
			if e.Registered {
				if _, err := git(ctx, root, nil, "worktree", "remove", "--force", info.Path); err != nil {
					return err
				}
			} else if _, err := os.Stat(info.Path); err == nil {
				return errors.New("unregistered worktree directory requires manual inspection")
			}
		}
	}
	if _, err := git(ctx, root, nil, "show-ref", "--verify", "--quiet", "refs/heads/"+info.Branch); err == nil {
		if _, err := git(ctx, root, nil, "branch", "-D", "--", info.Branch); err != nil {
			return err
		}
	}
	if err := os.Remove(info.Path + ".json"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
