package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

func TestRunTokenAndCostLimitsFromFileAndEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness.json")
	if err := os.WriteFile(path, []byte(`{"max_run_tokens": 5000, "max_run_cost_usd": 1.5}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, func(string) string { return "" })
	if err != nil || cfg.MaxRunTokens != 5000 || cfg.MaxRunCostUSD != 1.5 {
		t.Fatalf("file limits=%d %v err=%v", cfg.MaxRunTokens, cfg.MaxRunCostUSD, err)
	}
	env := map[string]string{"CLOUD_HARNESS_MAX_RUN_TOKENS": "9000", "CLOUD_HARNESS_MAX_RUN_COST_USD": "0.25"}
	cfg, err = Load(path, func(k string) string { return env[k] })
	if err != nil || cfg.MaxRunTokens != 9000 || cfg.MaxRunCostUSD != 0.25 {
		t.Fatalf("env limits=%d %v err=%v", cfg.MaxRunTokens, cfg.MaxRunCostUSD, err)
	}
	cfg, err = Load("", func(string) string { return "" })
	if err != nil || cfg.MaxRunTokens != 0 || cfg.MaxRunCostUSD != 0 {
		t.Fatalf("default limits=%d %v err=%v", cfg.MaxRunTokens, cfg.MaxRunCostUSD, err)
	}
	for name, value := range map[string]string{
		"CLOUD_HARNESS_MAX_RUN_TOKENS":   "-1",
		"CLOUD_HARNESS_MAX_RUN_COST_USD": "NaN",
	} {
		if _, err := Load("", func(k string) string {
			if k == name {
				return value
			}
			return ""
		}); !domain.IsCode(err, domain.CodeInvalidArgument) {
			t.Fatalf("%s=%s accepted: %v", name, value, err)
		}
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`{"max_run_cost_usd": -2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad, func(string) string { return "" }); !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("negative cost limit accepted: %v", err)
	}
}
