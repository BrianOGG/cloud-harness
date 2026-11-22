package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

const (
	reviewLargeFileBytes = 1_000_000
	reviewLargeDeletion  = 500
)

type reviewReport struct {
	Clean     bool         `json:"clean"`
	Message   string       `json:"message,omitempty"`
	Risks     []reviewRisk `json:"risks"`
	FileCount int          `json:"file_count"`
	Staged    int          `json:"staged"`
	Unstaged  int          `json:"unstaged"`
	Untracked int          `json:"untracked"`
	Additions int64        `json:"additions"`
	Deletions int64        `json:"deletions"`
	Files     []reviewFile `json:"files"`
	Diff      *string      `json:"diff,omitempty"`
}

type reviewFile struct {
	Path           string `json:"path"`
	Status         string `json:"status"`
	StagedStatus   string `json:"staged_status,omitempty"`
	UnstagedStatus string `json:"unstaged_status,omitempty"`
	Untracked      bool   `json:"untracked,omitempty"`
	Additions      int64  `json:"additions"`
	Deletions      int64  `json:"deletions"`
	Binary         bool   `json:"binary"`

	untrackedDiff    bool
	untrackedSymlink bool
}

type reviewRisk struct {
	Kind      string `json:"kind"`
	Path      string `json:"path"`
	Source    string `json:"source,omitempty"`
	Line      int    `json:"line,omitempty"`
	Bytes     int64  `json:"bytes,omitempty"`
	Deletions int64  `json:"deletions,omitempty"`
}

type reviewLineStats struct {
	additions int64
	deletions int64
	binary    bool
}

func cmdReview(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	fs.SetOutput(stderr)
	workspace := fs.String("workspace", ".", "directory in the Git worktree")
	jsonOutput := fs.Bool("json", false, "emit structured JSON instead of Markdown")
	includeDiff := fs.Bool("diff", false, "include the patch with suspected secrets masked")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "usage: cloud-harness review [--workspace dir] [--json] [--diff]")
		return 2
	}
	key := os.Getenv("OPENAI_API_KEY")
	report, err := collectReview(*workspace, key, *includeDiff)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "review:", redactReviewLines(err.Error(), key))
		return 1
	}
	if *jsonOutput {
		return writeJSON(stdout, report)
	}
	if err := writeReviewMarkdown(stdout, report); err != nil {
		_, _ = fmt.Fprintln(stderr, "review:", err)
		return 1
	}
	return 0
}

