package security

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

type Effect string

const (
	EffectAllow Effect = "allow"
	EffectAsk   Effect = "ask"
	EffectDeny  Effect = "deny"
)

// Rule selectors are conjunctive. Tool is a name or *, and ** matches zero or
// more complete path segments. Rules retain their order within each file.
type Rule struct {
	Tool          string `json:"tool"`
	PathGlob      string `json:"path_glob,omitempty"`
	CommandPrefix string `json:"command_prefix,omitempty"`
	Effect        Effect `json:"effect"`
}

// Policy is a run's configuration snapshot. It is also saved in checkpoints so
// an approval resume, including in another process, never reloads edited files.
type Policy struct {
	Workspace      string `json:"workspace"`
	WorkspaceRules []Rule `json:"workspace_rules,omitempty"`
	GlobalRules    []Rule `json:"global_rules,omitempty"`
}

// PolicyRequest contains only selectors, never file contents. Paths are file
// targets; commands and scripts are kept separate from resource identifiers.
type PolicyRequest struct {
	Tool      string
	Paths     []string
	Command   string
	FileRead  bool
	FileWrite bool
}

// PolicyDecision is safe to record as an event. Command selectors and commands
// are represented by hashes, including in the rule summary and denial reason.
type PolicyDecision struct {
	Effect        Effect `json:"effect"`
	Source        string `json:"source"`
	Tool          string `json:"tool"`
	Rule          string `json:"rule"`
	RuleIndex     int    `json:"rule_index,omitempty"`
	CommandSHA256 string `json:"command_sha256,omitempty"`
}

func (d PolicyDecision) Reason() string {
	return fmt.Sprintf("policy %s (%s): %s", d.Effect, d.Source, d.Rule)
}

// ConfigDirectory follows the existing explicit configuration-file convention.
// Without CLOUD_HARNESS_CONFIG, use the platform's user configuration directory.
func ConfigDirectory() (string, error) {
	if file := os.Getenv("CLOUD_HARNESS_CONFIG"); file != "" {
		abs, err := filepath.Abs(file)
		return filepath.Dir(abs), err
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("policy configuration directory: %w", err)
	}
	return filepath.Join(dir, "cloud-harness"), nil
}

func LoadPolicy(workspace string) (*Policy, error) {
	dir, err := ConfigDirectory()
	if err != nil {
		return nil, err
	}
	return LoadPolicyFrom(workspace, dir)
}

// LoadPolicyFrom reads each layer once. Missing files are empty layers; all
// other I/O errors and invalid configuration fail the load with the file name.
func LoadPolicyFrom(workspace, configDir string) (*Policy, error) {
	if workspace == "" {
		workspace = "."
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	p := &Policy{Workspace: filepath.Clean(abs)}
	p.WorkspaceRules, err = loadRules(filepath.Join(abs, ".cloud-harness", "policy.json"))
	if err != nil {
		return nil, err
	}
	p.GlobalRules, err = loadRules(filepath.Join(configDir, "policy.json"))
	if err != nil {
		return nil, err
	}
	return p, nil
}

func decodeStrict(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("expected exactly one JSON value")
	}
	return nil
}

