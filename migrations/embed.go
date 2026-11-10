// Package migrations embeds the ordered SQLite schema scripts.
package migrations

import (
	"embed"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

//go:embed *.sql
var FS embed.FS

// File is one versioned script.
type File struct {
	Version int
	Name    string
	SQL     string
}

// Files returns scripts in version order.
func Files() ([]File, error) {
	entries, err := fs.ReadDir(FS, ".")
	if err != nil {
		return nil, err
	}
	var out []File
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		ver, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			return nil, err
		}
		body, err := FS.ReadFile(name)
		if err != nil {
			return nil, err
		}
		out = append(out, File{Version: ver, Name: name, SQL: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}
