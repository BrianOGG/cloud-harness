package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/domain"
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

func TestNotifyConfigValidationAndCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	load := func(body string, env map[string]string) (Config, error) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return Load(path, func(key string) string { return env[key] })
	}
	if _, err := load(`{"notify":{"channels":[{"type":"unknown","url":"https://example.com"}]}}`, nil); err == nil {
		t.Fatal("unknown notification channel was accepted")
	}
	if _, err := load(`{"notify":{"channels":[{"type":"bark","url":"https://bark.example","key":"file-secret"}]}}`, nil); err == nil {
		t.Fatal("credential field in config file was accepted")
	}
	cfg, err := load(`{"notify":{"approval_base_url":"https://approve.example/root","channels":[{"type":"webhook","url":"https://notify.example/hook"},{"type":"bark","url":"https://bark.example"},{"type":"ntfy","url":"https://ntfy.sh/topic"}]}}`, map[string]string{
		"CLOUD_HARNESS_NOTIFY_CHANNEL_1_WEBHOOK_BEARER": "webhook-secret",
		"CLOUD_HARNESS_NOTIFY_CHANNEL_2_BARK_KEY":       "bark-secret",
		"CLOUD_HARNESS_NOTIFY_CHANNEL_3_NTFY_TOKEN":     "ntfy-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notify.Channels[0].Credential != "webhook-secret" || cfg.Notify.Channels[1].Credential != "bark-secret" || cfg.Notify.Channels[2].Credential != "ntfy-secret" {
		t.Fatalf("environment credentials were not injected: %+v", cfg.Notify.Channels)
	}
	redacted, err := json.Marshal(cfg.Redacted())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"webhook-secret", "bark-secret", "ntfy-secret"} {
		if strings.Contains(string(redacted), secret) {
			t.Fatalf("redacted config contains a credential %q: %s", secret, redacted)
		}
	}
	if strings.Contains(string(redacted), "https://notify.example") || strings.Contains(string(redacted), "https://approve.example") {
		t.Fatalf("redacted config contains a full URL: %s", redacted)
	}
}

