package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

const (
	maxFileBytes          = 1 << 20
	defaultListDirEntries = 200
	defaultListFiles      = 200
	defaultGrepMaxHits    = 50
	hardGrepMaxHits       = 200
	defaultGitLogLimit    = 20
	hardGitLogLimit       = 100
	defaultCommandTimeout = 120 * time.Second
	truncationMarker      = "\n...[truncated: output capped at 1MiB]"
	securityNone          = "none"
)

var defaultCommandEnvVars = []string{"PATH", "HOME", "LANG", "LC_ALL", "TERM"}

// RegisterLocal adds read_file, write_file, edit_file, list_dir, list_files, grep, git_status, git_diff, git_log, and run_command rooted at root.
// read_file, list_dir, list_files, grep, git_status, git_diff, and git_log are readonly. write_file, edit_file, and any command outside the safe
// set require a scoped grant or an approval decision before Execute runs.
func RegisterLocal(b *Broker, root string) error {
	specs, err := LocalSpecs(b, root)
	if err != nil {
		return err
	}
	for _, spec := range specs {
		b.Register(spec)
	}
	return nil
}

// ForkWorkspace copies a broker's registrations and execution settings, then
// binds its local tools to root. reset names are rebound by the owning manager
// because their executors close over that manager's workspace and broker.
func (b *Broker) ForkWorkspace(root string, reset ...string) (*Broker, error) {
	b.mu.RLock()
	fork := NewBroker(b.charge, b.grants)
	fork.now = b.now
	fork.CommandEnvAllow = append([]string(nil), b.CommandEnvAllow...)
	fork.commandLimits = b.commandLimits
	for name, spec := range b.reg {
		fork.reg[name] = spec
	}
	b.mu.RUnlock()
	for _, name := range reset {
		delete(fork.reg, name)
	}
	if err := RegisterLocal(fork, root); err != nil {
		return nil, err
	}
	return fork, nil
}

// EnsureLocal registers any missing workspace tools on b without overwriting existing registrations.
func EnsureLocal(b *Broker, root string) error {
	specs, err := LocalSpecs(b, root)
	if err != nil {
		return err
	}
	for _, spec := range specs {
		if !b.Has(spec.Name) {
			b.Register(spec)
		}
	}
	return nil
}

// LocalSpecs builds the workspace tool specifications rooted at root.
func LocalSpecs(b *Broker, root string) ([]Spec, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	abs = filepath.Clean(abs)
	return []Spec{
		readFileSpec(abs),
		writeFileSpec(abs),
		editFileSpec(abs),
		listDirSpec(abs),
		listFilesSpec(abs),
		grepSpec(abs),
		gitStatusSpec(b, abs),
		gitDiffSpec(b, abs),
		gitLogSpec(b, abs),
		runCommandSpec(b, abs),
	}, nil
}

func readFileSpec(abs string) Spec {
	return Spec{
		Name:            "read_file",
		Description:     "Read a UTF-8 text file inside the workspace, optionally restricted to a 1-based inclusive line range.",
		Required:        []string{"path"},
		Readonly:        true,
		Idempotent:      true,
		Pollable:        true,
		SideEffectClass: securityNone,
		Parameters: strictSchema(map[string]schemaProp{
			"path":       {Type: "string", Description: "Workspace-relative or absolute path inside the workspace."},
			"start_line": {Type: "integer", Description: "Optional 1-based first line to read (inclusive)."},
			"end_line":   {Type: "integer", Description: "Optional 1-based last line to read (inclusive)."},
		}, "path"),
		ExtractResources: func(args json.RawMessage) ([]string, error) {
			p, err := resolveInside(abs, stringField(args, "path"))
			if err != nil {
				return nil, err
			}
			return []string{p}, nil
		},
		Execute: func(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
			p, err := resolveInside(abs, stringField(args, "path"))
			if err != nil {
				return nil, err
			}
			startLine, hasStart, err := intFieldOpt(args, "start_line")
			if err != nil {
				return nil, err
			}
			if hasStart && startLine < 1 {
				return nil, domain.E(domain.CodeInvalidArgument, "start_line must be >= 1")
			}
			endLine, hasEnd, err := intFieldOpt(args, "end_line")
			if err != nil {
				return nil, err
			}
			if hasEnd && endLine < 1 {
				return nil, domain.E(domain.CodeInvalidArgument, "end_line must be >= 1")
			}
			if hasStart && hasEnd && endLine < startLine {
				return nil, domain.E(domain.CodeInvalidArgument, "end_line must be >= start_line")
			}
			body, err := os.ReadFile(p)
			if err != nil {
				return nil, domain.E(domain.CodeToolError, err.Error())
			}
			lines := splitFileLines(string(body))
			totalLines := len(lines)
			var (
				content     string
				actualStart int
				actualEnd   int
				truncated   bool
				notice      string
			)
			if !hasStart && !hasEnd {
				outBody := body
				if len(outBody) > maxFileBytes {
					outBody = outBody[:maxFileBytes]
					truncated = true
					notice = fmt.Sprintf("truncated: content capped at %d bytes (1MiB limit) out of %d bytes (%d total lines)", maxFileBytes, len(body), totalLines)
				}
				content = string(outBody)
				if totalLines > 0 {
					actualStart = 1
					actualEnd = totalLines
				}
			} else {
				reqStart := 1
				if hasStart {
					reqStart = startLine
				}
				reqEnd := totalLines
				if hasEnd {
					reqEnd = endLine
				}
				if totalLines == 0 {
					content = ""
					if hasStart && startLine > 1 {
						truncated = true
						notice = fmt.Sprintf("truncated: requested start_line %d exceeds total_lines 0", reqStart)
					}
				} else if reqStart > totalLines {
					content = ""
					actualStart = reqStart
					actualEnd = 0
					truncated = true
					notice = fmt.Sprintf("truncated: requested start_line %d exceeds total_lines %d", reqStart, totalLines)
				} else {
					actualStart = reqStart
					actualEnd = reqEnd
					if actualEnd > totalLines {
						actualEnd = totalLines
					}
					if actualEnd < actualStart {
						actualEnd = actualStart
					}
					selected := strings.Join(lines[actualStart-1:actualEnd], "\n")
					rangeTruncated := actualStart > 1 || actualEnd < totalLines
					sizeTruncated := false
					if len(selected) > maxFileBytes {
						selected = selected[:maxFileBytes]
						sizeTruncated = true
					}
					content = selected
					truncated = rangeTruncated || sizeTruncated
					switch {
					case rangeTruncated && sizeTruncated:
						notice = fmt.Sprintf("truncated: showing lines %d-%d of %d total lines and capped at %d bytes (1MiB limit)", actualStart, actualEnd, totalLines, maxFileBytes)
					case rangeTruncated:
						notice = fmt.Sprintf("truncated: showing lines %d-%d of %d total lines", actualStart, actualEnd, totalLines)
					case sizeTruncated:
						notice = fmt.Sprintf("truncated: content capped at %d bytes (1MiB limit); file has %d total lines", maxFileBytes, totalLines)
					}
				}
			}
			rel, _ := filepath.Rel(abs, p)
			out := map[string]any{
				"path":        filepath.ToSlash(rel),
				"content":     content,
				"total_lines": totalLines,
				"start_line":  actualStart,
				"end_line":    actualEnd,
				"truncated":   truncated,
			}
			if notice != "" {
				out["notice"] = notice
			}
			return json.Marshal(out)
		},
	}
}

