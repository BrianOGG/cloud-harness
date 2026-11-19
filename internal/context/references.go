package context

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	MaxReferenceFileBytes = 8 << 10
	MaxReferenceDiffBytes = 16 << 10
	MaxFileReferences     = 10
)

// Reference is the safe, content-free event payload produced by expansion.
type Reference struct {
	Type  string `json:"type"`
	Path  string `json:"path,omitempty"`
	Range string `json:"range,omitempty"`
	Bytes int    `json:"bytes"`
}

// DiffReader is supplied by the agent so reference expansion follows the
// existing workspace git tool path.
type DiffReader func(staged bool) (string, error)

// FileReader can route file reads through the current run's existing
// workspace read path. A nil reader uses the local file system directly.
type FileReader func(path string) ([]byte, error)

var fileReferencePattern = regexp.MustCompile(`@file:([^\s]+)`)
var fileLinePattern = regexp.MustCompile(`^L([0-9]+)(?:-L?([0-9]+))?$`)
var diffReferencePattern = regexp.MustCompile(`@diff(?:[ \t]+--staged)?\b`)

// ExpandReferences expands explicit @file and @diff markers. File errors are
// returned to the caller; a diff error is rendered as a clear data message so
// a non-git or clean workspace does not fail a run.
func ExpandReferences(text, workspace string, readFile FileReader, readDiff DiffReader) (string, []Reference, error) {
	if !strings.Contains(text, "@file:") && !strings.Contains(text, "@diff") {
		return text, nil, nil
	}
	root, err := filepath.Abs(workspace)
	if err != nil {
		return text, nil, fmt.Errorf("resolve workspace: %w", err)
	}
	root = filepath.Clean(root)
	refs := make([]Reference, 0, 4)
	fileCount := 0
	totalFileMarkers := 0
	var firstErr error

	// Replace file markers first. This preserves the user's text and appends
	// each expansion immediately after the message, before prior history.
	fileBlocks := make([]string, 0)
	fileReplaced := fileReferencePattern.ReplaceAllStringFunc(text, func(marker string) string {
		totalFileMarkers++
		if fileCount >= MaxFileReferences {
			return marker
		}
		fileCount++
		m := fileReferencePattern.FindStringSubmatch(marker)
		refToken := strings.TrimRight(m[1], ".,;:!?)]}")
		pathText, lineSpec, hasRange := strings.Cut(refToken, "#")
		startText, endText := "", ""
		rangeText := "all"
		if hasRange {
			lineMatch := fileLinePattern.FindStringSubmatch(lineSpec)
			if lineMatch == nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("invalid @file line range %q; expected #Lstart-Lend", lineSpec)
				}
				return marker
			}
			startText, endText = lineMatch[1], lineMatch[2]
			rangeText = "L" + startText
			if endText != "" {
				rangeText += "-L" + endText
			}
		}
		refIndex := len(refs)
		refs = append(refs, Reference{Type: "file", Path: filepath.ToSlash(pathText), Range: rangeText})
		resolved, pathErr := insideWorkspace(root, pathText)
		if pathErr != nil {
			if firstErr == nil {
				firstErr = pathErr
			}
			return marker
		}
		var data []byte
		var readErr error
		if readFile != nil {
			data, readErr = readFile(resolved)
		} else {
			data, readErr = os.ReadFile(resolved)
		}
		if readErr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("file %q does not exist or cannot be read: %w", pathText, readErr)
			}
			return marker
		}
		start, end, _, rangeErr := lineRange(startText, endText, countLines(data))
		if rangeErr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("file %q: %w", pathText, rangeErr)
			}
			return marker
		}
		body := renderFile(pathText, data, start, end)
		body, _ = capReference(body, MaxReferenceFileBytes, "\n[truncated at 8KiB]", "\n```")
		refs[refIndex].Bytes = len(body)
		fileBlocks = append(fileBlocks, body)
		return marker
	})
	if strings.Contains(fileReplaced, "@file:") && totalFileMarkers == 0 && firstErr == nil {
		firstErr = errors.New("invalid @file reference; expected @file:path or @file:path#Lstart-Lend")
	}
	diffBlocks := make([]string, 0, 1)
	diffReplaced := diffReferencePattern.ReplaceAllStringFunc(fileReplaced, func(marker string) string {
		staged := strings.Contains(marker, "--staged")
		rangeText := "working-tree"
		if staged {
			rangeText = "staged"
		}
		refIndex := len(refs)
		refs = append(refs, Reference{Type: "diff", Range: rangeText})
		if readDiff == nil {
			diagnostic := "[diff unavailable: workspace git diff is not configured]"
			refs[refIndex].Bytes = len(diagnostic)
			diffBlocks = append(diffBlocks, diagnostic)
			return marker
		}
		diff, diffErr := readDiff(staged)
		if diffErr != nil {
			// The existing git tool reports non-git workspaces and command errors
			// as ordinary tool errors. Keep that diagnostic visible in context.
			diagnostic := "[diff unavailable: " + diffErr.Error() + "]"
			refs[refIndex].Bytes = len(diagnostic)
			diffBlocks = append(diffBlocks, diagnostic)
			return marker
		}
		if strings.TrimSpace(diff) == "" {
			diagnostic := "[diff: workspace has no " + map[bool]string{true: "staged", false: "unstaged"}[staged] + " changes]"
			refs[refIndex].Bytes = len(diagnostic)
			diffBlocks = append(diffBlocks, diagnostic)
			return marker
		}
		body, _ := capReference(diff, MaxReferenceDiffBytes, "\n[truncated at 16KiB]", "")
		refs[refIndex].Bytes = len(body)
		diffBlocks = append(diffBlocks, "[workspace diff ("+rangeText+")]\n```diff\n"+body+"\n```")
		return marker
	})

	// Markers remain in the original user sentence, while expanded snippets
	// are appended as untrusted data blocks in a stable position.
	out := diffReplaced
	if len(fileBlocks)+len(diffBlocks) > 0 {
		out += "\n\n" + strings.Join(append(fileBlocks, diffBlocks...), "\n\n")
	}
	if totalFileMarkers > MaxFileReferences {
		out += "\n\n[reference limit: only the first 10 @file references were expanded]"
	}
	return out, refs, firstErr
}

