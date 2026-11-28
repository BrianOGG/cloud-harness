package transport

import (
	"context"
	"net/http"
	"path/filepath"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/snapshot"
	"github.com/BrianOGG/cloud-harness/internal/storage"
	"github.com/BrianOGG/cloud-harness/internal/worktree"
)

type reviewedFile struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}
type reviewedBatch struct {
	Number int            `json:"number"`
	Before string         `json:"before"`
	After  string         `json:"after"`
	Files  []reviewedFile `json:"files"`
}
type review struct {
	Batches    []reviewedBatch `json:"batches"`
	Incomplete bool            `json:"incomplete"`
	Terminal   bool            `json:"terminal"`
}

func retainedBinding(ctx context.Context, workspace string, run storage.Run) error {
	if run.WorktreePath == "" {
		return nil
	}
	if run.WorktreeState != "" {
		return domain.E(domain.CodeConflict, "运行工作树已合并或丢弃")
	}
	info := worktree.Info{RunID: run.ID, Path: run.WorktreePath, Branch: run.WorktreeBranch, BaseSHA: run.WorktreeBaseSHA}
	if err := worktree.CheckRetained(ctx, workspace, info); err != nil {
		return domain.E(domain.CodeConflict, "运行工作树已移除或绑定已改变")
	}
	return nil
}

type changeReview struct {
	run        storage.Run
	binding    storage.Run
	snapper    *snapshot.Snapshotter
	pairs      []snapshot.BatchPair
	incomplete bool
}