func writeFileSpec(abs string) Spec {
	return Spec{
		Name:            "write_file",
		Description:     "Write a UTF-8 text file inside the workspace. Requires a scoped grant or approval.",
		Required:        []string{"path", "content"},
		Readonly:        false,
		Idempotent:      true,
		SideEffectClass: "reversible",
		Parameters:      objectSchema(map[string]string{"path": "Workspace path to write.", "content": "UTF-8 text content to write."}, "path", "content"),
		ExtractResources: func(args json.RawMessage) ([]string, error) {
			p, err := resolveInside(abs, stringField(args, "path"))
			if err != nil {
				return nil, err
			}
			return []string{p}, nil
		},
		StatusProbe: func(_ context.Context, args json.RawMessage) (Probe, error) {
			p, err := resolveInside(abs, stringField(args, "path"))
			if err != nil {
				return Probe{}, nil
			}
			content := stringField(args, "content")
			body, err := os.ReadFile(p)
			if err != nil || string(body) != content {
				return Probe{Known: false}, nil
			}
			rel, _ := filepath.Rel(abs, p)
			data, err := json.Marshal(map[string]any{"path": filepath.ToSlash(rel), "bytes": len(content), "probed": true})
			if err != nil {
				return Probe{}, err
			}
			return Probe{Known: true, OK: true, Data: data}, nil
		},
		Execute: func(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
			p, err := resolveInside(abs, stringField(args, "path"))
			if err != nil {
				return nil, err
			}
			content := stringField(args, "content")
			if len(content) > maxFileBytes {
				return nil, domain.E(domain.CodeInvalidArgument, "content exceeds 1MiB")
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return nil, domain.E(domain.CodeToolError, err.Error())
			}
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				return nil, domain.E(domain.CodeToolError, err.Error())
			}
			rel, _ := filepath.Rel(abs, p)
			return json.Marshal(map[string]any{"path": filepath.ToSlash(rel), "bytes": len(content)})
		},
	}
}

func editFileSpec(abs string) Spec {
	return Spec{
		Name:            "edit_file",
		Description:     "Replace a unique string occurrence in a UTF-8 text file inside the workspace. Requires a scoped grant or approval.",
		Required:        []string{"path", "old_string", "new_string"},
		Readonly:        false,
		Idempotent:      true,
		SideEffectClass: "reversible",
		Parameters: objectSchema(map[string]string{
			"path":       "Workspace path to edit.",
			"old_string": "Exact text to replace (must occur exactly once in the file).",
			"new_string": "Replacement text.",
		}, "path", "old_string", "new_string"),
		ExtractResources: func(args json.RawMessage) ([]string, error) {
			p, err := resolveInside(abs, stringField(args, "path"))
			if err != nil {
				return nil, err
			}
			return []string{p}, nil
		},
		StatusProbe: func(_ context.Context, args json.RawMessage) (Probe, error) {
			p, err := resolveInside(abs, stringField(args, "path"))
			if err != nil {
				return Probe{}, nil
			}
			oldStr := stringField(args, "old_string")
			newStr := stringField(args, "new_string")
			if oldStr == "" || oldStr == newStr {
				return Probe{Known: false}, nil
			}
			body, err := os.ReadFile(p)
			if err != nil {
				return Probe{Known: false}, nil
			}
			text := string(body)
			if strings.Count(text, oldStr) != 0 || (newStr != "" && strings.Count(text, newStr) != 1) {
				return Probe{Known: false}, nil
			}
			rel, _ := filepath.Rel(abs, p)
			data, err := json.Marshal(map[string]any{"path": filepath.ToSlash(rel), "bytes": len(body), "probed": true})
			if err != nil {
				return Probe{}, err
			}
			return Probe{Known: true, OK: true, Data: data}, nil
		},
		Execute: func(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
			p, err := resolveInside(abs, stringField(args, "path"))
			if err != nil {
				return nil, err
			}
			oldStr := stringField(args, "old_string")
			if oldStr == "" {
				return nil, domain.E(domain.CodeInvalidArgument, "old_string must not be empty")
			}
			newStr, ok := stringFieldPresent(args, "new_string")
			if !ok {
				return nil, domain.E(domain.CodeInvalidArgument, "missing argument new_string")
			}
			body, err := os.ReadFile(p)
			if err != nil {
				return nil, domain.E(domain.CodeToolError, err.Error())
			}
			text := string(body)
			count := strings.Count(text, oldStr)
			if count == 0 {
				return nil, domain.E(domain.CodeToolError, "old_string not found in file")
			}
			if count > 1 {
				return nil, domain.E(domain.CodeToolError, fmt.Sprintf("old_string is not unique: found %d occurrences (must be 1)", count))
			}
			updated := strings.Replace(text, oldStr, newStr, 1)
			if len(updated) > maxFileBytes {
				return nil, domain.E(domain.CodeInvalidArgument, "content exceeds 1MiB")
			}
			mode := os.FileMode(0o644)
			if info, statErr := os.Stat(p); statErr == nil {
				mode = info.Mode().Perm()
			}
			if err := os.WriteFile(p, []byte(updated), mode); err != nil {
				return nil, domain.E(domain.CodeToolError, err.Error())
			}
			rel, _ := filepath.Rel(abs, p)
			return json.Marshal(map[string]any{
				"path":         filepath.ToSlash(rel),
				"bytes":        len(updated),
				"replacements": 1,
			})
		},
	}
}