func insideWorkspace(root, pathText string) (string, error) {
	pathText = filepath.FromSlash(pathText)
	resolved := pathText
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(root, resolved)
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("resolve file %q: %w", pathText, err)
	}
	abs = filepath.Clean(abs)
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("file %q is outside the workspace", pathText)
	}
	if realRoot, rootErr := filepath.EvalSymlinks(root); rootErr == nil {
		if realPath, pathErr := filepath.EvalSymlinks(abs); pathErr == nil {
			realRel, relErr := filepath.Rel(realRoot, realPath)
			if relErr != nil || realRel == ".." || strings.HasPrefix(realRel, ".."+string(filepath.Separator)) {
				return "", fmt.Errorf("file %q is outside the workspace", pathText)
			}
		}
	}
	return abs, nil
}

func countLines(data []byte) int {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if text == "" {
		return 1
	}
	lines := strings.Split(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return 1
	}
	return len(lines)
}

func lineRange(startText, endText string, lines int) (int, int, string, error) {
	if startText == "" {
		return 1, lines, "", nil
	}
	start, _ := strconv.Atoi(startText)
	end := start
	if endText != "" {
		end, _ = strconv.Atoi(endText)
	}
	if start < 1 || end < start || end > lines {
		return 0, 0, "", errors.New("line range is outside the file")
	}
	return start, end, fmt.Sprintf("L%d-L%d", start, end), nil
}

func renderFile(path string, data []byte, start, end int) string {
	normalized := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	if len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var b strings.Builder
	b.WriteString("[file " + filepath.ToSlash(path))
	if start != 1 || end != len(lines) {
		b.WriteString(fmt.Sprintf("#L%d-L%d", start, end))
	}
	b.WriteString("]\n```text\n")
	for i := start; i <= end && i <= len(lines); i++ {
		b.WriteString(fmt.Sprintf("%s:%d %s\n", filepath.ToSlash(path), i, lines[i-1]))
	}
	b.WriteString("```")
	return b.String()
}

func capReference(s string, max int, marker, suffix string) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	limit := max - len(marker) - len(suffix)
	if limit < 0 {
		return marker[:max], true
	}
	if limit > len(s) {
		limit = len(s)
	}
	for limit > 0 && !utf8.ValidString(s[:limit]) {
		limit--
	}
	return s[:limit] + marker + suffix, true
}
