// Package snapshot captures workspace trees in a Git repository kept outside
// the workspace's own Git metadata.
package snapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/storage"
)

var ErrUnavailable = errors.New("workspace snapshots are unavailable")

// UnavailableError explains why the snapshotter is disabled. It matches
// ErrUnavailable through errors.Is.
type UnavailableError struct {
	Reason string
}

func (e *UnavailableError) Error() string {
	if e == nil || e.Reason == "" {
		return ErrUnavailable.Error()
	}
	return ErrUnavailable.Error() + ": " + e.Reason
}

func (e *UnavailableError) Unwrap() error { return ErrUnavailable }

const (
	StateEnabled  = "enabled"
	StateDisabled = "disabled"
)

// Status is a stable description of whether Git-backed snapshots can run.
type Status struct {
	State  string
	Reason string
}

// RecordStore is the persistence surface needed by a workspace snapshotter.
type RecordStore interface {
	CreateWorkspaceSnapshot(context.Context, storage.WorkspaceSnapshot) (storage.WorkspaceSnapshot, error)
	ListWorkspaceSnapshots(context.Context, string) ([]storage.WorkspaceSnapshot, error)
}

// Snapshotter binds one absolute workspace path to a shadow repository.
type Snapshotter struct {
	workspace   string
	workspaceID string
	shadowDir   string
	gitBin      string
	gitErr      error
	store       RecordStore
	mu          *sync.Mutex
}

// All handles for the same shadow repository share a lock, including Open's
// initialization. Git's index lock alone cannot protect a multi-command capture.
var workspaceLocks sync.Map

// Open binds a snapshotter to an existing workspace directory. When Git is
// unavailable it still returns a snapshotter so callers can report Status;
// operations then return an UnavailableError.
func Open(workspace string, store RecordStore) (*Snapshotter, error) {
	if store == nil {
		return nil, errors.New("snapshot record store is required")
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace path: %w", err)
	}
	abs = filepath.Clean(abs)
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("stat workspace: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace is not a directory: %s", abs)
	}
	workspaceID := workspaceIDFor(abs)
	shadowDir := filepath.Join(abs, ".cloud-harness", "snapshots", workspaceID)
	mu, _ := workspaceLocks.LoadOrStore(shadowDir, &sync.Mutex{})
	gitBin, gitErr := exec.LookPath("git")
	snapshotter := &Snapshotter{
		workspace:   abs,
		workspaceID: workspaceID,
		shadowDir:   shadowDir,
		gitBin:      gitBin,
		gitErr:      gitErr,
		store:       store,
		mu:          mu.(*sync.Mutex),
	}
	snapshotter.mu.Lock()
	defer snapshotter.mu.Unlock()
	if gitErr == nil {
		if err := snapshotter.ensureGit(context.Background()); err != nil {
			return nil, err
		}
	}
	return snapshotter, nil
}

// WorkspaceID returns the stable ID derived from the absolute workspace path.
func (s *Snapshotter) WorkspaceID() string { return s.workspaceID }

// WorkspacePath returns the absolute path bound to this snapshotter.
func (s *Snapshotter) WorkspacePath() string { return s.workspace }

// WorkspaceIDForPath returns the stable workspace ID without initializing the
// shadow repository. Callers can persist the workspace binding before the
// first (possibly much later) snapshot is captured.
func WorkspaceIDForPath(workspace string) (string, error) {
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	return workspaceIDFor(filepath.Clean(abs)), nil
}

// Status reports disabled when the git executable could not be resolved.
func (s *Snapshotter) Status() Status {
	if s.gitErr != nil {
		return Status{State: StateDisabled, Reason: s.gitErr.Error()}
	}
	return Status{State: StateEnabled}
}

