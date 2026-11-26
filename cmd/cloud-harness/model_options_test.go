package main

import (
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/config"
)

func TestConfiguredModelOptions(t *testing.T) {
	cfg := config.Config{Provider: "anthropic", AnthropicModel: "default-anthropic", ModelProfiles: map[string]config.ModelProfile{"z": {Model: "last"}, "a": {Model: "first", FallbackModel: "backup"}}}
	rows := configuredModelOptions(cfg)
	if len(rows) != 3 || rows[0].ID != "default" || rows[0].Model != "default-anthropic" || rows[0].Provider != "anthropic" || rows[1].ID != "profile:a" || rows[1].Model != "first" || rows[1].FallbackModel != "backup" || rows[2].ID != "profile:z" {
		t.Fatalf("catalog=%+v", rows)
	}
	cfg.ModelRoles.Executor = "a"
	rows = configuredModelOptions(cfg)
	if rows[0].Model != "first" || rows[0].FallbackModel != "backup" {
		t.Fatal("default executor route was lost")
	}
	rows = configuredModelOptions(config.Config{})
	if len(rows) != 1 || rows[0].ID != "default" || rows[0].Model != config.DefaultModel || rows[0].Provider != "openai" {
		t.Fatalf("defaults=%+v", rows)
	}
}
