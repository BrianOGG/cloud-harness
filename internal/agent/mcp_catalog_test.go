package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/tools"
)

func TestMCPSearchUsesFilteredRegistry(t *testing.T) {
	b := tools.NewBroker(nil, nil)
	m := Manager{Broker: b, Depth: 1, AllowedTools: []string{searchMCPName}}
	for i := range 40 {
		name := fmt.Sprintf("mcp.local.search_%02d", i)
		m.AllowedTools = append(m.AllowedTools, name)
		b.Register(tools.Spec{Name: name, Description: "indexed lookup", Readonly: true, SideEffectClass: "none"})
	}
	// Neither a child-excluded tool nor a plan-excluded tool can leak in a
	// search response or be reintroduced by a stale checkpoint selection.
	childHidden := "mcp.local.search_outside"
	planHidden := "mcp.local.search_write"
	b.Register(tools.Spec{Name: childHidden, Readonly: true, SideEffectClass: "none"})
	b.Register(tools.Spec{Name: planHidden, SideEffectClass: "reversible"})
	m.AllowedTools = append(m.AllowedTools, planHidden)
	m.ensureTools()
	plan := m.registerPlanTools()
	visible := m.filteredRegistry(plan)
	ctx := context.WithValue(context.Background(), mcpCatalogKey{}, visible)
	raw, err := searchMCPTools(ctx, json.RawMessage(`{"query":"indexed lookup","offset":32}`))
	var page mcpPage
	if err != nil || json.Unmarshal(raw, &page) != nil || len(page.Names) != 8 || page.Names[0] != "mcp.local.search_32" || page.NextOffset != nil {
		t.Fatal("wrong search page")
	}
	for _, hidden := range []string{childHidden, planHidden} {
		raw, _ := searchMCPTools(ctx, json.RawMessage(fmt.Sprintf(`{"query":%q}`, hidden)))
		var matches mcpPage
		_ = json.Unmarshal(raw, &matches)
		if len(matches.Names) != 0 {
			t.Fatal("search exposed hidden tool")
		}
		for _, def := range m.offerMCPTools(plan, []string{hidden}) {
			if def.Name == hidden {
				t.Fatal("stale selection exposed hidden tool")
			}
		}
	}
}
