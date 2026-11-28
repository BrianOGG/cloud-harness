package transport

import (
	"net/http"

	contextpkg "github.com/BrianOGG/cloud-harness/internal/context"
	"github.com/BrianOGG/cloud-harness/internal/domain"
)

// List only frontmatter. DiscoverSkills resolves names across both layers,
// skips invalid files and sorts the summaries before returning.
func (s *Server) skills(w http.ResponseWriter, _ *http.Request) {
	if s.Manager != nil {
		catalog := contextpkg.DiscoverSkills(s.Manager.Workspace)
		if len(catalog.Skills) > 0 {
			writeJSON(w, http.StatusOK, catalog.Skills)
			return
		}
		if len(catalog.Diagnostics) == 0 {
			writeJSON(w, http.StatusOK, []contextpkg.Skill{})
			return
		}
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]*domain.Error{"error": domain.E("unavailable", "技能列表暂不可用")})
}

// Resolve the skill at send time using the CLI loader. Only the short command
// goes into the transcript and history index.
func (s *Server) loadRequestedSkill(name, task string) (*contextpkg.LoadedSkill, string, error) {
	skill, err := contextpkg.LoadSkill(s.Manager.Workspace, name)
	if err != nil {
		return nil, "", domain.E(domain.CodeInvalidArgument, "无法加载所选技能，请检查技能名称或文件")
	}
	command := "/skill " + skill.Name
	if task != "" {
		command += " " + task
	}
	return &skill, command, nil
}