func collectReview(workspace, key string, includeDiff bool) (reviewReport, error) {
	report := reviewReport{Files: []reviewFile{}, Risks: []reviewRisk{}}
	git, err := openReviewGit(workspace)
	if err != nil {
		return report, err
	}
	status, err := git.read(nil, false, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames", "--ignore-submodules=dirty")
	if err != nil {
		return report, err
	}
	if err := report.readStatus(status); err != nil {
		return report, err
	}
	if report.FileCount == 0 {
		report.Clean = true
		report.Message = "工作区干净"
	} else if err := report.inspect(git, key); err != nil {
		return report, err
	}
	if includeDiff {
		patch, err := report.readPatch(git, key)
		if err != nil {
			return report, err
		}
		report.Diff = &patch
	}
	for i := range report.Files {
		report.Files[i].Path = redactExportText(report.Files[i].Path, key)
	}
	for i := range report.Risks {
		report.Risks[i].Path = redactExportText(report.Risks[i].Path, key)
	}
	return report, nil
}

func (r *reviewReport) readStatus(status []byte) error {
	files := make(map[string]int)
	for entry := range reviewGitRecords(status) {
		if len(entry) < 4 || entry[2] != ' ' {
			return errors.New("git returned malformed porcelain status")
		}
		xy, path := entry[:2], entry[3:]
		i, ok := files[path]
		if !ok {
			i = len(r.Files)
			files[path] = i
			r.Files = append(r.Files, reviewFile{Path: path})
		}
		f := &r.Files[i]
		switch xy {
		case "??":
			f.Untracked = true
		case "DD", "AU", "UD", "UA", "DU", "AA", "UU":
			f.UnstagedStatus = "U"
		default:
			f.StagedStatus = strings.TrimSpace(xy[:1])
			f.UnstagedStatus = strings.TrimSpace(xy[1:])
		}
	}
	sort.Slice(r.Files, func(i, j int) bool { return r.Files[i].Path < r.Files[j].Path })
	r.FileCount = len(r.Files)
	for i := range r.Files {
		f := &r.Files[i]
		if f.StagedStatus != "" {
			r.Staged++
		}
		if f.UnstagedStatus != "" {
			r.Unstaged++
		}
		if f.Untracked {
			r.Untracked++
		}
		switch {
		case f.StagedStatus == "A" || f.UnstagedStatus == "A":
			f.Status = "A"
		case f.StagedStatus == "D" || f.UnstagedStatus == "D":
			f.Status = "D"
		case f.UnstagedStatus == "U":
			f.Status = "U"
		case f.StagedStatus != "" || f.UnstagedStatus != "":
			f.Status = "M"
		default:
			f.Status = "??"
		}
	}
	return nil
}

func (r *reviewReport) inspect(git reviewGit, key string) error {
	staged, err := git.numstat(r.Staged != 0, true)
	if err != nil {
		return err
	}
	unstaged, err := git.numstat(r.Unstaged != 0, false)
	if err != nil {
		return err
	}
	blobs, err := git.stagedBlobs(r.Files)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(git.dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	for i := range r.Files {
		f := &r.Files[i]
		indexStats := staged[f.Path]
		f.addStats(indexStats)
		f.addStats(unstaged[f.Path])
		blob := blobs[f.Path]
		if f.StagedStatus == "A" && blob.oid != "" && !indexStats.binary {
			if err := git.scanBlob(blob.oid, func(contents io.Reader) error {
				return r.scanSecrets(contents, f.Path, "staged", key)
			}); err != nil {
				return fmt.Errorf("inspect staged file %q: %w", f.Path, err)
			}
		}
		size, err := r.inspectWorktree(git, root, f, unstaged[f.Path], key)
		if err != nil {
			return fmt.Errorf("inspect worktree file %q: %w", f.Path, err)
		}
		if size = max(size, blob.size); size > reviewLargeFileBytes {
			r.Risks = append(r.Risks, reviewRisk{Kind: "large_file", Path: f.Path, Bytes: size})
		}
		if f.Binary {
			r.Risks = append(r.Risks, reviewRisk{Kind: "binary", Path: f.Path})
		}
		if f.Deletions > reviewLargeDeletion {
			r.Risks = append(r.Risks, reviewRisk{Kind: "large_deletion", Path: f.Path, Deletions: f.Deletions})
		}
		r.Additions += f.Additions
		r.Deletions += f.Deletions
	}
	return nil
}

func (r *reviewReport) inspectWorktree(git reviewGit, root *os.Root, f *reviewFile, stats reviewLineStats, key string) (int64, error) {
	if !f.Untracked && (f.UnstagedStatus == "" || f.UnstagedStatus == "D") {
		return 0, nil
	}
	name := filepath.FromSlash(strings.TrimSuffix(f.Path, "/"))
	info, err := root.Lstat(name)
	if err != nil {
		if f.UnstagedStatus == "U" && errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	regular := info.Mode().IsRegular()
	symlink := info.Mode()&os.ModeSymlink != 0
	if f.Untracked && (regular || symlink) {
		f.untrackedSymlink = symlink
		data, err := git.diffUntracked(*f, "--numstat", "-z")
		if err != nil {
			return 0, err
		}
		// --no-index emits one numstat record and may append both paths.
		stats, err = parseReviewLineStats(strings.SplitN(string(data), "\t", 3))
		if err != nil {
			return 0, err
		}
		f.addStats(stats)
		f.untrackedDiff = true
	}
	if !regular {
		return 0, nil
	}
	if !stats.binary && (f.Untracked || f.UnstagedStatus == "A" || f.StagedStatus == "A") {
		source := "unstaged"
		if f.Untracked {
			source = "untracked"
		}
		contents, err := root.Open(name)
		if err != nil {
			return 0, err
		}
		defer func() { _ = contents.Close() }()
		if err := r.scanSecrets(contents, f.Path, source, key); err != nil {
			return 0, err
		}
	}
	return info.Size(), nil
}

func (f *reviewFile) addStats(stats reviewLineStats) {
	f.Additions += stats.additions
	f.Deletions += stats.deletions
	f.Binary = f.Binary || stats.binary
}

func (r *reviewReport) scanSecrets(contents io.Reader, path, source, key string) error {
	reader := bufio.NewReader(contents)
	for number := 1; ; number++ {
		line, err := reader.ReadString('\n')
		line = strings.TrimSuffix(line, "\n")
		if redactExportText(line, key) != line {
			r.Risks = append(r.Risks, reviewRisk{Kind: "suspected_secret", Path: path, Source: source, Line: number})
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (r reviewReport) readPatch(git reviewGit, key string) (string, error) {
	var patch strings.Builder
	for _, section := range []struct {
		count  int
		staged bool
		label  string
	}{
		{r.Staged, true, "已暂存"},
		{r.Unstaged, false, "未暂存"},
	} {
		if section.count == 0 {
			continue
		}
		data, err := git.diff(section.staged, "--patch")
		if err != nil {
			return "", err
		}
		if len(data) > 0 {
			fmt.Fprintf(&patch, "# %s\n", section.label)
			patch.WriteString(redactReviewLines(string(data), key))
			patch.WriteByte('\n')
		}
	}
	untrackedHeader := false
	for _, f := range r.Files {
		if !f.untrackedDiff {
			continue
		}
		data, err := git.diffUntracked(f, "--patch")
		if err != nil {
			return "", err
		}
		if !untrackedHeader {
			patch.WriteString("# 未跟踪\n")
			untrackedHeader = true
		}
		patch.WriteString(redactReviewLines(string(data), key))
	}
	return strings.TrimRight(patch.String(), "\n"), nil
}

func redactReviewLines(text, key string) string {
	var out strings.Builder
	for line := range strings.SplitAfterSeq(text, "\n") {
		body, newline := strings.CutSuffix(line, "\n")
		out.WriteString(redactExportText(body, key))
		if newline {
			out.WriteByte('\n')
		}
	}
	return out.String()
}

type reviewGit struct {
	executable string
	dir        string
	env        []string
	options    []string
}

func openReviewGit(workspace string) (reviewGit, error) {
	executable, err := exec.LookPath("git")
	if err != nil {
		return reviewGit{}, fmt.Errorf("git is unavailable: %w", err)
	}
	git := reviewGit{
		executable: executable,
		dir:        workspace,
		options:    []string{"--no-pager", "--no-optional-locks", "-c", "core.fsmonitor=", "-c", "diff.autoRefreshIndex=false"},
	}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(name), "GIT_") {
			git.env = append(git.env, entry)
		}
	}
	git.env = append(git.env, "LC_ALL=C", "GIT_NO_LAZY_FETCH=1", "GIT_ALLOW_PROTOCOL=")
	root, err := git.read(nil, false, "rev-parse", "--show-toplevel")
	if err != nil {
		return reviewGit{}, err
	}
	git.dir = strings.TrimSuffix(string(root), "\n")
	if err := git.checkSplitIndex(); err != nil {
		return reviewGit{}, err
	}
	// Both status and diff can run a worktree clean filter when comparing
	// against the index. Disable it per invocation without editing config.
	filters, err := git.read(nil, true, "config", "--null", "--name-only", "--get-regexp", `^filter\..*\.(clean|process|required)$`)
	if err != nil {
		return reviewGit{}, err
	}
	seen := make(map[string]bool)
	for name := range reviewGitRecords(filters) {
		if seen[name] {
			continue
		}
		seen[name] = true
		value := ""
		if strings.HasSuffix(name, ".required") {
			value = "false"
		}
		git.options = append(git.options, "-c", name+"="+value)
	}
	return git, nil
}

// Git touches the base file of a split index even with optional locking off.
// Inspect the link extension before asking Git to read the index.
func (g reviewGit) checkSplitIndex() error {
	data, err := g.read(nil, false, "rev-parse", "--git-path", "index")
	if err != nil {
		return err
	}
	path := strings.TrimSuffix(string(data), "\n")
	if !filepath.IsAbs(path) {
		path = filepath.Join(g.dir, path)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return err
	}
	var baseIDs [][]byte
	for _, entry := range entries {
		name, ok := strings.CutPrefix(entry.Name(), "sharedindex.")
		if !ok || (len(name) != 40 && len(name) != 64) {
			continue
		}
		if oid, err := hex.DecodeString(name); err == nil {
			baseIDs = append(baseIDs, oid)
		}
	}
	if len(baseIDs) == 0 {
		return nil
	}
	index, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for {
		offset := bytes.Index(index, []byte("link"))
		if offset < 0 || len(index)-offset < 8 {
			return nil
		}
		size := uint64(binary.BigEndian.Uint32(index[offset+4 : offset+8]))
		payload := index[offset+8:]
		for _, oid := range baseIDs {
			if uint64(len(oid)) <= size && size <= uint64(len(payload)) && bytes.HasPrefix(payload, oid) {
				return errors.New("git split index cannot be reviewed without changing its timestamp")
			}
		}
		index = index[offset+len("link"):]
	}
}

func (g reviewGit) command(args ...string) *exec.Cmd {
	cmd := exec.Command(g.executable, append(append([]string(nil), g.options...), args...)...)
	cmd.Dir, cmd.Env = g.dir, g.env
	return cmd
}

func (g reviewGit) read(input io.Reader, allowExitOne bool, args ...string) ([]byte, error) {
	cmd := g.command(args...)
	cmd.Stdin = input
	data, err := cmd.Output()
	if err == nil {
		return data, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		detail := strings.TrimSpace(string(exit.Stderr))
		// --no-index uses 1 for differences, including when Git warns about
		// a line ending conversion. A failed read has no diff output.
		if allowExitOne && exit.ExitCode() == 1 && (len(data) > 0 || detail == "") {
			return data, nil
		}
		if detail != "" {
			return nil, fmt.Errorf("git %s: %s", args[0], detail)
		}
	}
	return nil, fmt.Errorf("git %s: %w", args[0], err)
}

func (g reviewGit) diff(staged bool, format ...string) ([]byte, error) {
	args := []string{"diff", "--no-ext-diff", "--no-textconv", "--no-color", "--no-renames", "--ignore-submodules=dirty", "--submodule=short", "--ita-invisible-in-index"}
	if staged {
		args = append(args, "--cached")
	}
	args = append(args, format...)
	return g.read(nil, false, append(args, "--")...)
}

func (g reviewGit) diffUntracked(file reviewFile, format ...string) ([]byte, error) {
	if file.untrackedSymlink {
		// Git's --no-index can follow a directory link even with an empty
		// stdin base. Read only the link text and synthesize its addition.
		target, err := os.Readlink(filepath.Join(g.dir, filepath.FromSlash(file.Path)))
		if err != nil {
			return nil, err
		}
		lines := strings.SplitAfter(target, "\n")
		if lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		if slices.Contains(format, "--numstat") {
			return fmt.Appendf(nil, "%d\t0\t%s\x00", len(lines), file.Path), nil
		}
		a, b := reviewDiffPath("a/"+file.Path), reviewDiffPath("b/"+file.Path)
		marker := b
		if strings.ContainsRune(b, ' ') && !strings.HasPrefix(b, `"`) {
			marker += "\t"
		}
		var patch strings.Builder
		fmt.Fprintf(&patch, "diff --git %s %s\nnew file mode 120000\n--- /dev/null\n+++ %s\n@@ -0,0 +1", a, b, marker)
		if len(lines) != 1 {
			fmt.Fprintf(&patch, ",%d", len(lines))
		}
		patch.WriteString(" @@\n")
		for _, line := range lines {
			patch.WriteByte('+')
			patch.WriteString(line)
		}
		if !strings.HasSuffix(target, "\n") {
			patch.WriteString("\n\\ No newline at end of file\n")
		}
		return []byte(patch.String()), nil
	}
	args := []string{"diff", "--no-index", "--no-ext-diff", "--no-textconv", "--no-color", "--src-prefix=a/", "--dst-prefix=b/"}
	args = append(args, format...)
	path := file.Path
	if path == "-" {
		path = "./-" // Git treats a bare dash as stdin even after --.
	}
	return g.read(nil, true, append(args, "--", "/dev/null", path)...)
}

func reviewDiffPath(path string) string {
	// Git patch paths use bytewise C quoting, including octal escapes.
	var quoted strings.Builder
	for i := 0; i < len(path); i++ {
		c := path[i]
		switch {
		case c == '\\' || c == '"':
			quoted.WriteByte('\\')
			quoted.WriteByte(c)
		case c < ' ' || c >= 0x7f:
			fmt.Fprintf(&quoted, "\\%03o", c)
		default:
			quoted.WriteByte(c)
		}
	}
	if quoted.Len() == len(path) {
		return path
	}
	return `"` + quoted.String() + `"`
}

func (g reviewGit) numstat(changed, staged bool) (map[string]reviewLineStats, error) {
	if !changed {
		return nil, nil
	}
	data, err := g.diff(staged, "--numstat", "-z")
	if err != nil {
		return nil, err
	}
	stats := make(map[string]reviewLineStats)
	for entry := range reviewGitRecords(data) {
		parts := strings.SplitN(entry, "\t", 3)
		count, err := parseReviewLineStats(parts)
		if err != nil {
			return nil, err
		}
		path := parts[2]
		previous := stats[path]
		count.additions += previous.additions
		count.deletions += previous.deletions
		count.binary = count.binary || previous.binary
		stats[path] = count
	}
	return stats, nil
}

func reviewGitRecords(output []byte) iter.Seq[string] {
	return strings.FieldsFuncSeq(string(output), func(r rune) bool { return r == 0 })
}

func parseReviewLineStats(parts []string) (reviewLineStats, error) {
	if len(parts) == 3 {
		if parts[0] == "-" && parts[1] == "-" {
			return reviewLineStats{binary: true}, nil
		}
		additions, addErr := strconv.ParseInt(parts[0], 10, 64)
		deletions, delErr := strconv.ParseInt(parts[1], 10, 64)
		if addErr == nil && delErr == nil && additions >= 0 && deletions >= 0 {
			return reviewLineStats{additions: additions, deletions: deletions}, nil
		}
	}
	return reviewLineStats{}, errors.New("git returned malformed diff numstat")
}

type reviewBlob struct {
	oid  string
	size int64
}

func (g reviewGit) stagedBlobs(files []reviewFile) (map[string]reviewBlob, error) {
	wanted := make(map[string]bool)
	for _, f := range files {
		if f.StagedStatus != "" && f.StagedStatus != "D" {
			wanted[f.Path] = true
		}
	}
	if len(wanted) == 0 {
		return nil, nil
	}
	data, err := g.read(nil, false, "ls-files", "--stage", "-z")
	if err != nil {
		return nil, err
	}
	var paths, oids []string
	for entry := range reviewGitRecords(data) {
		metadata, path, ok := strings.Cut(entry, "\t")
		if !ok || !wanted[path] {
			continue
		}
		fields := strings.Fields(metadata)
		if len(fields) != 3 {
			return nil, errors.New("git returned malformed index entry")
		}
		if fields[2] == "0" && (fields[0] == "100644" || fields[0] == "100755") {
			paths = append(paths, path)
			oids = append(oids, fields[1])
		}
	}
	if len(paths) == 0 {
		return nil, nil
	}
	input := strings.NewReader(strings.Join(oids, "\n") + "\n")
	data, err = g.read(input, false, "cat-file", "--batch-check=%(objecttype) %(objectsize)")
	if err != nil {
		return nil, err
	}
	entries := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(entries) != len(paths) {
		return nil, errors.New("git returned unexpected staged blob count")
	}
	blobs := make(map[string]reviewBlob, len(paths))
	for i, entry := range entries {
		kind, sizeText, _ := strings.Cut(entry, " ")
		size, err := strconv.ParseInt(sizeText, 10, 64)
		if kind != "blob" || err != nil || size < 0 {
			return nil, fmt.Errorf("git could not determine the size of staged file %q", paths[i])
		}
		blobs[paths[i]] = reviewBlob{oid: oids[i], size: size}
	}
	return blobs, nil
}

func (g reviewGit) scanBlob(oid string, scan func(io.Reader) error) error {
	cmd := g.command("cat-file", "blob", oid)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	contents, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	scanErr := scan(contents)
	_ = contents.Close()
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("git cat-file: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return scanErr
}

var reviewMarkdownEscapes = strings.NewReplacer("|", "&#124;", "`", "&#96;")

func reviewMarkdownPath(path string) string {
	quoted := strconv.QuoteToGraphic(path)
	return "<code>" + reviewMarkdownEscapes.Replace(html.EscapeString(quoted[1:len(quoted)-1])) + "</code>"
}

func writeReviewMarkdown(out io.Writer, report reviewReport) error {
	var b strings.Builder
	b.WriteString("# Git 改动审查\n\n")
	if len(report.Risks) > 0 {
		b.WriteString("## 风险提示\n\n")
		for _, risk := range report.Risks {
			path := reviewMarkdownPath(risk.Path)
			switch risk.Kind {
			case "suspected_secret":
				source := risk.Source
				switch source {
				case "staged":
					source = "已暂存"
				case "unstaged":
					source = "未暂存"
				case "untracked":
					source = "未跟踪"
				}
				fmt.Fprintf(&b, "- 疑似密钥 %s（%s，第 %d 行）\n", path, source, risk.Line)
			case "large_file":
				fmt.Fprintf(&b, "- 超大文件 %s（%d 字节）\n", path, risk.Bytes)
			case "binary":
				fmt.Fprintf(&b, "- 二进制文件改动 %s\n", path)
			case "large_deletion":
				fmt.Fprintf(&b, "- 删除行数异常 %s（删除 %d 行）\n", path, risk.Deletions)
			}
		}
		b.WriteByte('\n')
	}
	if report.Clean {
		fmt.Fprintf(&b, "%s\n\n", report.Message)
	}
	fmt.Fprintf(&b, "文件总数 %d（已暂存 %d，未暂存 %d，未跟踪 %d），总新增 %d 行，总删除 %d 行。\n",
		report.FileCount, report.Staged, report.Unstaged, report.Untracked, report.Additions, report.Deletions)
	if len(report.Files) > 0 {
		b.WriteString("\n| 路径 | 状态 | 已暂存 | 未暂存 | 未跟踪 | 新增 | 删除 |\n| --- | --- | --- | --- | --- | ---: | ---: |\n")
		for _, f := range report.Files {
			untracked := ""
			if f.Untracked {
				untracked = "??"
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %d | %d |\n", reviewMarkdownPath(f.Path), f.Status,
				f.StagedStatus, f.UnstagedStatus, untracked, f.Additions, f.Deletions)
		}
	}
	if report.Diff != nil && *report.Diff != "" {
		b.WriteString("\n## Diff\n\n")
		for line := range strings.SplitSeq(*report.Diff, "\n") {
			fmt.Fprintf(&b, "    %s\n", line)
		}
	}
	_, err := io.WriteString(out, b.String())
	return err
}
