// Package rules discovers and freezes layered AGENTS.md project instructions.
package rules

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MaxBytes bounds the entire rendered project-rules section, including markers.
const MaxBytes = 32 * 1024

const heading = "Project rules (AGENTS.md; later sources are more specific):\n\n"

// File describes a rule source. Bytes is its size on disk in Files, and the
// number of body bytes actually injected in Loaded. Source is a display label
// (global or a workspace-relative path), excluded from diagnostic event payloads.
type File struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	Source string `json:"-"`
}

// Diagnostic records a non-fatal discovery or read failure, without rule text.
type Diagnostic struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// Result is a snapshot for one run. Content never exceeds MaxBytes. Files lists
// discovered sources; Loaded and Omitted describe the final rendered section.
type Result struct {
	Content     string
	Files       []File
	Loaded      []File
	Omitted     []File
	Diagnostics []Diagnostic
}

// ConfigDir follows config.Load's CLOUD_HARNESS_CONFIG override. Without an
// explicit config file, rules live in the OS user config directory/cloud-harness
// (on Linux, $XDG_CONFIG_HOME/cloud-harness or ~/.config/cloud-harness).
func ConfigDir() (string, error) {
	if path := os.Getenv("CLOUD_HARNESS_CONFIG"); path != "" {
		return filepath.Abs(filepath.Dir(path))
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "cloud-harness"), nil
}

// Load reads each selected file once. A missing AGENTS.md is silent; errors on
// individual sources are returned as diagnostics and never fail a run.
func Load(workspace string) Result {
	globalDir, err := ConfigDir()
	result := load(workspace, globalDir, func(path string) (io.ReadCloser, error) {
		return os.Open(path)
	})
	if err != nil {
		result.Diagnostics = append(result.Diagnostics, Diagnostic{Path: "global", Error: err.Error()})
	}
	return result
}

type section struct {
	file                  File
	start, bodyStart, end int
	complete              bool
}

func load(workspace, globalDir string, open func(string) (io.ReadCloser, error)) Result {
	result := discover(workspace, globalDir)
	if len(result.Files) == 0 {
		return result
	}
	var text strings.Builder
	text.WriteString(heading)
	var sections []section
	var tail []File
	for i, file := range result.Files {
		marker := "<!-- AGENTS.md: " + file.Source + " -->\n"
		remaining := MaxBytes - text.Len() - len(marker) - 2
		if remaining < 0 {
			tail = result.Files[i:]
			break
		}
		reader, err := open(file.Path)
		if err != nil {
			result.Diagnostics = append(result.Diagnostics, Diagnostic{Path: file.Path, Error: err.Error()})
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(reader, int64(remaining)+1))
		closeErr := reader.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			result.Diagnostics = append(result.Diagnostics, Diagnostic{Path: file.Path, Error: err.Error()})
			continue
		}
		complete := len(body) <= remaining
		if !complete {
			body = body[:remaining]
		}
		part := section{file: file, start: text.Len(), complete: complete}
		text.WriteString(marker)
		part.bodyStart = text.Len()
		text.Write(body)
		part.end = text.Len()
		text.WriteString("\n\n")
		sections = append(sections, part)
		if !complete {
			tail = result.Files[i+1:]
			break
		}
	}
	if len(sections) == 0 && len(tail) == 0 {
		return result // All files were unreadable: no empty project-rules message.
	}
	full := text.String()
	cut := len(full)
	for {
		result.Loaded, result.Omitted = describe(sections, tail, cut)
		if len(result.Omitted) == 0 {
			result.Content = full
			return result
		}
		footer := truncationMarker(result.Omitted)
		next := min(cut, MaxBytes-len(footer))
		// Keep source markers whole, and never split a UTF-8 code point.
		for _, part := range sections {
			if next > part.start && next < part.bodyStart {
				next = part.start
				break
			}
		}
		for next > len(heading) && next < len(full) && !utf8.RuneStart(full[next]) {
			next--
		}
		if next == cut {
			result.Content = full[:cut] + footer
			return result
		}
		cut = next
	}
}