func (s *Server) openReview(ctx context.Context, id string) (*changeReview, error) {
	run, err := s.Manager.Store.GetRun(ctx, id)
	if err != nil {
		return nil, err
	}
	binding := run
	// Delegated runs inherit the parent workspace ID without owning a
	// separate worktree. Check the actual owner through its persisted run.
	if run.ParentRunID != "" && run.WorktreePath == "" {
		parent, err := s.Manager.Store.GetRun(ctx, run.ParentRunID)
		if err != nil || parent.SessionID != run.SessionID || parent.WorkspaceID != run.WorkspaceID {
			return nil, domain.E(domain.CodeConflict, "子运行工作区绑定不可用")
		}
		binding = parent
	}
	path := s.Manager.Workspace
	if path == "" {
		return nil, domain.E(domain.CodeConflict, "服务未绑定工作区")
	}
	if err := retainedBinding(ctx, path, binding); err != nil {
		return nil, err
	}
	if binding.WorktreePath != "" {
		path = binding.WorktreePath
	}
	idForPath, err := snapshot.WorkspaceIDForPath(path)
	if err != nil || idForPath != run.WorkspaceID {
		return nil, domain.E(domain.CodeConflict, "此运行未绑定当前工作区")
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	store, ok := s.Manager.Store.(snapshot.RecordStore)
	if !ok {
		return nil, domain.E(domain.CodeNotImplemented, "此存储不支持工作区快照")
	}
	records, err := store.ListWorkspaceSnapshots(ctx, idForPath)
	if err != nil {
		return nil, err
	}
	pairs, incomplete := snapshot.ReviewPairs(records, run.ID, idForPath, path)
	result := &changeReview{run: run, binding: binding, pairs: pairs, incomplete: incomplete}
	if len(pairs) != 0 {
		result.snapper, err = snapshot.OpenExisting(path, store)
		if err != nil || result.snapper.Status().State != snapshot.StateEnabled {
			return nil, domain.E(domain.CodeConflict, "无法读取此运行的 Git 快照")
		}
	}
	return result, nil
}

func (s *Server) runChanges(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx := r.Context()
	reviewed, err := s.openReview(ctx, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	reply := review{Batches: []reviewedBatch{}, Incomplete: reviewed.incomplete, Terminal: domain.TerminalRun(reviewed.run.Status)}
	for _, pair := range reviewed.pairs {
		files, err := reviewed.snapper.ChangedPaths(ctx, pair.Before, pair.After)
		if err != nil {
			writeErr(w, domain.E(domain.CodeConflict, "快照内容已不可用"))
			return
		}
		batch := reviewedBatch{Number: pair.Number, Before: pair.Before.ID, After: pair.After.ID, Files: []reviewedFile{}}
		for _, change := range files {
			batch.Files = append(batch.Files, reviewedFile{Path: change.Path, Status: change.Status})
		}
		reply.Batches = append(reply.Batches, batch)
	}
	writeJSON(w, http.StatusOK, reply)
}

func (rv *changeReview) findFile(ctx context.Context, before, after, path string) (snapshot.BatchPair, error) {
	if before == "" || after == "" || path == "" {
		return snapshot.BatchPair{}, domain.E(domain.CodeInvalidArgument, "缺少快照或文件路径")
	}
	for _, pair := range rv.pairs {
		if pair.Before.ID != before || pair.After.ID != after {
			continue
		}
		files, err := rv.snapper.ChangedPaths(ctx, pair.Before, pair.After)
		if err != nil {
			return snapshot.BatchPair{}, domain.E(domain.CodeConflict, "快照内容已不可用")
		}
		for _, file := range files {
			if file.Path == path {
				return pair, nil
			}
		}
		return snapshot.BatchPair{}, domain.E(domain.CodeNotFound, "此文件不在本批更改中")
	}
	return snapshot.BatchPair{}, domain.E(domain.CodeConflict, "本批快照已改变或不属于此运行，请刷新")
}

func (s *Server) runFileDiff(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx := r.Context()
	reviewed, err := s.openReview(ctx, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	q := r.URL.Query()
	pair, err := reviewed.findFile(ctx, q.Get("before"), q.Get("after"), q.Get("path"))
	if err != nil {
		writeErr(w, err)
		return
	}
	patch, truncated, err := reviewed.snapper.FileDiff(ctx, pair, q.Get("path"))
	if err != nil {
		writeErr(w, domain.E(domain.CodeConflict, "无法读取此文件的快照差异"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"diff": patch, "truncated": truncated})
}

func (s *Server) restoreRunFile(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var body struct {
		Before  string `json:"before"`
		After   string `json:"after"`
		Path    string `json:"path"`
		Confirm bool   `json:"confirm"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	if !body.Confirm {
		writeErr(w, domain.E(domain.CodeInvalidArgument, "请明确确认还原文件"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	id := r.PathValue("id")
	store := s.Manager.Store
	run, err := store.GetRun(ctx, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !domain.TerminalRun(run.Status) {
		writeErr(w, domain.E(domain.CodeConflict, "运行结束后才能还原文件"))
		return
	}
	owner := domain.NewID("rewind_")
	now := time.Now().UTC()
	if _, err := store.AcquireLease(ctx, id, owner, now, now.Add(3*time.Minute)); err != nil {
		writeErr(w, err)
		return
	}
	defer func() { _ = store.ReleaseLease(context.WithoutCancel(ctx), id, owner) }()
	rv, err := s.openReview(ctx, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !domain.TerminalRun(rv.run.Status) {
		writeErr(w, domain.E(domain.CodeConflict, "运行结束后才能还原文件"))
		return
	}
	pair, err := rv.findFile(ctx, body.Before, body.After, body.Path)
	if err != nil {
		writeErr(w, err)
		return
	}
	guard, ok := store.(interface {
		WithReviewedRestore(context.Context, storage.WorkspaceSnapshot, storage.WorkspaceSnapshot, string, string, func() error) error
	})
	if !ok {
		writeErr(w, domain.E(domain.CodeNotImplemented, "此存储不支持安全还原"))
		return
	}
	safety, err := rv.snapper.RestoreReviewedFile(ctx, pair, body.Path, func(restore func() error) error {
		return guard.WithReviewedRestore(ctx, pair.Before, pair.After, body.Path, owner, func() error {
			if err := retainedBinding(ctx, s.Manager.Workspace, rv.binding); err != nil {
				return err
			}
			return restore()
		})
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"safety_snapshot": safety.ID})
}
