//go:build linux

package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

func TestFullAutoShellKeepsLimitsAndControlMask(t *testing.T) {
	root := t.TempDir()
	control := filepath.Join(root, ".cloud-harness")
	if err := os.MkdirAll(control, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(control, "sentinel"), []byte("CONTROL_MASK_SENTINEL"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := "cat /proc/self/limits | cat; cat .cloud-harness/sentinel 2>/dev/null; printf done"
	b := NewBroker(&memCharge{b: domain.Budget{MaxToolCalls: 1}}, &memGrants{grants: []domain.Grant{
		{Tool: "run_command", RunID: "run", InvocationID: "inv", ResourceScope: command, State: "active"},
	}})
	if err := RegisterLocal(b, root); err != nil {
		t.Fatal(err)
	}
	b.SetCommandLimits(CommandLimits{OpenFiles: u64(64)})
	args, _ := json.Marshal(map[string]string{"command": command})
	res, err := b.Invoke(t.Context(), Request{RunID: "run", InvocationID: "inv", Tool: "run_command", Args: args, ApprovalMode: "full-auto"})
	if err != nil || !res.OK {
		t.Fatalf("shell=%+v err=%v", res, err)
	}
	var output limitedResult
	if err := json.Unmarshal(res.Data, &output); err != nil {
		t.Fatal(err)
	}
	if output.ExitCode != 0 || !strings.HasSuffix(output.Stdout, "done") || strings.Contains(output.Stdout+output.Stderr, "CONTROL_MASK_SENTINEL") {
		t.Fatalf("shell mask failed: %+v", output)
	}
	if got := softLimits(t, output.Stdout)["Max open files"]; got != wantSoft(t, syscall.RLIMIT_NOFILE, 64) {
		t.Fatalf("shell descendant open-file limit=%q", got)
	}
}
