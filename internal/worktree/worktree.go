// Package worktree creates persistent, isolated Git workspaces for runs.
// Retained workspaces can be reviewed and integrated through lifecycle operations.
package worktree

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

const MaxDiffBytes = 64 << 10

var runIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// Info binds a run to its original dispatch commit and retained worktree.
type Info struct {
	RunID   string `json:"run_id"`
	BaseSHA string `json:"base_sha"`
	Branch  string `json:"branch"`
	Path    string `json:"worktree_path"`
}

// Summary includes committed and pending workspace edits relative to the
// branch's merge base, as with base...HEAD followed by the pending edits.
type Summary struct {
	Info
	Files     []string `json:"files"`
	Stat      string   `json:"stat"`
	Diff      string   `json:"diff"`
	Truncated bool     `json:"truncated"`
	Error     string   `json:"error,omitempty"`
}

// Validate checks prerequisites without creating directories or changing Git.
// A linked worktree cannot be used as the parent of another worktree run.
func Validate(ctx context.Context, workspace string) error {
	root, err := filepath.Abs(workspace)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		return fmt.Errorf("--worktree requires a Git repository with .git at workspace root %q: %w", root, err)
	}
	gitDir, err := git(ctx, root, nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	commonDir, err := git(ctx, root, nil, "rev-parse", "--git-common-dir")
	if err != nil {
		return err
	}
	common := strings.TrimSpace(string(commonDir))
	if !filepath.IsAbs(common) {
		common = filepath.Join(root, common)
	}
	if filepath.Clean(common) != filepath.Clean(strings.TrimSpace(string(gitDir))) {
		return errors.New("--worktree cannot create a nested worktree: workspace is already a linked Git worktree")
	}
	_, err = git(ctx, root, nil, "rev-parse", "--verify", "HEAD^{commit}")
	return err
}

// Create captures HEAD at dispatch and checks out a new branch. Metadata is
// kept beside (not inside) the worktree so Lookup retains the original base
// even after commits, resets, or a process restart. Nothing is auto-deleted.
func Create(ctx context.Context, workspace, runID string) (Info, error) {
	if !runIDPattern.MatchString(runID) {
		return Info{}, fmt.Errorf("invalid worktree run id %q", runID)
	}
	root, err := filepath.Abs(workspace)
	if err != nil {
		return Info{}, err
	}
	if err := Validate(ctx, root); err != nil {
		return Info{}, err
	}
	base, err := git(ctx, root, nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return Info{}, err
	}
	info := Info{RunID: runID, BaseSHA: strings.TrimSpace(string(base)), Branch: "cloud-harness/" + runID,
		Path: filepath.Join(root, ".cloud-harness", "worktrees", runID)}
	if err := ensureExclude(ctx, root); err != nil {
		return Info{}, err
	}
	if err := os.MkdirAll(filepath.Dir(info.Path), 0755); err != nil {
		return Info{}, err
	}
	if _, err := git(ctx, root, nil, "worktree", "add", info.Path, "-b", info.Branch, info.BaseSHA); err != nil {
		return Info{}, err
	}
	data, err := json.Marshal(info)
	if err != nil {
		return Info{}, err
	}
	if err := os.WriteFile(info.Path+".json", data, 0644); err != nil {
		return Info{}, fmt.Errorf("persist worktree metadata (worktree retained at %s): %w", info.Path, err)
	}
	return info, nil
}

// Lookup reports whether a retained worktree exists and returns its current
// branch and original base SHA. It does not initialize a repository or store.
func Lookup(ctx context.Context, workspace, runID string) (Info, bool, error) {
	if !runIDPattern.MatchString(runID) {
		return Info{}, false, fmt.Errorf("invalid worktree run id %q", runID)
	}
	root, err := filepath.Abs(workspace)
	if err != nil {
		return Info{}, false, err
	}
	path := filepath.Join(root, ".cloud-harness", "worktrees", runID)
	if _, err := os.Stat(filepath.Join(path, ".git")); errors.Is(err, os.ErrNotExist) {
		return Info{}, false, nil
	} else if err != nil {
		return Info{}, false, err
	}
	data, err := os.ReadFile(path + ".json")
	if err != nil {
		return Info{}, false, err
	}
	var info Info
	if err := json.Unmarshal(data, &info); err != nil {
		return Info{}, false, fmt.Errorf("read worktree metadata: %w", err)
	}
	if info.RunID != runID || info.Path != path || info.BaseSHA == "" {
		return Info{}, false, errors.New("worktree metadata does not match requested run")
	}
	branch, err := git(ctx, path, nil, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return Info{}, false, err
	}
	info.Branch = strings.TrimSpace(string(branch))
	return info, true, nil
}

