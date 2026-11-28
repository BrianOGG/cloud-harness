package context

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/rules"
)

const skillFile = "SKILL.md"

type ScopedSkill struct {
	Skill
	Scope     string `json:"scope"`
	Effective bool   `json:"effective"`
}

type SkillDocument struct {
	LoadedSkill
	Description string `json:"description"`
	Scope       string `json:"scope"`
	Revision    string `json:"revision,omitempty"`
}

func skillLayer(workspace, scope string) (string, error) {
	switch scope {
	case "project":
		if workspace == "" {
			workspace = "."
		}
		return workspace, nil
	case "global":
		return rules.ConfigDir()
	default:
		return "", domain.E(domain.CodeInvalidArgument, "无效的技能作用域")
	}
}

// A server started inside its own config directory can discover the same
// physical file twice. A scoped mutation there would change both entries.
func checkSkillLayers(workspace string) error {
	project, _ := skillLayer(workspace, "project")
	global, err := skillLayer(workspace, "global")
	if err != nil {
		return nil // A missing global config does not prevent project writes.
	}
	p, projectErr := os.Stat(project)
	g, globalErr := os.Stat(global)
	if projectErr == nil && globalErr == nil && os.SameFile(p, g) {
		return domain.E(domain.CodeConflict, "工作区与全局配置指向同一目录，请切换工作区后再修改技能")
	}
	return nil
}

// ListScopedSkills exposes the layers separately, using the same frontmatter
// reader and workspace-over-global precedence as DiscoverSkills.
func ListScopedSkills(workspace string) ([]ScopedSkill, error) {
	result := []ScopedSkill{}
	seen := make(map[string]bool)
	for _, scope := range []string{"project", "global"} {
		root, err := skillLayer(workspace, scope)
		if err != nil {
			return nil, err
		}
		rows := make(map[string]Skill)
		if err := discoverSkills(root, rows, &SkillCatalog{}); err != nil {
			return nil, err
		}
		for _, skill := range rows {
			result = append(result, ScopedSkill{Skill: skill, Scope: scope, Effective: !seen[skill.Name]})
			seen[skill.Name] = true
		}
	}
	slices.SortFunc(result, func(a, b ScopedSkill) int {
		if byName := strings.Compare(a.Name, b.Name); byName != 0 {
			return byName
		}
		return strings.Compare(b.Scope, a.Scope)
	})
	return result, nil
}

// Open each component as a separate, pinned directory. Lstat rejects links,
// SameFile detects a replacement during open, and os.Root confines each step.
func skillDirectory(workspace, scope, name string, create bool) (*os.Root, error) {
	if !skillName.MatchString(name) {
		return nil, domain.E(domain.CodeInvalidArgument, "名称只能包含英文字母、数字、下划线或连字符")
	}
	path, err := skillLayer(workspace, scope)
	if err != nil {
		return nil, err
	}
	// The CLI's configured root may be a symlink. Below that root the CLI
	// skips all symlinks, including the .agents and skills directories.
	if scope == "global" && create {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, err
		}
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	for _, part := range []string{".agents", "skills", name} {
		next, err := skillSubdir(root, part, create)
		_ = root.Close()
		if err != nil {
			return nil, err
		}
		root = next
	}
	return root, nil
}

func skillSubdir(root *os.Root, part string, create bool) (*os.Root, error) {
	if create {
		if err := root.Mkdir(part, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}
	info, err := root.Lstat(part)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, domain.E(domain.CodeInvalidArgument, "技能路径必须是普通目录")
	}
	dir, err := root.OpenRoot(part)
	if err != nil {
		return nil, err
	}
	opened, err := dir.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		_ = dir.Close()
		return nil, skillConflict()
	}
	return dir, nil
}

func skillConflict() error {
	return domain.E(domain.CodeConflict, "技能文件已改变或正在使用，请重新打开后再试")
}

func skillRevision(content string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
}