// Snapshot stages the current worktree in the shadow index, writes its tree,
// and persists the resulting record.
func (s *Snapshotter) Snapshot(ctx context.Context, label string) (storage.WorkspaceSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureGit(ctx); err != nil {
		return storage.WorkspaceSnapshot{}, err
	}
	if _, err := s.git(ctx, "add", "-A", "--", "."); err != nil {
		return storage.WorkspaceSnapshot{}, err
	}
	if err := s.replaceFilteredBlobs(ctx); err != nil {
		return storage.WorkspaceSnapshot{}, err
	}
	out, err := s.git(ctx, "write-tree")
	if err != nil {
		return storage.WorkspaceSnapshot{}, err
	}
	treeOID := strings.TrimSpace(string(out))
	if treeOID == "" {
		return storage.WorkspaceSnapshot{}, errors.New("git write-tree returned an empty tree OID")
	}
	entries, err := s.treeEntries(ctx, treeOID)
	if err != nil {
		return storage.WorkspaceSnapshot{}, err
	}
	var totalBytes int64
	for _, entry := range entries {
		totalBytes += entry.size
	}
	rec := storage.WorkspaceSnapshot{
		ID:            domain.NewID("snap_"),
		WorkspaceID:   s.workspaceID,
		WorkspacePath: s.workspace,
		TreeOID:       treeOID,
		Label:         label,
		FileCount:     int64(len(entries)),
		TotalBytes:    totalBytes,
		CreatedAt:     time.Now().UTC(),
	}
	rec.RunID = runIDFromLabel(label)
	if _, err := s.git(ctx, "update-ref", "refs/cloud-harness/snapshots/"+rec.ID, treeOID); err != nil {
		return storage.WorkspaceSnapshot{}, err
	}
	created, err := s.store.CreateWorkspaceSnapshot(ctx, rec)
	if err != nil {
		_, _ = s.git(ctx, "update-ref", "-d", "refs/cloud-harness/snapshots/"+rec.ID)
		return storage.WorkspaceSnapshot{}, err
	}
	return created, nil
}

// List returns records for this workspace in chronological order.
func (s *Snapshotter) List(ctx context.Context) ([]storage.WorkspaceSnapshot, error) {
	if err := s.requireGit(); err != nil {
		return nil, err
	}
	return s.store.ListWorkspaceSnapshots(ctx, s.workspaceID)
}

// Restore replaces snapshot-tracked workspace paths with the target tree and
// deletes only paths present in persisted snapshots but absent from the
// target. Paths that have never appeared in a snapshot are preserved.
func (s *Snapshotter) Restore(ctx context.Context, target storage.WorkspaceSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureGit(ctx); err != nil {
		return err
	}
	if target.WorkspaceID != s.workspaceID {
		return fmt.Errorf("snapshot %q belongs to workspace %q, not %q", target.ID, target.WorkspaceID, s.workspaceID)
	}
	records, err := s.store.ListWorkspaceSnapshots(ctx, s.workspaceID)
	if err != nil {
		return err
	}
	targetEntries, err := s.treeEntries(ctx, target.TreeOID)
	if err != nil {
		return err
	}
	targetPaths := make(map[string]struct{}, len(targetEntries))
	for _, entry := range targetEntries {
		targetPaths[entry.path] = struct{}{}
	}
	tracked := make(map[string]struct{})
	treeOIDs := make(map[string]struct{})
	for _, rec := range records {
		treeOIDs[rec.TreeOID] = struct{}{}
	}
	refs, err := s.git(ctx, "for-each-ref", "--format=%(objectname)", "refs/cloud-harness/snapshots/")
	if err != nil {
		return err
	}
	for _, oid := range strings.Fields(string(refs)) {
		treeOIDs[oid] = struct{}{}
	}
	for treeOID := range treeOIDs {
		entries, err := s.treeEntries(ctx, treeOID)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			tracked[entry.path] = struct{}{}
		}
	}
	// Remove historical paths first. In particular, this allows a tracked file
	// to become a directory when the restored tree contains a nested path.
	trackedPaths := make([]string, 0, len(tracked))
	for path := range tracked {
		trackedPaths = append(trackedPaths, path)
	}
	sort.Slice(trackedPaths, func(i, j int) bool {
		return strings.Count(trackedPaths[i], "/") < strings.Count(trackedPaths[j], "/")
	})
	for _, path := range trackedPaths {
		if _, keep := targetPaths[path]; keep {
			continue
		}
		if err := s.removeWorkspacePath(path); err != nil {
			return fmt.Errorf("remove snapshot path %q: %w", path, err)
		}
	}
	for _, entry := range targetEntries {
		if err := s.restoreEntry(ctx, entry); err != nil {
			return fmt.Errorf("restore snapshot path %q: %w", entry.path, err)
		}
	}
	return nil
}

