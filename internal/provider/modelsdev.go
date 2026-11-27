package provider

import (
	"context"
	"encoding/json"
	"github.com/BrianOGG/cloud-harness/internal/modelconfig"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const modelsDevURL = "https://models.dev/api.json"
const modelsDevTTL = 24 * time.Hour
const maxModelsDevBytes = 32 << 20

type modelsDevModel struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Reasoning        bool   `json:"reasoning"`
	ReasoningOptions []struct {
		Type   string   `json:"type"`
		Values []string `json:"values"`
	} `json:"reasoning_options"`
	Modalities struct{ Input, Output []string } `json:"modalities"`
	ToolCall   bool                             `json:"tool_call"`
}
type modelsDevProvider struct {
	ID     string                    `json:"id"`
	API    string                    `json:"api"`
	Models map[string]modelsDevModel `json:"models"`
}
type modelsDevDocument map[string]modelsDevProvider
type modelsDevCache struct {
	FetchedAt time.Time         `json:"fetched_at"`
	Providers modelsDevDocument `json:"providers"`
}
type modelsDevEntry struct {
	provider, api string
	model         modelsDevModel
}

// ModelsDev only fetches a fixed public URL. It never accepts credentials or an
// endpoint from provider configuration. The base URL is used locally for matching.
// A nil catalog still applies offline classification heuristics.
type ModelsDev struct {
	mu         sync.Mutex
	directory  string
	client     *http.Client
	now        func() time.Time
	loaded     bool
	cache      modelsDevCache
	index      map[string][]modelsDevEntry
	retryAfter time.Time
}

func NewModelsDev(directory string) *ModelsDev {
	return &ModelsDev{directory: directory, client: &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, now: time.Now}
}

func (c *ModelsDev) path() string { return filepath.Join(c.directory, "models-dev-cache.json") }

func (c *ModelsDev) load() {
	if c.loaded {
		return
	}
	c.loaded = true
	if c.directory == "" {
		return
	}
	f, err := os.Open(c.path())
	if err != nil {
		return
	}
	defer f.Close()
	var cached modelsDevCache
	if json.NewDecoder(io.LimitReader(f, maxModelsDevBytes+1)).Decode(&cached) == nil && validModelsDevDocument(cached.Providers) {
		c.cache = cached
		c.reindex()
	}
}
func validModelsDevDocument(doc modelsDevDocument) bool {
	for _, p := range doc {
		if len(p.Models) > 0 {
			return true
		}
	}
	return false
}

func (c *ModelsDev) reindex() {
	c.index = map[string][]modelsDevEntry{}
	for pid, p := range c.cache.Providers {
		for mid, m := range p.Models {
			if m.ID == "" {
				m.ID = mid
			}
			key := modelID(m.ID)
			entry := modelsDevEntry{provider: pid, api: p.API, model: m}
			c.index[key] = append(c.index[key], entry)
		}
	}
	for id := range c.index {
		sort.Slice(c.index[id], func(i, j int) bool {
			a, b := c.index[id][i], c.index[id][j]
			if providerRank(a.provider) != providerRank(b.provider) {
				return providerRank(a.provider) < providerRank(b.provider)
			}
			if a.provider != b.provider {
				return a.provider < b.provider
			}
			return a.model.ID < b.model.ID
		})
	}
}

// Refresh is best effort. Failed refreshes retain stale data, and a short retry
// backoff avoids making every settings/list request wait on an unavailable site.
func (c *ModelsDev) Refresh(ctx context.Context) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.load()
	now := c.now()
	if c.cache.Providers != nil && now.Sub(c.cache.FetchedAt) < modelsDevTTL || now.Before(c.retryAfter) {
		c.mu.Unlock()
		return
	}
	c.retryAfter = now.Add(time.Minute)
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsDevURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "cloud-harness-model-catalog/1")
	res, err := c.client.Do(req)
	if err != nil {
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxModelsDevBytes+1))
	if err != nil || len(raw) > maxModelsDevBytes {
		return
	}
	var doc modelsDevDocument
	if json.Unmarshal(raw, &doc) != nil || !validModelsDevDocument(doc) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache = modelsDevCache{FetchedAt: now, Providers: doc}
	c.reindex()
	if c.directory == "" {
		return
	}
	// Persist public data only, with the same atomic-write pattern as settings.
	if os.MkdirAll(c.directory, 0700) != nil {
		return
	}
	f, err := os.CreateTemp(c.directory, ".models-dev-*")
	if err != nil {
		return
	}
	defer os.Remove(f.Name())
	err = json.NewEncoder(f).Encode(c.cache)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil && closeErr == nil {
		_ = os.Rename(f.Name(), c.path())
	}
}

