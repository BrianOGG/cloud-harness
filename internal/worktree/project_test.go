package worktree

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMemoryProjectPathCanonicalAndIsolated(t *testing.T) {
	root := testRepo(t)
	linked := filepath.Join(t.TempDir(), "linked repo")
	testGit(t, root, "worktree", "add", "--detach", linked)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(linked, alias); err != nil {
		t.Skip(err)
	}
	foreign := testRepo(t)
	unregistered := t.TempDir()
	write(t, unregistered, ".git", "gitdir: "+filepath.Join(root, ".git")+"\n")
	if err := os.Mkdir(filepath.Join(linked, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, want string }{
		{root, root}, {linked, root}, {alias, root}, {foreign, foreign}, {unregistered, unregistered},
		{filepath.Join(linked, "sub"), filepath.Join(root, "sub")},
	} {
		got, err := ProjectPath(t.Context(), tc.path)
		if err != nil || got != tc.want {
			t.Errorf("%q maps to %q, want %q: %v", tc.path, got, tc.want, err)
		}
	}
}