func listFilesSpec(abs string) Spec {
	return Spec{
		Name:            "list_files",
		Description:     "List files inside the workspace (relative paths), skipping .git and .cloud-harness directories.",
		Required:        nil,
		Readonly:        true,
		Idempotent:      true,
		Pollable:        true,
		SideEffectClass: securityNone,
		Parameters: strictSchema(map[string]schemaProp{
			"pattern": {Type: "string", Description: "Optional glob pattern to filter workspace-relative file paths."},
		}),
		ExtractResources: func(args json.RawMessage) ([]string, error) {
			p, err := resolveFilterScope(abs, stringField(args, "pattern"))
			if err != nil {
				return nil, err
			}
			return []string{p}, nil
		},
		Execute: func(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
			pattern := stringField(args, "pattern")
			if _, err := resolveFilterScope(abs, pattern); err != nil {
				return nil, err
			}
			if err := validateGlob(pattern); err != nil {
				return nil, err
			}
			isControl := controlFiles(abs)
			files := []string{}
			total := 0
			truncated := false
			err := filepath.WalkDir(abs, func(p string, d fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return nil
				}
				if p == abs {
					return nil
				}
				if d.IsDir() && (d.Name() == ".git" || d.Name() == ".cloud-harness") {
					return filepath.SkipDir
				}
				resolved, rerr := resolveInside(abs, p)
				if rerr != nil {
					if d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				if isControl(resolved) {
					if d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				if d.IsDir() {
					return nil
				}
				info, serr := os.Stat(resolved)
				if serr != nil || info.IsDir() {
					return nil
				}
				rel, err := filepath.Rel(abs, p)
				if err != nil {
					return nil
				}
				relSlash := filepath.ToSlash(rel)
				matched, merr := matchGlob(pattern, relSlash)
				if merr != nil {
					return merr
				}
				if !matched {
					return nil
				}
				total++
				if len(files) < defaultListFiles {
					files = append(files, relSlash)
				} else {
					truncated = true
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			out := map[string]any{
				"files":     files,
				"count":     len(files),
				"total":     total,
				"truncated": truncated,
			}
			if truncated {
				out["notice"] = fmt.Sprintf("truncated: showing %d of %d matching files (cap %d)", len(files), total, defaultListFiles)
			}
			return json.Marshal(out)
		},
	}
}

func listDirSpec(abs string) Spec {
	return Spec{
		Name:            "list_dir",
		Description:     "List entries in one workspace directory without recursing, skipping .git and .cloud-harness entries.",
		Required:        nil,
		Readonly:        true,
		Idempotent:      true,
		Pollable:        true,
		SideEffectClass: securityNone,
		Parameters: strictSchema(map[string]schemaProp{
			"path": {Type: "string", Description: "Workspace-relative or absolute directory path inside the workspace (default \".\")."},
		}),
		ExtractResources: func(args json.RawMessage) ([]string, error) {
			p, err := listDirPath(abs, stringField(args, "path"))
			if err != nil {
				return nil, err
			}
			return []string{p}, nil
		},
		Execute: func(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
			p, err := listDirPath(abs, stringField(args, "path"))
			if err != nil {
				return nil, err
			}
			dirEntries, err := os.ReadDir(p)
			if err != nil {
				return nil, domain.E(domain.CodeToolError, "list_dir: "+err.Error())
			}
			rel, err := filepath.Rel(abs, p)
			if err != nil {
				return nil, domain.E(domain.CodeAuthDenied, "path is outside the workspace")
			}
			relSlash := filepath.ToSlash(rel)
			isControl := controlFiles(abs)
			entries := []map[string]any{}
			total := 0
			truncated := false
			for _, d := range dirEntries {
				name := d.Name()
				if name == ".git" || name == ".cloud-harness" {
					continue
				}
				resolved, rerr := resolveInside(abs, filepath.Join(p, name))
				if rerr != nil || isControl(resolved) {
					continue
				}
				total++
				if len(entries) >= defaultListDirEntries {
					truncated = true
					continue
				}
				entry := map[string]any{
					"name": name,
					"path": path.Join(relSlash, name),
					"type": listDirEntryType(d),
				}
				if d.Type().IsRegular() {
					if info, ierr := d.Info(); ierr == nil {
						entry["size"] = info.Size()
					}
				}
				entries = append(entries, entry)
			}
			out := map[string]any{
				"path":      relSlash,
				"entries":   entries,
				"count":     len(entries),
				"total":     total,
				"truncated": truncated,
			}
			if truncated {
				out["notice"] = fmt.Sprintf("truncated: showing %d of %d entries (cap %d)", len(entries), total, defaultListDirEntries)
			}
			return json.Marshal(out)
		},
	}
}

func listDirPath(abs, raw string) (string, error) {
	target := raw
	if target == "" {
		target = "."
	}
	p, err := resolveInside(abs, target)
	if err != nil {
		return "", err
	}
	if controlFiles(abs)(p) {
		return "", domain.E(domain.CodeAuthDenied, "path is inside the harness control directory")
	}
	return p, nil
}

func listDirEntryType(d fs.DirEntry) string {
	if d.IsDir() {
		return "directory"
	}
	if d.Type()&os.ModeSymlink != 0 {
		return "symlink"
	}
	if d.Type().IsRegular() {
		return "file"
	}
	return "other"
}

func grepSpec(abs string) Spec {
	return Spec{
		Name:            "grep",
		Description:     "Search workspace files for a regular expression or literal pattern, returning file:line:content matches.",
		Required:        []string{"pattern"},
		Readonly:        true,
		Idempotent:      true,
		Pollable:        true,
		SideEffectClass: securityNone,
		Parameters: strictSchema(map[string]schemaProp{
			"pattern":  {Type: "string", Description: "Regular expression (or literal string when literal is true) to search for."},
			"glob":     {Type: "string", Description: "Optional glob filter for workspace-relative file paths."},
			"literal":  {Type: "boolean", Description: "When true, treat pattern as literal text instead of a regular expression."},
			"max_hits": {Type: "integer", Description: "Optional maximum number of matches to return."},
		}, "pattern"),
		ExtractResources: func(args json.RawMessage) ([]string, error) {
			p, err := resolveFilterScope(abs, stringField(args, "glob"))
			if err != nil {
				return nil, err
			}
			return []string{p}, nil
		},
		Execute: func(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
			pattern := stringField(args, "pattern")
			if pattern == "" {
				return nil, domain.E(domain.CodeInvalidArgument, "pattern is required")
			}
			glob := stringField(args, "glob")
			if _, err := resolveFilterScope(abs, glob); err != nil {
				return nil, err
			}
			if err := validateGlob(glob); err != nil {
				return nil, err
			}
			literal := boolField(args, "literal")
			expr := pattern
			if literal {
				expr = regexp.QuoteMeta(pattern)
			}
			re, err := regexp.Compile(expr)
			if err != nil {
				return nil, domain.E(domain.CodeInvalidArgument, "invalid pattern: "+err.Error())
			}
			limit, hasLimit, err := intFieldOpt(args, "max_hits")
			if err != nil {
				return nil, err
			}
			if !hasLimit || limit <= 0 {
				limit = defaultGrepMaxHits
			}
			if limit > hardGrepMaxHits {
				limit = hardGrepMaxHits
			}
			isControl := controlFiles(abs)
			matches := []string{}
			total := 0
			truncated := false
			walkErr := filepath.WalkDir(abs, func(p string, d fs.DirEntry, werr error) error {
				if werr != nil {
					return nil
				}
				if p == abs {
					return nil
				}
				if d.IsDir() && (d.Name() == ".git" || d.Name() == ".cloud-harness") {
					return filepath.SkipDir
				}
				resolved, rerr := resolveInside(abs, p)
				if rerr != nil {
					if d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				if isControl(resolved) {
					if d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				if d.IsDir() {
					return nil
				}
				info, serr := os.Stat(resolved)
				if serr != nil || info.IsDir() {
					return nil
				}
				rel, err := filepath.Rel(abs, p)
				if err != nil {
					return nil
				}
				relSlash := filepath.ToSlash(rel)
				if glob != "" {
					matched, merr := matchGlob(glob, relSlash)
					if merr != nil {
						return merr
					}
					if !matched {
						return nil
					}
				}
				f, oerr := os.Open(resolved)
				if oerr != nil {
					return nil
				}
				defer f.Close()
				scanner := bufio.NewScanner(f)
				scanner.Buffer(make([]byte, 0, 64*1024), maxFileBytes)
				lineNum := 0
				for scanner.Scan() {
					lineNum++
					line := scanner.Text()
					if re.MatchString(line) {
						total++
						if len(matches) < limit {
							matches = append(matches, fmt.Sprintf("%s:%d:%s", relSlash, lineNum, line))
						} else {
							truncated = true
						}
					}
				}
				return nil
			})
			if walkErr != nil {
				return nil, walkErr
			}
			out := map[string]any{
				"matches":   matches,
				"count":     len(matches),
				"total":     total,
				"truncated": truncated,
			}
			if truncated {
				out["notice"] = fmt.Sprintf("truncated: showing %d of %d matches (cap %d)", len(matches), total, limit)
			}
			return json.Marshal(out)
		},
	}
}

func gitStatusSpec(b *Broker, abs string) Spec {
	return Spec{
		Name:            "git_status",
		Description:     "Show the working tree status of the workspace git repository.",
		Required:        nil,
		Readonly:        true,
		Idempotent:      true,
		Pollable:        true,
		SideEffectClass: securityNone,
		Parameters:      strictSchema(map[string]schemaProp{}),
		ExtractResources: func(_ json.RawMessage) ([]string, error) {
			p, err := resolveInside(abs, ".")
			if err != nil {
				return nil, err
			}
			return []string{p}, nil
		},
		Execute: func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
			stdout, truncated, notice, err := runGit(ctx, b, abs, "status", "--porcelain=v1", "-b")
			if err != nil {
				return nil, err
			}
			lines := splitFileLines(stdout)
			clean := true
			for _, l := range lines {
				if !strings.HasPrefix(l, "## ") && strings.TrimSpace(l) != "" {
					clean = false
					break
				}
			}
			out := map[string]any{
				"status":    stdout,
				"stdout":    stdout,
				"lines":     lines,
				"clean":     clean,
				"truncated": truncated,
			}
			if notice != "" {
				out["notice"] = notice
			}
			return json.Marshal(out)
		},
	}
}

func gitDiffSpec(b *Broker, abs string) Spec {
	return Spec{
		Name:            "git_diff",
		Description:     "Show unstaged or staged git diff for the workspace or a path inside the workspace.",
		Required:        nil,
		Readonly:        true,
		Idempotent:      true,
		Pollable:        true,
		SideEffectClass: securityNone,
		Parameters: strictSchema(map[string]schemaProp{
			"staged": {Type: "boolean", Description: "When true, show staged changes (--staged) instead of unstaged changes."},
			"path":   {Type: "string", Description: "Optional workspace-relative or absolute path inside the workspace to diff."},
		}),
		ExtractResources: func(args json.RawMessage) ([]string, error) {
			rawPath := strings.TrimSpace(stringField(args, "path"))
			if rawPath == "" {
				p, err := resolveInside(abs, ".")
				if err != nil {
					return nil, err
				}
				return []string{p}, nil
			}
			p, err := resolveInside(abs, rawPath)
			if err != nil {
				return nil, err
			}
			return []string{p}, nil
		},
		Execute: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
			staged, err := boolFieldOpt(args, "staged")
			if err != nil {
				return nil, err
			}
			rawPath := strings.TrimSpace(stringField(args, "path"))
			relPath := ""
			if rawPath != "" {
				resolved, err := resolveInside(abs, rawPath)
				if err != nil {
					return nil, err
				}
				rel, err := filepath.Rel(abs, resolved)
				if err != nil {
					return nil, domain.E(domain.CodeAuthDenied, "path is outside the workspace")
				}
				relPath = filepath.ToSlash(rel)
			}
			gitArgs := []string{"diff", "--no-color", "--no-ext-diff"}
			if staged {
				gitArgs = append(gitArgs, "--staged")
			}
			if relPath != "" && relPath != "." {
				gitArgs = append(gitArgs, "--", relPath)
			}
			stdout, truncated, notice, err := runGit(ctx, b, abs, gitArgs...)
			if err != nil {
				return nil, err
			}
			out := map[string]any{
				"diff":      stdout,
				"stdout":    stdout,
				"staged":    staged,
				"truncated": truncated,
			}
			if relPath != "" {
				out["path"] = relPath
			}
			if notice != "" {
				out["notice"] = notice
			}
			return json.Marshal(out)
		},
	}
}

func gitLogSpec(b *Broker, abs string) Spec {
	return Spec{
		Name:            "git_log",
		Description:     "Show recent commit history of the workspace git repository.",
		Required:        nil,
		Readonly:        true,
		Idempotent:      true,
		Pollable:        true,
		SideEffectClass: securityNone,
		Parameters: strictSchema(map[string]schemaProp{
			"limit": {Type: "integer", Description: "Optional maximum number of commits to return (default 20, max 100)."},
		}),
		ExtractResources: func(_ json.RawMessage) ([]string, error) {
			p, err := resolveInside(abs, ".")
			if err != nil {
				return nil, err
			}
			return []string{p}, nil
		},
		Execute: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
			limit, hasLimit, err := intFieldOpt(args, "limit")
			if err != nil {
				return nil, err
			}
			if hasLimit && limit < 0 {
				return nil, domain.E(domain.CodeInvalidArgument, "limit must be >= 1")
			}
			if !hasLimit || limit == 0 {
				limit = defaultGitLogLimit
			}
			if limit > hardGitLogLimit {
				limit = hardGitLogLimit
			}
			stdout, truncated, notice, err := runGit(ctx, b, abs, "log", fmt.Sprintf("-n%d", limit), "--no-color")
			if err != nil {
				return nil, err
			}
			out := map[string]any{
				"log":       stdout,
				"stdout":    stdout,
				"limit":     limit,
				"truncated": truncated,
			}
			if notice != "" {
				out["notice"] = notice
			}
			return json.Marshal(out)
		},
	}
}

func runGit(ctx context.Context, b *Broker, abs string, gitArgs ...string) (string, bool, string, error) {
	workDir, err := resolveInside(abs, ".")
	if err != nil {
		return "", false, "", err
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return "", false, "", domain.E(domain.CodeToolError, "git is not installed: "+err.Error())
	}
	cctx, cancel := context.WithTimeout(ctx, defaultCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, gitBin, gitArgs...)
	cmd.Dir = workDir
	env := b.commandEnv()
	env = append(env, "GIT_TERMINAL_PROMPT=0", "GIT_CEILING_DIRECTORIES="+gitCeilingDirs(workDir))
	cmd.Env = env
	var stdout, stderr limitedBuffer
	stdout.limit = maxFileBytes
	stderr.limit = maxFileBytes
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		errText := strings.TrimSpace(stderr.String())
		if errText == "" {
			errText = err.Error()
		}
		return "", false, "", domain.E(domain.CodeToolError, "git "+gitArgs[0]+": "+errText)
	}
	truncated := stdout.truncated
	notice := ""
	if truncated {
		notice = fmt.Sprintf("truncated: git output capped at %d bytes (1MiB limit)", maxFileBytes)
	}
	return stdout.StringWithMarker(), truncated, notice, nil
}

func gitCeilingDirs(workDir string) string {
	parent := filepath.Dir(filepath.Clean(workDir))
	if evalRoot, err := evalPath(workDir); err == nil {
		evalParent := filepath.Dir(filepath.Clean(evalRoot))
		if evalParent != parent {
			return parent + string(os.PathListSeparator) + evalParent
		}
	}
	return parent
}

func runCommandSpec(b *Broker, abs string) Spec {
	return Spec{
		Name:            "run_command",
		Description:     "Run a single allowlisted command in the workspace without a shell. On Linux, harness control files are masked from the command. Dangerous commands require approval and are not started.",
		Required:        []string{"command"},
		Timeout:         defaultCommandTimeout,
		Readonly:        true,
		Idempotent:      true,
		SideEffectClass: securityNone,
		Parameters: strictSchema(map[string]schemaProp{
			"command": {Type: "string", Description: "Command and arguments, for example: ls"},
			"cwd":     {Type: "string", Description: "Optional working directory inside the workspace."},
		}, "command"),
		ExtractResources: func(args json.RawMessage) ([]string, error) {
			return commandResourcesWithCwd(abs, stringField(args, "cwd"), stringField(args, "command"))
		},
		Execute: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
			workDir := abs
			if rawCwd := strings.TrimSpace(stringField(args, "cwd")); rawCwd != "" {
				var err error
				workDir, err = resolveInside(abs, rawCwd)
				if err != nil {
					return nil, err
				}
				info, serr := os.Stat(workDir)
				if serr != nil {
					return nil, domain.E(domain.CodeToolError, serr.Error())
				}
				if !info.IsDir() {
					return nil, domain.E(domain.CodeInvalidArgument, "cwd is not a directory")
				}
			}
			command := stringField(args, "command")
			argv, dangerous, err := parseCommandInDir(abs, workDir, command)
			if err != nil {
				return nil, err
			}
			if dangerous {
				if len(argv) == 0 || !b.isApprovedCommand(ctx, command) {
					return nil, domain.E(domain.CodeAuthDenied, "dangerous command was not approved and was not started")
				}
				resolved, err := exec.LookPath(argv[0])
				if err != nil {
					return nil, domain.E(domain.CodeToolError, "command not found: "+argv[0])
				}
				argv = append([]string{resolved}, argv[1:]...)
			}
			cctx, cancel := context.WithTimeout(ctx, defaultCommandTimeout)
			defer cancel()
			cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
			cmd.Dir = workDir
			cmd.Env = b.commandEnv()
			var stdout, stderr limitedBuffer
			stdout.limit = maxFileBytes
			stderr.limit = maxFileBytes
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			guard := guardCommand(cmd, b.CommandLimits(), abs)
			err = guard.Run()
			exitCode := 0
			if err != nil {
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					exitCode = exitErr.ExitCode()
				} else {
					exitCode = -1
				}
			}
			truncated := stdout.truncated || stderr.truncated
			out := map[string]any{
				"argv":      argv,
				"stdout":    stdout.StringWithMarker(),
				"stderr":    stderr.StringWithMarker(),
				"exit_code": exitCode,
				"truncated": truncated,
			}
			if truncated {
				out["notice"] = fmt.Sprintf("truncated: command output capped at %d bytes (1MiB limit)", maxFileBytes)
			}
			if err != nil {
				out["error"] = err.Error()
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || cctx.Err() != nil {
					raw, _ := json.Marshal(out)
					return raw, domain.E(domain.CodeToolError, err.Error())
				}
				if hit := guard.limitHit(err, stderr.String()); hit != nil {
					out["resource_limit"] = hit
					if hit.Certain {
						out["error"] = hit.Message
						raw, _ := json.Marshal(out)
						return raw, domain.E(domain.CodeResourceLimit, hit.Message)
					}
				}
			}
			return json.Marshal(out)
		},
	}
}

type limitedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (lb *limitedBuffer) Write(p []byte) (int, error) {
	total := len(p)
	limit := lb.limit
	if limit <= 0 {
		limit = maxFileBytes
	}
	rem := limit - lb.buf.Len()
	if rem > 0 {
		if len(p) > rem {
			_, _ = lb.buf.Write(p[:rem])
			lb.truncated = true
		} else {
			_, _ = lb.buf.Write(p)
		}
	} else if len(p) > 0 {
		lb.truncated = true
	}
	return total, nil
}

func (lb *limitedBuffer) String() string {
	return lb.buf.String()
}

func (lb *limitedBuffer) StringWithMarker() string {
	s := lb.buf.String()
	if lb.truncated {
		return s + truncationMarker
	}
	return s
}

func (b *Broker) commandEnv() []string {
	seen := make(map[string]bool, len(defaultCommandEnvVars))
	names := make([]string, 0, len(defaultCommandEnvVars))
	for _, k := range defaultCommandEnvVars {
		seen[k] = true
		names = append(names, k)
	}
	for _, extra := range b.commandEnvAllowList() {
		extra = strings.TrimSpace(extra)
		if extra == "" || strings.Contains(extra, "=") || seen[extra] {
			continue
		}
		seen[extra] = true
		names = append(names, extra)
	}
	env := make([]string, 0, len(names))
	for _, k := range names {
		if val, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+val)
		}
	}
	return env
}

