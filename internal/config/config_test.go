package config

import (
	"os"
	"path/filepath"
	"strings"
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

func TestOpenAIDisableImagesConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"openai_disable_images":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, func(string) string { return "" })
	if err != nil || !cfg.OpenAIDisableImages {
		t.Fatalf("file config=%+v err=%v", cfg, err)
	}
	cfg, err = Load(path, func(key string) string {
		if key == "OPENAI_DISABLE_IMAGES" {
			return "false"
		}
		return ""
	})
	if err != nil || cfg.OpenAIDisableImages {
		t.Fatalf("environment override=%+v err=%v", cfg, err)
	}
	_, err = Load("", func(key string) string {
		if key == "OPENAI_DISABLE_IMAGES" {
			return "invalid"
		}
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), "OPENAI_DISABLE_IMAGES") {
		t.Fatalf("invalid boolean error=%v", err)
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

func TestModelPricesConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "harness.json")
	if err := os.WriteFile(path, []byte(`{"model_prices":{"gpt-test":{"input":1.25,"output":2,"cached":0.5}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	price := cfg.ModelPrices["gpt-test"]
	if price.Input != 1.25 || price.Output != 2 || price.Cached != 0.5 {
		t.Fatalf("model price: %+v", price)
	}
	if err := os.WriteFile(path, []byte(`{"model_prices":{"gpt-test":{"input":-1,"output":2,"cached":0.5}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "model_prices.gpt-test.input") {
		t.Fatalf("negative model price error: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"model_prices":{"gpt-test":{"input":1,"output":2}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "model_prices.gpt-test.cached is required") {
		t.Fatalf("incomplete model price error: %v", err)
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

func TestDisableSnapshotsConfig(t *testing.T) {
	for _, tt := range []struct {
		name string
		file string
		env  string
		want bool
	}{
		{name: "default", file: `{}`},
		{name: "file-disabled", file: `{"disable_snapshots":true}`, want: true},
		{name: "file-enabled", file: `{"disable_snapshots":false}`},
		{name: "env-disabled", file: `{}`, env: "1", want: true},
		{name: "env-true", file: `{}`, env: "TRUE", want: true},
		{name: "env-overrides-file", file: `{"disable_snapshots":true}`, env: "false"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tt.file), 0644); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path, func(k string) string {
				if k == "CLOUD_HARNESS_DISABLE_SNAPSHOTS" {
					return tt.env
				}
				return ""
			})
			if err != nil || cfg.DisableSnapshots != tt.want || cfg.Redacted()["disable_snapshots"] != tt.want {
				t.Fatalf("config: %+v, %v, want disabled=%t", cfg, err, tt.want)
			}
		})
	}
}
