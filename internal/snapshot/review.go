package snapshot

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BrianOGG/cloud-harness/internal/storage"
)

// BatchPair identifies two immutable capture points, including the latest
// post-dispatch segment of a batch resumed after approval.
type BatchPair struct {
	Number        int
	Before, After storage.WorkspaceSnapshot
}

// ReviewPairs consumes chronological workspace history. A missing capture or
// another run's interleaved capture cannot supply an attributable diff.
func ReviewPairs(records []storage.WorkspaceSnapshot, runID, workspaceID, path string) (pairs []BatchPair, incomplete bool) {
	type interval struct {
		batch       BatchPair
		first, last int
	}
	groups := map[int]*interval{}
	var ordered []*interval
	// Mark foreign capture windows, including batches enclosing this run's
	// entire pair. Prefix sums let each pair check its window in O(1).
	coverage := make([]int, len(records)+1)
	foreignStarts := map[string]int{}
	for index, rec := range records {
		if rec.WorkspaceID != workspaceID || (rec.WorkspacePath != "" && rec.WorkspacePath != path) {
			continue
		}
		// v4 snapshots predate workspace_path. Their persisted path hash
		// above still binds them to the verified server workspace.
		rec.WorkspacePath = path
		tail, hasPrefix := strings.CutPrefix(rec.Label, rec.RunID+"/batch-")
		num, phase, ok := strings.Cut(tail, "/")
		n, err := strconv.Atoi(num)
		batch := hasPrefix && ok && rec.RunID != "" && err == nil && n > 0 && strconv.Itoa(n) == num && (phase == "pre" || phase == "post")
		if rec.RunID != runID {
			start := index
			if batch {
				key := rec.RunID + "/batch-" + num
				if phase == "pre" {
					foreignStarts[key] = index
				} else if pre, ok := foreignStarts[key]; ok {
					start = pre
				}
			}
			coverage[start]++
			coverage[index+1]--
			continue
		}
		if !batch {
			continue
		}
		g := groups[n]
		if g == nil {
			g = &interval{batch: BatchPair{Number: n}}
			groups[n] = g
			ordered = append(ordered, g)
		}
		if phase == "pre" {
			g.batch.Before, g.first = rec, index
			g.batch.After = storage.WorkspaceSnapshot{}
		} else {
			g.batch.After, g.last = rec, index
		}
	}
	covered, total := 0, 0
	for i, v := range coverage {
		covered += v
		coverage[i] = total
		if covered > 0 {
			total++
		}
	}
	for _, g := range ordered {
		if g.batch.Before.ID == "" || g.batch.After.ID == "" || coverage[g.first] != coverage[g.last+1] {
			incomplete = true
			continue
		}
		pairs = append(pairs, g.batch)
	}
	return pairs, incomplete
}

func reviewPath(path string) bool {
	if !filepath.IsLocal(path) || filepath.ToSlash(filepath.Clean(path)) != path || strings.ContainsAny(path, "\\\x00") {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "." || part == ".." || strings.EqualFold(part, ".git") || strings.EqualFold(part, ".cloud-harness") {
			return false
		}
	}
	return true
}

func (s *Snapshotter) checkPair(pair BatchPair, path string) error {
	if pair.Before.WorkspaceID != s.workspaceID || pair.After.WorkspaceID != s.workspaceID ||
		pair.Before.WorkspacePath != s.workspace || pair.After.WorkspacePath != s.workspace ||
		pair.Before.RunID == "" || pair.Before.RunID != pair.After.RunID || !reviewPath(path) {
		return errors.New("文件或快照绑定无效")
	}
	return s.requireGit()
}

const maxReviewDiff = 64 << 10

type patchBuffer struct {
	data      []byte
	truncated bool
}

func (b *patchBuffer) Write(p []byte) (int, error) {
	n := min(len(p), maxReviewDiff-len(b.data))
	b.data = append(b.data, p[:n]...)
	if n < len(p) {
		b.truncated = true
	}
	return len(p), nil
}

// FileDiff accepts literal path names only. Stream the patch into a capped
// buffer so one large file cannot exhaust the HTTP server's memory.
func (s *Snapshotter) FileDiff(ctx context.Context, pair BatchPair, path string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkPair(pair, path); err != nil {
		return "", false, err
	}
	cmd := exec.CommandContext(ctx, s.gitBin, "--literal-pathspecs", "diff", "--no-ext-diff", "--no-textconv", "--no-color", "--no-renames", "--patch", pair.Before.TreeOID, pair.After.TreeOID, "--", path)
	cmd.Dir, cmd.Env = s.workspace, gitEnvironment(s.shadowDir, s.workspace)
	var out patchBuffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", false, err
	}
	return strings.ToValidUTF8(string(out.data), "�"), out.truncated, nil
}