type schemaProp struct {
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
}

func strictSchema(props map[string]schemaProp, required ...string) json.RawMessage {
	req := required
	if req == nil {
		req = []string{}
	}
	raw, _ := json.Marshal(map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             req,
		"additionalProperties": false,
	})
	return raw
}

func objectSchema(props map[string]string, required ...string) json.RawMessage {
	typed := make(map[string]schemaProp, len(props))
	for field, desc := range props {
		typed[field] = schemaProp{Type: "string", Description: desc}
	}
	return strictSchema(typed, required...)
}

func splitFileLines(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

func (b *Broker) isApprovedCommand(ctx context.Context, command string) bool {
	if b == nil || b.grants == nil {
		return false
	}
	req, ok := ctx.Value(requestKey).(Request)
	if !ok || req.InvocationID == "" || req.RunID == "" {
		return false
	}
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}
	now := req.Now
	if now.IsZero() {
		now = b.now()
	}
	grants, err := b.grants.ListGrants(ctx, req.RunID, req.SessionID, now)
	if err != nil {
		return false
	}
	for _, g := range grants {
		if g.State != "" && g.State != "active" {
			continue
		}
		if g.Tool != "run_command" {
			continue
		}
		if g.Op != "" && g.Op != "call" {
			continue
		}
		if !g.ValidUntil.IsZero() && !g.ValidUntil.After(now) {
			continue
		}
		if g.RunID == "" || g.RunID != req.RunID {
			continue
		}
		if g.SessionID != "" && g.SessionID != req.SessionID {
			continue
		}
		if g.InvocationID == "" || g.InvocationID != req.InvocationID {
			continue
		}
		if g.ResourceScope == command {
			return true
		}
	}
	return false
}

