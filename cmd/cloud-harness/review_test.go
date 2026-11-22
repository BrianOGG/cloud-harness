package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestReviewReportsChangesReadOnly(t *testing.T) {
	root := newReviewRepo(t)
	const (
		stagedSecret = "sk-ReviewFixtureOnly000000000001"
		surrogate    = "hsurr:review-fixture-only"
		bearer       = "reviewBearerFixtureOnly"
		assigned     = "reviewAssignmentFixtureOnly"
		configured   = "reviewConfiguredFixtureOnly"
	)
	t.Setenv("OPENAI_API_KEY", configured)
	modified := "src/file with spaces.txt"
	writeReviewFile(t, root, modified, "stage base\nworktree base\nunchanged\n")
	writeReviewFile(t, root, "removed.txt", strings.Repeat("remove this line\n", 501))
	writeReviewFile(t, root, "image.dat", "\x00initial binary")
	writeReviewFile(t, root, "unchanged.txt", "same contents\n")
	reviewTestGit(t, root, "add", "--", ".")
	reviewTestGit(t, root, "commit", "-qm", "baseline")

	writeReviewFile(t, root, modified, "stage edit\nworktree base\nunchanged\n")
	writeReviewFile(t, root, "staged.env", "ordinary setting\napi_key="+stagedSecret+"\n")
	const bigSize = 1_000_001
	writeReviewFile(t, root, "image.dat", "\x00"+strings.Repeat("x", bigSize-1))
	reviewTestGit(t, root, "add", "--", modified, "staged.env", "image.dat")

	worktree := map[string]string{
		modified:        "stage edit\nworktree edit\nunchanged\n",
		"staged.env":    "ordinary setting\nordinary replacement\n",
		"image.dat":     "\x00small binary worktree version",
		"loose note":    "untracked patch body\nsecond line\n",
		"untracked.env": "plain line\n" + surrogate + "\nBearer " + bearer + "\napi_key=\"" + assigned + "\"\n" + configured,
	}
	for path, text := range worktree {
		writeReviewFile(t, root, path, text)
	}
	if err := os.Remove(filepath.Join(root, "removed.txt")); err != nil {
		t.Fatal(err)
	}
	// Git can refresh the index if a cached stat changed without changing contents.
	oldTime := time.Unix(1_000_000_000, 0)
	if err := os.Chtimes(filepath.Join(root, "unchanged.txt"), oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	beforeStatus := reviewTestGit(t, root, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	indexPath := filepath.Join(root, ".git", "index")
	indexBefore := readReviewTestFile(t, indexPath)
	indexTime := reviewTestMtime(t, indexPath)

	for _, mode := range []string{"markdown", "json", "markdown diff", "json diff"} {
		t.Run(mode, func(t *testing.T) {
			var flags []string
			asJSON, includeDiff := strings.Contains(mode, "json"), strings.Contains(mode, "diff")
			if asJSON {
				flags = append(flags, "--json")
			}
			if includeDiff {
				flags = append(flags, "--diff")
			}
			// Passing a directory inside the worktree should still report all paths.
			code, out, stderr := invokeReview(filepath.Join(root, "src"), flags...)
			if code != 0 || stderr != "" {
				t.Fatalf("review exit=%d stderr=%s", code, stderr)
			}
			for _, secret := range []string{stagedSecret, surrogate, bearer, assigned, configured} {
				if strings.Contains(out+stderr, secret) {
					t.Fatal("review printed a fixture credential")
				}
			}
			if strings.Contains(out, "untracked patch body") != includeDiff || strings.Contains(out, "[redacted]") != includeDiff {
				t.Fatalf("patch presence did not match --diff: %q", out)
			}
			if asJSON {
				var report reviewReport
				if err := json.Unmarshal([]byte(out), &report); err != nil {
					t.Fatal(err)
				}
				if report.Clean || report.FileCount != 6 || report.Staged != 3 || report.Unstaged != 4 || report.Untracked != 2 || report.Additions != 12 || report.Deletions != 504 {
					t.Fatalf("incorrect totals: %+v", report)
				}
				wantFiles := []reviewFile{
					{Path: "image.dat", Status: "M", StagedStatus: "M", UnstagedStatus: "M", Binary: true},
					{Path: "loose note", Status: "??", Untracked: true, Additions: 2},
					{Path: "removed.txt", Status: "D", UnstagedStatus: "D", Deletions: 501},
					{Path: modified, Status: "M", StagedStatus: "M", UnstagedStatus: "M", Additions: 2, Deletions: 2},
					{Path: "staged.env", Status: "A", StagedStatus: "A", UnstagedStatus: "M", Additions: 3, Deletions: 1},
					{Path: "untracked.env", Status: "??", Untracked: true, Additions: 5},
				}
				if !slices.Equal(report.Files, wantFiles) {
					t.Fatalf("files = %+v; want %+v", report.Files, wantFiles)
				}
				wantRisks := []reviewRisk{
					{Kind: "large_file", Path: "image.dat", Bytes: bigSize},
					{Kind: "binary", Path: "image.dat"},
					{Kind: "large_deletion", Path: "removed.txt", Deletions: 501},
					{Kind: "suspected_secret", Path: "staged.env", Source: "staged", Line: 2},
					{Kind: "suspected_secret", Path: "untracked.env", Source: "untracked", Line: 2},
					{Kind: "suspected_secret", Path: "untracked.env", Source: "untracked", Line: 3},
					{Kind: "suspected_secret", Path: "untracked.env", Source: "untracked", Line: 4},
					{Kind: "suspected_secret", Path: "untracked.env", Source: "untracked", Line: 5},
				}
				if !slices.Equal(report.Risks, wantRisks) {
					t.Fatalf("risks = %+v; want %+v", report.Risks, wantRisks)
				}
				if (report.Diff != nil) != includeDiff {
					t.Fatalf("diff field presence did not match --diff: %v", report.Diff)
				}
				if includeDiff {
					assertReviewContains(t, *report.Diff, "# 已暂存", "# 未暂存", "# 未跟踪", "+stage edit", "+worktree edit", "-remove this line", "+untracked patch body", "Binary files")
				}
			} else {
				assertReviewContains(t, out, "## 风险提示", "疑似密钥 <code>staged.env</code>（已暂存，第 2 行）",
					"超大文件", "二进制文件改动", "删除行数异常", "文件总数 6（已暂存 3，未暂存 4，未跟踪 2），总新增 12 行，总删除 504 行。",
					"| <code>"+modified+"</code> | M | M | M |  | 2 | 2 |", "| <code>staged.env</code> | A |", "| <code>removed.txt</code> | D |", "| <code>loose note</code> | ?? |")
				if strings.Index(out, "## 风险提示") > strings.Index(out, "文件总数") {
					t.Fatalf("risks appeared below the summary: %q", out)
				}
				if strings.Contains(out, "## Diff") != includeDiff {
					t.Fatalf("Markdown diff heading presence did not match --diff: %q", out)
				}
			}
		})
	}
	if after := reviewTestGit(t, root, "status", "--porcelain=v1", "-z", "--untracked-files=all"); after != beforeStatus {
		t.Fatalf("git status changed: before=%q after=%q", beforeStatus, after)
	}
	if !bytes.Equal(indexBefore, readReviewTestFile(t, indexPath)) || !indexTime.Equal(reviewTestMtime(t, indexPath)) {
		t.Fatal("review changed the index")
	}
	for path, want := range worktree {
		if got := string(readReviewTestFile(t, filepath.Join(root, path))); got != want {
			t.Errorf("review changed worktree file %q", path)
		}
	}
}

func TestReviewCleanAndErrors(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	root := newReviewRepo(t)
	t.Chdir(root)
	var stdout, stderr bytes.Buffer
	if code := Main([]string{"review"}, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("clean review exit=%d stderr=%s", code, &stderr)
	}
	assertReviewContains(t, stdout.String(), "工作区干净")
	code, out, errOut := invokeReview(root, "--json", "--diff")
	var report reviewReport
	if err := json.Unmarshal([]byte(out), &report); err != nil || code != 0 || errOut != "" {
		t.Fatalf("clean JSON review exit=%d stdout=%q stderr=%q err=%v", code, out, errOut, err)
	}
	if !report.Clean || report.FileCount != 0 || report.Message != "工作区干净" || report.Files == nil || report.Risks == nil || len(report.Files) != 0 || len(report.Risks) != 0 || report.Diff == nil || *report.Diff != "" {
		t.Fatalf("incorrect clean report: %+v", report)
	}
	code, out, errOut = invokeReview(t.TempDir(), "--json")
	if code != 1 || out != "" || !strings.Contains(errOut, "not a git repository") {
		t.Fatalf("non-repo review exit=%d stdout=%q stderr=%q", code, out, errOut)
	}
	t.Setenv("PATH", t.TempDir())
	code, out, errOut = invokeReview(root)
	if code != 1 || out != "" || !strings.Contains(errOut, "git is unavailable") {
		t.Fatalf("missing git review exit=%d stdout=%q stderr=%q", code, out, errOut)
	}
}

func TestReviewSkipsGitHelpers(t *testing.T) {
	root := newReviewRepo(t)
	// Other programs on the machine (editor integrations, repository watchers)
	// can run Git in the fixture and start its helpers too. A helper records a
	// run only in a directory named by a variable that review's Git inherits.
	markers := t.TempDir()
	mark := func(name string) string {
		return `echo ran > "${REVIEW_TEST_MARKERS:?}/` + name + `"`
	}
	writeReviewFile(t, root, ".gitattributes", "*.txt filter=review diff=review\n")
	writeReviewFile(t, root, "tracked.txt", "initial\n")
	reviewTestGit(t, root, "add", "--", ".")
	reviewTestGit(t, root, "commit", "-qm", "baseline")
	reviewTestGit(t, root, "config", "filter.review.clean", `tee "${REVIEW_TEST_MARKERS:?}/ran-clean"`)
	reviewTestGit(t, root, "config", "filter.review.process", mark("ran-process"))
	reviewTestGit(t, root, "config", "filter.review.required", "true")
	reviewTestGit(t, root, "config", "diff.review.textconv", mark("ran-textconv"))
	reviewTestGit(t, root, "config", "diff.external", mark("ran-ext-diff"))
	reviewTestGit(t, root, "config", "core.autocrlf", "true")
	writeReviewFile(t, root, "fsmonitor.sh", "#!/bin/sh\n"+mark("ran-fsmonitor")+"\n")
	if err := os.Chmod(filepath.Join(root, "fsmonitor.sh"), 0755); err != nil {
		t.Fatal(err)
	}
	reviewTestGit(t, root, "config", "core.fsmonitor", "./fsmonitor.sh")
	writeReviewFile(t, root, "tracked.txt", "worktree edit\n")
	writeReviewFile(t, root, "untracked.txt", "untracked edit\n")
	t.Setenv("REVIEW_TEST_MARKERS", markers)
	t.Setenv("GIT_TRACE", filepath.Join(markers, "git-trace"))

	code, out, stderr := invokeReview(root, "--diff")
	if code != 0 || stderr != "" {
		t.Fatalf("review exit=%d stderr=%q", code, stderr)
	}
	assertReviewContains(t, out, "+worktree edit", "+untracked edit")
	if entries, err := os.ReadDir(markers); err != nil || len(entries) != 0 {
		t.Errorf("review ran Git helpers: %v (err=%v)", entries, err)
	}

	// Without review's overrides the same Git does start them, so the empty
	// directory above is review's doing.
	git, err := openReviewGit(root)
	if err != nil {
		t.Fatal(err)
	}
	git.options = nil
	for _, args := range [][]string{{"status"}, {"diff"}} {
		_ = git.command(args...).Run() // diff fails on the broken required filter
	}
	for _, name := range []string{"ran-fsmonitor", "ran-process"} {
		if _, err := os.Stat(filepath.Join(markers, name)); err != nil {
			t.Errorf("Git without review's overrides did not write %q: %v", name, err)
		}
	}
}

func TestReviewDiffsUntrackedSymlinksWithoutFollowingThem(t *testing.T) {
	root, outside := newReviewRepo(t), t.TempDir()
	writeReviewFile(t, outside, "null", "outside contents\n")
	writeReviewFile(t, root, "tracked.txt", "baseline\n")
	reviewTestGit(t, root, "add", "--", "tracked.txt")
	reviewTestGit(t, root, "commit", "-qm", "baseline")
	links := []struct {
		path   string
		target string
	}{
		{"directory link", outside},
		{"file link", filepath.Join(outside, "null")},
		{"dangling link", filepath.Join(outside, "missing")},
	}
	for _, link := range links {
		if err := os.Symlink(link.target, filepath.Join(root, link.path)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	beforeStatus := reviewTestGit(t, root, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	indexPath := filepath.Join(root, ".git", "index")
	indexBefore := readReviewTestFile(t, indexPath)
	indexTime := reviewTestMtime(t, indexPath)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"native git", "reject no-index"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "reject no-index" {
				// Exercise the failure even on older Git versions that accept
				// stdin/directory-link comparisons. This fixture has only
				// untracked symlinks, which must not need --no-index at all.
				bin := t.TempDir()
				writeReviewFile(t, bin, "git", "#!/bin/sh\nfor arg do\n  if [ \"$arg\" = --no-index ]; then\n    printf '%s\\n' 'fatal: cannot compare stdin to a directory' >&2\n    exit 128\n  fi\ndone\nexec \"$REVIEW_TEST_REAL_GIT\" \"$@\"\n")
				if err := os.Chmod(filepath.Join(bin, "git"), 0755); err != nil {
					t.Fatal(err)
				}
				t.Setenv("REVIEW_TEST_REAL_GIT", realGit)
				t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
			for _, includeDiff := range []bool{false, true} {
				flags := []string{"--json"}
				if includeDiff {
					flags = append(flags, "--diff")
				}
				code, out, stderr := invokeReview(root, flags...)
				if code != 0 || stderr != "" {
					t.Fatalf("review diff=%v exit=%d stderr=%q", includeDiff, code, stderr)
				}
				var report reviewReport
				if err := json.Unmarshal([]byte(out), &report); err != nil {
					t.Fatal(err)
				}
				if report.Clean || report.FileCount != len(links) || report.Untracked != len(links) || report.Staged != 0 || report.Unstaged != 0 || report.Additions != int64(len(links)) || report.Deletions != 0 || len(report.Files) != len(links) || len(report.Risks) != 0 || (report.Diff != nil) != includeDiff {
					t.Fatalf("incorrect symlink report: %+v", report)
				}
				for _, file := range report.Files {
					if file.Status != "??" || !file.Untracked || file.Additions != 1 || file.Deletions != 0 || file.Binary {
						t.Errorf("incorrect symlink file: %+v", file)
					}
				}
				if !includeDiff {
					continue
				}
				for _, link := range links {
					assertReviewContains(t, *report.Diff, "diff --git a/"+link.path+" b/"+link.path+"\nnew file mode 120000\n", "\n+"+link.target+"\n\\ No newline at end of file")
				}
				if strings.Count(*report.Diff, "diff --git ") != len(links) || strings.Contains(*report.Diff, "outside contents") || strings.Contains(*report.Diff, "deleted file mode") {
					t.Fatalf("unexpected content in symlink patch: %q", *report.Diff)
				}
			}
		})
	}
	if after := reviewTestGit(t, root, "status", "--porcelain=v1", "-z", "--untracked-files=all"); after != beforeStatus {
		t.Fatalf("git status changed: before=%q after=%q", beforeStatus, after)
	}
	if !bytes.Equal(indexBefore, readReviewTestFile(t, indexPath)) || !indexTime.Equal(reviewTestMtime(t, indexPath)) {
		t.Fatal("review changed the index")
	}
	for _, link := range links {
		if target, err := os.Readlink(filepath.Join(root, link.path)); err != nil || target != link.target {
			t.Errorf("review changed symlink %q: target=%q err=%v", link.path, target, err)
		}
	}
}

func TestReviewUntrackedSymlinkPatchesApply(t *testing.T) {
	for _, link := range []struct {
		path   string
		target string
	}{
		{"file link", "target"},
		{"-", "target\n"},
		{"nested/tab\tquote\"backslash\\link", "first\nsecond"},
		{"目录/line\nbreak", "first\nsecond\n"},
	} {
		t.Run(link.path, func(t *testing.T) {
			root := newReviewRepo(t)
			path := filepath.Join(root, link.path)
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(link.target, path); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			code, out, stderr := invokeReview(root, "--json", "--diff")
			if code != 0 || stderr != "" {
				t.Fatalf("review exit=%d stderr=%q", code, stderr)
			}
			var report reviewReport
			if err := json.Unmarshal([]byte(out), &report); err != nil || report.Diff == nil {
				t.Fatalf("invalid symlink report: %q err=%v", out, err)
			}
			wantAdditions := int64(strings.Count(link.target, "\n"))
			if !strings.HasSuffix(link.target, "\n") {
				wantAdditions++
			}
			if report.Additions != wantAdditions || report.Deletions != 0 {
				t.Fatalf("incorrect symlink line counts: %+v", report)
			}
			destination := t.TempDir()
			cmd := exec.Command("git", "apply")
			// The report trims trailing newlines; restore the patch terminator.
			cmd.Dir, cmd.Stdin = destination, strings.NewReader(*report.Diff+"\n")
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("apply symlink patch: %v: %s\n%s", err, output, *report.Diff)
			}
			if target, err := os.Readlink(filepath.Join(destination, link.path)); err != nil || target != link.target {
				t.Fatalf("incorrect applied symlink: target=%q err=%v", target, err)
			}
		})
	}
}

func TestReviewLeavesSplitIndexUntouched(t *testing.T) {
	root := newReviewRepo(t)
	writeReviewFile(t, root, "tracked.txt", "staged\n")
	reviewTestGit(t, root, "add", "--", "tracked.txt")
	reviewTestGit(t, root, "update-index", "--split-index")
	base := strings.TrimSuffix(reviewTestGit(t, root, "rev-parse", "--shared-index-path"), "\n")
	if !filepath.IsAbs(base) {
		base = filepath.Join(root, base)
	}
	oldTime := time.Unix(1_000_000_000, 0)
	if err := os.Chtimes(base, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	before := reviewTestMtime(t, base)
	code, out, stderr := invokeReview(root, "--json", "--diff")
	if code != 1 || out != "" || !strings.Contains(stderr, "split index") {
		t.Fatalf("split index review exit=%d stdout=%q stderr=%q", code, out, stderr)
	}
	if !reviewTestMtime(t, base).Equal(before) {
		t.Fatal("review touched the shared index")
	}
	reviewTestGit(t, root, "update-index", "--no-split-index")
	code, out, stderr = invokeReview(root, "--diff")
	if code != 0 || stderr != "" {
		t.Fatalf("ordinary index review exit=%d stderr=%q", code, stderr)
	}
	assertReviewContains(t, out, "+staged")
}

func invokeReview(root string, flags ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := Main(append([]string{"review", "--workspace", root}, flags...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func newReviewRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for the review fixture")
	}
	root := t.TempDir()
	reviewTestGit(t, root, "init", "-q", "--template=")
	reviewTestGit(t, root, "config", "user.name", "Review Test")
	reviewTestGit(t, root, "config", "user.email", "review@example.invalid")
	reviewTestGit(t, root, "config", "commit.gpgsign", "false")
	reviewTestGit(t, root, "config", "core.autocrlf", "false")
	return root
}

func reviewTestGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"--no-optional-locks", "-C", root}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return string(output)
}

func writeReviewFile(t *testing.T, root, path, text string) {
	t.Helper()
	name := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
}

func readReviewTestFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func reviewTestMtime(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime()
}

func assertReviewContains(t *testing.T, text string, want ...string) {
	t.Helper()
	for _, part := range want {
		if !strings.Contains(text, part) {
			t.Errorf("output missing %q: %q", part, text)
		}
	}
}