func loadRules(file string) ([]Rule, error) {
	raw, err := os.ReadFile(file)
	if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("policy %s: %w", file, err)
	}
	var doc struct {
		Rules json.RawMessage `json:"rules"`
	}
	if err := decodeStrict(raw, &doc); err != nil {
		return nil, fmt.Errorf("policy %s: %w", file, err)
	}
	var rows []json.RawMessage
	if len(doc.Rules) == 0 || bytes.Equal(bytes.TrimSpace(doc.Rules), []byte("null")) {
		return nil, fmt.Errorf("policy %s: rules must be an array", file)
	}
	if err := json.Unmarshal(doc.Rules, &rows); err != nil {
		return nil, fmt.Errorf("policy %s: rules must be an array: %w", file, err)
	}
	rules := make([]Rule, 0, len(rows))
	for i, row := range rows {
		var fields struct {
			Tool          json.RawMessage `json:"tool"`
			PathGlob      json.RawMessage `json:"path_glob"`
			CommandPrefix json.RawMessage `json:"command_prefix"`
			Effect        json.RawMessage `json:"effect"`
		}
		fail := func(err error) ([]Rule, error) {
			return nil, fmt.Errorf("policy %s: rule %d: %w", file, i+1, err)
		}
		if err := decodeStrict(row, &fields); err != nil {
			return fail(err)
		}
		stringValue := func(name string, raw json.RawMessage, required bool) (string, error) {
			if len(raw) == 0 && !required {
				return "", nil
			}
			var value string
			if err := json.Unmarshal(raw, &value); err != nil || strings.TrimSpace(value) == "" {
				return "", fmt.Errorf("%s must be a non-empty string", name)
			}
			return value, nil
		}
		var rule Rule
		if rule.Tool, err = stringValue("tool", fields.Tool, true); err != nil {
			return fail(err)
		}
		if strings.ContainsAny(rule.Tool, " \t\r\n?[]") || (rule.Tool != "*" && strings.Contains(rule.Tool, "*")) {
			return fail(fmt.Errorf("tool must be a tool name or *"))
		}
		if rule.PathGlob, err = stringValue("path_glob", fields.PathGlob, false); err != nil {
			return fail(err)
		}
		if rule.PathGlob != "" {
			for _, segment := range strings.Split(rule.PathGlob, "/") {
				if _, err := path.Match(segment, ""); err != nil {
					return fail(fmt.Errorf("invalid path_glob: %w", err))
				}
				if segment != "**" && strings.Contains(segment, "**") {
					return fail(fmt.Errorf("path_glob: ** must occupy a complete path segment"))
				}
			}
		}
		if rule.CommandPrefix, err = stringValue("command_prefix", fields.CommandPrefix, false); err != nil {
			return fail(err)
		}
		if rule.CommandPrefix != "" && rule.Tool != "*" && !commandTool(rule.Tool) {
			return fail(fmt.Errorf("command_prefix requires a command tool or *"))
		}
		effect, err := stringValue("effect", fields.Effect, true)
		if err != nil {
			return fail(err)
		}
		rule.Effect = Effect(effect)
		if rule.Effect != EffectAllow && rule.Effect != EffectAsk && rule.Effect != EffectDeny {
			return fail(fmt.Errorf("effect must be allow, ask, or deny"))
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

func (p *Policy) Evaluate(req PolicyRequest) PolicyDecision {
	d := PolicyDecision{Effect: EffectAsk, Source: "default", Tool: req.Tool, Rule: "no matching policy rule; use existing permissions"}
	if req.Command != "" {
		d.CommandSHA256 = policyHash(req.Command)
	}
	for _, layer := range []struct {
		name  string
		rules []Rule
	}{{"workspace", p.WorkspaceRules}, {"global", p.GlobalRules}} {
		for i, rule := range layer.rules {
			if p.matches(rule, req) {
				d.Effect, d.Source, d.RuleIndex, d.Rule = rule.Effect, layer.name, i+1, ruleSummary(rule)
				return d
			}
		}
	}
	for _, target := range req.Paths {
		lexical, resolved := p.targetPaths(target)
		for _, name := range []string{lexical, resolved} {
			base := filepath.Base(name)
			if req.FileRead && base != ".env.example" && (base == ".env" || strings.HasPrefix(base, ".env.")) {
				d.Effect, d.Source, d.RuleIndex, d.Rule = EffectDeny, "builtin", 1, "file read matches **/.env or **/.env.* (except .env.example)"
				return d
			}
		}
		if req.FileWrite && (!inside(p.Workspace, lexical) || !inside(resolvePolicyPath(p.Workspace), resolved)) {
			d.Effect, d.Source, d.RuleIndex, d.Rule = EffectDeny, "builtin", 2, "file write target is outside the workspace"
			return d
		}
	}
	return d
}

func (p *Policy) matches(rule Rule, req PolicyRequest) bool {
	if rule.Tool != "*" && rule.Tool != req.Tool {
		return false
	}
	if rule.CommandPrefix != "" {
		prefix, command := strings.TrimSpace(rule.CommandPrefix), strings.TrimSpace(req.Command)
		if !commandTool(req.Tool) || !strings.HasPrefix(command, prefix) {
			return false
		}
		if len(command) > len(prefix) && !strings.ContainsRune(" \t\r\n", rune(command[len(prefix)])) {
			return false
		}
	}
	if rule.PathGlob == "" {
		return true
	}
	if len(req.Paths) == 0 {
		return false
	}
	for _, target := range req.Paths {
		lexical, resolved := p.targetPaths(target)
		matched := p.matchesPath(rule.PathGlob, lexical) || p.matchesPath(rule.PathGlob, resolved)
		// An allow for one resource must not authorize other targets in the call.
		if rule.Effect == EffectAllow && !matched {
			return false
		}
		if rule.Effect != EffectAllow && matched {
			return true
		}
	}
	return rule.Effect == EffectAllow
}

func commandTool(tool string) bool {
	switch tool {
	case "run_command", "execute_program", "shell", "bash", "exec_command":
		return true
	default:
		return false
	}
}

func (p *Policy) matchesPath(pattern, target string) bool {
	if filepath.IsAbs(pattern) {
		return matchPolicyGlob(pattern, filepath.ToSlash(target))
	}
	for _, root := range []string{p.Workspace, resolvePolicyPath(p.Workspace)} {
		if inside(root, target) {
			rel, _ := filepath.Rel(root, target)
			if matchPolicyGlob(strings.TrimPrefix(pattern, "./"), filepath.ToSlash(rel)) {
				return true
			}
		}
	}
	return false
}

func matchPolicyGlob(pattern, target string) bool {
	patterns, names := strings.Split(pattern, "/"), strings.Split(target, "/")
	// Dynamic programming bounds repeated ** patterns to O(segments squared).
	matched := make([]bool, len(names)+1)
	matched[0] = true
	for _, segment := range patterns {
		next := make([]bool, len(names)+1)
		for j := 0; j <= len(names); j++ {
			if segment == "**" {
				next[j] = matched[j] || j > 0 && next[j-1]
			} else if j > 0 && matched[j-1] {
				next[j], _ = path.Match(segment, names[j-1])
			}
		}
		matched = next
	}
	return matched[len(names)]
}

func (p *Policy) targetPaths(target string) (string, string) {
	if !filepath.IsAbs(target) {
		target = filepath.Join(p.Workspace, target)
	}
	target = filepath.Clean(target)
	return target, resolvePolicyPath(target)
}

// Resolve the nearest existing ancestor too, so new files under a symlinked
// directory are checked against its actual destination.
func resolvePolicyPath(target string) string {
	resolved, err := resolvePolicyPathHop(filepath.Clean(target), 0)
	if err != nil {
		// Unresolvable/cyclic paths are never evidence of a contained target.
		return string(filepath.Separator)
	}
	return resolved
}

func resolvePolicyPathHop(target string, links int) (string, error) {
	if links > 40 {
		return "", os.ErrInvalid
	}
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		return resolved, nil
	}
	info, err := os.Lstat(target)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		destination, err := os.Readlink(target)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(destination) {
			destination = filepath.Join(filepath.Dir(target), destination)
		}
		return resolvePolicyPathHop(filepath.Clean(destination), links+1)
	}
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(target)
	if parent == target {
		return target, err
	}
	resolved, err := resolvePolicyPathHop(parent, links)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(target)), nil
}

func inside(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func ruleSummary(rule Rule) string {
	s := fmt.Sprintf("tool=%s effect=%s", rule.Tool, rule.Effect)
	if rule.PathGlob != "" {
		s += " path_glob=" + rule.PathGlob
	}
	if rule.CommandPrefix != "" {
		s += " command_prefix_sha256=" + policyHash(rule.CommandPrefix)
	}
	return s
}

func policyHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
