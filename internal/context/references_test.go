package context

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpandFileReferencesRangesLimitsAndErrors(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("first\nsecond\nthird\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, refs, err := ExpandReferences("inspect @file:note.txt#L2-L3", root, nil, nil)
	if err != nil || len(refs) != 1 || !strings.Contains(got, "note.txt:2 second") || !strings.Contains(got, "note.txt:3 third") || strings.Contains(got, "note.txt:1 first") {
		t.Fatalf("range expansion = %q, refs=%+v, err=%v", got, refs, err)
	}
	got, refs, err = ExpandReferences("inspect @file:note.txt", root, nil, nil)
	if err != nil || len(refs) != 1 || !strings.Contains(got, "note.txt:1 first") || !strings.Contains(got, "note.txt:3 third") {
		t.Fatalf("whole-file expansion = %q, refs=%+v, err=%v", got, refs, err)
	}

	large := strings.Repeat("x", MaxReferenceFileBytes*2)
	if err := os.WriteFile(filepath.Join(root, "large.txt"), []byte(large), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _, err = ExpandReferences("@file:large.txt", root, nil, nil)
	if err != nil || !strings.Contains(got, "[truncated at 8KiB]") || !strings.Contains(got, "```") {
		t.Fatalf("large file was not safely truncated: bytes=%d err=%v", len(got), err)
	}

	_, _, err = ExpandReferences("@file:../outside", root, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "outside the workspace") {
		t.Fatalf("outside path error = %v", err)
	}
	_, _, err = ExpandReferences("@file:missing.txt", root, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing file error = %v", err)
	}

	var b strings.Builder
	for i := 0; i < 11; i++ {
		b.WriteString(" @file:note.txt")
	}
	got, refs, err = ExpandReferences(b.String(), root, nil, nil)
	if err != nil || len(refs) != 10 || !strings.Contains(got, "only the first 10") {
		t.Fatalf("file reference limit: refs=%d text=%q err=%v", len(refs), got, err)
	}
}

func TestExpandDiffReferencesAndLimits(t *testing.T) {
	root := t.TempDir()
	calls := 0
	got, refs, err := ExpandReferences("inspect @diff --staged", root, nil, func(staged bool) (string, error) {
		calls++
		if !staged {
			t.Fatal("--staged was not passed to the reader")
		}
		return "diff --git a/note b/note\n+changed\n", nil
	})
	if err != nil || calls != 1 || len(refs) != 1 || refs[0].Range != "staged" || !strings.Contains(got, "+changed") {
		t.Fatalf("diff expansion = %q, refs=%+v, calls=%d, err=%v", got, refs, calls, err)
	}
	got, refs, err = ExpandReferences("inspect @diff", root, nil, func(bool) (string, error) {
		return strings.Repeat("d", MaxReferenceDiffBytes*2), nil
	})
	if err != nil || len(refs) != 1 || !strings.Contains(got, "[truncated at 16KiB]") {
		t.Fatalf("large diff was not truncated: bytes=%d refs=%+v err=%v", len(got), refs, err)
	}
	got, refs, err = ExpandReferences("inspect @diff", root, nil, func(bool) (string, error) { return "", nil })
	if err != nil || len(refs) != 1 || !strings.Contains(got, "no unstaged changes") {
		t.Fatalf("empty diff diagnostic = %q refs=%+v err=%v", got, refs, err)
	}
	got, refs, err = ExpandReferences("inspect @diff", root, nil, func(bool) (string, error) { return "", os.ErrNotExist })
	if err != nil || len(refs) != 1 || !strings.Contains(got, "diff unavailable") {
		t.Fatalf("unavailable diff diagnostic = %q refs=%+v err=%v", got, refs, err)
	}
}

func TestNormalizeCompactionSummaryIsBoundedStructuredJSON(t *testing.T) {
	got := NormalizeCompactionSummary(`{"confirmed_facts":["port 8080"],"pending":["test"]}`)
	if !strings.Contains(got, "port 8080") || !strings.Contains(got, `"key_files_and_decisions":[]`) {
		t.Fatalf("normalized summary = %s", got)
	}
	got = NormalizeCompactionSummary(strings.Repeat("重要事实", MaxCompactionSummaryBytes))
	if len(got) > MaxCompactionSummaryBytes || !strings.Contains(got, "truncated at 4KiB") || !json.Valid([]byte(got)) {
		t.Fatalf("bounded summary invalid: bytes=%d valid=%v", len(got), json.Valid([]byte(got)))
	}
}
