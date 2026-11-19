package context

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func writeSkillFixture(t *testing.T, root, name, description, body string) string {
	t.Helper()
	path := filepath.Join(root, ".agents", "skills", name, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	text := "---\nname: " + name + "\ndescription: " + description + "\n---\n" + body
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return text
}

func TestDiscoverSkillsGlobalAndWorkspaceOverride(t *testing.T) {
	config := t.TempDir()
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(config, "settings.json"))
	writeSkillFixture(t, config, "shared", "global description", "global body")
	root := t.TempDir()
	workspaceText := writeSkillFixture(t, root, "shared", `'workspace description: literal'`, "workspace body")
	globalText := writeSkillFixture(t, config, "global-only", "global only", "global body")
	writeSkillFixture(t, root, "invalid", "", "malformed")
	catalog := DiscoverSkills(root)
	if len(catalog.Diagnostics) != 1 || len(catalog.Skills) != 2 || catalog.Skills[0].Name != "global-only" || catalog.Skills[1].Description != "workspace description: literal" {
		t.Fatalf("catalog=%+v", catalog)
	}
	for name, text := range map[string]string{"shared": workspaceText, "global-only": globalText} {
		skill, err := LoadSkill(root, name)
		if err != nil || skill.Content != text || skill.Truncated {
			t.Fatalf("load %s: %+v err=%v", name, skill, err)
		}
	}
}

func TestSkillLimits(t *testing.T) {
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(t.TempDir(), "settings.json"))
	root := t.TempDir()
	for i := 0; i < MaxSkillEntries+3; i++ {
		writeSkillFixture(t, root, fmt.Sprintf("skill-%02d", i), "description", "body")
	}
	lines := strings.Split(DiscoverSkills(root).Prompt(), "\n")
	if len(lines) != MaxSkillEntries+1 || !strings.Contains(lines[0], "truncated: 3 more skills") {
		t.Fatalf("catalog did not mark truncation at 50 entries: %q", lines)
	}
	for i, line := range lines[1:] {
		if want := fmt.Sprintf("- skill-%02d: description", i); line != want {
			t.Fatalf("catalog row %d = %q, want %q", i, line, want)
		}
	}
	if skill, err := LoadSkill(root, "skill-52"); err != nil || skill.Truncated {
		t.Fatalf("catalog cap affected explicit load: %+v %v", skill, err)
	}
	writeSkillFixture(t, root, "large", "large", strings.Repeat("技能", MaxSkillBytes))
	skill, err := LoadSkill(root, "large")
	if err != nil || !skill.Truncated || len(skill.Content) > MaxSkillBytes || !utf8.ValidString(skill.Content) || !strings.HasSuffix(skill.Content, skillTruncated) {
		t.Fatalf("large load truncated=%t err=%v len=%d", skill.Truncated, err, len(skill.Content))
	}
}
