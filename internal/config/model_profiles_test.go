package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

func TestLoadModelRoutingMissingConfigFile(t *testing.T) {
	// A config path that names no file must mean "routing unconfigured",
	// not a fatal run error: the loop calls LoadModelRouting on every run,
	// and callers (e.g. policy tests) legitimately point CLOUD_HARNESS_CONFIG
	// at an isolated path that may not exist yet.
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	t.Setenv("CLOUD_HARNESS_MODEL_PROFILES", "")
	t.Setenv("CLOUD_HARNESS_MODEL_ROLES", "")
	routing, err := LoadModelRouting()
	if err != nil {
		t.Fatalf("LoadModelRouting() error = %v", err)
	}
	if _, _, ok := routing.Profile(RoleExecutor); ok {
		t.Fatalf("missing config file should yield no executor profile: %+v", routing)
	}
}

func TestLoadModelProfilesAndRoleOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness.json")
	body := `{
		"openai_model": "legacy",
		"model_profiles": {
			"plan": {"model": "plan-primary"},
			"work": {"model": "work-primary", "fallback_model": "work-backup"},
			"light": {"model": "light-primary"}
		},
		"model_roles": {"planner": "plan", "executor": "work", "fast": "light"}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		role                  ModelRole
		name, model, fallback string
	}{
		{RolePlanner, "plan", "plan-primary", ""},
		{RoleExecutor, "work", "work-primary", "work-backup"},
		{RoleFast, "light", "light-primary", ""},
	} {
		name, profile, ok := cfg.Profile(tc.role)
		if !ok || name != tc.name || profile.Model != tc.model || profile.FallbackModel != tc.fallback {
			t.Fatalf("%s selected %q, %+v, %t", tc.role, name, profile, ok)
		}
	}
	if cfg.OpenAIModel != "legacy" {
		t.Fatalf("default model = %q", cfg.OpenAIModel)
	}
	env := map[string]string{
		"OPENAI_MODEL":                 "environment-default",
		"CLOUD_HARNESS_MODEL_PROFILES": `{"alternate":{"model":"environment-model"}}`,
		"CLOUD_HARNESS_MODEL_ROLES":    `{"executor":"alternate"}`,
	}
	cfg, err = Load(path, func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	if _, profile, ok := cfg.Profile(RoleExecutor); !ok || profile.Model != "environment-model" {
		t.Fatalf("effective executor profile = %+v, mapped = %t", profile, ok)
	}
	if _, _, ok := cfg.Profile(RolePlanner); ok || len(cfg.ModelProfiles) != 1 || cfg.OpenAIModel != "environment-default" {
		t.Fatalf("environment did not replace the routing objects: %+v", cfg.ModelRouting)
	}
}

func TestInvalidModelProfileConfig(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"model_profiles":{"empty":{"fallback_model":"backup"}}}`, "model_profiles.empty.model"},
		{`{"model_profiles":{"p":{"model":"a","fallback_model":"a"}}}`, "model_profiles.p.fallback_model"},
		{`{"model_roles":{"fast":"missing"}}`, `model_roles.fast refers to unknown profile "missing"`},
		{`{"model_roles":{"oracle":"p"}}`, `unknown field "oracle"`},
		{`{"model_profiles":{"p":{"model":"a","api_key":"secret"}}}`, `unknown field "api_key"`},
	} {
		path := filepath.Join(t.TempDir(), "harness.json")
		if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path, func(string) string { return "" })
		if !domain.IsCode(err, domain.CodeInvalidArgument) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Load(%s) = %v; want %q", tc.body, err, tc.want)
		}
	}
	_, err := Load("", func(key string) string {
		if key == "CLOUD_HARNESS_MODEL_ROLES" {
			return `{} {}`
		}
		return ""
	})
	if !domain.IsCode(err, domain.CodeInvalidArgument) || !strings.Contains(err.Error(), "CLOUD_HARNESS_MODEL_ROLES") {
		t.Fatalf("malformed environment override error = %v", err)
	}
}
