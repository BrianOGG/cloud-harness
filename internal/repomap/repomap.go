// Package repomap builds a small, deterministic symbol map for a workspace.
package repomap

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	// DefaultMaxBytes is the default rendered map size limit.
	DefaultMaxBytes = 64 * 1024
)

// Symbol is a top-level definition found in a source file.
type Symbol struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Receiver string `json:"receiver,omitempty"`
	Line     int    `json:"line,omitempty"`
}

// File is one source file in a map. Package is populated for Go files.
type File struct {
	Path    string   `json:"path"`
	Lines   int      `json:"lines"`
	Package string   `json:"package,omitempty"`
	Symbols []Symbol `json:"symbols,omitempty"`
}

// Diagnostic records a file that could not be read or parsed. A bad file does
// not prevent the rest of the workspace from being mapped.
type Diagnostic struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// Map is the structured result of scanning a workspace.
type Map struct {
	Files       []File       `json:"files"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}

// Rendered is the deterministic plain-text representation of a Map.
type Rendered struct {
	Text          string
	Truncated     bool
	OmittedFiles  int
	LimitExceeded bool
}

// Scan walks root and returns its supported source files in path order.
// Supported source files are Go, Python, JavaScript, TypeScript, Rust, Java,
// and Ruby files. The root must exist and be a directory.
func Scan(root string) (Map, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return Map{}, err
	}
	info, err := os.Stat(absRoot)
	if err != nil {
		return Map{}, err
	}
	if !info.IsDir() {
		return Map{}, fmt.Errorf("workspace %q is not a directory", root)
	}

	result := Map{Files: make([]File, 0)}
	err = filepath.WalkDir(absRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			result.Diagnostics = append(result.Diagnostics, Diagnostic{Path: relativePath(absRoot, path), Error: walkErr.Error()})
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if path != absRoot && skippedDirectory(entry.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if !supportedExtension(ext) {
			return nil
		}
		file, diagnostic := parseFile(absRoot, path, ext)
		if diagnostic != nil {
			result.Diagnostics = append(result.Diagnostics, *diagnostic)
			return nil
		}
		result.Files = append(result.Files, *file)
		return nil
	})
	if err != nil {
		return Map{}, err
	}

	sort.Slice(result.Files, func(i, j int) bool { return result.Files[i].Path < result.Files[j].Path })
	sort.Slice(result.Diagnostics, func(i, j int) bool {
		if result.Diagnostics[i].Path != result.Diagnostics[j].Path {
			return result.Diagnostics[i].Path < result.Diagnostics[j].Path
		}
		return result.Diagnostics[i].Error < result.Diagnostics[j].Error
	})
	return result, nil
}

// Render formats a map as one tab-separated line per file. If maxBytes is
// positive, complete file lines are retained in path order until the limit is
// reached; a final marker reports the number of omitted files. A non-positive
// value uses DefaultMaxBytes.
func (m Map) Render(maxBytes int) Rendered {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	lines := make([]string, len(m.Files))
	for i, file := range m.Files {
		lines[i] = formatFile(file)
	}
	if len(lines) == 0 {
		return Rendered{}
	}

	total := 0
	for _, line := range lines {
		total += len(line)
	}
	if total <= maxBytes {
		return Rendered{Text: strings.Join(lines, "")}
	}

	used := 0
	included := 0
	for i, line := range lines {
		omitted := len(lines) - i - 1
		markerBytes := 0
		if omitted > 0 {
			markerBytes = len(truncationMarker(omitted))
		}
		if used+len(line)+markerBytes > maxBytes {
			break
		}
		used += len(line)
		included = i + 1
	}
	marker := truncationMarker(len(lines) - included)
	text := strings.Join(lines[:included], "") + marker
	return Rendered{
		Text:          text,
		Truncated:     true,
		OmittedFiles:  len(lines) - included,
		LimitExceeded: len(text) > maxBytes,
	}
}

func formatFile(file File) string {
	parts := []string{file.Path, fmt.Sprintf("lines=%d", file.Lines)}
	if file.Package != "" {
		parts = append(parts, "package="+file.Package)
	}
	if len(file.Symbols) > 0 {
		symbols := make([]string, 0, len(file.Symbols))
		for _, symbol := range file.Symbols {
			name := symbol.Name
			if symbol.Receiver != "" {
				name = "(" + symbol.Receiver + ")." + name
			}
			symbols = append(symbols, symbol.Kind+":"+name)
		}
		parts = append(parts, "symbols="+strings.Join(symbols, ","))
	}
	return strings.Join(parts, "\t") + "\n"
}

func truncationMarker(omitted int) string {
	return fmt.Sprintf("# TRUNCATED omitted_files=%d\n", omitted)
}

func parseFile(root, path, ext string) (*File, *Diagnostic) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &Diagnostic{Path: relativePath(root, path), Error: err.Error()}
	}
	file := &File{Path: relativePath(root, path), Lines: lineCount(data)}
	if ext == ".go" {
		parsed, err := parseGoFile(file.Path, data, file)
		if err != nil {
			return nil, &Diagnostic{Path: file.Path, Error: err.Error()}
		}
		return parsed, nil
	}
	file.Symbols = parseLightweight(data, ext)
	return file, nil
}

func parseGoFile(path string, data []byte, file *File) (*File, error) {
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, path, data, 0)
	if err != nil {
		return nil, err
	}
	file.Package = parsed.Name.Name
	type positioned struct {
		symbol Symbol
		pos    token.Pos
	}
	positionedSymbols := make([]positioned, 0)
	for _, declaration := range parsed.Decls {
		switch decl := declaration.(type) {
		case *ast.GenDecl:
			if decl.Tok.String() != "type" {
				continue
			}
			for _, spec := range decl.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				positionedSymbols = append(positionedSymbols, positioned{
					symbol: Symbol{Kind: "type", Name: typeSpec.Name.Name, Line: fset.Position(typeSpec.Pos()).Line},
					pos:    typeSpec.Pos(),
				})
			}
		case *ast.FuncDecl:
			symbol := Symbol{Kind: "func", Name: decl.Name.Name, Line: fset.Position(decl.Pos()).Line}
			if decl.Recv != nil && len(decl.Recv.List) > 0 {
				symbol.Kind = "method"
				symbol.Receiver = receiverType(fset, decl.Recv.List[0].Type)
			}
			positionedSymbols = append(positionedSymbols, positioned{symbol: symbol, pos: decl.Pos()})
		}
	}
	sort.SliceStable(positionedSymbols, func(i, j int) bool { return positionedSymbols[i].pos < positionedSymbols[j].pos })
	seen := make(map[string]struct{}, len(positionedSymbols))
	for _, item := range positionedSymbols {
		key := item.symbol.Kind + "\x00" + item.symbol.Receiver + "\x00" + item.symbol.Name
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		file.Symbols = append(file.Symbols, item.symbol)
	}
	return file, nil
}

func receiverType(fset *token.FileSet, expr ast.Expr) string {
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, expr); err != nil {
		return "?"
	}
	return strings.TrimSpace(buf.String())
}

type lightweightPattern struct {
	kind string
	re   *regexp.Regexp
}

var lightweightPatterns = map[string][]lightweightPattern{
	".py": {
		{kind: "def", re: regexp.MustCompile(`^(?:async[ \t]+)?def[ \t]+([A-Za-z_][A-Za-z0-9_]*)`)},
		{kind: "class", re: regexp.MustCompile(`^class[ \t]+([A-Za-z_][A-Za-z0-9_]*)`)},
	},
	".js": {
		{kind: "func", re: regexp.MustCompile(`^(?:(?:export|default|declare|async)[ \t]+)*function[ \t]+([A-Za-z_$][A-Za-z0-9_$]*)`)},
		{kind: "class", re: regexp.MustCompile(`^(?:(?:export|default|declare|abstract)[ \t]+)*class[ \t]+([A-Za-z_$][A-Za-z0-9_$]*)`)},
	},
	".ts": {
		{kind: "func", re: regexp.MustCompile(`^(?:(?:export|default|declare|async)[ \t]+)*function[ \t]+([A-Za-z_$][A-Za-z0-9_$]*)`)},
		{kind: "class", re: regexp.MustCompile(`^(?:(?:export|default|declare|abstract)[ \t]+)*class[ \t]+([A-Za-z_$][A-Za-z0-9_$]*)`)},
	},
	".rs": {
		{kind: "func", re: regexp.MustCompile(`^(?:(?:pub|unsafe|async|const|extern)[ \t]+)*fn[ \t]+([A-Za-z_][A-Za-z0-9_]*)`)},
		{kind: "struct", re: regexp.MustCompile(`^(?:(?:pub)[ \t]+)*struct[ \t]+([A-Za-z_][A-Za-z0-9_]*)`)},
		{kind: "enum", re: regexp.MustCompile(`^(?:(?:pub)[ \t]+)*enum[ \t]+([A-Za-z_][A-Za-z0-9_]*)`)},
		{kind: "trait", re: regexp.MustCompile(`^(?:(?:pub)[ \t]+)*trait[ \t]+([A-Za-z_][A-Za-z0-9_]*)`)},
	},
	".java": {
		{kind: "class", re: regexp.MustCompile(`^(?:(?:public|private|protected|static|final|abstract|sealed|non-sealed)[ \t]+)*(?:class|interface|enum|record)[ \t]+([A-Za-z_$][A-Za-z0-9_$]*)`)},
	},
	".rb": {
		{kind: "def", re: regexp.MustCompile(`^(?:(?:private|protected|public)[ \t]+)?def[ \t]+([A-Za-z_][A-Za-z0-9_!?=]*)`)},
		{kind: "class", re: regexp.MustCompile(`^(?:class|module)[ \t]+([A-Za-z_][A-Za-z0-9_:]*)`)},
	},
}

func parseLightweight(data []byte, ext string) []Symbol {
	patterns := lightweightPatterns[ext]
	if len(patterns) == 0 {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	symbols := make([]Symbol, 0)
	seen := make(map[string]struct{})
	for lineNumber, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		if len(line) == 0 || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		for _, pattern := range patterns {
			match := pattern.re.FindStringSubmatch(line)
			if len(match) != 2 {
				continue
			}
			name := match[1]
			key := pattern.kind + "\x00" + name
			if _, ok := seen[key]; ok {
				break
			}
			seen[key] = struct{}{}
			symbols = append(symbols, Symbol{Kind: pattern.kind, Name: name, Line: lineNumber + 1})
			break
		}
	}
	return symbols
}

func lineCount(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	count := bytes.Count(data, []byte{'\n'})
	if data[len(data)-1] != '\n' {
		count++
	}
	return count
}

func relativePath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

func supportedExtension(ext string) bool {
	if ext == ".go" {
		return true
	}
	_, ok := lightweightPatterns[ext]
	return ok
}

func skippedDirectory(name string) bool {
	switch name {
	case ".git", ".cloud-harness", "node_modules", "vendor", "dist", "__pycache__":
		return true
	default:
		return false
	}
}