func stringField(raw json.RawMessage, key string) string {
	s, _ := stringFieldPresent(raw, key)
	return s
}

func stringFieldPresent(raw json.RawMessage, key string) (string, bool) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", false
	}
	v, ok := m[key]
	if !ok || v == nil {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func boolField(raw json.RawMessage, key string) bool {
	b, _ := boolFieldOpt(raw, key)
	return b
}

func boolFieldOpt(raw json.RawMessage, key string) (bool, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return false, nil
	}
	v, ok := m[key]
	if !ok || v == nil {
		return false, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, domain.E(domain.CodeInvalidArgument, key+" must be a boolean")
	}
	return b, nil
}

func intFieldOpt(raw json.RawMessage, key string) (int, bool, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0, false, nil
	}
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false, nil
	}
	f, ok := v.(float64)
	if !ok || f != float64(int(f)) {
		return 0, true, domain.E(domain.CodeInvalidArgument, key+" must be an integer")
	}
	return int(f), true, nil
}

// SensitivePath reports files whose contents must not be sent onward.
func SensitivePath(p string) bool {
	base := strings.ToLower(filepath.Base(p))
	switch base {
	case ".env", "credentials.json", "id_rsa", "secrets.txt":
		return true
	default:
		return strings.Contains(base, "secret") || strings.Contains(base, "credential")
	}
}

