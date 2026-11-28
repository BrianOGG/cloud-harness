package snapshot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/storage"
	"github.com/BrianOGG/cloud-harness/internal/worktree"
)

func (s *Snapshotter) fileEntry(ctx context.Context, rec storage.WorkspaceSnapshot, path string) (treeEntry, error) {
	entries, err := s.treeEntries(ctx, rec.TreeOID, path)
	if err != nil || len(entries) == 0 {
		return treeEntry{}, err
	}
	entry := entries[0]
	if len(entries) != 1 || entry.path != path || entry.typeName != "blob" ||
		(entry.mode != "100644" && entry.mode != "100755" && entry.mode != "120000") {
		return treeEntry{}, domain.E(domain.CodeConflict, "此路径是目录或不支持的文件类型")
	}
	return entry, nil
}

// RestoreReviewedFile captures a CLI-compatible safety point and atomically
// applies one exact snapshot entry. guard must exclude workspace writers and
// revalidate the run binding during each callback.
func (s *Snapshotter) RestoreReviewedFile(ctx context.Context, pair BatchPair, path string, guard func(func() error) error) (storage.WorkspaceSnapshot, error) {
	var safety storage.WorkspaceSnapshot
	if err := s.checkPair(pair, path); err != nil {
		return safety, domain.E(domain.CodeInvalidArgument, "文件或快照绑定无效")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	before, err := s.fileEntry(ctx, pair.Before, path)
	if err != nil {
		return safety, err
	}
	after, err := s.fileEntry(ctx, pair.After, path)
	if err != nil {
		return safety, err
	}
	if before == after {
		return safety, domain.E(domain.CodeConflict, "文件没有可还原的更改")
	}
	root, err := os.OpenRoot(s.workspace)
	if err != nil {
		return safety, err
	}
	defer func() { _ = root.Close() }()
	var expected []byte
	if after.oid != "" {
		expected, err = s.git(ctx, "cat-file", "blob", after.oid)
		if err != nil {
			return safety, err
		}
	}
	checkLive := func() error {
		if err := s.checkExistingParents(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return domain.E(domain.CodeConflict, "文件父目录已改变")
		}
		if err := worktree.CheckRestoreIndex(ctx, s.workspace, path); err != nil {
			return domain.E(domain.CodeConflict, "工作区存在暂存更改、合并冲突或索引不可读")
		}
		if !matchesFile(root, path, after, expected) {
			return domain.E(domain.CodeConflict, "文件在本批结束后已改变，请刷新并检查工作区")
		}
		return nil
	}
	if err := guard(checkLive); err != nil {
		return safety, err
	}

	// Preserve the existing CLI's code-only rewind recovery point. Never use
	// full-tree Restore here: unselected paths may belong to a later run.
	safety, err = s.snapshotLocked(ctx, fmt.Sprintf("rewind-safety/%s/%s", pair.Before.RunID, time.Now().UTC().Format("20060102T150405.000000000Z")))
	if err != nil {
		return safety, fmt.Errorf("create restore safety snapshot: %w", err)
	}
	current, err := s.fileEntry(ctx, safety, path)
	if err != nil {
		return safety, err
	}
	if current != after {
		return safety, domain.E(domain.CodeConflict, "文件在本批结束后已改变，请刷新并检查工作区")
	}
	// Stage exact bytes outside the visible tree before acquiring the database
	// write reservation. A failed write or rename leaves the original intact.
	staged := ""
	if before.oid != "" {
		data, err := s.git(ctx, "cat-file", "blob", before.oid)
		if err != nil {
			return safety, err
		}
		temp, err := os.MkdirTemp(s.shadowDir, "restore-")
		if err != nil {
			return safety, err
		}
		defer func() { _ = os.RemoveAll(temp) }()
		full := filepath.Join(temp, "file")
		if before.mode == "120000" {
			err = os.Symlink(string(data), full)
		} else {
			err = os.WriteFile(full, data, 0600)
		}
		if err != nil {
			return safety, err
		}
		staged, err = filepath.Rel(s.workspace, full)
		if err != nil {
			return safety, err
		}
	}
	err = guard(func() error {
		if err := checkLive(); err != nil {
			return err
		}
		if before.oid == "" {
			return root.Remove(path)
		}
		if before.mode != "120000" {
			// Git retains only the owner's execute bit. Never publish old
			// bytes to group/other based on a synthesized Git mode.
			mode := os.FileMode(0600)
			if after.mode == "100644" || after.mode == "100755" {
				info, err := root.Lstat(path)
				if err != nil {
					return err
				}
				mode &= info.Mode().Perm()
			}
			if before.mode == "100755" {
				mode |= 0100
			}
			if err := root.Chmod(staged, mode); err != nil {
				return err
			}
		}
		if after.oid == "" {
			if err := root.MkdirAll(filepath.Dir(path), 0755); err != nil {
				return err
			}
			// Link is a no-clobber creation, including for a dangling symlink.
			return root.Link(staged, path)
		}
		return root.Rename(staged, path)
	})
	return safety, err
}

func matchesFile(root *os.Root, path string, entry treeEntry, expected []byte) bool {
	info, err := root.Lstat(path)
	if entry.oid == "" {
		return errors.Is(err, os.ErrNotExist)
	}
	if err != nil {
		return false
	}
	if entry.mode == "120000" {
		target, err := root.Readlink(path)
		return err == nil && target == string(expected)
	}
	if !info.Mode().IsRegular() || (info.Mode()&0100 != 0) != (entry.mode == "100755") || info.Size() != int64(len(expected)) {
		return false
	}
	data, err := root.ReadFile(path)
	return err == nil && bytes.Equal(data, expected)
}
