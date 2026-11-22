package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

func TestMCPAdapterMapsSpecAndDeniedCall(t *testing.T) {
	spec := Spec{
		Name:        "read_file",
		Description: "Read a workspace file.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		Readonly:    true,
	}
	tool := ToMCPTool(spec)
	if tool.Name != "read_file" || !strings.Contains(string(tool.InputSchema), "path") {
		t.Fatalf("mapped %#v", tool)
	}
	empty := ToMCPTool(Spec{Name: "echo"})
	if !strings.Contains(string(empty.InputSchema), "object") {
		t.Fatalf("schema %s", empty.InputSchema)
	}
	adapter := StaticMCP{Label: "local-map", Specs: []Spec{spec}}
	if adapter.Protocol() != "mcp" || adapter.ServerLabel() != "local-map" {
		t.Fatalf("adapter %s %s", adapter.Protocol(), adapter.ServerLabel())
	}
	got, err := adapter.Discover(context.Background())
	if err != nil || len(got) != 1 || got[0].Name != "read_file" {
		t.Fatalf("discover %+v %v", got, err)
	}
	denied := MCPErrorResult(domain.E(domain.CodeAuthDenied, "grant does not cover this path"))
	raw, _ := json.Marshal(denied)
	if !denied.IsError || denied.Code != domain.CodeAuthDenied || !strings.Contains(string(raw), "auth_denied") {
		t.Fatalf("denied %#v", denied)
	}
	if strings.Contains(string(raw), `"ok":true`) {
		t.Fatal("denial looked like success")
	}
}
