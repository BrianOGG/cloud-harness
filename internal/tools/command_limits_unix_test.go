//go:build linux || darwin

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

type limitedResult struct {
	Stdout        string    `json:"stdout"`
	Stderr        string    `json:"stderr"`
	ExitCode      int       `json:"exit_code"`
	Error         string    `json:"error"`
	ResourceLimit *limitHit `json:"resource_limit"`
}

// limitHelpers puts one shell script per name on PATH. run_command rejects
// shell syntax in the command itself, so tests run scripts by name.
func limitHelpers(t *testing.T, scripts map[string]string) {
	t.Helper()
	binDir := t.TempDir()
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// runLimited runs one approved command through the broker under limits.
func runLimited(t *testing.T, ctx context.Context, root string, limits CommandLimits, command string) (Result, limitedResult) {
	t.Helper()
	charge := &memCharge{b: domain.Budget{MaxToolCalls: 20}}
	grants := &memGrants{grants: []domain.Grant{
		{ID: "g_root", Tool: "run_command", Op: "call", State: "active", RunID: "run", ResourceScope: root},
		{ID: "g_cmd", Tool: "run_command", Op: "call", State: "active", RunID: "run", InvocationID: "inv", ResourceScope: command},
	}}
	b := NewBroker(charge, grants)
	if err := RegisterLocal(b, root); err != nil {
		t.Fatal(err)
	}
	b.SetCommandLimits(limits)
	res, err := b.Invoke(ctx, Request{
		RunID: "run", InvocationID: "inv", Tool: "run_command",
		Args: json.RawMessage(fmt.Sprintf(`{"command":%q}`, command)),
	})
	if err != nil || !res.Dispatched {
		t.Fatalf("%s: not dispatched: %+v %v", command, res, err)
	}
	var out limitedResult
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatalf("%s: %v (result=%+v)", command, err, res)
	}
	return res, out
}
