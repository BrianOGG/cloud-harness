package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/domain"
)

func TestWorkspaceToolsDenyEscapeAndDanger(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(root, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	charge := &memCharge{b: domain.Budget{MaxToolCalls: 10}}
	grants := &memGrants{grants: []domain.Grant{{
		ID: "g", Tool: "read_file", Op: "call", State: "active", RunID: "run",
		ResourceScope: root,
	}}}
	b := NewBroker(charge, grants)
	if err := RegisterLocal(b, root); err != nil {
		t.Fatal(err)
	}
	ok, err := b.Invoke(context.Background(), Request{RunID: "run", Tool: "read_file", Args: json.RawMessage(`{"path":"note.txt"}`)})
	if err != nil || !ok.OK || !ok.Dispatched {
		t.Fatalf("read %+v %v", ok, err)
	}
	escaped, err := b.Invoke(context.Background(), Request{RunID: "run", Tool: "read_file", Args: json.RawMessage(`{"path":"../secret"}`)})
	if err != nil || escaped.Dispatched || escaped.Error == nil || escaped.Error.Code != domain.CodeAuthDenied {
		t.Fatalf("escape %+v %v", escaped, err)
	}
	write, err := b.Authorize(context.Background(), Request{
		RunID: "run", Tool: "write_file", ApprovalPossible: true,
		Args: json.RawMessage(`{"path":"out.txt","content":"x"}`),
	})
	if err != nil || !write.NeedsApproval || write.Dispatched {
		t.Fatalf("write should wait for approval: %+v %v", write, err)
	}
	if _, err := os.Stat(filepath.Join(root, "out.txt")); !os.IsNotExist(err) {
		t.Fatal("write ran without a grant")
	}
	danger, err := b.Authorize(context.Background(), Request{
		RunID: "run", Tool: "run_command", ApprovalPossible: true,
		Args: json.RawMessage(`{"command":"rm -rf sentinel"}`), Now: time.Now(),
	})
	if err != nil || danger.Dispatched || danger.NeedsApproval && danger.OK {
		t.Fatalf("danger %+v %v", danger, err)
	}
	if danger.Error == nil {
		t.Fatal("dangerous command was allowed")
	}
	// A direct invoke still must not start a dangerous command, even if a grant exists.
	grants.grants = append(grants.grants, domain.Grant{
		ID: "g2", Tool: "run_command", Op: "call", State: "active", RunID: "run", ResourceScope: root,
	})
	ran, err := b.Invoke(context.Background(), Request{
		RunID: "run", Tool: "run_command", Args: json.RawMessage(`{"command":"rm -rf sentinel"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ran.OK {
		t.Fatal("dangerous command succeeded")
	}
	body, err := os.ReadFile(sentinel)
	if err != nil || string(body) != "keep" {
		t.Fatalf("sentinel changed: %q %v", body, err)
	}
}

func TestWriteProbeConfirmsWithoutWriting(t *testing.T) {
	root := t.TempDir()
	b := NewBroker(&memCharge{b: domain.Budget{MaxToolCalls: 1}}, &memGrants{})
	if err := RegisterLocal(b, root); err != nil {
		t.Fatal(err)
	}
	args := json.RawMessage(`{"path":"out.txt","content":"x"}`)
	missing, err := b.Probe(context.Background(), Request{Tool: "write_file", Args: args})
	if err != nil || missing.Known {
		t.Fatalf("missing file %+v %v", missing, err)
	}
	if _, err := os.Stat(filepath.Join(root, "out.txt")); !os.IsNotExist(err) {
		t.Fatal("probe created the file")
	}
	if err := os.WriteFile(filepath.Join(root, "out.txt"), []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	other, err := b.Probe(context.Background(), Request{Tool: "write_file", Args: args})
	if err != nil || other.Known {
		t.Fatalf("mismatched content confirmed %+v %v", other, err)
	}
	if err := os.WriteFile(filepath.Join(root, "out.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	hit, err := b.Probe(context.Background(), Request{Tool: "write_file", Args: args})
	if err != nil || !hit.Known || !hit.OK || !bytes.Contains(hit.Data, []byte(`"probed":true`)) {
		t.Fatalf("probe %+v %v", hit, err)
	}
	body, err := os.ReadFile(filepath.Join(root, "out.txt"))
	if err != nil || string(body) != "x" {
		t.Fatalf("probe changed the file: %q %v", body, err)
	}
	cmd, err := b.Probe(context.Background(), Request{Tool: "run_command", Args: json.RawMessage(`{"command":"ls"}`)})
	if err != nil || cmd.Known {
		t.Fatalf("command probe %+v %v", cmd, err)
	}
}