func modelID(id string) string {
	// Provider namespaces (including nvidia/stepfun-ai/) are transport aliases.
	parts := strings.Split(strings.TrimSpace(id), "/")
	return parts[len(parts)-1]
}
func providerRank(id string) int {
	switch id {
	case "stepfun", "stepfun-ai", "agnes", "openai", "anthropic", "google", "google-vertex", "deepseek", "mistral", "xai", "groq", "cerebras", "cohere", "moonshotai", "zai", "alibaba", "minimax":
		return 0
	case "requesty", "edenai", "opencode", "opencode-go", "llmgateway", "aihubmix", "openrouter":
		return 2
	}
	return 1
}
func apiMatch(base, api string) int {
	b, e1 := url.Parse(base)
	a, e2 := url.Parse(api)
	if e1 != nil || e2 != nil || b.Host == "" || !strings.EqualFold(b.Hostname(), a.Hostname()) {
		return -1
	}
	bp, ap := strings.TrimRight(b.Path, "/"), strings.TrimRight(a.Path, "/")
	if bp == "" {
		return 0
	}
	if bp == ap || strings.HasPrefix(bp, ap+"/") {
		return len(ap) + 1
	}
	return 0
}
func (c *ModelsDev) match(base, id string) (modelsDevModel, bool) {
	entries := c.index[modelID(id)]
	if len(entries) == 0 {
		return modelsDevModel{}, false
	}
	best, score := entries[0], -1
	for _, e := range entries {
		if s := apiMatch(base, e.api); s > score {
			best, score = e, s
		}
	}
	return best.model, true
}

func has(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}

func category(m modelsDevModel) string {
	// Router/realtime are special-purpose even when text output is advertised.
	text := strings.ToLower(m.ID + " " + m.Name)
	if strings.Contains(text, "router") || strings.Contains(text, "realtime") || strings.Contains(text, "real-time") {
		return "other"
	}
	if !has(m.Modalities.Output, "text") || len(m.Modalities.Input) == 1 && m.Modalities.Input[0] == "audio" || has(m.Modalities.Input, "audio") && !m.ToolCall {
		return "other"
	}
	return "chat"
}

// Enrich reads the current/stale cache without network I/O. Call Refresh at the
// settings boundary, never during inference. Supplied IDs are always preserved.
func (c *ModelsDev) Enrich(base, protocol string, models []modelconfig.Model) []modelconfig.Model {
	out := append([]modelconfig.Model{}, models...)
	if c != nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.load()
	}
	for i := range out {
		m := &out[i]
		if m.Category == "" {
			m.Category = modelconfig.ModelCategory(m.ID, m.DisplayName)
		}
		if m.Thinking == "" {
			m.Thinking = modelconfig.ThinkingSupport(protocol, m.ID)
		}
		if c == nil {
			continue
		}
		found, ok := c.match(base, m.ID)
		if !ok {
			continue
		}
		if found.Name != "" {
			m.DisplayName = found.Name
		}
		m.Category = category(found)
		m.Thinking, m.ReasoningType, m.ReasoningValues = "unsupported", "", nil
		if !found.Reasoning {
			continue
		}
		m.Thinking = "supported"
		for _, option := range found.ReasoningOptions {
			if option.Type == "toggle" || option.Type == "effort" {
				m.ReasoningType = option.Type
				if option.Type == "effort" {
					m.ReasoningValues = append([]string(nil), option.Values...)
				}
				break // Never union conflicting capability records or option types.
			}
		}
	}
	return out
}
