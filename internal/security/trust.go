package security

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// ControlDir holds harness state inside a workspace: hooks.json, policy.json,
// the default database, snapshots and worktree metadata.
const ControlDir = ".cloud-harness"

const (
	TrustHooksFile  = "hooks.json"
	TrustPolicyFile = "policy.json"
	trustStoreFile  = "trust.json"
	maxTrustedBytes = 1 << 20
	trustLockWait   = 5 * time.Second
)

// TrustedFiles are the workspace files that change later runs' authority and
// therefore only take effect after the user trusts their exact content.
var TrustedFiles = []string{TrustHooksFile, TrustPolicyFile}

type TrustState string

const (
	TrustMissing   TrustState = "missing"
	TrustTrusted   TrustState = "trusted"
	TrustUntrusted TrustState = "untrusted"
	// TrustChanged means another content hash of this file was trusted.
	TrustChanged TrustState = "changed"
)

// TrustStatus never contains file content.
type TrustStatus struct {
	File      string     `json:"file"`
	Path      string     `json:"path"`
	Workspace string     `json:"workspace"`
	State     TrustState `json:"state"`
	SHA256    string     `json:"sha256,omitempty"`
	Reason    string     `json:"reason,omitempty"`
}

// Effective reports whether the file's content may take effect.
func (s TrustStatus) Effective() bool { return s.State == TrustTrusted }

// Pending reports whether the file exists and is waiting for user trust.
func (s TrustStatus) Pending() bool { return s.State == TrustUntrusted || s.State == TrustChanged }

type trustEntry struct {
	Workspace string    `json:"workspace"`
	File      string    `json:"file"`
	SHA256    string    `json:"sha256"`
	TrustedAt time.Time `json:"trusted_at"`
}

type trustStore struct {
	Version int          `json:"version"`
	Entries []trustEntry `json:"entries"`
}

// TrustStorePath is in the configuration directory, next to the global
// policy.json and hooks.json, and outside any workspace by default.
func TrustStorePath(configDir string) string { return filepath.Join(configDir, trustStoreFile) }

// TrustWorkspaceKey identifies the workspace whose trust decision applies.
// Run worktrees live at <root>/.cloud-harness/worktrees/<run> and are created
// when a run starts, so they share the decision recorded for <root>; the
// content hash must still match.
func TrustWorkspaceKey(workspace string) (string, error) {
	if workspace == "" {
		workspace = "."
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	parent := filepath.Dir(abs)
	if filepath.Base(parent) == "worktrees" && filepath.Base(filepath.Dir(parent)) == ControlDir {
		return filepath.Dir(filepath.Dir(parent)), nil
	}
	return abs, nil
}

// ReadControlFile reads a workspace control file once. Callers hash and parse
// the returned bytes, so the trusted content is the content that is applied.
func ReadControlFile(workspace, name string) ([]byte, bool, error) {
	file := filepath.Join(workspace, ControlDir, name)
	f, err := os.Open(file)
	if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxTrustedBytes+1))
	if err != nil {
		return nil, false, err
	}
	if len(raw) > maxTrustedBytes {
		return nil, false, errors.New("file exceeds 1 MiB")
	}
	return raw, true, nil
}

// CheckTrust compares content with the decision recorded in configDir.
func CheckTrust(workspace, configDir, name string, raw []byte, exists bool) (TrustStatus, error) {
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return TrustStatus{}, err
	}
	key, err := TrustWorkspaceKey(workspace)
	if err != nil {
		return TrustStatus{}, err
	}
	status := TrustStatus{File: name, Path: filepath.Join(abs, ControlDir, name), Workspace: key, State: TrustMissing}
	if !exists {
		return status, nil
	}
	status.SHA256, status.State = policyHash(string(raw)), TrustUntrusted
	if reason := trustStoreExposed(key, configDir); reason != "" {
		status.Reason = reason
		return status, nil
	}
	store, err := readTrustStore(configDir)
	if err != nil {
		return TrustStatus{}, err
	}
	for _, entry := range store.Entries {
		if entry.Workspace == key && entry.File == name {
			if entry.SHA256 == status.SHA256 {
				status.State = TrustTrusted
			} else {
				status.State = TrustChanged
			}
		}
	}
	return status, nil
}

// WorkspaceTrust reports both control files using the default configuration
// directory. It reads files but never changes the trust store.
func WorkspaceTrust(workspace string) ([]TrustStatus, error) {
	dir, err := ConfigDirectory()
	if err != nil {
		return nil, err
	}
	return WorkspaceTrustFrom(workspace, dir)
}

func WorkspaceTrustFrom(workspace, configDir string) ([]TrustStatus, error) {
	out := make([]TrustStatus, 0, len(TrustedFiles))
	for _, name := range TrustedFiles {
		raw, exists, err := ReadControlFile(workspace, name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Join(workspace, ControlDir, name), err)
		}
		status, err := CheckTrust(workspace, configDir, name, raw, exists)
		if err != nil {
			return nil, err
		}
		out = append(out, status)
	}
	return out, nil
}

