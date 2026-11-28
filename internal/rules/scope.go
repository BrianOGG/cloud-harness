// Scoped run loading visits only ancestors of referenced workspace paths.
// The diagnostic inventory in Load also discovers inactive sources.
package rules

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Scope is checkpointed with the messages containing the frozen rule bodies.
// Remember missing sources as well, so agent writes cannot change a visited scope.
type Scope struct {
	Visited     map[string]bool `json:"visited"`
	LoadedPaths map[string]bool `json:"loaded_paths"`
	Bytes       int             `json:"bytes"`
}

func ignoredRuleDir(rel string) bool {
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		switch part {
		case ".git", ".cloud-harness", "node_modules", "vendor":
			return true
		}
	}
	return false
}

// Next returns only the additional instructions for this run. Empty targets
// select the global and workspace root instructions on the first call.
func (s *Scope) Next(workspace string, targets []string, allowRead func(string) bool) Result {
	var result Result
	if s.Visited == nil {
		s.Visited = map[string]bool{}
		s.LoadedPaths = map[string]bool{}
	}
	if workspace == "" {
		workspace = "."
	}
	lexicalRoot, err := filepath.Abs(workspace)
	root := lexicalRoot
	if err == nil {
		root, err = filepath.EvalSymlinks(root)
	}
	if err != nil {
		return Result{Diagnostics: []Diagnostic{{Path: workspace, Error: err.Error()}}}
	}

	var globalFile string
	add := func(path, label string) {
		if s.Visited[label] {
			return
		}
		s.Visited[label] = true
		file, err := resolveRule(path, label)
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		if err != nil {
			result.Diagnostics = append(result.Diagnostics, Diagnostic{Path: path, Error: err.Error()})
			return
		}
		real := file.Path
		if label != "global" {
			rel, err := filepath.Rel(root, real)
			if err != nil || !filepath.IsLocal(rel) || ignoredRuleDir(rel) || filepath.Base(rel) != "AGENTS.md" {
				result.Diagnostics = append(result.Diagnostics, Diagnostic{Path: path, Error: "rule target must be a workspace AGENTS.md"})
				return
			}
		}
		if label != "global" && allowRead != nil && (!allowRead(path) || !allowRead(real)) {
			result.Diagnostics = append(result.Diagnostics, Diagnostic{Path: path, Error: "file read denied by policy"})
			return
		}
		if s.LoadedPaths[real] {
			return
		}
		s.LoadedPaths[real] = true
		if label == "global" {
			globalFile = real
		}
		result.Files = append(result.Files, file)
	}
	if !s.Visited["global"] {
		if global, err := ConfigDir(); err == nil {
			add(filepath.Join(global, "AGENTS.md"), "global")
		} else {
			result.Diagnostics = append(result.Diagnostics, Diagnostic{Path: "global", Error: err.Error()})
		}
	}
	add(filepath.Join(root, "AGENTS.md"), "AGENTS.md")
	dirs := map[string]bool{}
	for _, target := range targets {
		if filepath.IsAbs(target) && lexicalRoot != root {
			if rel, err := filepath.Rel(lexicalRoot, target); err == nil && filepath.IsLocal(rel) {
				target = filepath.Join(root, rel)
			}
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(root, target)
		}
		rel, err := filepath.Rel(root, target)
		if err != nil || !filepath.IsLocal(rel) || ignoredRuleDir(rel) {
			continue
		}
		if info, err := os.Stat(target); err != nil || !info.IsDir() {
			rel = filepath.Dir(rel)
		}
		for rel != "." && !dirs[rel] {
			dirs[rel] = true
			rel = filepath.Dir(rel)
		}
	}
	ordered := make([]string, 0, len(dirs))
	for dir := range dirs {
		ordered = append(ordered, dir)
	}
	sort.Strings(ordered) // each ancestor sorts before its descendants
	for _, dir := range ordered {
		path := filepath.Join(dir, "AGENTS.md")
		add(filepath.Join(root, path), filepath.ToSlash(path))
	}
	result = render(result, func(path string) (io.ReadCloser, error) {
		if path == globalFile {
			return os.Open(path)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil, err
		}
		return os.OpenInRoot(root, rel)
	}, MaxBytes-s.Bytes)
	s.Bytes += len(result.Content)
	return result
}
