// Package hooks executes deterministic lifecycle checks without granting tools
// any additional authority. Configuration is frozen for the lifetime of a run.
package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/program"
	"github.com/cloud-harness/cloud-harness/internal/security"
)

type Point string

const (
	SessionStart     Point = "SessionStart"
	UserPromptSubmit Point = "UserPromptSubmit"
	PreToolUse       Point = "PreToolUse"
	PostToolUse      Point = "PostToolUse"
	Stop             Point = "Stop"
	DefaultTimeout         = 10 * time.Second
	MaxOutputBytes         = 4 << 10
)

var points = []Point{SessionStart, UserPromptSubmit, PreToolUse, PostToolUse, Stop}

// Action is either a command run by an allowlisted shell, or an inline script
// run by the existing isolated Starlark worker. Cwd is relative to Workspace.
type Action struct {
	Type      string `json:"type"`
	Command   string `json:"command,omitempty"`
	Shell     string `json:"shell,omitempty"`
	Script    string `json:"script,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}

// Config is the merged, checkpointable configuration. A nil GoCheck means on.
// An explicitly empty action list overrides that hook point in the global file.
type Config struct {
	Workspace string             `json:"workspace"`
	Hooks     map[Point][]Action `json:"hooks"`
	GoCheck   *bool              `json:"go_check,omitempty"`
	// WorkspaceTrust is the trust decision for the workspace hooks.json. The
	// file is validated but contributes nothing unless it is trusted.
	WorkspaceTrust *security.TrustStatus `json:"workspace_trust,omitempty"`
}

func Load(workspace string) (*Config, error) {
	dir, err := security.ConfigDirectory()
	if err != nil {
		return nil, fmt.Errorf("hooks configuration directory: %w", err)
	}
	return LoadFrom(workspace, dir)
}

// LoadFrom uses the same two directories as policy.json. Both layers are
// validated, including global hook points overridden by the workspace.
func LoadFrom(workspace, configDir string) (*Config, error) {
	if workspace == "" {
		workspace = "."
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	cfg := &Config{Workspace: filepath.Clean(abs), Hooks: make(map[Point][]Action)}
	if err := mergeFile(cfg, filepath.Join(configDir, "hooks.json")); err != nil {
		return nil, fmt.Errorf("hooks %s: %w", filepath.Join(configDir, "hooks.json"), err)
	}
	// The bytes that are hashed for the trust check are the bytes that are
	// parsed, so a file changed in between cannot inherit the decision.
	file := filepath.Join(abs, security.ControlDir, security.TrustHooksFile)
	raw, exists, err := security.ReadControlFile(abs, security.TrustHooksFile)
	if err != nil {
		return nil, fmt.Errorf("hooks %s: %w", file, err)
	}
	status, err := security.CheckTrust(abs, configDir, security.TrustHooksFile, raw, exists)
	if err != nil {
		return nil, err
	}
	cfg.WorkspaceTrust = &status
	if exists {
		target := cfg
		if !status.Effective() {
			// Validate only: nothing from an untrusted file reaches cfg.
			target = &Config{Workspace: cfg.Workspace, Hooks: make(map[Point][]Action)}
		}
		if err := mergeConfig(target, raw); err != nil {
			return nil, fmt.Errorf("hooks %s: %w", file, err)
		}
	}
	return cfg, nil
}

func decode(raw []byte, out any) error {
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

func mergeFile(cfg *Config, file string) error {
	f, err := os.Open(file)
	if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(raw) > 1<<20 {
		return fmt.Errorf("file exceeds 1 MiB")
	}
	return mergeConfig(cfg, raw)
}

func mergeConfig(cfg *Config, raw []byte) error {
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		return fmt.Errorf("configuration must be an object")
	}
	var doc struct {
		Hooks   json.RawMessage `json:"hooks"`
		GoCheck json.RawMessage `json:"go_check"`
	}
	if err := decode(raw, &doc); err != nil {
		return err
	}
	if len(doc.GoCheck) > 0 {
		var enabled bool
		if string(doc.GoCheck) == "null" || json.Unmarshal(doc.GoCheck, &enabled) != nil {
			return fmt.Errorf("go_check must be a boolean")
		}
		cfg.GoCheck = &enabled
	}
	if len(doc.Hooks) == 0 {
		return nil
	}
	if !bytes.HasPrefix(bytes.TrimSpace(doc.Hooks), []byte("{")) {
		return fmt.Errorf("hooks must be an object")
	}
	var entries map[Point]json.RawMessage
	if err := decode(doc.Hooks, &entries); err != nil {
		return err
	}
	for point := range entries {
		known := false
		for _, p := range points {
			known = known || point == p
		}
		if !known {
			return fmt.Errorf("unknown hook point %q", point)
		}
	}
	for _, point := range points {
		entry, ok := entries[point]
		if !ok {
			continue
		}
		if !bytes.HasPrefix(bytes.TrimSpace(entry), []byte("[")) {
			return fmt.Errorf("%s must be an array", point)
		}
		var rows []json.RawMessage
		if err := decode(entry, &rows); err != nil {
			return fmt.Errorf("%s: %w", point, err)
		}
		actions := make([]Action, 0, len(rows))
		for i, row := range rows {
			var a Action
			if err := decode(row, &a); err != nil {
				return fmt.Errorf("%s action %d: %w", point, i+1, err)
			}
			// JSON null must not silently turn a typed optional field into its
			// default value (notably timeout_ms and cwd).
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(row, &fields)
			for name, value := range fields {
				if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
					return fmt.Errorf("%s action %d: %s must not be null", point, i+1, name)
				}
			}
			if err := validateAction(cfg.Workspace, a); err != nil {
				return fmt.Errorf("%s action %d: %w", point, i+1, err)
			}
			actions = append(actions, a)
		}
		cfg.Hooks[point] = actions
	}
	return nil
}

func validateAction(workspace string, a Action) error {
	if a.TimeoutMS < 0 || a.TimeoutMS > 600_000 {
		return fmt.Errorf("timeout_ms must be between 0 (default 10 seconds) and 600000")
	}
	switch a.Type {
	case "command":
		if strings.TrimSpace(a.Command) == "" || a.Script != "" {
			return fmt.Errorf("command action requires command and forbids script")
		}
		if _, err := shellPath(a.Shell); err != nil {
			return err
		}
	case "starlark":
		if strings.TrimSpace(a.Script) == "" || len(a.Script) > program.MaxScriptBytes || a.Command != "" || a.Shell != "" || a.Cwd != "" {
			return fmt.Errorf("starlark action requires script (at most 64 KiB) and forbids command, shell and cwd")
		}
	default:
		return fmt.Errorf("type must be command or starlark")
	}
	_, err := workingDir(workspace, a.Cwd)
	return err
}

func shellPath(shell string) (string, error) {
	switch shell {
	case "", "sh", "/bin/sh":
		return "/bin/sh", nil
	case "bash", "/bin/bash":
		return "/bin/bash", nil
	default:
		return "", fmt.Errorf("shell must be sh (/bin/sh) or bash (/bin/bash)")
	}
}

func workingDir(workspace, cwd string) (string, error) {
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", fmt.Errorf("workspace: %w", err)
	}
	if cwd == "" {
		cwd = "."
	}
	if !filepath.IsAbs(cwd) {
		cwd = filepath.Join(root, cwd)
	}
	dir, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", fmt.Errorf("cwd: %w", err)
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("cwd must remain inside the workspace")
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("cwd must be a directory")
	}
	return dir, nil
}