// TrustContent records the hash of each reviewed file, replacing an earlier
// decision for the same workspace and file.
func TrustContent(workspace, configDir string, files map[string][]byte, now time.Time) error {
	key, err := TrustWorkspaceKey(workspace)
	if err != nil {
		return err
	}
	if reason := trustStoreExposed(key, configDir); reason != "" {
		return errors.New(reason)
	}
	unlock, err := lockTrustStore(configDir)
	if err != nil {
		return err
	}
	defer unlock()
	store, err := readTrustStore(configDir)
	if err != nil {
		return err
	}
	kept := store.Entries[:0]
	for _, entry := range store.Entries {
		if _, replaced := files[entry.File]; !(replaced && entry.Workspace == key) {
			kept = append(kept, entry)
		}
	}
	for name, raw := range files {
		kept = append(kept, trustEntry{Workspace: key, File: name, SHA256: policyHash(string(raw)), TrustedAt: now.UTC()})
	}
	store.Entries = kept
	return writeTrustStore(configDir, store)
}

// RevokeTrust removes every decision for the workspace and reports how many.
func RevokeTrust(workspace, configDir string) (int, error) {
	key, err := TrustWorkspaceKey(workspace)
	if err != nil {
		return 0, err
	}
	unlock, err := lockTrustStore(configDir)
	if err != nil {
		return 0, err
	}
	defer unlock()
	store, err := readTrustStore(configDir)
	if err != nil {
		return 0, err
	}
	kept := store.Entries[:0]
	for _, entry := range store.Entries {
		if entry.Workspace != key {
			kept = append(kept, entry)
		}
	}
	removed := len(store.Entries) - len(kept)
	if removed == 0 {
		return 0, nil
	}
	store.Entries = kept
	return removed, writeTrustStore(configDir, store)
}

// A store the agent can reach with its workspace file tools proves nothing.
func trustStoreExposed(workspaceKey, configDir string) string {
	abs, err := filepath.Abs(configDir)
	if err != nil {
		return "trust store location cannot be resolved"
	}
	if inside(workspaceKey, filepath.Clean(abs)) || inside(workspaceKey, resolvePolicyPath(abs)) {
		return "trust store " + TrustStorePath(abs) + " is inside the workspace; move the configuration directory outside it"
	}
	return ""
}

// lockTrustStore serializes the read-modify-write of the store between
// processes, so a concurrent approval cannot write back a decision another
// process just revoked. The lock is a file created exclusively next to the
// store; a process that dies while holding it leaves the file behind, and the
// error names it rather than guessing that it is stale.
func lockTrustStore(configDir string) (func(), error) {
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return nil, err
	}
	lock := TrustStorePath(configDir) + ".lock"
	deadline := time.Now().Add(trustLockWait)
	for {
		f, err := os.OpenFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(lock) }, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("trust store lock %s: %w", lock, err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("trust store is locked by another cloud-harness process; retry, or remove %s if none is running", lock)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readTrustStore(configDir string) (trustStore, error) {
	store := trustStore{Version: 1}
	file := TrustStorePath(configDir)
	raw, err := os.ReadFile(file)
	if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
		return store, nil
	}
	if err != nil {
		return store, fmt.Errorf("trust store %s: %w", file, err)
	}
	if err := decodeStrict(raw, &store); err != nil {
		return store, fmt.Errorf("trust store %s: %w", file, err)
	}
	if store.Version != 1 {
		return store, fmt.Errorf("trust store %s: unsupported version %d", file, store.Version)
	}
	return store, nil
}

func writeTrustStore(configDir string, store trustStore) error {
	sort.SliceStable(store.Entries, func(i, j int) bool {
		if store.Entries[i].Workspace != store.Entries[j].Workspace {
			return store.Entries[i].Workspace < store.Entries[j].Workspace
		}
		return store.Entries[i].File < store.Entries[j].File
	})
	if store.Entries == nil {
		store.Entries = []trustEntry{}
	}
	raw, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(configDir, trustStoreFile+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), TrustStorePath(configDir))
}

// ControlPath reports whether target is the workspace's harness control
// directory or a path below it. The name is compared without case so a
// case-insensitive file system cannot be used to reach the same directory.
func ControlPath(workspace, target string) bool {
	for _, root := range []string{filepath.Clean(workspace), resolvePolicyPath(workspace)} {
		rel, err := filepath.Rel(root, target)
		if err != nil {
			continue
		}
		first, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
		if strings.EqualFold(first, ControlDir) {
			return true
		}
	}
	// A control directory that is a symlink stays protected under the real
	// name of the directory it points at.
	if dir, err := resolvePolicyPathHop(filepath.Join(filepath.Clean(workspace), ControlDir), 0); err == nil {
		return inside(dir, target)
	}
	return false
}

// Behavior files are ordinary workspace files, but later runs load them as
// instructions: a write to one outlives the run that made it.
const (
	BehaviorAgents = "agents"
	BehaviorRules  = "rules"
)

// BehaviorFile classifies target as BehaviorAgents (under the workspace
// .agents directory, which holds skills and agent definitions), BehaviorRules
// (an AGENTS.md at any depth), or "". Like ControlPath it compares names
// case-insensitively and also inspects the symlink-resolved target.
func BehaviorFile(workspace, target string) string {
	if !filepath.IsAbs(target) {
		target = filepath.Join(workspace, target)
	}
	target = filepath.Clean(target)
	kind := ""
	for _, name := range []string{target, resolvePolicyPath(target)} {
		for _, root := range []string{filepath.Clean(workspace), resolvePolicyPath(workspace)} {
			rel, err := filepath.Rel(root, name)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			first, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
			if strings.EqualFold(first, ".agents") {
				return BehaviorAgents
			}
			if strings.EqualFold(filepath.Base(rel), "AGENTS.md") {
				kind = BehaviorRules
			}
		}
	}
	return kind
}