func TestNotifyLocalChannelCredentialEnvironment(t *testing.T) {
	for _, channelType := range []string{"osc9", "desktop"} {
		t.Run(channelType, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			body := fmt.Sprintf(`{"notify":{"channels":[{"type":"webhook","url":"https://notify.example/hook"},{"type":%q}]}}`, channelType)
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Run("no_credentials", func(t *testing.T) {
				if channelType == "desktop" && runtime.GOOS != "linux" {
					t.Skip("desktop notifications are only supported on Linux")
				}
				cfg, err := Load(path, func(string) string { return "" })
				if err != nil {
					t.Fatal(err)
				}
				if len(cfg.Notify.Channels) != 2 || cfg.Notify.Channels[1].Type != channelType || cfg.Notify.Channels[1].Credential != "" {
					t.Fatalf("unexpected local channel config: %+v", cfg.Notify.Channels)
				}
			})
			for _, suffix := range []string{"WEBHOOK_BEARER", "BARK_KEY", "NTFY_TOKEN"} {
				t.Run(suffix, func(t *testing.T) {
					envName := "CLOUD_HARNESS_NOTIFY_CHANNEL_2_" + suffix
					const credential = "local-channel-secret"
					_, err := Load(path, func(key string) string {
						if key == envName {
							return credential
						}
						return ""
					})
					if !domain.IsCode(err, domain.CodeInvalidArgument) {
						t.Fatalf("expected invalid_argument for %s, got %v", envName, err)
					}
					if !strings.Contains(err.Error(), "notify channel 2:") || !strings.Contains(err.Error(), envName) {
						t.Fatalf("error must identify the channel and environment variable: %v", err)
					}
					if strings.Contains(err.Error(), credential) {
						t.Fatal("validation error exposed the credential")
					}
				})
			}
		})
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
	if err := ValidateListen("0.0.0.0:19421", true); err == nil {
		t.Fatal("legacy allow flag bypassed the HTTP token requirement")
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

func TestCommandLimitsConfig(t *testing.T) {
	noEnv := func(string) string { return "" }
	cfg, err := Load("", noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if l := cfg.CommandLimits; l.Disabled || l.MemoryBytes != nil || l.AddressSpaceBytes != nil || l.CPUSeconds != nil || l.FileSizeBytes != nil || l.OpenFiles != nil || l.Processes != nil {
		t.Fatalf("default command_limits = %+v, want every field unset", l)
	}

	dir := t.TempDir()
	write := func(body string) string {
		t.Helper()
		path := filepath.Join(dir, "harness.json")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	path := write(`{"command_limits":{"memory_bytes":1073741824,"address_space_bytes":0,"cpu_seconds":30,"file_size_bytes":1048576,"open_files":256,"processes":512}}`)
	cfg, err = Load(path, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	l := cfg.CommandLimits
	if l.Disabled || l.MemoryBytes == nil || *l.MemoryBytes != 1<<30 || l.AddressSpaceBytes == nil || *l.AddressSpaceBytes != 0 ||
		l.CPUSeconds == nil || *l.CPUSeconds != 30 || l.FileSizeBytes == nil || *l.FileSizeBytes != 1<<20 ||
		l.OpenFiles == nil || *l.OpenFiles != 256 || l.Processes == nil || *l.Processes != 512 {
		t.Fatalf("command_limits = %+v", l)
	}

	// The environment overrides single fields and keeps the rest of the file.
	cfg, err = Load(path, func(key string) string {
		if key == "CLOUD_HARNESS_COMMAND_LIMITS" {
			return "cpu_seconds=0, memory_bytes=2147483648"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	l = cfg.CommandLimits
	if l.Disabled || *l.CPUSeconds != 0 || *l.MemoryBytes != 2<<30 || *l.OpenFiles != 256 {
		t.Fatalf("command_limits after environment override = %+v", l)
	}

	cfg, err = Load(path, func(key string) string {
		if key == "CLOUD_HARNESS_COMMAND_LIMITS" {
			return "off"
		}
		return ""
	})
	if err != nil || !cfg.CommandLimits.Disabled {
		t.Fatalf("CLOUD_HARNESS_COMMAND_LIMITS=off: %+v %v", cfg.CommandLimits, err)
	}

	cfg, err = Load(write(`{"command_limits":{"disabled":true}}`), noEnv)
	if err != nil || !cfg.CommandLimits.Disabled {
		t.Fatalf("disabled in file: %+v %v", cfg.CommandLimits, err)
	}
	// Limits named in the environment apply even when the file disables them.
	cfg, err = Load(path, func(key string) string {
		if key == "CLOUD_HARNESS_COMMAND_LIMITS" {
			return "open_files=64"
		}
		return ""
	})
	if err != nil || cfg.CommandLimits.Disabled || *cfg.CommandLimits.OpenFiles != 64 {
		t.Fatalf("environment limits over a disabled file: %+v %v", cfg.CommandLimits, err)
	}

	for _, body := range []string{
		`{"command_limits":{"cpu_seconds":-1}}`,
		`{"command_limits":{"cpu_seconds":1.5}}`,
		`{"command_limits":{"cpu_seconds":"30"}}`,
		`{"command_limits":{"cpu":30}}`,
		`{"command_limits":"off"}`,
	} {
		if _, err := Load(write(body), noEnv); err == nil {
			t.Fatalf("accepted invalid config %s", body)
		}
	}
	for _, value := range []string{"cpu_seconds", "cpu_seconds=-1", "cpu_seconds=abc", "cpu=30", "disabled=true"} {
		_, err := Load("", func(key string) string {
			if key == "CLOUD_HARNESS_COMMAND_LIMITS" {
				return value
			}
			return ""
		})
		if !domain.IsCode(err, domain.CodeInvalidArgument) {
			t.Fatalf("CLOUD_HARNESS_COMMAND_LIMITS=%q: err = %v, want invalid_argument", value, err)
		}
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

func TestOracleMaxCostConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness.json")
	load := func(body string, env map[string]string) (Config, error) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return Load(path, func(name string) string { return env[name] })
	}
	for _, tc := range []struct {
		body string
		want float64
	}{
		{`{}`, 0},
		{`{"oracle_max_cost_usd":0}`, 0},
		{`{"oracle_max_cost_usd":0.25}`, 0.25},
	} {
		cfg, err := load(tc.body, nil)
		if err != nil || cfg.OracleMaxCostUSD != tc.want {
			t.Fatalf("Load(%s) = %.8f, error = %v", tc.body, cfg.OracleMaxCostUSD, err)
		}
	}
	for _, body := range []string{`{"oracle_max_cost_usd":-0.1}`, `{"oracle_max_cost_usd":"not-a-number"}`, `{"oracle_max_cost_usd":null}`} {
		if _, err := load(body, nil); err == nil || !strings.Contains(err.Error(), "oracle_max_cost_usd") {
			t.Errorf("invalid oracle cost %s error = %v", body, err)
		}
	}
	for _, value := range []string{"-1", "not-a-number", "NaN", "+Inf"} {
		if _, err := load(`{}`, map[string]string{"CLOUD_HARNESS_ORACLE_MAX_COST_USD": value}); err == nil || !strings.Contains(err.Error(), "CLOUD_HARNESS_ORACLE_MAX_COST_USD") {
			t.Errorf("invalid oracle cost env %q error = %v", value, err)
		}
	}
	cfg, err := load(`{"oracle_max_cost_usd":0.25}`, map[string]string{"CLOUD_HARNESS_ORACLE_MAX_COST_USD": "0.75"})
	if err != nil || cfg.OracleMaxCostUSD != 0.75 {
		t.Fatalf("oracle cost env override = %.8f, error = %v", cfg.OracleMaxCostUSD, err)
	}
}

func TestServeRefusesNonLoopbackWithoutToken(t *testing.T) {
	validToken := "0123456789abcdef"

	// Every external bind requires a token, including unspecified IPv4/IPv6
	// addresses, mapped addresses, and configurations with legacy bypass flags.
	for _, addr := range []string{"0.0.0.0:19421", ":19421", "[::]:19421", "192.0.2.1:19421", "[::ffff:192.0.2.1]:19421", "example.com:19421"} {
		for _, allow := range []bool{false, true} {
			err := ValidateBind(addr, "", allow)
			if err == nil || !strings.Contains(err.Error(), "non-loopback listen requires an HTTP token") || !strings.Contains(err.Error(), "--http-token") {
				t.Fatalf("external bind %s allow=%t: %v", addr, allow, err)
			}
			cfg := Config{ListenAddr: addr, HTTPAllowInsecure: allow, AllowExternal: allow}
			if err := cfg.ValidateBind(); err == nil {
				t.Fatalf("legacy config flags bypassed token requirement for %s", addr)
			}
		}
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
	for _, addr := range []string{"127.0.0.1:19421", "127.0.0.2:19421", "localhost:19421", "[::1]:19421", "[::ffff:127.0.0.1]:19421"} {
		if err := ValidateBind(addr, "", false); err != nil {
			t.Fatalf("expected loopback %s without token to succeed, got %v", addr, err)
		}
	}

	// Token shorter than minimum length -> error.
	if err := ValidateBind("127.0.0.1:19421", "short-token", false); err == nil {
		t.Fatal("expected short http_token to be rejected")
	} else if strings.Contains(err.Error(), "short-token") {
		t.Fatal("validation error echoed the token")
	}
	if err := ValidateBind("invalid-listen-address", validToken, false); err == nil || !strings.Contains(err.Error(), "listen address") {
		t.Fatalf("malformed listen address: %v", err)
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

func TestAnthropicConfigEnvAndRedacted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"provider":"anthropic","anthropic_model":"claude-x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, func(k string) string {
		switch k {
		case "ANTHROPIC_API_KEY":
			return "anthropic-secret"
		case "ANTHROPIC_BASE_URL":
			return "https://anthropic.example"
		default:
			return ""
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "anthropic" || cfg.AnthropicModel != "claude-x" || cfg.AnthropicBaseURL != "https://anthropic.example" || cfg.AnthropicAPIKey != "anthropic-secret" {
		t.Fatalf("%+v", cfg)
	}
	logged := cfg.Redacted()
	if logged["anthropic_api_key"] != "set" {
		t.Fatalf("redacted %+v", logged)
	}
	raw, err := json.Marshal(logged)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "anthropic-secret") {
		t.Fatalf("redacted config contained key: %s", raw)
	}
	if err := os.WriteFile(path, []byte(`{"anthropic_api_key":"file-secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, func(string) string { return "" }); err == nil {
		t.Fatal("file anthropic api key was accepted")
	}
}
