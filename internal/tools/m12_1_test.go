package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

// list_files and grep take no per-file policy decision, so they must not walk
// into the harness control directory through a symlink either.
func TestSearchToolsSkipControlFilesBehindSymlinks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, root string)
	}{
		{"file symlink into the control directory", func(t *testing.T, root string) {
			writeControlFixture(t, filepath.Join(root, ".cloud-harness", "policy.json"))
			if err := os.Symlink(filepath.Join(".cloud-harness", "policy.json"), filepath.Join(root, "alias.txt")); err != nil {
				t.Fatal(err)
			}
		}},
		{"control directory is a symlink", func(t *testing.T, root string) {
			writeControlFixture(t, filepath.Join(root, "state", "policy.json"))
			if err := os.Symlink("state", filepath.Join(root, ".cloud-harness")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tc.setup(t, root)
			if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("FIND_ME in notes\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			grants := &memGrants{grants: []domain.Grant{
				{ID: "g1", Tool: "grep", Op: "call", State: "active", RunID: "run", ResourceScope: root},
				{ID: "g2", Tool: "list_files", Op: "call", State: "active", RunID: "run", ResourceScope: root},
			}}
			b := NewBroker(&memCharge{b: domain.Budget{MaxToolCalls: 20}}, grants)
			if err := RegisterLocal(b, root); err != nil {
				t.Fatal(err)
			}

			res, err := b.Invoke(context.Background(), Request{RunID: "run", Tool: "grep", Args: json.RawMessage(`{"pattern":"FIND_ME"}`)})
			if err != nil || !res.OK {
				t.Fatalf("grep: %+v %v", res, err)
			}
			var hits struct {
				Matches []string `json:"matches"`
			}
			if err := json.Unmarshal(res.Data, &hits); err != nil {
				t.Fatal(err)
			}
			if len(hits.Matches) != 1 || hits.Matches[0] != "notes.txt:1:FIND_ME in notes" {
				t.Fatalf("grep matches = %v", hits.Matches)
			}

			res, err = b.Invoke(context.Background(), Request{RunID: "run", Tool: "list_files", Args: json.RawMessage(`{}`)})
			if err != nil || !res.OK {
				t.Fatalf("list_files: %+v %v", res, err)
			}
			var listed struct {
				Files []string `json:"files"`
			}
			if err := json.Unmarshal(res.Data, &listed); err != nil {
				t.Fatal(err)
			}
			if strings.Join(listed.Files, ",") != "notes.txt" {
				t.Fatalf("list_files = %v", listed.Files)
			}
		})
	}
}

func writeControlFixture(t *testing.T, file string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("FIND_ME in control file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
