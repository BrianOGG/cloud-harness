// Package config loads process settings. The API key is read only from the
// environment, never from the config file.
package config

import (
	"encoding/json"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/cloud-harness/cloud-harness/internal/domain"
)

const (
	DefaultListen = "127.0.0.1:19421"
	DefaultDB     = ".cloud-harness/harness.db"
	DefaultModel  = "gpt-4.1-mini"
	DefaultOpenAI = "https://api.openai.com/v1"
)

// Config is the process configuration.
type Config struct {
	ListenAddr    string `json:"listen"`
	DBPath        string `json:"db"`
	Provider      string `json:"provider"`
	OpenAIBaseURL string `json:"openai_base_url"`
	OpenAIModel   string `json:"openai_model"`
	OpenAIAPIKey  string `json:"-"`
	AllowExternal bool   `json:"-"`
	OwnerID       string `json:"-"`
	// RetentionDays prunes the history index. Zero keeps every history row.
	RetentionDays int `json:"retention_days"`
}

// Load reads an optional JSON file and applies environment overrides.
// A file that contains an API key field is rejected.
func Load(path string, env func(string) string) (Config, error) {
	if env == nil {
		env = os.Getenv
	}
	cfg := Config{
		ListenAddr:    DefaultListen,
		DBPath:        DefaultDB,
		Provider:      "mock",
		OpenAIBaseURL: DefaultOpenAI,
		OpenAIModel:   DefaultModel,
		OwnerID:       "local",
	}
	if path == "" {
		path = env("CLOUD_HARNESS_CONFIG")
	}
	if path != "" {
		body, err := os.ReadFile(path)
		if err != nil {
			return Config{}, err
		}
		dec := json.NewDecoder(strings.NewReader(string(body)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			return Config{}, domain.E(domain.CodeInvalidArgument, "config file: "+err.Error())
		}
	}
	if v := env("CLOUD_HARNESS_ADDR"); v != "" {
		cfg.ListenAddr = v
	}
	if v := env("CLOUD_HARNESS_DB"); v != "" {
		cfg.DBPath = v
	}
	if v := env("CLOUD_HARNESS_PROVIDER"); v != "" {
		cfg.Provider = v
	}
	if v := env("OPENAI_BASE_URL"); v != "" {
		cfg.OpenAIBaseURL = v
	}
	if v := env("OPENAI_MODEL"); v != "" {
		cfg.OpenAIModel = v
	}
	cfg.OpenAIAPIKey = env("OPENAI_API_KEY")
	cfg.AllowExternal = env("CLOUD_HARNESS_ALLOW_EXTERNAL") == "1"
	if v := env("CLOUD_HARNESS_RETENTION_DAYS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return Config{}, domain.E(domain.CodeInvalidArgument, "CLOUD_HARNESS_RETENTION_DAYS must be a non-negative integer")
		}
		cfg.RetentionDays = n
	}
	if v := env("CLOUD_HARNESS_OWNER"); v != "" {
		cfg.OwnerID = v
	}
	if cfg.Provider == "" {
		cfg.Provider = "mock"
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = DefaultListen
	}
	return cfg, nil
}

// ValidateListen refuses a non-loopback bind unless allow is set.
// M0 has no authentication, so an external bind stays opt-in and warned.
func ValidateListen(addr string, allow bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return domain.E(domain.CodeInvalidArgument, "listen address: "+err.Error())
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return nil
	}
	if allow {
		return nil
	}
	return domain.E(domain.CodeExternalBind, "refusing to listen on "+addr+"; M0 has no authentication. Use 127.0.0.1 or set CLOUD_HARNESS_ALLOW_EXTERNAL=1 only on a trusted network")
}

// Redacted returns a copy safe to log. The API key is never included.
func (c Config) Redacted() map[string]any {
	key := "unset"
	if c.OpenAIAPIKey != "" {
		key = "set"
	}
	return map[string]any{
		"listen":          c.ListenAddr,
		"db":              c.DBPath,
		"provider":        c.Provider,
		"openai_base_url": c.OpenAIBaseURL,
		"openai_model":    c.OpenAIModel,
		"openai_api_key":  key,
		"allow_external":  c.AllowExternal,
		"owner":           c.OwnerID,
	}
}
