package worktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// ProjectPath maps a registered linked worktree to the main workspace. A
// separately configured subdirectory stays scoped to that same subdirectory.
func ProjectPath(ctx context.Context, workspace string) (string, error) {
	root, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	for dir := root; ; dir = filepath.Dir(dir) {
		marker := filepath.Join(dir, ".git")
		info, statErr := os.Lstat(marker)
		if statErr == nil {
			if info.Mode().IsRegular() {
				if main := linkedProject(ctx, dir); main != "" {
					suffix, err := filepath.Rel(dir, root)
					if err != nil {
						return "", err
					}
					return filepath.Join(main, suffix), nil
				}
			}
			break
		}
		if !os.IsNotExist(statErr) {
			return "", statErr
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	return root, nil
}

func linkedProject(ctx context.Context, dir string) string {
	raw, err := git(ctx, dir, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return ""
	}
	common, err := filepath.EvalSymlinks(strings.TrimSuffix(string(raw), "\n"))
	if err != nil {
		return ""
	}
	// A .git file can point anywhere. Require Git's reverse registration in
	// the actual common directory before sharing that repository's memories.
	registered, err := git(ctx, dir, nil, "--git-dir="+common, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return ""
	}
	for _, field := range strings.Split(string(registered), "\x00") {
		if path, ok := strings.CutPrefix(field, "worktree "); ok && path == dir {
			if filepath.Base(common) == ".git" {
				return filepath.Dir(common)
			}
			return common
		}
	}
	return ""
}
