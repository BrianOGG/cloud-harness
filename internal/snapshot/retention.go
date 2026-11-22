package snapshot

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/BrianOGG/cloud-harness/internal/storage"
)

// ReclaimStats describes unique Git objects reachable only from the removed
// snapshot refs. Bytes are the stored sizes Git reports for those objects.
type ReclaimStats struct {
	Objects int64 `json:"objects"`
	Bytes   int64 `json:"bytes"`
}

// ReclaimWorkspaceSnapshotObjects removes refs for deleted snapshot records
// and prunes unreachable objects. In dry-run mode it only estimates the
// snapshot objects no longer reachable from another shadow ref.
func ReclaimWorkspaceSnapshotObjects(ctx context.Context, workspacePath, workspaceID string, records []storage.WorkspaceSnapshot, dryRun bool) (ReclaimStats, error) {
	var stats ReclaimStats
	if len(records) == 0 {
		return stats, nil
	}
	abs, err := filepath.Abs(workspacePath)
	if err != nil {
		return stats, err
	}
	abs = filepath.Clean(abs)
	if got := workspaceIDFor(abs); got != workspaceID {
		return stats, fmt.Errorf("workspace path %q identifies %q, not %q", abs, got, workspaceID)
	}
	if _, err := os.Stat(abs); err != nil {
		return stats, err
	}
	shadowDir := filepath.Join(abs, ".cloud-harness", "snapshots", workspaceID)
	info, err := os.Stat(filepath.Join(shadowDir, "HEAD"))
	if errors.Is(err, os.ErrNotExist) {
		return stats, nil
	}
	if err != nil {
		return stats, fmt.Errorf("inspect shadow repository: %w", err)
	}
	if info.IsDir() {
		return stats, fmt.Errorf("shadow repository HEAD is a directory: %s", shadowDir)
	}
	mu, _ := workspaceLocks.LoadOrStore(shadowDir, &sync.Mutex{})
	lock := mu.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	gitBin, err := exec.LookPath("git")
	if err != nil {
		return stats, err
	}
	deletedRefs := make(map[string]struct{}, len(records))
	var candidateTrees []string
	for _, rec := range records {
		deletedRefs["refs/cloud-harness/snapshots/"+rec.ID] = struct{}{}
		if rec.TreeOID != "" {
			candidateTrees = append(candidateTrees, rec.TreeOID)
		}
	}

	allRefs, err := runShadowGit(ctx, gitBin, abs, shadowDir, nil, "for-each-ref", "--format=%(refname) %(objectname)", "refs/cloud-harness/snapshots/")
	if err != nil {
		return stats, err
	}
	var liveTrees []string
	for _, line := range strings.Split(strings.TrimSpace(string(allRefs)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if _, remove := deletedRefs[fields[0]]; !remove {
			liveTrees = append(liveTrees, fields[1])
		}
	}
	candidateObjects, err := reachableObjects(ctx, gitBin, abs, shadowDir, candidateTrees)
	if err != nil {
		return stats, err
	}
	liveObjects, err := reachableObjects(ctx, gitBin, abs, shadowDir, liveTrees)
	if err != nil {
		return stats, err
	}
	var reclaimable []string
	for oid := range candidateObjects {
		if _, retained := liveObjects[oid]; !retained {
			reclaimable = append(reclaimable, oid)
		}
	}
	stats.Objects = int64(len(reclaimable))
	if len(reclaimable) > 0 {
		input := []byte(strings.Join(reclaimable, "\n") + "\n")
		meta, err := runShadowGit(ctx, gitBin, abs, shadowDir, input, "cat-file", "--batch-check=%(objectname) %(objecttype) %(objectsize)")
		if err != nil {
			return ReclaimStats{}, err
		}
		for _, line := range strings.Split(strings.TrimSpace(string(meta)), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 3 {
				return ReclaimStats{}, fmt.Errorf("git returned malformed object size %q", line)
			}
			size, err := strconv.ParseInt(fields[2], 10, 64)
			if err != nil {
				return ReclaimStats{}, fmt.Errorf("parse Git object size %q: %w", fields[2], err)
			}
			stats.Bytes += size
		}
	}
	if dryRun {
		return stats, nil
	}
	for ref := range deletedRefs {
		if _, err := runShadowGit(ctx, gitBin, abs, shadowDir, nil, "update-ref", "-d", ref); err != nil {
			return stats, err
		}
	}
	if _, err := runShadowGit(ctx, gitBin, abs, shadowDir, nil, "reflog", "expire", "--expire=now", "--all"); err != nil {
		return stats, err
	}
	if _, err := runShadowGit(ctx, gitBin, abs, shadowDir, nil, "gc", "--prune=now"); err != nil {
		return stats, err
	}
	return stats, nil
}

func reachableObjects(ctx context.Context, gitBin, workspace, shadowDir string, roots []string) (map[string]struct{}, error) {
	objects := make(map[string]struct{})
	if len(roots) == 0 {
		return objects, nil
	}
	input := []byte(strings.Join(roots, "\n") + "\n")
	raw, err := runShadowGit(ctx, gitBin, workspace, shadowDir, input, "rev-list", "--objects", "--no-object-names", "--stdin")
	if err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		if oid := strings.TrimSpace(scanner.Text()); oid != "" {
			objects[oid] = struct{}{}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return objects, nil
}

func runShadowGit(ctx context.Context, gitBin, workspace, shadowDir string, input []byte, args ...string) ([]byte, error) {
	fullArgs := append([]string{"--git-dir=" + shadowDir, "--work-tree=" + workspace}, args...)
	cmd := exec.CommandContext(ctx, gitBin, fullArgs...)
	cmd.Dir = workspace
	cmd.Env = gitEnvironment(shadowDir, workspace)
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}
