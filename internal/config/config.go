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
	HTTPAllowInsecure        bool   `json:"http_allow_insecure"` // Deprecated; cannot bypass HTTP token requirements.
	AllowExternal            bool   `json:"-"`                   // Deprecated legacy setting, retained for config compatibility.
	OwnerID                  string `json:"-"`
	// HTTPAllowedHosts lists Host header values serve accepts besides loopback
	// names, as "host" or "host:port".
	HTTPAllowedHosts []string `json:"http_allowed_hosts,omitempty"`
	// RetentionDays prunes the history index. Zero keeps every history row.
	RetentionDays int `json:"retention_days"`
	// CommandEnvAllow lists extra environment variable names passed to run_command subprocesses.
	CommandEnvAllow []string `json:"command_env_allow"`
	// CommandLimits adjusts or disables the resource limits of run_command subprocesses.
	CommandLimits CommandLimits `json:"command_limits"`
	// DisableSnapshots opts out of automatic pre/post workspace snapshots.
	DisableSnapshots bool `json:"disable_snapshots"`
	// Servers declares local stdio MCP servers.
	Servers []MCPServerConfig `json:"servers,omitempty"`
	// MCPServers is an alias for Servers in config files.
	MCPServers []MCPServerConfig `json:"mcp_servers,omitempty"`
	ModelRouting
	// ModelPrices optionally maps a model name to per-million-token prices.
	ModelPrices map[string]ModelPrice `json:"model_prices,omitempty"`
	// OracleMaxCostUSD limits estimated ask_oracle cost per run. Zero is unlimited.
	OracleMaxCostUSD float64 `json:"oracle_max_cost_usd,omitempty"`
	// Notify configures optional run lifecycle notifications.
	Notify notify.Config `json:"notify,omitempty"`
}

// CommandLimits configures the Linux resource limits of run_command
// subprocesses. An omitted field keeps the built-in default and 0 leaves that
// resource unlimited. The defaults live in internal/tools.
type CommandLimits struct {
	// Disabled runs commands without any of these limits.
	Disabled bool `json:"disabled,omitempty"`
	// MemoryBytes caps the private writable memory of each process.
	MemoryBytes *uint64 `json:"memory_bytes,omitempty"`
	// AddressSpaceBytes caps the virtual address space of each process. It is
	// unlimited by default.
	AddressSpaceBytes *uint64 `json:"address_space_bytes,omitempty"`
	// CPUSeconds caps the CPU time of each process.
	CPUSeconds *uint64 `json:"cpu_seconds,omitempty"`
	// FileSizeBytes caps the size of any file a process writes.
	FileSizeBytes *uint64 `json:"file_size_bytes,omitempty"`
	// OpenFiles caps the open file descriptors of each process.
	OpenFiles *uint64 `json:"open_files,omitempty"`
	// Processes caps the processes and threads of the user running the command.
	Processes *uint64 `json:"processes,omitempty"`
}

