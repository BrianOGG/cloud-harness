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

func TestServeRefusesNonLoopbackWithoutToken(t *testing.T) {
	validToken := "0123456789abcdef"

	// Non-loopback + no token + no allow flag -> error.
	if err := ValidateBind("0.0.0.0:19421", "", false); err == nil {
		t.Fatal("expected error for non-loopback bind without token or allow flag")
	}
	cfg := Config{ListenAddr: "0.0.0.0:19421"}
	if err := cfg.ValidateBind(); err == nil {
		t.Fatal("expected Config.ValidateBind error for non-loopback without token or allow flag")
	}

	// Non-loopback + no token + allow flag -> ok.
	if err := ValidateBind("0.0.0.0:19421", "", true); err != nil {
		t.Fatalf("expected non-loopback with allowInsecure=true to succeed, got %v", err)
	}
	cfgAllow := Config{ListenAddr: "0.0.0.0:19421", HTTPAllowInsecure: true}
	if err := cfgAllow.ValidateBind(); err != nil {
		t.Fatalf("expected Config.ValidateBind with HTTPAllowInsecure=true to succeed, got %v", err)
	}

	// Non-loopback + valid token + no allow flag -> ok.
	if err := ValidateBind("0.0.0.0:19421", validToken, false); err != nil {
		t.Fatalf("expected non-loopback with valid token to succeed, got %v", err)
	}
	cfgToken := Config{ListenAddr: "0.0.0.0:19421", HTTPToken: validToken}
	if err := cfgToken.ValidateBind(); err != nil {
		t.Fatalf("expected Config.ValidateBind with token to succeed, got %v", err)
	}

	// Loopback + no token + no allow flag -> ok.
	for _, addr := range []string{"127.0.0.1:19421", "localhost:19421", "[::1]:19421"} {
		if err := ValidateBind(addr, "", false); err != nil {
			t.Fatalf("expected loopback %s without token to succeed, got %v", addr, err)
		}
	}

	// Token shorter than minimum length -> error.
	if err := ValidateBind("127.0.0.1:19421", "short-token", false); err == nil {
		t.Fatal("expected short http_token to be rejected")
	}
	if _, err := Load("", func(k string) string {
		if k == "CLOUD_HARNESS_HTTP_TOKEN" {
			return "too-short"
		}
		return ""
	}); err == nil {
		t.Fatal("expected Load to reject short CLOUD_HARNESS_HTTP_TOKEN")
	}
}

func TestMCPServersConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "harness.json")
	body := []byte(`{
		"servers": [
			{"name": "fetcher", "command": "python", "args": ["-m", "mcp_fetch"], "readonly": true},
			{"name": "writer", "command": "node", "args": ["srv.js"], "readonly": false}
		]
	}`)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Servers) != 2 {
		t.Fatalf("expected 2 servers, got %d", len(cfg.Servers))
	}
	if cfg.Servers[0].Name != "fetcher" || !cfg.Servers[0].Readonly {
		t.Fatalf("unexpected server[0]: %+v", cfg.Servers[0])
	}
	if cfg.Servers[1].Name != "writer" || cfg.Servers[1].Readonly {
		t.Fatalf("unexpected server[1]: %+v", cfg.Servers[1])
	}
}
