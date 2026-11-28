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

// BatchPair identifies one tool execution within a batch. Resumes and retries
// have independent baselines, even when they share a batch and invocation ID.
type BatchPair struct {
	Number        int
	Before, After storage.WorkspaceSnapshot
}

type captureWindow struct {
	BatchPair
	run, batch  string
	first, last int
	records     int
}

func captureLabel(rec storage.WorkspaceSnapshot) (batch, attempt, phase string, number int, ok bool) {
	tail, ok := strings.CutPrefix(rec.Label, rec.RunID+"/batch-")
	if !ok || rec.RunID == "" {
		return "", "", "", 0, false
	}
	num, rest, _ := strings.Cut(tail, "/")
	n, err := strconv.Atoi(num)
	if err != nil || n < 1 || strconv.Itoa(n) != num {
		return "", "", "", 0, false
	}
	batch = rec.RunID + "/batch-" + num
	attempt, phase, ok = strings.Cut(rest, "/")
	if !ok {
		attempt, phase = "", rest // Historical rewind checkpoint.
	} else if !strings.HasPrefix(attempt, "exec_") || attempt == "exec_" {
		return "", "", "", 0, false
	}
	return batch, attempt, phase, n, phase == "pre" || phase == "post"
}

// ReviewPairs consumes chronological workspace history. Legacy batch-wide
// checkpoints cannot prove that no approval wait occurred inside their diff.
func ReviewPairs(records []storage.WorkspaceSnapshot, runID, workspaceID, path string) (pairs []BatchPair, incomplete bool) {
	windows := map[string]*captureWindow{}
	var ordered []*captureWindow
	type checkpointHistory struct {
		run                                  string
		first, last                          int
		hasDispatch, dispatchSinceCheckpoint bool
	}
	checkpoints := map[string]*checkpointHistory{}
	depth := make([]int, len(records)+1)
	for i, rec := range records {
		if rec.WorkspaceID != workspaceID || (rec.WorkspacePath != "" && rec.WorkspacePath != path) {
			continue
		}
		rec.WorkspacePath = path
		batch, attempt, phase, n, ok := captureLabel(rec)
		if !ok {
			if rec.RunID != runID {
				depth[i]++
				depth[i+1]--
			}
			continue
		}
		h := checkpoints[batch]
		if h == nil {
			h = &checkpointHistory{run: rec.RunID, first: -1, last: -1}
			checkpoints[batch] = h
		}
		if attempt == "" {
			if h.first < 0 {
				h.first = i
			}
			// A broad post with no dispatch boundaries since the previous
			// checkpoint can span an approval wait, even in a batch resumed
			// after upgrading. Never expose that interval for restore.
			if phase == "post" && !h.dispatchSinceCheckpoint {
				if rec.RunID == runID {
					incomplete = true
				} else {
					start := max(h.last, h.first)
					depth[start]++
					depth[i+1]--
				}
			}
			h.last, h.dispatchSinceCheckpoint = i, false
			continue
		}
		h.hasDispatch, h.dispatchSinceCheckpoint = true, true
		key := batch + "/" + attempt
		w := windows[key]
		if w == nil {
			w = &captureWindow{BatchPair: BatchPair{Number: n}, run: rec.RunID, batch: batch, first: i}
			windows[key] = w
			ordered = append(ordered, w)
		}
		w.records++
		w.last = i
		if phase == "pre" {
			w.Before = rec
		} else {
			w.After = rec
		}
	}

	for _, h := range checkpoints {
		if h.hasDispatch {
			continue
		}
		if h.run == runID {
			incomplete = true
		} else {
			depth[h.first]++
			depth[h.last+1]--
		}
	}
	for _, w := range ordered {
		first, last := w.first, w.last
		if w.run != runID {
			// A missing boundary from a concurrent writer leaves the
			// extent of that execution unknown up to its rewind checkpoints.
			h := checkpoints[w.batch]
			if w.Before.ID == "" {
				first = 0
				if h.first >= 0 && h.first < w.first {
					first = h.first
				}
			}
			if w.After.ID == "" {
				last = len(records) - 1
				if h.last > w.last {
					last = h.last
				}
			}
		}
		depth[first]++
		depth[last+1]--
	}
	// Prefix counts allow each complete pair to check for an overlapping
	// execution (including an enclosing interval) in constant time.
	active, overlaps := 0, 0
	for i, delta := range depth {
		active += delta
		depth[i] = overlaps
		if active > 1 {
			overlaps++
		}
	}
	for _, w := range ordered {
		if w.run != runID {
			continue
		}
		if w.records != 2 || w.Before.ID == "" || w.After.ID == "" ||
			records[w.first].ID != w.Before.ID || depth[w.first] != depth[w.last+1] {
			incomplete = true
			continue
		}
		pairs = append(pairs, w.BatchPair)
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