// applyEnv applies CLOUD_HARNESS_COMMAND_LIMITS: "off" disables the limits,
// otherwise a comma-separated list of name=value pairs overrides single fields.
func (l *CommandLimits) applyEnv(value string) error {
	invalid := func(detail string) error {
		return domain.E(domain.CodeInvalidArgument, "CLOUD_HARNESS_COMMAND_LIMITS "+detail)
	}
	if strings.EqualFold(strings.TrimSpace(value), "off") {
		l.Disabled = true
		return nil
	}
	fields := map[string]**uint64{
		"memory_bytes":        &l.MemoryBytes,
		"address_space_bytes": &l.AddressSpaceBytes,
		"cpu_seconds":         &l.CPUSeconds,
		"file_size_bytes":     &l.FileSizeBytes,
		"open_files":          &l.OpenFiles,
		"processes":           &l.Processes,
	}
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, text, ok := strings.Cut(part, "=")
		name = strings.TrimSpace(name)
		field := fields[name]
		if !ok || field == nil {
			return invalid("must be \"off\" or name=value pairs of memory_bytes, address_space_bytes, cpu_seconds, file_size_bytes, open_files, processes")
		}
		n, err := strconv.ParseUint(strings.TrimSpace(text), 10, 64)
		if err != nil {
			return invalid(name + " must be a non-negative integer")
		}
		*field = &n
		l.Disabled = false
	}
	return nil
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
	if err := loadFile(path, env, &cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.ModelRouting.applyEnv(env); err != nil {
		return Config{}, err
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
		case "osc9", "desktop":
			for _, suffix := range []string{"WEBHOOK_BEARER", "BARK_KEY", "NTFY_TOKEN"} {
				envName := fmt.Sprintf("CLOUD_HARNESS_NOTIFY_CHANNEL_%d_%s", i+1, suffix)
				if env(envName) != "" {
					return Config{}, domain.E(domain.CodeInvalidArgument, fmt.Sprintf("notify channel %d: %s must be empty", i+1, envName))
				}
			}
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
	if v := env("CLOUD_HARNESS_HTTP_ALLOWED_HOSTS"); v != "" {
		cfg.HTTPAllowedHosts = strings.Split(v, ",")
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
	if v := env("CLOUD_HARNESS_COMMAND_LIMITS"); v != "" {
		if err := cfg.CommandLimits.applyEnv(v); err != nil {
			return Config{}, err
		}
	}
	if v := env("CLOUD_HARNESS_ORACLE_MAX_COST_USD"); v != "" {
		limit, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(limit) || math.IsInf(limit, 0) || limit < 0 {
			return Config{}, domain.E(domain.CodeInvalidArgument, "CLOUD_HARNESS_ORACLE_MAX_COST_USD must be a non-negative finite number")
		}
		cfg.OracleMaxCostUSD = limit
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
	if err := ValidateOracleMaxCostUSD(cfg.OracleMaxCostUSD); err != nil {
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
	hosts, err := ParseHTTPAllowedHosts(cfg.HTTPAllowedHosts)
	if err != nil {
		return Config{}, err
	}
	cfg.HTTPAllowedHosts = hosts
	return cfg, nil
}

// ValidateOracleMaxCostUSD accepts zero as unlimited and otherwise requires a
// finite, non-negative per-run amount.
func ValidateOracleMaxCostUSD(limit float64) error {
	if math.IsNaN(limit) || math.IsInf(limit, 0) || limit < 0 {
		return domain.E(domain.CodeInvalidArgument, "oracle_max_cost_usd must be a non-negative finite number")
	}
	return nil
}

func loadFile(path string, env func(string) string, cfg *Config) error {
	if path == "" {
		path = env("CLOUD_HARNESS_CONFIG")
	}
	if path == "" {
		return nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var rawConfig map[string]json.RawMessage
	if json.Unmarshal(body, &rawConfig) == nil {
		if value, ok := rawConfig["oracle_max_cost_usd"]; ok && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return domain.E(domain.CodeInvalidArgument, "oracle_max_cost_usd must be a non-negative finite number")
		}
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return domain.E(domain.CodeInvalidArgument, "config file: "+err.Error())
	}
	return nil
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

// ParseHTTPAllowedHosts normalizes extra Host header values to lower case and
// drops blanks. Each entry is a host name or IP address with an optional port.
func ParseHTTPAllowedHosts(entries []string) ([]string, error) {
	var hosts []string
	for _, entry := range entries {
		host := strings.ToLower(strings.TrimSpace(entry))
		if host == "" {
			continue
		}
		name := host
		if h, port, err := net.SplitHostPort(host); err == nil {
			if n, perr := strconv.Atoi(port); perr != nil || n < 1 || n > 65535 {
				return nil, domain.E(domain.CodeInvalidArgument, "http_allowed_hosts entry "+strconv.Quote(entry)+" has an invalid port")
			}
			name = h
		} else {
			name = strings.TrimSuffix(strings.TrimPrefix(name, "["), "]")
			host = name
		}
		if net.ParseIP(name) == nil && !validHostName(name) {
			return nil, domain.E(domain.CodeInvalidArgument, "http_allowed_hosts entry "+strconv.Quote(entry)+" must be a host name or IP address with an optional port, without a scheme or path")
		}
		hosts = append(hosts, host)
	}
	return hosts, nil
}

func validHostName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
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

// ValidateBind requires a token for a non-loopback bind. The legacy insecure
// argument is retained for caller compatibility and no longer bypasses the rule.
func ValidateBind(addr, token string, _ bool) error {
	if err := ValidateHTTPToken(token); err != nil {
		return err
	}
	loopback, err := IsLoopback(addr)
	if err != nil {
		return err
	}
	if loopback || token != "" {
		return nil
	}
	return domain.E(domain.CodeExternalBind, "non-loopback listen requires an HTTP token; configure --http-token, CLOUD_HARNESS_HTTP_TOKEN, or http_token (at least 16 characters)")
}

// ValidateBind checks the config's listen address and HTTP token.
func (c Config) ValidateBind() error {
	return ValidateBind(c.ListenAddr, c.HTTPToken, false)
}

// ValidateListen is a legacy token-free check. Non-loopback binds are refused
// regardless of allow; callers with a token should use ValidateBind.
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
	if len(c.HTTPAllowedHosts) > 0 {
		redacted["http_allowed_hosts"] = c.HTTPAllowedHosts
	}
	if len(c.Notify.Channels) > 0 || c.Notify.ApprovalBaseURL != "" {
		redacted["notify"] = map[string]any{"channels": notifyChannels, "approval_base_url": notify.SafeTarget(c.Notify.ApprovalBaseURL)}
	}
	return redacted
}
