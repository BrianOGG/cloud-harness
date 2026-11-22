package tools

import (
	"context"
	"encoding/json"

	contextpkg "github.com/BrianOGG/cloud-harness/internal/context"
	"github.com/BrianOGG/cloud-harness/internal/domain"
)

const LoadSkillName = "load_skill"

// LoadSkillSpec builds the read-only load_skill tool for one workspace. The
// same context loader is used by the CLI /skill command.
func LoadSkillSpec(workspace string) Spec {
	return Spec{
		Name:            LoadSkillName,
		Description:     "Load the full text of one workspace or global SKILL.md by name.",
		Required:        []string{"name"},
		Readonly:        true,
		Idempotent:      true,
		SideEffectClass: "none",
		Parameters:      json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"Skill name from the available skills list."}},"required":["name"],"additionalProperties":false}`),
		Execute: func(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
			var body struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(args, &body); err != nil || body.Name == "" {
				return nil, domain.E(domain.CodeInvalidArgument, "load_skill requires a name")
			}
			skill, err := contextpkg.LoadSkill(workspace, body.Name)
			if err != nil {
				return nil, domain.E(domain.CodeToolError, err.Error())
			}
			return json.Marshal(skill)
		},
	}
}
