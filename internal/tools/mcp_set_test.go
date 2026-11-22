package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/domain"
)

func mcpProcessGone(pid int) bool {
	for i := 0; i < 50; i++ {
		if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); err != nil {
			if killErr := syscall.Kill(pid, 0); killErr != nil {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func TestMCPSetRegistersHealthyServersAndIsolatesFailures(t *testing.T) {
	bin, args := helperArgs()
	broker := NewBroker(&memCharge{b: domain.Budget{MaxToolCalls: 10}}, memGrants{})
	set := StartMCPServers(context.Background(), broker, []StdioMCPConfig{
		{Name: "missing", Command: filepath.Join(t.TempDir(), "no-such-mcp")},
		{Name: "good", Command: bin, Args: args},
		{Name: "silent", Command: "/bin/sh", Args: []string{"-c", "sleep 30"}},
		{Name: "bad name", Command: bin, Args: args},
		{Name: "good", Command: bin, Args: args},
	}, 2*time.Second)
	defer set.Close()

	status := set.Status()
	if len(status) != 5 {
		t.Fatalf("status entries = %d, want 5", len(status))
	}
	if status[0].State != MCPStateFailed || !strings.Contains(status[0].Error, "failed to start mcp server process") {
		t.Fatalf("missing command status = %+v", status[0])
	}
	if status[1].State != MCPStateRunning || len(status[1].Tools) != 3 {
		t.Fatalf("good server status = %+v", status[1])
	}
	if status[2].State != MCPStateFailed || !strings.Contains(status[2].Error, "mcp initialize failed") || !strings.Contains(status[2].Error, "timed out") {
		t.Fatalf("silent server status = %+v", status[2])
	}
	if status[3].State != MCPStateFailed || !strings.Contains(status[3].Error, "server name") {
		t.Fatalf("invalid name status = %+v", status[3])
	}
	if status[4].State != MCPStateFailed || status[4].Error != "duplicate server name" {
		t.Fatalf("duplicate name status = %+v", status[4])
	}
	for _, name := range []string{"mcp.good.echo", "mcp.good.large", "mcp.good.slow"} {
		if !broker.Has(name) {
			t.Fatalf("broker is missing %s", name)
		}
	}
	for _, spec := range broker.List() {
		if strings.HasPrefix(spec.Name, "mcp.") && !strings.HasPrefix(spec.Name, "mcp.good.") {
			t.Fatalf("failed server registered %s", spec.Name)
		}
	}

	// The registered tool stays behind the broker: no grant means approval, not execution.
	res, err := broker.Invoke(context.Background(), Request{RunID: "run_1", Tool: "mcp.good.echo", Args: json.RawMessage(`{"msg":"x"}`), ApprovalPossible: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || res.Dispatched || !res.NeedsApproval {
		t.Fatalf("ungranted mcp call = %+v, want needs approval", res)
	}

	pid := set.servers[1].adapter.ProcessPID()
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		t.Fatalf("good server pid %d is not running", pid)
	}
	if err := set.Close(); err != nil {
		t.Fatal(err)
	}
	if !mcpProcessGone(pid) {
		t.Fatalf("mcp server %d survived Close", pid)
	}
	if err := set.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestMCPSetProbeWithoutBrokerRegistersNothing(t *testing.T) {
	bin, args := helperArgs()
	set := StartMCPServers(context.Background(), nil, []StdioMCPConfig{{Name: "probe", Command: bin, Args: args, Readonly: true}}, 5*time.Second)
	defer set.Close()
	status := set.Status()
	if len(status) != 1 || status[0].State != MCPStateRunning || len(status[0].Tools) != 3 || !status[0].Readonly {
		t.Fatalf("probe status = %+v", status)
	}
}

func TestMCPSetSkipsToolNamesAlreadyRegistered(t *testing.T) {
	bin, args := helperArgs()
	broker := NewBroker(&memCharge{b: domain.Budget{MaxToolCalls: 10}}, memGrants{})
	broker.Register(Spec{Name: "mcp.dup.echo", Description: "pre-existing", Readonly: true, SideEffectClass: "none"})
	set := StartMCPServers(context.Background(), broker, []StdioMCPConfig{{Name: "dup", Command: bin, Args: args}}, 5*time.Second)
	defer set.Close()
	status := set.Status()
	if len(status[0].Tools) != 2 || len(status[0].Warnings) != 1 || !strings.Contains(status[0].Warnings[0], "mcp.dup.echo") {
		t.Fatalf("status = %+v", status[0])
	}
	if fact, _ := broker.Lookup("mcp.dup.echo"); !fact.Readonly {
		t.Fatal("discovered tool overwrote an existing registration")
	}
}

func TestMCPSetReportsAndReapsServerThatExitsAfterStartup(t *testing.T) {
	bin, args := helperArgs()
	set := StartMCPServers(context.Background(), nil, []StdioMCPConfig{{Name: "crash", Command: bin, Args: args}}, 5*time.Second)
	defer set.Close()
	pid := set.servers[0].adapter.ProcessPID()
	if pid <= 0 {
		t.Fatalf("invalid pid %d", pid)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	// A reaped child has no /proc entry; an unreaped zombie still does.
	if !mcpProcessGone(pid) {
		t.Fatalf("exited mcp server %d was left as a zombie", pid)
	}
	status := set.Status()
	if status[0].State != MCPStateFailed || !strings.Contains(status[0].Error, "exited after startup") {
		t.Fatalf("status after crash = %+v", status[0])
	}
}