// controlFiles returns a test for whether a path returned by resolveInside is
// the harness control directory or lies inside it. list_dir, list_files and grep walk
// the workspace without a policy decision per file, so a symlink into that
// directory, or a control directory that is itself a symlink, must not expose
// what the builtin policy denies to read_file.
func controlFiles(root string) func(resolved string) bool {
	clean := filepath.Clean(root)
	dir, dirErr := resolveInside(clean, ".cloud-harness")
	return func(resolved string) bool {
		if rel, err := filepath.Rel(clean, resolved); err == nil {
			if first, _, _ := strings.Cut(filepath.ToSlash(rel), "/"); strings.EqualFold(first, ".cloud-harness") {
				return true
			}
		}
		if dirErr != nil {
			return false
		}
		rel, err := filepath.Rel(dir, resolved)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
}

func resolveInside(root, p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", domain.E(domain.CodeInvalidArgument, "path is empty")
	}
	cleanRoot := filepath.Clean(root)
	evalRoot, err := evalPath(cleanRoot)
	if err != nil {
		return "", domain.E(domain.CodeAuthDenied, "path is outside the workspace")
	}
	target := p
	if !filepath.IsAbs(target) {
		target = filepath.Join(cleanRoot, target)
	}
	target = filepath.Clean(target)
	evalTarget, err := evalPath(target)
	if err != nil {
		return "", domain.E(domain.CodeAuthDenied, "path is outside the workspace")
	}
	rel, err := filepath.Rel(evalRoot, evalTarget)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", domain.E(domain.CodeAuthDenied, "path is outside the workspace")
	}
	return filepath.Clean(filepath.Join(cleanRoot, rel)), nil
}

func evalPath(p string) (string, error) {
	return evalPathHop(filepath.Clean(p), 0)
}

func evalPathHop(p string, hops int) (string, error) {
	if hops > 40 {
		return "", os.ErrInvalid
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.Clean(resolved), nil
	}
	var tail []string
	cur := p
	for {
		fi, lerr := os.Lstat(cur)
		if lerr == nil {
			if fi.Mode()&os.ModeSymlink != 0 {
				target, rerr := os.Readlink(cur)
				if rerr != nil {
					return "", rerr
				}
				if !filepath.IsAbs(target) {
					parentDir, perr := evalPathHop(filepath.Dir(cur), hops+1)
					if perr != nil {
						return "", perr
					}
					target = filepath.Join(parentDir, target)
				}
				resolvedTarget, err := evalPathHop(filepath.Clean(target), hops+1)
				if err != nil {
					return "", err
				}
				for i := len(tail) - 1; i >= 0; i-- {
					resolvedTarget = filepath.Join(resolvedTarget, tail[i])
				}
				return filepath.Clean(resolvedTarget), nil
			}
			resolvedCur, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return "", err
			}
			for i := len(tail) - 1; i >= 0; i-- {
				resolvedCur = filepath.Join(resolvedCur, tail[i])
			}
			return filepath.Clean(resolvedCur), nil
		}
		if !os.IsNotExist(lerr) {
			return "", lerr
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", lerr
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}

func resolveFilterScope(root, filter string) (string, error) {
	trimmed := strings.TrimSpace(filter)
	if trimmed == "" {
		return resolveInside(root, ".")
	}
	if !strings.ContainsAny(trimmed, "*?[") {
		return resolveInside(root, trimmed)
	}
	slash := filepath.ToSlash(trimmed)
	for _, seg := range strings.Split(slash, "/") {
		if seg == ".." {
			return "", domain.E(domain.CodeAuthDenied, "path is outside the workspace")
		}
	}
	if filepath.IsAbs(trimmed) {
		prefix := staticGlobPrefix(slash)
		if prefix == "" {
			prefix = "/"
		}
		return resolveInside(root, prefix)
	}
	prefix := staticGlobPrefix(slash)
	if prefix == "" {
		return resolveInside(root, ".")
	}
	return resolveInside(root, prefix)
}

func staticGlobPrefix(slashPattern string) string {
	leadingSlash := strings.HasPrefix(slashPattern, "/")
	parts := strings.Split(slashPattern, "/")
	var keep []string
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if strings.ContainsAny(part, "*?[") {
			break
		}
		keep = append(keep, part)
	}
	joined := strings.Join(keep, "/")
	if leadingSlash {
		return "/" + joined
	}
	return joined
}

