package main

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/agents"
)

func TestM1021DeclarativeAgentsMilestone(t *testing.T) {
	workspace := t.TempDir()
	writeAgentFixture(t, workspace, "locator", "---\ndescription: Locate code in this workspace\ntools: [read_file, grep]\nmodel: fixture-model\nmax_model_calls: 4\nmax_tool_calls: 8\n---\nLocate definitions and cite files.\n\n---\nPreserve this body separator.")
	writeAgentFixture(t, workspace, "planner", "---\ndescription: Plan without tools\ntools: []\n---\nProduce a concise plan.")
	writeAgentFixture(t, workspace, "explorer", "---\nname: explorer\ndescription: Workspace explorer override\n---\nExplore using workspace instructions.")
	before := agentWorkspaceState(t, workspace)
	for _, verb := range [][]string{nil, {"list"}} {
		args := append(slices.Clone(verb), "--workspace", workspace)
		code, out, stderr := invokeAgents(args...)
		if code != 0 || stderr != "" {
			t.Fatalf("milestone list code=%d stdout=%s stderr=%s", code, out, stderr)
		}
		assertAgentsContains(t, out, "explorer", "Workspace explorer override", "true", "locator", "planner", "reviewer", "builtin", "file", "inherit", "none", "fixture-model")
	}
	code, out, stderr := invokeAgents("list", "--workspace", workspace, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("milestone list JSON code=%d stdout=%s stderr=%s", code, out, stderr)
	}
	var defs []agents.Definition
	if err := json.Unmarshal([]byte(out), &defs); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, def := range defs {
		names = append(names, def.Name)
	}
	if !slices.Equal(names, []string{"explorer", "locator", "planner", "reviewer"}) {
		t.Fatalf("milestone registry names = %v", names)
	}
	if defs[0].Source != "file" || !defs[0].OverridesBuiltin || defs[0].Tools != nil || defs[0].System != "Explore using workspace instructions." {
		t.Fatalf("milestone override = %+v", defs[0])
	}
	if defs[1].OverridesBuiltin || defs[1].Model != "fixture-model" || defs[1].MaxModelCalls != 4 || defs[1].MaxToolCalls != 8 || !slices.Equal(defs[1].Tools, []string{"read_file", "grep"}) {
		t.Fatalf("milestone custom = %+v", defs[1])
	}
	if defs[2].Tools == nil || len(defs[2].Tools) != 0 || defs[3].Source != "builtin" {
		t.Fatalf("milestone empty tools/builtin = %+v", defs)
	}
	for _, def := range defs {
		code, out, stderr := invokeAgents("show", def.Name, "--workspace", workspace)
		if code != 0 || stderr != "" || !strings.Contains(out, def.System) {
			t.Fatalf("milestone show %s code=%d stdout=%s stderr=%s", def.Name, code, out, stderr)
		}
		code, out, stderr = invokeAgents("show", def.Name, "--workspace", workspace, "--json")
		if code != 0 || stderr != "" {
			t.Fatalf("milestone show JSON %s code=%d stdout=%s stderr=%s", def.Name, code, out, stderr)
		}
		var shown agents.Definition
		if err := json.Unmarshal([]byte(out), &shown); err != nil {
			t.Fatal(err)
		}
		if shown.Name != def.Name || shown.System != def.System || shown.Source != def.Source || shown.OverridesBuiltin != def.OverridesBuiltin {
			t.Fatalf("milestone show mismatch: shown=%+v listed=%+v", shown, def)
		}
	}
	if after := agentWorkspaceState(t, workspace); !reflect.DeepEqual(before, after) {
		t.Fatal("milestone agents commands changed fixture workspace")
	}
}
