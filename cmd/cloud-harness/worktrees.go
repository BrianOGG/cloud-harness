package main

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/BrianOGG/cloud-harness/internal/agent"
	"github.com/BrianOGG/cloud-harness/internal/config"
	"github.com/BrianOGG/cloud-harness/internal/storage/sqlite"
	"github.com/BrianOGG/cloud-harness/internal/worktree"
)

func openRunManager(ctx context.Context, cfg config.Config, workspace string, isolated bool) (*agent.Manager, *sqlite.DB, error) {
	if isolated {
		// Validate before openManager creates the database or any run/session.
		if err := worktree.Validate(ctx, workspace); err != nil {
			return nil, nil, err
		}
		if cfg.DBPath == config.DefaultDB {
			root, err := filepath.Abs(workspace)
			if err != nil {
				return nil, nil, err
			}
			cfg.DBPath = filepath.Join(root, config.DefaultDB)
		}
	}
	mgr, db, err := openManager(ctx, cfg, workspace)
	if err != nil {
		return nil, nil, err
	}
	mgr.Worktree = isolated
	return mgr, db, nil
}

func runWorktreeContext(isolated bool) (context.Context, context.CancelFunc) {
	if isolated {
		return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	}
	return context.Background(), func() {}
}