func validateGlob(pattern string) error {
	p := filepath.ToSlash(strings.TrimSpace(pattern))
	if p == "" {
		return nil
	}
	check := strings.ReplaceAll(p, "**", "*")
	if _, err := path.Match(check, ""); err != nil {
		return domain.E(domain.CodeInvalidArgument, "invalid glob pattern: "+err.Error())
	}
	for _, seg := range strings.Split(p, "/") {
		if _, err := path.Match(strings.ReplaceAll(seg, "**", "*"), ""); err != nil {
			return domain.E(domain.CodeInvalidArgument, "invalid glob pattern: "+err.Error())
		}
	}
	return nil
}

func matchGlob(pattern, rel string) (bool, error) {
	p := filepath.ToSlash(strings.TrimSpace(pattern))
	p = strings.TrimPrefix(p, "./")
	if p == "" || p == "*" || p == "**" || p == "**/*" {
		return true, nil
	}
	if err := validateGlob(p); err != nil {
		return false, err
	}
	if strings.Contains(p, "**") {
		re, err := globToRegex(p)
		if err != nil {
			return false, domain.E(domain.CodeInvalidArgument, "invalid glob pattern: "+err.Error())
		}
		return re.MatchString(rel), nil
	}
	if !strings.Contains(p, "/") {
		if ok, _ := path.Match(p, rel); ok {
			return true, nil
		}
		if ok, _ := path.Match(p, path.Base(rel)); ok {
			return true, nil
		}
		return false, nil
	}
	ok, err := path.Match(p, rel)
	if err != nil {
		return false, domain.E(domain.CodeInvalidArgument, "invalid glob pattern: "+err.Error())
	}
	return ok, nil
}

func globToRegex(p string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(p); {
		if strings.HasPrefix(p[i:], "**/") {
			b.WriteString("(?:.+/)?")
			i += 3
			continue
		}
		if strings.HasPrefix(p[i:], "**") {
			b.WriteString(".*")
			i += 2
			continue
		}
		switch p[i] {
		case '*':
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(p[i])))
		}
		i++
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

var (
	safeCommands  = map[string]bool{"ls": true, "pwd": true, "wc": true}
	shellCommands = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "csh": true, "tcsh": true}
)

// Dangerous reports whether a run_command must wait for approval.
func Dangerous(command string, root string) bool {
	_, dangerous, err := parseCommand(root, command)
	if err != nil {
		return true
	}
	return dangerous
}

// ValidCwd reports whether an optional cwd parameter resolves inside root.
func ValidCwd(root, cwd string) bool {
	if strings.TrimSpace(cwd) == "" {
		return true
	}
	if strings.TrimSpace(root) == "" {
		root = "."
	}
	_, err := resolveInside(root, cwd)
	return err == nil
}

func commandResources(root, command string) ([]string, error) {
	return commandResourcesWithCwd(root, "", command)
}

func commandResourcesWithCwd(root, cwd, command string) ([]string, error) {
	workDir := root
	if strings.TrimSpace(cwd) != "" {
		var err error
		workDir, err = resolveInside(root, cwd)
		if err != nil {
			return nil, err
		}
	}
	argv, dangerous, err := parseCommandInDir(root, workDir, command)
	if err != nil {
		return nil, err
	}
	if dangerous {
		return []string{strings.TrimSpace(command)}, nil
	}
	out := []string{root}
	if workDir != root {
		out = append(out, workDir)
	}
	for _, arg := range argv[1:] {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		target := arg
		if !filepath.IsAbs(target) {
			target = filepath.Join(workDir, target)
		}
		p, err := resolveInside(root, target)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func parseCommand(root, command string) ([]string, bool, error) {
	return parseCommandInDir(root, root, command)
}

func parseCommandInDir(root, workDir, command string) ([]string, bool, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, true, domain.E(domain.CodeInvalidArgument, "command is empty")
	}
	if strings.ContainsAny(command, ";&|`$<>\n") || strings.Contains(command, "$(") {
		return nil, true, nil
	}
	fields, ok := splitCommandFields(command)
	if !ok {
		return nil, true, nil
	}
	if len(fields) == 0 {
		return nil, true, domain.E(domain.CodeInvalidArgument, "command is empty")
	}
	name := fields[0]
	if strings.Contains(name, "/") || strings.Contains(name, `\`) || shellCommands[name] {
		return fields, true, nil
	}
	if !safeCommands[name] {
		return fields, true, nil
	}
	isControl := controlFiles(root)
	for _, arg := range fields[1:] {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		target := arg
		if !filepath.IsAbs(target) {
			target = filepath.Join(workDir, target)
		}
		p, err := resolveInside(root, target)
		if err != nil {
			return nil, true, err
		}
		if isControl(p) {
			return nil, true, domain.E(domain.CodeAuthDenied, "command arguments cannot target harness control files")
		}
	}
	resolved, err := exec.LookPath(name)
	if err != nil {
		return nil, true, domain.E(domain.CodeToolError, "command not found: "+name)
	}
	argv := append([]string{resolved}, fields[1:]...)
	return argv, false, nil
}

func splitCommandFields(s string) ([]string, bool) {
	var fields []string
	var cur strings.Builder
	inToken := false
	var quote rune
	for _, r := range s {
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
			inToken = true
		case ' ', '\t', '\r':
			if inToken {
				fields = append(fields, cur.String())
				cur.Reset()
				inToken = false
			}
		default:
			cur.WriteRune(r)
			inToken = true
		}
	}
	if quote != 0 {
		return nil, false
	}
	if inToken {
		fields = append(fields, cur.String())
	}
	return fields, true
}
