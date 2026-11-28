package context

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/BrianOGG/cloud-harness/internal/rules"
)

const (
	MaxSkillEntries = 50
	MaxSkillBytes   = 64 * 1024
	skillTruncated  = "\n[SKILL.md truncated at 64 KiB]"
)

var (
	skillDir  = filepath.Join(".agents", "skills")
	skillName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
)

type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// SkillCatalog contains only frontmatter summaries. Invalid files are skipped
// with a diagnostic so they cannot prevent a run from starting.
type SkillCatalog struct {
	Skills      []Skill
	Diagnostics []error
}

// DiscoverSkills reads frontmatter from the workspace and global skill
// directories. Valid workspace entries win on a name collision.
func DiscoverSkills(workspace string) SkillCatalog {
	var catalog SkillCatalog
	roots, err := skillRoots(workspace)
	if err != nil {
		catalog.Diagnostics = append(catalog.Diagnostics, err)
	}
	byName := make(map[string]Skill)
	for _, root := range roots {
		if err := discoverSkills(root, byName, &catalog); err != nil {
			catalog.Diagnostics = append(catalog.Diagnostics, err)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		catalog.Skills = append(catalog.Skills, byName[name])
	}
	return catalog
}

// Prompt returns a stable, name-sorted catalog with at most 50 skill rows.
func (c SkillCatalog) Prompt() string {
	if len(c.Skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Available skills (use load_skill to load a SKILL.md by name)")
	if omitted := len(c.Skills) - MaxSkillEntries; omitted > 0 {
		fmt.Fprintf(&b, " [truncated: %d more skills]", omitted)
	}
	for _, skill := range c.Skills[:min(len(c.Skills), MaxSkillEntries)] {
		fmt.Fprintf(&b, "\n- %s: %s", skill.Name, skill.Description)
	}
	return b.String()
}

// LoadedSkill keeps the full file, including frontmatter, unless truncated.
type LoadedSkill struct {
	Name      string `json:"name"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
}

// LoadSkill reads only the requested skill. The return content, including any
// truncation marker, is bounded by MaxSkillBytes.
func LoadSkill(workspace, name string) (LoadedSkill, error) {
	if !skillName.MatchString(name) {
		return LoadedSkill{}, fmt.Errorf("invalid skill name %q", name)
	}
	roots, firstErr := skillRoots(workspace)
	for i := len(roots) - 1; i >= 0; i-- {
		data, err := readSkill(roots[i], name)
		if err == nil {
			return data, nil
		}
		if firstErr == nil && !errors.Is(err, fs.ErrNotExist) {
			firstErr = err
		}
	}
	if firstErr != nil {
		return LoadedSkill{}, fmt.Errorf("skill %q: %w", name, firstErr)
	}
	return LoadedSkill{}, fmt.Errorf("skill %q not found", name)
}

func skillRoots(workspace string) ([]string, error) {
	if workspace == "" {
		workspace = "."
	}
	global, err := rules.ConfigDir()
	if err != nil {
		return []string{workspace}, fmt.Errorf("global skill directory: %w", err)
	}
	return []string{global, workspace}, nil
}

// Root confines opens to the configured layer even if the files change while
// being scanned. As with agent definitions, skip symlink entries.
func openSkills(root string) (*os.Root, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	for _, path := range []string{".agents", skillDir} {
		info, err := r.Lstat(path)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, fs.ErrNotExist
		}
	}
	return r.OpenRoot(skillDir)
}

func openSkill(dir *os.Root, name string) (*os.File, error) {
	info, err := dir.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fs.ErrNotExist
	}
	path := filepath.Join(name, "SKILL.md")
	info, err = dir.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fs.ErrNotExist
	}
	return dir.Open(path)
}

func discoverSkills(root string, byName map[string]Skill, catalog *SkillCatalog) error {
	dir, err := openSkills(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	entries, err := fs.ReadDir(dir.FS(), ".")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || !skillName.MatchString(name) {
			continue
		}
		f, err := openSkill(dir, name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		var skill Skill
		if err == nil {
			skill, err = parseSkill(name, f)
			_ = f.Close()
		}
		if err != nil {
			catalog.Diagnostics = append(catalog.Diagnostics, fmt.Errorf("%s: %w", filepath.Join(dir.Name(), name, "SKILL.md"), err))
			continue
		}
		byName[name] = skill
	}
	return nil
}

func readSkill(root, name string) (LoadedSkill, error) {
	dir, err := openSkills(root)
	if err != nil {
		return LoadedSkill{}, err
	}
	defer func() { _ = dir.Close() }()
	f, err := openSkill(dir, name)
	if err != nil {
		return LoadedSkill{}, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, MaxSkillBytes+1))
	if err != nil {
		return LoadedSkill{}, err
	}
	if _, err := parseSkill(name, bytes.NewReader(data)); err != nil {
		return LoadedSkill{}, fmt.Errorf("%s: %w", filepath.Join(dir.Name(), name, "SKILL.md"), err)
	}
	truncated := len(data) > MaxSkillBytes
	if truncated {
		cut := MaxSkillBytes - len(skillTruncated)
		for cut > 0 && !utf8.RuneStart(data[cut]) {
			cut--
		}
		data = append(data[:cut], skillTruncated...)
	}
	if !utf8.Valid(data) {
		return LoadedSkill{}, errors.New("SKILL.md must be UTF-8 text")
	}
	return LoadedSkill{Name: name, Content: string(data), Truncated: truncated}, nil
}

func parseSkill(directoryName string, r io.Reader) (Skill, error) {
	scanner := bufio.NewScanner(io.LimitReader(r, MaxSkillBytes))
	if !scanner.Scan() || scanner.Text() != "---" {
		return Skill{}, errors.New("missing frontmatter: file must start with a --- line")
	}
	skill := Skill{}
	seen := make(map[string]bool)
	for line := 2; scanner.Scan(); line++ {
		if scanner.Text() == "---" {
			if skill.Name != directoryName {
				return Skill{}, fmt.Errorf("name must match skill directory %q", directoryName)
			}
			if skill.Description == "" {
				return Skill{}, errors.New("description is required and must not be empty")
			}
			return skill, nil
		}
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		key, value, ok := strings.Cut(text, ":")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || key == "" || !utf8.ValidString(value) {
			return Skill{}, fmt.Errorf("line %d: expected a UTF-8 single-line key: value", line)
		}
		if seen[key] {
			return Skill{}, fmt.Errorf("line %d: duplicate field %q", line, key)
		}
		seen[key] = true
		if len(value) > 0 && (value[0] == '\'' || value[0] == '"') {
			if len(value) < 2 || value[len(value)-1] != value[0] {
				return Skill{}, fmt.Errorf("line %d: unterminated quoted string", line)
			}
			value = value[1 : len(value)-1]
		}
		switch key {
		case "name":
			skill.Name = value
		case "description":
			skill.Description = strings.Join(strings.Fields(value), " ")
		}
	}
	if err := scanner.Err(); err != nil {
		return Skill{}, err
	}
	return Skill{}, errors.New("unclosed frontmatter within 64 KiB: missing closing --- line")
}
