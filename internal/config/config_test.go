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