func readSkillDocument(dir *os.Root, scope, name string) (SkillDocument, fs.FileMode, error) {
	info, err := dir.Lstat(skillFile)
	if err != nil {
		return SkillDocument{}, 0, err
	}
	if !info.Mode().IsRegular() {
		return SkillDocument{}, 0, domain.E(domain.CodeInvalidArgument, "SKILL.md 必须是普通文件")
	}
	f, err := dir.Open(skillFile)
	if err != nil {
		return SkillDocument{}, 0, err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return SkillDocument{}, 0, skillConflict()
	}
	loaded, summary, err := readSkillFile(f, name)
	if err != nil {
		return SkillDocument{}, 0, domain.E(domain.CodeInvalidArgument, "无法读取有效的 SKILL.md")
	}
	doc := SkillDocument{LoadedSkill: loaded, Description: summary.Description, Scope: scope}
	if !doc.Truncated {
		doc.Revision = skillRevision(doc.Content)
	}
	return doc, info.Mode().Perm(), nil
}

func OpenSkillDocument(workspace, scope, name string) (SkillDocument, error) {
	dir, err := skillDirectory(workspace, scope, name, false)
	if err != nil {
		return SkillDocument{}, err
	}
	defer func() { _ = dir.Close() }()
	doc, _, err := readSkillDocument(dir, scope, name)
	return doc, err
}

// Create takes the name from frontmatter. Updates bind both scope and name
// to the URL; the revision covers the complete original bytes.
func SaveSkillDocument(workspace, scope, name, content, revision string) (SkillDocument, error) {
	create := name == ""
	if len(content) > MaxSkillBytes || !utf8.ValidString(content) {
		return SkillDocument{}, domain.E(domain.CodeInvalidArgument, "SKILL.md 必须是 64 KiB 以内的 UTF-8 文本")
	}
	skill, err := parseSkill(name, strings.NewReader(content))
	if err != nil {
		return SkillDocument{}, domain.E(domain.CodeInvalidArgument, err.Error())
	}
	if err := checkSkillLayers(workspace); err != nil {
		return SkillDocument{}, err
	}
	dir, err := skillDirectory(workspace, scope, skill.Name, create)
	if err != nil {
		return SkillDocument{}, err
	}
	defer func() { _ = dir.Close() }()
	unlock, err := lockSkillDirectory(dir)
	if err != nil {
		return SkillDocument{}, err
	}
	defer unlock()
	if err := publishSkill(dir, scope, name, content, revision); err != nil {
		return SkillDocument{}, err
	}
	return SkillDocument{LoadedSkill: LoadedSkill{Name: skill.Name, Content: content}, Description: skill.Description, Scope: scope, Revision: skillRevision(content)}, nil
}

func checkSkillRevision(dir *os.Root, scope, name, revision string) (fs.FileMode, error) {
	if revision == "" {
		return 0, domain.E(domain.CodeInvalidArgument, "请先读取技能版本")
	}
	doc, mode, err := readSkillDocument(dir, scope, name)
	if err != nil {
		return 0, err
	}
	if doc.Revision != revision {
		return 0, skillConflict()
	}
	return mode, nil
}

func publishSkill(dir *os.Root, scope, name, content, revision string) error {
	tmp := ".SKILL-" + rand.Text()
	f, err := dir.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Remove(tmp) }()
	_, err = f.WriteString(content)
	if err == nil && name != "" {
		var mode fs.FileMode
		mode, err = checkSkillRevision(dir, scope, name, revision)
		if err == nil {
			err = f.Chmod(mode)
		}
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}
	if name == "" {
		// Link publishes a complete file without replacing an existing entry,
		// even if it is invalid, a symlink, or just created by another writer.
		err := dir.Link(tmp, skillFile)
		if errors.Is(err, fs.ErrExist) {
			return domain.E(domain.CodeConflict, "该作用域已存在同名技能文件")
		}
		return err
	}
	return dir.Rename(tmp, skillFile)
}

func DeleteSkillDocument(workspace, scope, name, revision string) error {
	if err := checkSkillLayers(workspace); err != nil {
		return err
	}
	dir, err := skillDirectory(workspace, scope, name, false)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	unlock, err := lockSkillDirectory(dir)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := checkSkillRevision(dir, scope, name, revision); err != nil {
		return err
	}
	// Skills can have arbitrary user-authored scripts and reference files.
	return dir.Remove(skillFile)
}
