package architecture

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportBoundaries(t *testing.T) {
	root := moduleRoot(t)
	var bad []string
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		pkg := filepath.ToSlash(filepath.Dir(rel))
		for _, imp := range file.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if reason := forbidden(pkg, p); reason != "" {
				bad = append(bad, rel+" imports "+p+" ("+reason+")")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) > 0 {
		t.Fatalf("import boundary:\n%s", strings.Join(bad, "\n"))
	}
}

func forbidden(pkg, imp string) string {
	switch {
	case pkg == "internal/domain" && (strings.Contains(imp, ".") || strings.Contains(imp, "cloud-harness")):
		return "domain must stay on the standard library"
	case strings.Contains(imp, "modernc.org/sqlite") && pkg != "internal/storage/sqlite":
		return "sqlite driver is confined to the store"
	case strings.Contains(imp, "database/sql") && pkg != "internal/storage/sqlite":
		return "database handles stay inside the store"
	case strings.HasPrefix(imp, "net/http") && pkg != "internal/transport" && pkg != "internal/provider" && pkg != "internal/eval" && pkg != "internal/notify":
		return "http stays in transport, provider, evaluation, and notification adapters"
	case strings.Contains(imp, "go.starlark.net") && pkg != "internal/program":
		return "starlark stays in the worker"
	case strings.Contains(imp, "github.com/openai") || strings.Contains(imp, "github.com/anthropics"):
		return "provider SDK types are not imported in M0"
	default:
		return ""
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
