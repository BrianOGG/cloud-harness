//go:build linux || darwin

package tools

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/BrianOGG/cloud-harness/internal/security"
)

type commandSandboxPath struct {
	Path   string `json:"path"`
	Create bool   `json:"create,omitempty"`
}

type commandSandboxSpec struct {
	Enabled  bool                 `json:"enabled"`
	Paths    []commandSandboxPath `json:"paths,omitempty"`
	Preserve string               `json:"preserve,omitempty"`
}

// commandSandboxSpecFor returns the control-file mask for a command rooted
// at root. Run worktrees live inside the original workspace's control directory,
// so that directory is masked while the worktree itself is preserved. A
// configuration directory that contains the workspace cannot be masked without
// hiding the workspace, so commands fail closed there instead.
func commandSandboxSpecFor(root string) (commandSandboxSpec, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return commandSandboxSpec{}, err
	}
	spec := commandSandboxSpec{Enabled: true}
	preserve := ""
	if control, ok := worktreeControlDir(abs); ok {
		resolvedControl, _ := resolveSandboxMask(control)
		resolvedPreserve, _ := resolveSandboxMask(abs)
		if !pathWithin(resolvedPreserve, resolvedControl) {
			return commandSandboxSpec{}, errors.New("command sandbox cannot preserve a worktree outside its control directory")
		}
		spec.Paths = append(spec.Paths, commandSandboxPath{Path: control})
		preserve = abs
	}
	workspaceControl := controlSandboxPath(filepath.Join(abs, security.ControlDir))
	if preserve != "" {
		// Creating .cloud-harness inside a retained worktree would write into
		// the original control directory on the host.
		workspaceControl.Create = false
	}
	spec.Paths = append(spec.Paths, workspaceControl)
	config, ok, contains := commandConfigPath(abs)
	if contains {
		return commandSandboxSpec{}, errors.New("command sandbox cannot mask a configuration directory that contains the workspace")
	}
	if ok {
		spec.Paths = append(spec.Paths, controlSandboxPath(config))
	}
	spec.Preserve = preserve
	return spec, nil
}

// worktreeControlDir reports the original control directory when root is a
// retained run worktree at <workspace>/.cloud-harness/worktrees/<run>.
func worktreeControlDir(root string) (string, bool) {
	parent := filepath.Dir(root)
	if filepath.Base(parent) != "worktrees" {
		return "", false
	}
	control := filepath.Dir(parent)
	if filepath.Base(control) != security.ControlDir {
		return "", false
	}
	return control, true
}

func controlSandboxPath(path string) commandSandboxPath {
	_, err := os.Lstat(path)
	return commandSandboxPath{Path: path, Create: os.IsNotExist(err)}
}

func commandConfigPath(root string) (string, bool, bool) {
	dir, err := security.ConfigDirectory()
	if err != nil {
		return "", false, false
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", false, false
	}
	absRoot = filepath.Clean(absRoot)
	if resolved, err := filepath.EvalSymlinks(absRoot); err == nil {
		absRoot = resolved
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", false, false
	}
	absDir = filepath.Clean(absDir)
	if resolved, err := filepath.EvalSymlinks(absDir); err == nil {
		absDir = resolved
	}
	rel, err := filepath.Rel(absDir, absRoot)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false, true
	}
	return absDir, true, false
}

func pathWithin(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func resolveSandboxMask(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		resolved = path
	}
	return filepath.Clean(resolved), nil
}

func sandboxNotApplied(detail string) error {
	return errors.New("command sandbox could not be applied, so the command was not started: " + detail)
}
