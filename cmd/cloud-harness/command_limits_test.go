package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/config"
	"github.com/BrianOGG/cloud-harness/internal/tools"
)

func TestOpenManagerAppliesCommandLimits(t *testing.T) {
	cpu := uint64(7)
	cfg := config.Config{
		DBPath: filepath.Join(t.TempDir(), "harness.db"), Provider: "mock",
		CommandLimits: config.CommandLimits{CPUSeconds: &cpu},
	}
	mgr, db, err := openManager(context.Background(), cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	limits := mgr.Broker.CommandLimits().Effective()
	if limits["cpu_seconds"] != 7 || limits["open_files"] != tools.DefaultCommandOpenFiles {
		t.Fatalf("broker command limits = %v", limits)
	}

	cfg.DBPath = filepath.Join(t.TempDir(), "harness.db")
	cfg.CommandLimits = config.CommandLimits{Disabled: true}
	mgr, db2, err := openManager(context.Background(), cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db2.Close() }()
	if limits := mgr.Broker.CommandLimits().Effective(); len(limits) != 0 {
		t.Fatalf("disabled command limits = %v, want none", limits)
	}
}
