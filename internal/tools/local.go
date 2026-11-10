package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/domain"
)

const maxFileBytes = 1 << 20

// RegisterLocal adds read_file, write_file, and run_command rooted at root.
// read_file is readonly. write_file and any command outside the safe set require
// a scoped grant or an approval decision before Execute runs.
func RegisterLocal(b *Broker, root string) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	abs = filepath.Clean(abs)
	b.Register(Spec{
		Name:            "read_file",
		Description:     "Read a UTF-8 text file inside the workspace.",
		Required:        []string{"path"},
		Readonly:        true,
		Idempotent:      true,
		Pollable:        true,
		SideEffectClass: securityNone,
		Parameters:      objectSchema("path", "Workspace-relative or absolute path inside the workspace."),
		ExtractResources: func(args json.RawMessage) ([]string, error) {
			p, err := resolveInside(abs, stringField(args, "path"))
			if err != nil {
				return nil, err
			}
			return []string{p}, nil
		},
		Execute: func(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
			p, err := resolveInside(abs, stringField(args, "path"))
			if err != nil {
				return nil, err
			}
			body, err := os.ReadFile(p)
			if err != nil {
				return nil, domain.E(domain.CodeToolError, err.Error())
			}
			if len(body) > maxFileBytes {
				body = body[:maxFileBytes]
			}
			rel, _ := filepath.Rel(abs, p)
			return json.Marshal(map[string]string{"path": filepath.ToSlash(rel), "content": string(body)})
		},
	})
	b.Register(Spec{
		Name:            "write_file",
		Description:     "Write a UTF-8 text file inside the workspace. Requires a scoped grant or approval.",
		Required:        []string{"path", "content"},
		Readonly:        false,
		Idempotent:      true,
		SideEffectClass: "reversible",
		Parameters:      objectSchema("path", "Workspace path to write."),
		ExtractResources: func(args json.RawMessage) ([]string, error) {
			p, err := resolveInside(abs, stringField(args, "path"))
			if err != nil {
				return nil, err
			}
			return []string{p}, nil
		},
		StatusProbe: func(_ context.Context, args json.RawMessage) (Probe, error) {
			p, err := resolveInside(abs, stringField(args, "path"))
			if err != nil {
				return Probe{}, nil
			}
			content := stringField(args, "content")
			body, err := os.ReadFile(p)
			if err != nil || string(body) != content {
				return Probe{Known: false}, nil
			}
			rel, _ := filepath.Rel(abs, p)
			data, err := json.Marshal(map[string]any{"path": filepath.ToSlash(rel), "bytes": len(content), "probed": true})
			if err != nil {
				return Probe{}, err
			}
			return Probe{Known: true, OK: true, Data: data}, nil
		},
		Execute: func(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
			p, err := resolveInside(abs, stringField(args, "path"))
			if err != nil {
				return nil, err
			}
			content := stringField(args, "content")
			if len(content) > maxFileBytes {
				return nil, domain.E(domain.CodeInvalidArgument, "content exceeds 1MiB")
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return nil, domain.E(domain.CodeToolError, err.Error())
			}
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				return nil, domain.E(domain.CodeToolError, err.Error())
			}
			rel, _ := filepath.Rel(abs, p)
			return json.Marshal(map[string]any{"path": filepath.ToSlash(rel), "bytes": len(content)})
		},
	})
	b.Register(Spec{
		Name:            "run_command",
		Description:     "Run a single allowlisted command in the workspace without a shell. Dangerous commands require approval and are not started.",
		Required:        []string{"command"},
		Readonly:        true,
		Idempotent:      true,
		SideEffectClass: securityNone,
		Parameters:      objectSchema("command", "Command and arguments, for example: ls"),
		ExtractResources: func(args json.RawMessage) ([]string, error) {
			return commandResources(abs, stringField(args, "command"))
		},
		Execute: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
			argv, dangerous, err := parseCommand(abs, stringField(args, "command"))
			if err != nil {
				return nil, err
			}
			if dangerous {
				return nil, domain.E(domain.CodeAuthDenied, "dangerous command was not approved and was not started")
			}
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
			cmd.Dir = abs
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err = cmd.Run()
			out := map[string]any{"argv": argv, "stdout": stdout.String(), "stderr": stderr.String()}
			if err != nil {
				out["error"] = err.Error()
				raw, _ := json.Marshal(out)
				return raw, domain.E(domain.CodeToolError, err.Error())
			}
			return json.Marshal(out)
		},
	})
	return nil
}

const securityNone = "none"

func objectSchema(field, desc string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			field:     map[string]string{"type": "string", "description": desc},
			"content": map[string]string{"type": "string"},
		},
		"required": []string{field},
	})
	return raw
}

func stringField(raw json.RawMessage, key string) string {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// SensitivePath reports files whose contents must not be sent onward.
func SensitivePath(p string) bool {
	base := strings.ToLower(filepath.Base(p))
	switch base {
	case ".env", "credentials.json", "id_rsa", "secrets.txt":
		return true
	default:
		return strings.Contains(base, "secret") || strings.Contains(base, "credential")
	}
}

func resolveInside(root, p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", domain.E(domain.CodeInvalidArgument, "path is empty")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	p = filepath.Clean(p)
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", domain.E(domain.CodeAuthDenied, "path is outside the workspace")
	}
	return p, nil
}

var safeCommands = map[string]bool{"ls": true, "pwd": true, "wc": true}

// Dangerous reports whether a run_command must wait for approval.
func Dangerous(command string, root string) bool {
	_, dangerous, err := parseCommand(root, command)
	if err != nil {
		return true
	}
	return dangerous
}

func commandResources(root, command string) ([]string, error) {
	argv, dangerous, err := parseCommand(root, command)
	if err != nil {
		return nil, err
	}
	if dangerous {
		// Still name the workspace so an approval record has a scope.
		return []string{root}, nil
	}
	out := []string{root}
	for _, arg := range argv[1:] {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		p, err := resolveInside(root, arg)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func parseCommand(root, command string) ([]string, bool, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, true, domain.E(domain.CodeInvalidArgument, "command is empty")
	}
	if strings.ContainsAny(command, ";&|`$<>\n") || strings.Contains(command, "$(") {
		return nil, true, nil
	}
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return nil, true, domain.E(domain.CodeInvalidArgument, "command is empty")
	}
	name := fields[0]
	if strings.Contains(name, "/") || strings.Contains(name, `\`) {
		return nil, true, nil
	}
	if !safeCommands[name] {
		return fields, true, nil
	}
	for _, arg := range fields[1:] {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		if _, err := resolveInside(root, arg); err != nil {
			return nil, true, err
		}
	}
	resolved, err := exec.LookPath(name)
	if err != nil {
		return nil, true, domain.E(domain.CodeToolError, "command not found: "+name)
	}
	argv := append([]string{resolved}, fields[1:]...)
	return argv, false, nil
}