// Change is one path and its Git name-status code.
type Change struct {
	Status string
	Path   string
}

// DiffSummary contains path-level name-status changes and Git's textual stat.
type DiffSummary struct {
	Changes []Change
	Stat    string
}

// Diff compares the immutable trees using only the shadow repository.
func (s *Snapshotter) Diff(ctx context.Context, a, b storage.WorkspaceSnapshot) (DiffSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireGit(); err != nil {
		return DiffSummary{}, err
	}
	if a.WorkspaceID != s.workspaceID || b.WorkspaceID != s.workspaceID {
		return DiffSummary{}, errors.New("both snapshots must belong to this workspace")
	}
	raw, err := s.git(ctx, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--name-status", "-z", a.TreeOID, b.TreeOID)
	if err != nil {
		return DiffSummary{}, err
	}
	fields := bytes.Split(bytes.TrimSuffix(raw, []byte{0}), []byte{0})
	var changes []Change
	for i := 0; i < len(fields); {
		if len(fields[i]) == 0 {
			i++
			continue
		}
		if i+1 >= len(fields) {
			return DiffSummary{}, errors.New("git returned malformed name-status output")
		}
		changes = append(changes, Change{Status: string(fields[i]), Path: string(fields[i+1])})
		i += 2
	}
	stat, err := s.git(ctx, "diff", "--no-ext-diff", "--no-textconv", "--stat", a.TreeOID, b.TreeOID)
	if err != nil {
		return DiffSummary{}, err
	}
	return DiffSummary{Changes: changes, Stat: strings.TrimSpace(string(stat))}, nil
}

// DiffCurrent compares the live workspace with a persisted target without
// creating a snapshot record. It stages only in the isolated shadow index.
func (s *Snapshotter) DiffCurrent(ctx context.Context, target storage.WorkspaceSnapshot) (DiffSummary, error) {
	// Capture under the same lock as Snapshot/Restore, then let Diff acquire
	// that lock itself. The captured tree is immutable and needs no DB record.
	current, err := func() (storage.WorkspaceSnapshot, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.ensureGit(ctx); err != nil {
			return storage.WorkspaceSnapshot{}, err
		}
		if target.WorkspaceID != s.workspaceID {
			return storage.WorkspaceSnapshot{}, errors.New("snapshot belongs to a different workspace")
		}
		if _, err := s.git(ctx, "add", "-A", "--", "."); err != nil {
			return storage.WorkspaceSnapshot{}, err
		}
		if err := s.replaceFilteredBlobs(ctx); err != nil {
			return storage.WorkspaceSnapshot{}, err
		}
		out, err := s.git(ctx, "write-tree")
		if err != nil {
			return storage.WorkspaceSnapshot{}, err
		}
		oid := strings.TrimSpace(string(out))
		if oid == "" {
			return storage.WorkspaceSnapshot{}, errors.New("git write-tree returned an empty tree OID")
		}
		return storage.WorkspaceSnapshot{WorkspaceID: s.workspaceID, TreeOID: oid}, nil
	}()
	if err != nil {
		return DiffSummary{}, err
	}
	return s.Diff(ctx, current, target)
}

type treeEntry struct {
	mode     string
	typeName string
	oid      string
	size     int64
	path     string
}

func (s *Snapshotter) treeEntries(ctx context.Context, treeOID string) ([]treeEntry, error) {
	raw, err := s.git(ctx, "ls-tree", "-r", "-l", "-z", "--full-tree", treeOID)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	records := bytes.Split(bytes.TrimSuffix(raw, []byte{0}), []byte{0})
	entries := make([]treeEntry, 0, len(records))
	for _, record := range records {
		parts := bytes.SplitN(record, []byte{'\t'}, 2)
		if len(parts) != 2 {
			return nil, errors.New("git returned malformed tree entry")
		}
		meta := strings.Fields(string(parts[0]))
		if len(meta) != 4 {
			return nil, errors.New("git returned malformed long tree entry")
		}
		var size int64
		if meta[3] != "-" {
			size, err = strconv.ParseInt(meta[3], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("parse blob size for %q: %w", string(parts[1]), err)
			}
		}
		path := string(parts[1])
		if _, err := workspaceRelativePath(path); err != nil {
			return nil, fmt.Errorf("invalid path in snapshot tree: %w", err)
		}
		entries = append(entries, treeEntry{mode: meta[0], typeName: meta[1], oid: meta[2], size: size, path: path})
	}
	return entries, nil
}

// git add applies clean filters while building its index. Replacing those
// blobs with the original working-tree bytes keeps restore byte-exact while
// retaining git add's path selection and ignore behavior.
func (s *Snapshotter) replaceFilteredBlobs(ctx context.Context) error {
	raw, err := s.git(ctx, "ls-files", "--stage", "-z")
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	for _, record := range bytes.Split(bytes.TrimSuffix(raw, []byte{0}), []byte{0}) {
		parts := bytes.SplitN(record, []byte{'\t'}, 2)
		if len(parts) != 2 {
			return errors.New("git returned malformed index entry")
		}
		meta := strings.Fields(string(parts[0]))
		if len(meta) != 3 || meta[2] != "0" {
			return errors.New("git returned malformed or unmerged index entry")
		}
		if meta[0] == "160000" {
			continue
		}
		path := string(parts[1])
		full, err := s.safeWorkspacePath(path)
		if err != nil {
			return err
		}
		var content []byte
		if meta[0] == "120000" {
			link, err := os.Readlink(full)
			if err != nil {
				return fmt.Errorf("read symlink %q: %w", path, err)
			}
			content = []byte(link)
		} else {
			content, err = os.ReadFile(full)
			if err != nil {
				return fmt.Errorf("read snapshot file %q: %w", path, err)
			}
		}
		blob, err := s.gitInput(ctx, content, "hash-object", "-w", "--no-filters", "--stdin")
		if err != nil {
			return err
		}
		oid := strings.TrimSpace(string(blob))
		if oid == "" {
			return fmt.Errorf("git hash-object returned an empty object ID for %q", path)
		}
		if _, err := s.git(ctx, "update-index", "--add", "--cacheinfo", meta[0], oid, path); err != nil {
			return err
		}
	}
	return nil
}

func (s *Snapshotter) restoreEntry(ctx context.Context, entry treeEntry) error {
	if entry.typeName != "blob" {
		return fmt.Errorf("unsupported Git object type %q", entry.typeName)
	}
	full, err := s.makeParents(entry.path)
	if err != nil {
		return err
	}
	data, err := s.git(ctx, "cat-file", "blob", entry.oid)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(full); err == nil {
		if info.IsDir() {
			return errors.New("target path is a directory containing potentially untracked files")
		}
		if err := os.Remove(full); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if entry.mode == "120000" {
		return os.Symlink(string(data), full)
	}
	mode := os.FileMode(0644)
	if entry.mode == "100755" {
		mode = 0755
	} else if entry.mode != "100644" {
		return fmt.Errorf("unsupported Git file mode %q", entry.mode)
	}
	if err := os.WriteFile(full, data, mode); err != nil {
		return err
	}
	return os.Chmod(full, mode)
}

func (s *Snapshotter) removeWorkspacePath(name string) error {
	full, err := s.safeWorkspacePath(name)
	if err != nil {
		return err
	}
	if err := s.checkExistingParents(name); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
		info, statErr := os.Lstat(full)
		if statErr != nil || !info.IsDir() {
			return err
		}
		entries, readErr := os.ReadDir(full)
		if readErr != nil || len(entries) == 0 {
			return err
		}
	}
	s.removeEmptyParents(filepath.Dir(full))
	return nil
}

func (s *Snapshotter) removeEmptyParents(dir string) {
	for dir != s.workspace && strings.HasPrefix(dir, s.workspace+string(filepath.Separator)) {
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

func (s *Snapshotter) makeParents(name string) (string, error) {
	full, err := s.safeWorkspacePath(name)
	if err != nil {
		return "", err
	}
	rel, _ := filepath.Rel(s.workspace, filepath.Dir(full))
	current := s.workspace
	if rel == "." {
		return full, nil
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0755); err != nil && !errors.Is(err, os.ErrExist) {
				return "", err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return "", err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("parent path %q is not a real directory", part)
		}
	}
	return full, nil
}

func (s *Snapshotter) safeWorkspacePath(name string) (string, error) {
	rel, err := workspaceRelativePath(name)
	if err != nil {
		return "", err
	}
	full := filepath.Join(s.workspace, rel)
	check, err := filepath.Rel(s.workspace, full)
	if err != nil || check == ".." || strings.HasPrefix(check, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the workspace", name)
	}
	return full, nil
}

func workspaceRelativePath(name string) (string, error) {
	if name == "" || strings.IndexByte(name, 0) >= 0 || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("unsafe workspace path %q", name)
	}
	rel := filepath.Clean(filepath.FromSlash(name))
	if rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe workspace path %q", name)
	}
	return rel, nil
}