// Summarize leaves the branch, real index and working files untouched. A
// private index captures pending tracked and untracked files; the synthetic
// tree makes ordinary uncommitted file-tool edits visible in the final diff.
// With a clean worktree the result is equivalent to git diff base...HEAD.
func Summarize(ctx context.Context, info Info) (Summary, error) {
	summary := Summary{Info: info, Files: []string{}}
	base, err := git(ctx, info.Path, nil, "merge-base", info.BaseSHA, "HEAD")
	if err != nil {
		return summary, err
	}
	temp, err := os.MkdirTemp("", "cloud-harness-worktree-index-*")
	if err != nil {
		return summary, err
	}
	defer os.RemoveAll(temp)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(temp, "index")}
	if _, err := git(ctx, info.Path, env, "read-tree", "HEAD"); err != nil {
		return summary, err
	}
	// The shared info/exclude installed by Create hides harness state. Git
	// add treats an explicit excluded-but-ignored directory as an error, so
	// only diff's pathspecs additionally exclude it below.
	if _, err := git(ctx, info.Path, env, "add", "-A", "--", "."); err != nil {
		return summary, err
	}
	tree, err := git(ctx, info.Path, env, "write-tree")
	if err != nil {
		return summary, err
	}
	refs := []string{strings.TrimSpace(string(base)), strings.TrimSpace(string(tree)), "--", ".", ":(exclude).cloud-harness"}
	files, err := git(ctx, info.Path, nil, append([]string{"diff", "--no-ext-diff", "--no-textconv", "--name-only", "-z"}, refs...)...)
	if err != nil {
		return summary, err
	}
	if len(files) > 0 {
		summary.Files = strings.Split(strings.TrimSuffix(string(files), "\x00"), "\x00")
	}
	stat, err := git(ctx, info.Path, nil, append([]string{"diff", "--no-ext-diff", "--no-textconv", "--stat=80", "--no-color"}, refs...)...)
	if err != nil {
		return summary, err
	}
	summary.Stat = string(stat)
	patch := &cappedBuffer{limit: MaxDiffBytes}
	err = gitOutput(ctx, info.Path, nil, patch, append([]string{"diff", "--no-ext-diff", "--no-textconv", "--no-color", "--patch"}, refs...)...)
	summary.Diff = strings.ToValidUTF8(patch.String(), "�")
	summary.Truncated = patch.truncated || len(summary.Diff) > MaxDiffBytes
	for len(summary.Diff) > MaxDiffBytes || !utf8.ValidString(summary.Diff) {
		_, n := utf8.DecodeLastRuneInString(summary.Diff)
		summary.Diff = summary.Diff[:len(summary.Diff)-n]
	}
	return summary, err
}

func ensureExclude(ctx context.Context, root string) error {
	raw, err := git(ctx, root, nil, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return err
	}
	path := strings.TrimSpace(string(raw))
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
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
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, []byte(".cloud-harness/\n")...), 0644)
}

func git(ctx context.Context, cwd string, env []string, args ...string) ([]byte, error) {
	var out bytes.Buffer
	err := gitOutput(ctx, cwd, env, &out, args...)
	return out.Bytes(), err
}

func gitOutput(ctx context.Context, cwd string, env []string, out io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = cwd
	// Inherited Git routing must not redirect worktree operations into a
	// caller's index, shadow repository, or another working directory.
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, env...)
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = out, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s (cwd %q): %w; stderr: %s", strings.Join(args, " "), cwd, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

type cappedBuffer struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.buffer.Len()
	if len(p) > remaining {
		b.truncated = true
		p = p[:remaining]
	}
	_, _ = b.buffer.Write(p)
	return n, nil
}

func (b *cappedBuffer) String() string { return b.buffer.String() }
