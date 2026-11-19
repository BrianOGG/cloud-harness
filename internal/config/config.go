// Package config loads process settings. The API key is read only from the
// environment, never from the config file.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/notify"
)

const (
	DefaultListen          = "127.0.0.1:19421"
	DefaultDB              = ".cloud-harness/harness.db"
	DefaultModel           = "gpt-4.1-mini"
	DefaultOpenAI          = "https://api.openai.com/v1"
	DefaultReasoningReplay = "message"
	MinHTTPTokenLen        = 16
)

// MCPServerConfig declares one local stdio MCP server subprocess.
type MCPServerConfig struct {
	Name     string   `json:"name"`
	Command  string   `json:"command"`
	Args     []string `json:"args"`
	Readonly bool     `json:"readonly"`
}

// ModelPrice contains prices in currency units per one million tokens. Input
// is the non-cached input price; cached is the price for the cached subset.
type ModelPrice struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
	Cached float64 `json:"cached"`

	fromJSON  bool
	inputSet  bool
	outputSet bool
	cachedSet bool
}

func (p *ModelPrice) UnmarshalJSON(data []byte) error {
	type wirePrice struct {
		Input  *float64 `json:"input"`
		Output *float64 `json:"output"`
		Cached *float64 `json:"cached"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var wire wirePrice
	if err := dec.Decode(&wire); err != nil {
		return err
	}
	*p = ModelPrice{fromJSON: true, inputSet: wire.Input != nil, outputSet: wire.Output != nil, cachedSet: wire.Cached != nil}
	if wire.Input != nil {
		p.Input = *wire.Input
	}
	if wire.Output != nil {
		p.Output = *wire.Output
	}
	if wire.Cached != nil {
		p.Cached = *wire.Cached
	}
	return nil
}

// Config is the process configuration.
type Config struct {
	ListenAddr               string `json:"listen"`
	DBPath                   string `json:"db"`
	Provider                 string `json:"provider"`
	OpenAIBaseURL            string `json:"openai_base_url"`
	OpenAIModel              string `json:"openai_model"`
	OpenAIAPIKey             string `json:"-"`
	OpenAIReasoningReplay    string `json:"openai_reasoning_replay"`
	OpenAIPrivatePassthrough bool   `json:"openai_private_passthrough"`
	OpenAIDisableImages      bool   `json:"openai_disable_images"`
	OpenAIHeaderTimeoutMS    int    `json:"openai_header_timeout_ms"`
	OpenAIIdleTimeoutMS      int    `json:"openai_idle_timeout_ms"`
	OpenAIRetryBaseMS        int    `json:"openai_retry_base_ms"`
	HTTPToken                string `json:"http_token"`
	HTTPAllowInsecure        bool   `json:"http_allow_insecure"`
	AllowExternal            bool   `json:"-"`
	OwnerID                  string `json:"-"`
	// RetentionDays prunes the history index. Zero keeps every history row.
	RetentionDays int `json:"retention_days"`
	// CommandEnvAllow lists extra environment variable names passed to run_command subprocesses.
	CommandEnvAllow []string `json:"command_env_allow"`
	// DisableSnapshots opts out of automatic pre/post workspace snapshots.
	DisableSnapshots bool `json:"disable_snapshots"`
	// Servers declares local stdio MCP servers.
	Servers []MCPServerConfig `json:"servers,omitempty"`
	// MCPServers is an alias for Servers in config files.
	MCPServers []MCPServerConfig `json:"mcp_servers,omitempty"`
	// ModelPrices optionally maps a model name to per-million-token prices.
	ModelPrices map[string]ModelPrice `json:"model_prices,omitempty"`
	// Notify configures optional run lifecycle notifications.
	Notify notify.Config `json:"notify,omitempty"`
}

// Load reads an optional JSON file and applies environment overrides.
// A file that contains an API key field is rejected.
func Load(path string, env func(string) string) (Config, error) {
	if env == nil {
		env = os.Getenv
	}
	cfg := Config{
		ListenAddr:            DefaultListen,
		DBPath:                DefaultDB,
		Provider:              "mock",
		OpenAIBaseURL:         DefaultOpenAI,
		OpenAIModel:           DefaultModel,
		OpenAIReasoningReplay: DefaultReasoningReplay,
		OwnerID:               "local",
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
	for i := range cfg.Notify.Channels {
		ch := &cfg.Notify.Channels[i]
		switch strings.ToLower(strings.TrimSpace(ch.Type)) {
		case "webhook":
			ch.Credential = env(fmt.Sprintf("CLOUD_HARNESS_NOTIFY_CHANNEL_%d_WEBHOOK_BEARER", i+1))
		case "bark":
			ch.Credential = env(fmt.Sprintf("CLOUD_HARNESS_NOTIFY_CHANNEL_%d_BARK_KEY", i+1))
		case "ntfy":
			ch.Credential = env(fmt.Sprintf("CLOUD_HARNESS_NOTIFY_CHANNEL_%d_NTFY_TOKEN", i+1))
		}
	}
	if _, err := notify.New(cfg.Notify); err != nil {
		return Config{}, domain.E(domain.CodeInvalidArgument, "notify config: "+err.Error())
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
	if v := env("OPENAI_REASONING_REPLAY"); v != "" {
		cfg.OpenAIReasoningReplay = v
	}
	if v := env("CLOUD_HARNESS_OPENAI_REASONING_REPLAY"); v != "" {
		cfg.OpenAIReasoningReplay = v
	}
	if v := env("OPENAI_PRIVATE_PASSTHROUGH"); v != "" {
		cfg.OpenAIPrivatePassthrough = v == "1" || strings.EqualFold(v, "true")
	}
	if v := env("OPENAI_DISABLE_IMAGES"); v != "" {
		disabled, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, domain.E(domain.CodeInvalidArgument, "OPENAI_DISABLE_IMAGES must be a boolean")
		}
		cfg.OpenAIDisableImages = disabled
	}
	if v := env("OPENAI_HEADER_TIMEOUT_MS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return Config{}, domain.E(domain.CodeInvalidArgument, "OPENAI_HEADER_TIMEOUT_MS must be a non-negative integer")
		}
		cfg.OpenAIHeaderTimeoutMS = n
	}
	if v := env("OPENAI_IDLE_TIMEOUT_MS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return Config{}, domain.E(domain.CodeInvalidArgument, "OPENAI_IDLE_TIMEOUT_MS must be a non-negative integer")
		}
		cfg.OpenAIIdleTimeoutMS = n
	}
	if v := env("OPENAI_RETRY_BASE_MS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return Config{}, domain.E(domain.CodeInvalidArgument, "OPENAI_RETRY_BASE_MS must be a non-negative integer")
		}
		cfg.OpenAIRetryBaseMS = n
	}
	cfg.OpenAIAPIKey = env("OPENAI_API_KEY")
	if v := env("CLOUD_HARNESS_HTTP_TOKEN"); v != "" {
		cfg.HTTPToken = v
	}
	if v := env("CLOUD_HARNESS_HTTP_ALLOW_INSECURE"); v != "" {
		cfg.HTTPAllowInsecure = v == "1" || strings.EqualFold(v, "true")
	}
	if v := env("CLOUD_HARNESS_DISABLE_SNAPSHOTS"); v != "" {
		cfg.DisableSnapshots = v == "1" || strings.EqualFold(v, "true")
	}
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
	if v := env("CLOUD_HARNESS_COMMAND_ENV_ALLOW"); v != "" {
		var allow []string
		for _, part := range strings.Split(v, ",") {
			if name := strings.TrimSpace(part); name != "" {
				allow = append(allow, name)
			}
		}
		cfg.CommandEnvAllow = allow
	}
	if cfg.Provider == "" {
		cfg.Provider = "mock"
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = DefaultListen
	}
	if cfg.OpenAIReasoningReplay == "" {
		cfg.OpenAIReasoningReplay = DefaultReasoningReplay
	}
	if cfg.OpenAIReasoningReplay != "message" && cfg.OpenAIReasoningReplay != "omit" {
		return Config{}, domain.E(domain.CodeInvalidArgument, "openai_reasoning_replay must be message or omit")
	}
	if err := ValidateModelPrices(cfg.ModelPrices); err != nil {
		return Config{}, err
	}
	if len(cfg.Servers) == 0 && len(cfg.MCPServers) > 0 {
		cfg.Servers = cfg.MCPServers
	} else if len(cfg.MCPServers) == 0 && len(cfg.Servers) > 0 {
		cfg.MCPServers = cfg.Servers
	}
	if err := ValidateHTTPToken(cfg.HTTPToken); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// ValidateModelPrices rejects malformed or negative prices before they can be
// used for a cost estimate. A missing map is valid and means costs are shown
// as unavailable.
func ValidateModelPrices(prices map[string]ModelPrice) error {
	names := make([]string, 0, len(prices))
	for name := range prices {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		price := prices[name]
		if strings.TrimSpace(name) == "" {
			return domain.E(domain.CodeInvalidArgument, "model_prices contains an empty model name")
		}
		for _, field := range []struct {
			name  string
			set   bool
			value float64
		}{{"input", price.inputSet, price.Input}, {"output", price.outputSet, price.Output}, {"cached", price.cachedSet, price.Cached}} {
			if price.fromJSON && !field.set {
				return domain.E(domain.CodeInvalidArgument, fmt.Sprintf("model_prices.%s.%s is required", name, field.name))
			}
			if math.IsNaN(field.value) || math.IsInf(field.value, 0) || field.value < 0 {
				return domain.E(domain.CodeInvalidArgument, fmt.Sprintf("model_prices.%s.%s must be a non-negative finite number", name, field.name))
			}
		}
	}
	return nil
}

// ValidateHTTPToken checks that a non-empty bearer token meets the minimum length.
func ValidateHTTPToken(token string) error {
	if token != "" && len(token) < MinHTTPTokenLen {
		return domain.E(domain.CodeInvalidArgument, "http_token must be at least 16 characters")
	}
	return nil
}

// IsLoopback reports whether a host:port listen address is on the loopback interface.
func IsLoopback(addr string) (bool, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false, domain.E(domain.CodeInvalidArgument, "listen address: "+err.Error())
	}
	if host == "localhost" {
		return true, nil
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback(), nil
}

// ValidateBind refuses a non-loopback bind when http_token is empty unless allowInsecure is true.
func ValidateBind(addr, token string, allowInsecure bool) error {
	if err := ValidateHTTPToken(token); err != nil {
		return err
	}
	loopback, err := IsLoopback(addr)
	if err != nil {
		return err
	}
	if loopback || token != "" || allowInsecure {
		return nil
	}
	return domain.E(domain.CodeExternalBind, "refusing to listen on "+addr+" without http_token; use 127.0.0.1, set http_token (min 16 chars), or set http_allow_insecure=true")
}

// ValidateBind checks the config's listen address, token, and insecure allow flag.
func (c Config) ValidateBind() error {
	return ValidateBind(c.ListenAddr, c.HTTPToken, c.HTTPAllowInsecure || c.AllowExternal)
}

// ValidateListen refuses a non-loopback bind unless allow is set.
func ValidateListen(addr string, allow bool) error {
	return ValidateBind(addr, "", allow)
}

// Redacted returns a copy safe to log. The API key and HTTP token are never included.
func (c Config) Redacted() map[string]any {
	key := "unset"
	if c.OpenAIAPIKey != "" {
		key = "set"
	}
	token := "unset"
	if c.HTTPToken != "" {
		token = "set"
	}
	notifyChannels := make([]map[string]any, 0, len(c.Notify.Channels))
	for _, ch := range c.Notify.Channels {
		credential := "unset"
		if ch.Credential != "" {
			credential = "set"
		}
		notifyChannels = append(notifyChannels, map[string]any{
			"type": ch.Type, "target": notify.SafeTarget(ch.URL), "events": ch.Events,
			"timeout_ms": ch.TimeoutMS, "credential": credential,
		})
	}
	redacted := map[string]any{
		"listen":                  c.ListenAddr,
		"db":                      c.DBPath,
		"provider":                c.Provider,
		"openai_base_url":         c.OpenAIBaseURL,
		"openai_model":            c.OpenAIModel,
		"openai_disable_images":   c.OpenAIDisableImages,
		"openai_reasoning_replay": c.OpenAIReasoningReplay,
		"openai_api_key":          key,
		"http_token":              token,
		"http_allow_insecure":     c.HTTPAllowInsecure,
		"allow_external":          c.AllowExternal,
		"owner":                   c.OwnerID,
		"disable_snapshots":       c.DisableSnapshots,
	}
	if len(c.Notify.Channels) > 0 || c.Notify.ApprovalBaseURL != "" {
		redacted["notify"] = map[string]any{"channels": notifyChannels, "approval_base_url": notify.SafeTarget(c.Notify.ApprovalBaseURL)}
	}
	return redacted
}
