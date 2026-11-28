package worktree

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
)

// CheckRestoreIndex also supports a plain (non-Git) workspace. Preserve staged
// edits to the reviewed file and avoid changing a checkout with merge conflicts.
func CheckRestoreIndex(ctx context.Context, workspace, path string) error {
	_, err := git(ctx, workspace, nil, "rev-parse", "--git-dir")
	if err != nil {
		// Missing Git metadata is normal for a plain workspace. Fail closed
		// if a checkout exists but its index or repository is unreadable.
		for dir := workspace; ; dir = filepath.Dir(dir) {
			if _, statErr := os.Lstat(filepath.Join(dir, ".git")); !errors.Is(statErr, os.ErrNotExist) {
				return err
			}
			if dir == filepath.Dir(dir) {
				break
			}
		}
		return nil
	}
	unmerged, err := git(ctx, workspace, nil, "ls-files", "--unmerged", "-z")
	if err != nil {
		return err
	}
	if len(unmerged) != 0 || merging(ctx, workspace) {
		return errors.New("工作区存在未解决的合并")
	}
	out, err := git(ctx, workspace, nil, "--literal-pathspecs", "diff", "--no-ext-diff", "--no-textconv", "--cached", "--name-only", "-z", "--", path)
	if err != nil {
		return err
	}
	if len(out) != 0 {
		return errors.New("此文件有已暂存更改")
	}
	return nil
}

// CheckRetained verifies both the persisted binding and the physical checkout.
// A replaced/symlinked directory or another repository with the same branch
// name cannot stand in for a retained worktree of this workspace.
func CheckRetained(ctx context.Context, workspace string, info Info) error {
	if err := validateBinding(ctx, workspace, info); err != nil {
		return err
	}
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	actual, err := filepath.EvalSymlinks(info.Path)
	if err != nil || actual != filepath.Join(root, ".cloud-harness", "worktrees", info.RunID) {
		return errors.New("worktree path changed")
	}
	mainGit, err := git(ctx, root, nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	common, err := git(ctx, actual, nil, "rev-parse", "--git-common-dir")
	if err != nil {
		return err
	}
	commonPath := string(bytes.TrimSpace(common))
	if !filepath.IsAbs(commonPath) {
		commonPath = filepath.Join(actual, commonPath)
	}
	if filepath.Clean(commonPath) != string(bytes.TrimSpace(mainGit)) {
		return errors.New("worktree repository changed")
	}
	return nil
}
