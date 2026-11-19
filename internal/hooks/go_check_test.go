package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoCheckDetectsRepeatedDirtyEditsWithoutMtimeChange(t *testing.T) {
	engine := engineFor(t)
	workspace := engine.Config.Workspace
	putFile(t, filepath.Join(workspace, "go.mod"), "module demo\n\ngo 1.20\n")
	file := filepath.Join(workspace, "demo.go")
	putFile(t, file, "package demo\nfunc F() { missingA() }\n")
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	before, err := engine.SnapshotGo(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	putFile(t, file, "package demo\nfunc F() { missingB() }\n")
	if err := os.Chtimes(file, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	result, err := engine.CheckGo(t.Context(), Input{}, before, nil, nil)
	if err != nil || !strings.Contains(result.Message, "missingB") || !strings.Contains(result.Message, "go vet") {
		t.Fatalf("dirty edit missed: %+v %v", result, err)
	}
	unchanged, err := engine.SnapshotGo(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	result, err = engine.CheckGo(t.Context(), Input{}, unchanged, nil, nil)
	if err != nil || result.Message != "" {
		t.Fatalf("unchanged dirty file triggered check: %+v %v", result, err)
	}
}

func TestGoCheckDoesNotFollowSymlinksOutsideWorkspace(t *testing.T) {
	engine := engineFor(t)
	outside := filepath.Join(t.TempDir(), "private.go")
	putFile(t, outside, "credential bytes")
	if err := os.Symlink(outside, filepath.Join(engine.Config.Workspace, "linked.go")); err != nil {
		t.Fatal(err)
	}
	snapshot, err := engine.SnapshotGo(t.Context())
	if err != nil || len(snapshot) != 0 {
		t.Fatalf("Go scan followed external symlink: %+v %v", snapshot, err)
	}
}
