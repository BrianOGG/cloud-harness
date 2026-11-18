package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadKeyFromEnvOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "harness.json")
	if err := os.WriteFile(path, []byte("{\n  \"provider\": \"openai\",\n  \"openai_model\": \"demo-model\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"CLOUD_HARNESS_ADDR": "127.0.0.1:19555",
		"OPENAI_API_KEY":     "sk-from-env",
		"OPENAI_BASE_URL":    "https://example.invalid/v1",
	}
	cfg, err := Load(path, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenAIAPIKey != "sk-from-env" || cfg.Provider != "openai" || cfg.OpenAIModel != "demo-model" {
		t.Fatalf("%+v", cfg)
	}
	if cfg.ListenAddr != "127.0.0.1:19555" || cfg.OpenAIBaseURL != "https://example.invalid/v1" {
		t.Fatalf("%+v", cfg)
	}
	logged := cfg.Redacted()
	if logged["openai_api_key"] != "set" {
		t.Fatal(logged)
	}
	for _, v := range logged {
		if s, ok := v.(string); ok && s == "sk-from-env" {
			t.Fatal("redacted config contained the key")
		}
	}
}

func TestConfigFileCannotCarryAPIKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "harness.json")
	body := []byte(`{"provider":"openai","api_key":"sk-file"}`)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path, func(string) string { return "" })
	if err == nil {
		t.Fatal("file api key was accepted")
	}
}

func TestListenLoopback(t *testing.T) {
	if err := ValidateListen("127.0.0.1:19421", false); err != nil {
		t.Fatal(err)
	}
	if err := ValidateListen("0.0.0.0:19421", false); err == nil {
		t.Fatal("external bind allowed")
	}
	if err := ValidateListen("0.0.0.0:19421", true); err != nil {
		t.Fatal(err)
	}
}

func TestOpenAIReasoningReplayConfig(t *testing.T) {
	cfg, err := Load("", func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenAIReasoningReplay != "message" {
		t.Fatalf("default reasoning replay = %q, want message", cfg.OpenAIReasoningReplay)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "harness.json")
	if err := os.WriteFile(path, []byte(`{"provider":"openai","openai_reasoning_replay":"omit"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(path, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenAIReasoningReplay != "omit" {
		t.Fatalf("reasoning replay = %q, want omit", cfg.OpenAIReasoningReplay)
	}

	if _, err := Load("", func(k string) string {
		if k == "OPENAI_REASONING_REPLAY" {
			return "invalid"
		}
		return ""
	}); err == nil {
		t.Fatal("accepted invalid openai_reasoning_replay")
	}
}

func TestCommandEnvAllowConfig(t *testing.T) {
	cfg, err := Load("", func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.CommandEnvAllow) != 0 {
		t.Fatalf("default command_env_allow = %v, want empty", cfg.CommandEnvAllow)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "harness.json")
	if err := os.WriteFile(path, []byte(`{"command_env_allow":["FOO","BAR"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(path, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.CommandEnvAllow) != 2 || cfg.CommandEnvAllow[0] != "FOO" || cfg.CommandEnvAllow[1] != "BAR" {
		t.Fatalf("command_env_allow = %v, want [FOO BAR]", cfg.CommandEnvAllow)
	}
}