func describe(sections []section, tail []File, cut int) (loaded, omitted []File) {
	for _, part := range sections {
		if cut >= part.bodyStart {
			file := part.file
			file.Bytes = int64(max(0, min(cut, part.end)-part.bodyStart))
			loaded = append(loaded, file)
		}
		if !part.complete || cut < part.end || cut < part.bodyStart {
			omitted = append(omitted, part.file)
		}
	}
	return loaded, append(omitted, tail...)
}

func truncationMarker(omitted []File) string {
	const start = "\n<!-- AGENTS.md truncated (32 KiB limit); omitted or partial sources: "
	const end = " -->\n"
	// Extremely large source lists must not bypass the section's byte limit.
	// doctor and the rules.truncated event retain the complete list of paths.
	budget := MaxBytes - len(heading) - len(start) - len(end) - 96
	var labels strings.Builder
	for i, file := range omitted {
		label := strconv.Quote(file.Source)
		if labels.Len()+len(label)+2 > budget {
			if labels.Len() > 0 {
				labels.WriteString(", ")
			}
			labels.WriteString(fmt.Sprintf("[%d more omitted sources; see doctor]", len(omitted)-i))
			break
		}
		if labels.Len() > 0 {
			labels.WriteString(", ")
		}
		labels.WriteString(label)
	}
	return start + labels.String() + end
}

func discover(workspace, globalDir string) Result {
	var result Result
	seen := make(map[string]bool)
	add := func(path, source string) {
		canonical, err := filepath.EvalSymlinks(path)
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		if err == nil {
			canonical, err = filepath.Abs(canonical)
		}
		if err != nil {
			result.Diagnostics = append(result.Diagnostics, Diagnostic{Path: path, Error: err.Error()})
			return
		}
		canonical = filepath.Clean(canonical)
		if seen[canonical] {
			return
		}
		info, err := os.Stat(canonical)
		if err != nil {
			result.Diagnostics = append(result.Diagnostics, Diagnostic{Path: path, Error: err.Error()})
			return
		}
		if !info.Mode().IsRegular() {
			result.Diagnostics = append(result.Diagnostics, Diagnostic{Path: path, Error: "not a regular file"})
			return
		}
		seen[canonical] = true
		result.Files = append(result.Files, File{Path: canonical, Source: source, Bytes: info.Size()})
	}
	if globalDir != "" {
		add(filepath.Join(globalDir, "AGENTS.md"), "global")
	}
	if workspace == "" {
		workspace = "."
	}
	root, err := filepath.Abs(workspace)
	if err == nil {
		root, err = filepath.EvalSymlinks(root)
	}
	if err != nil {
		result.Diagnostics = append(result.Diagnostics, Diagnostic{Path: workspace, Error: err.Error()})
		return result
	}
	add(filepath.Join(root, "AGENTS.md"), "AGENTS.md")
	// WalkDir does not follow directory symlinks, avoiding cycles and walks
	// outside the workspace. File symlinks still participate in canonical dedup.
	var children []string
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			result.Diagnostics = append(result.Diagnostics, Diagnostic{Path: path, Error: walkErr.Error()})
			return nil
		}
		if entry.IsDir() {
			if path != root {
				switch entry.Name() {
				case ".git", ".cloud-harness", "node_modules", "vendor":
					return filepath.SkipDir
				}
			}
			return nil
		}
		if entry.Name() == "AGENTS.md" && path != filepath.Join(root, "AGENTS.md") {
			children = append(children, path)
		}
		return nil
	})
	sort.Slice(children, func(i, j int) bool {
		di := strings.Count(children[i], string(filepath.Separator))
		dj := strings.Count(children[j], string(filepath.Separator))
		if di != dj {
			return di < dj
		}
		return children[i] < children[j]
	})
	for _, path := range children {
		rel, _ := filepath.Rel(root, path)
		add(path, filepath.ToSlash(rel))
	}
	return result
}
