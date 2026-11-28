package agent

import "github.com/BrianOGG/cloud-harness/internal/domain"

// planToolRegistry is the run's model-facing registry in plan mode. A nil
// registry selects the normal tool catalog. The shared broker stays untouched.
type planToolRegistry map[string]bool

func (m *Manager) registerPlanTools() planToolRegistry {
	registry := make(planToolRegistry)
	if m.Broker == nil {
		return registry
	}
	for _, spec := range m.Broker.Descriptors() {
		switch spec.Name {
		// Existing permission metadata treats these as readonly. They can
		// still execute commands, start runs, or replace a run's plan.
		case "run_command", domain.ToolExecuteProgram, domain.ToolDelegateTask, domain.ToolUpdatePlan:
			continue
		}
		if fact, ok := m.Broker.Lookup(spec.Name); ok && !sideEffectTool(fact) {
			registry[spec.Name] = true
		}
	}
	return registry
}

func (r planToolRegistry) unknown(name string) *domain.Error {
	if r != nil && !r[name] {
		return domain.E(domain.CodeToolError, "unknown tool "+name)
	}
	return nil
}

// Pair the registry's unknown-tool error with a call rejected before dispatch.
func (r planToolRegistry) markUnknownTools(batch *domain.Batch) {
	for i := range batch.Invocations {
		inv := &batch.Invocations[i]
		if inv.Status == domain.InvDenied {
			if err := r.unknown(inv.Name); err != nil {
				inv.Result = &domain.CallResult{Error: err.WithRef(inv.ProviderCallID)}
			}
		}
	}
}