func workspaceIDFor(absPath string) string {
	sum := sha256.Sum256([]byte(absPath))
	return hex.EncodeToString(sum[:])[:16]
}

func runIDFromLabel(label string) string {
	label = strings.TrimPrefix(label, "rewind-safety/")
	if !strings.HasPrefix(label, "run_") {
		return ""
	}
	if index := strings.IndexByte(label, '/'); index > 0 {
		return label[:index]
	}
	return ""
}

func (s *Snapshotter) checkExistingParents(name string) error {
	full, err := s.safeWorkspacePath(name)
	if err != nil {
		return err
	}
	rel, _ := filepath.Rel(s.workspace, filepath.Dir(full))
	current := s.workspace
	if rel == "." {
		return nil
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("parent path %q is not a real directory", part)
		}
	}
	return nil
}

func (s *Snapshotter) requireGit() error {
	if s.gitErr != nil {
		return &UnavailableError{Reason: s.gitErr.Error()}
	}
	return nil
}

func (s *Snapshotter) ensureGit(ctx context.Context) error {
	if err := s.requireGit(); err != nil {
		return err
	}
	if err := os.MkdirAll(s.shadowDir, 0755); err != nil {
		return fmt.Errorf("create shadow Git directory: %w", err)
	}
	if _, err := os.Stat(filepath.Join(s.shadowDir, "HEAD")); errors.Is(err, os.ErrNotExist) {
		if _, err := s.git(ctx, "init", "--quiet"); err != nil {
			return err
		} else if err := s.writeExclude(); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if err := s.writeExclude(); err != nil {
		return err
	}
	return nil
}

func (s *Snapshotter) writeExclude() error {
	path := filepath.Join(s.shadowDir, "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == ".cloud-harness/" {
			return nil
		}
	}
	var b strings.Builder
	b.Write(data)
	if len(data) > 0 && data[len(data)-1] != '\n' {
		b.WriteByte('\n')
	}
	b.WriteString(".cloud-harness/\n")
	return os.WriteFile(path, []byte(b.String()), 0644)
}

func (s *Snapshotter) git(ctx context.Context, args ...string) ([]byte, error) {
	return s.gitInput(ctx, nil, args...)
}

func (s *Snapshotter) gitInput(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	if err := s.requireGit(); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, s.gitBin, args...)
	cmd.Dir = s.workspace
	cmd.Env = gitEnvironment(s.shadowDir, s.workspace)
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, &UnavailableError{Reason: err.Error()}
		}
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func gitEnvironment(shadowDir, workspace string) []string {
	env := make([]string, 0, len(os.Environ())+5)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(strings.ToUpper(key), "GIT_") {
			continue
		}
		env = append(env, entry)
	}
	return append(env,
		"GIT_DIR="+shadowDir,
		"GIT_WORK_TREE="+workspace,
		"GIT_INDEX_FILE="+filepath.Join(shadowDir, "index"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
	)
}
