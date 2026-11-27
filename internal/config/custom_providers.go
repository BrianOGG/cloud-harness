package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/modelconfig"
	"github.com/BrianOGG/cloud-harness/internal/security"
)

// CustomProvider is the input and private on-disk record. Only CustomProviderInfo may be returned by the API.
type CustomProvider struct {
	ID               string              `json:"id"`
	Name             string              `json:"name"`
	BaseURL          string              `json:"base_url"`
	Model            string              `json:"model"`
	APIKey           string              `json:"api_key,omitempty"`
	Protocol         string              `json:"protocol"`
	ProtocolSource   string              `json:"protocol_source"`
	Models           []string            `json:"models"`
	Catalog          []modelconfig.Model `json:"catalog"`
	CatalogFetchedAt *time.Time          `json:"catalog_fetched_at,omitempty"`
}

type CustomProviderInfo struct {
	ID               string              `json:"id"`
	Name             string              `json:"name"`
	BaseURL          string              `json:"base_url"`
	Model            string              `json:"model"`
	HasAPIKey        bool                `json:"has_api_key"`
	Protocol         string              `json:"protocol"`
	ProtocolSource   string              `json:"protocol_source"`
	Models           []string            `json:"models"`
	Catalog          []modelconfig.Model `json:"catalog"`
	CatalogFetchedAt *time.Time          `json:"catalog_fetched_at,omitempty"`
}

type customProviderFile struct {
	Providers []CustomProvider `json:"providers"`
}
type CustomProviderStore struct{ Directory string }

var customProviderMu sync.Mutex
var providerIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func NewCustomProviderStore() (CustomProviderStore, error) {
	dir, err := security.ConfigDirectory()
	return CustomProviderStore{Directory: dir}, err
}

func (s CustomProviderStore) path() string {
	return filepath.Join(s.Directory, "compatible-providers.json")
}

func (s CustomProviderStore) load() (customProviderFile, error) {
	var data customProviderFile
	raw, err := os.ReadFile(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return customProviderFile{}, nil
	}
	if err != nil {
		return data, fmt.Errorf("read compatible provider settings: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&data); err != nil {
		return customProviderFile{}, errors.New("invalid compatible provider settings")
	}
	if data.Providers == nil {
		data.Providers = []CustomProvider{}
	}
	for i := range data.Providers {
		normalizeCustomProvider(&data.Providers[i])
	}
	return data, nil
}

func (s CustomProviderStore) List() ([]CustomProviderInfo, error) {
	data, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]CustomProviderInfo, 0, len(data.Providers))
	for _, p := range data.Providers {
		out = append(out, providerInfo(p))
	}
	return out, nil
}

func (s CustomProviderStore) Get(id string) (CustomProvider, error) {
	data, err := s.load()
	if err != nil {
		return CustomProvider{}, err
	}
	for _, p := range data.Providers {
		if p.ID == id {
			return p, nil
		}
	}
	return CustomProvider{}, domain.E(domain.CodeNotFound, "custom provider not found")
}

func (s CustomProviderStore) Save(input CustomProvider) (CustomProviderInfo, error) {
	customProviderMu.Lock()
	defer customProviderMu.Unlock()
	input.ID = strings.TrimSpace(input.ID)
	input.Name = strings.TrimSpace(input.Name)
	input.Model = strings.TrimSpace(input.Model)
	input.BaseURL = strings.TrimRight(strings.TrimSpace(input.BaseURL), "/")
	if !providerIDPattern.MatchString(input.ID) || input.Name == "" || len(input.Name) > 100 || input.Model == "" || len(input.Model) > 200 {
		return CustomProviderInfo{}, domain.E(domain.CodeInvalidArgument, "provider id, display name, model, and base URL are required")
	}
	base, err := modelconfig.NormalizeBaseURL(input.BaseURL)
	if err != nil {
		return CustomProviderInfo{}, domain.E(domain.CodeInvalidArgument, err.Error())
	}
	input.BaseURL = base
	normalizeCustomProvider(&input)
	if !modelconfig.ValidProtocol(input.Protocol) || (input.ProtocolSource != "auto" && input.ProtocolSource != "manual") {
		return CustomProviderInfo{}, domain.E(domain.CodeInvalidArgument, "无效的模型服务协议或来源")
	}
	if len(input.Models) > modelconfig.MaxCatalogModels || len(input.Catalog) > modelconfig.MaxCatalogModels {
		return CustomProviderInfo{}, domain.E(domain.CodeInvalidArgument, "模型列表最多 1000 项")
	}
	for _, model := range input.Models {
		if model == "" || len(model) > 200 {
			return CustomProviderInfo{}, domain.E(domain.CodeInvalidArgument, "无效的模型 ID")
		}
	}
	for _, model := range input.Catalog {
		if model.ID == "" || len(model.ID) > 200 || len(model.DisplayName) > 500 || model.Thinking != "" && model.Thinking != "unknown" && model.Thinking != "supported" && model.Thinking != "unsupported" {
			return CustomProviderInfo{}, domain.E(domain.CodeInvalidArgument, "无效的模型列表")
		}
	}
	data, err := s.load()
	if err != nil {
		return CustomProviderInfo{}, err
	}
	found := false
	for i, p := range data.Providers {
		if p.ID == input.ID {
			found = true
			if input.APIKey == "" {
				input.APIKey = p.APIKey
			}
			data.Providers[i] = input
			break
		}
	}
	if !found {
		if input.APIKey == "" {
			return CustomProviderInfo{}, domain.E(domain.CodeInvalidArgument, "API key is required for a new custom provider")
		}
		data.Providers = append(data.Providers, input)
	}
	if err := s.write(data); err != nil {
		return CustomProviderInfo{}, err
	}
	return providerInfo(input), nil
}

// Delete changes only the connection catalog, not session or run preferences.
func (s CustomProviderStore) Delete(id string) error {
	customProviderMu.Lock()
	defer customProviderMu.Unlock()
	data, err := s.load()
	if err != nil {
		return err
	}
	for i, p := range data.Providers {
		if p.ID == id {
			data.Providers = append(data.Providers[:i], data.Providers[i+1:]...)
			return s.write(data)
		}
	}
	return domain.E(domain.CodeNotFound, "custom provider not found")
}

// Callers hold customProviderMu. Replacement uses a private temporary file.
func (s CustomProviderStore) write(data customProviderFile) error {
	if err := os.MkdirAll(s.Directory, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.Directory, ".compatible-providers-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, s.path()); err != nil {
		return err
	}
	return nil
}

func normalizeCustomProvider(p *CustomProvider) {
	if p.Protocol == "" {
		p.Protocol = "openai"
	}
	if p.ProtocolSource == "" {
		p.ProtocolSource = "manual"
	}
	models := make([]string, 0, len(p.Models)+1)
	seen := map[string]bool{}
	for _, model := range append([]string{p.Model}, p.Models...) {
		model = strings.TrimSpace(model)
		if model != "" && !seen[model] {
			models = append(models, model)
			seen[model] = true
		}
	}
	p.Models = models
	if p.Catalog == nil {
		p.Catalog = []modelconfig.Model{}
	}
}
func providerInfo(p CustomProvider) CustomProviderInfo {
	return CustomProviderInfo{ID: p.ID, Name: p.Name, BaseURL: p.BaseURL, Model: p.Model, HasAPIKey: p.APIKey != "", Protocol: p.Protocol, ProtocolSource: p.ProtocolSource, Models: p.Models, Catalog: p.Catalog, CatalogFetchedAt: p.CatalogFetchedAt}
}

// UpdateCatalog atomically replaces only the cache. Concurrent edits cannot be
// overwritten by a refresh that started against an earlier URL/key/protocol.
func (s CustomProviderStore) UpdateCatalog(before CustomProvider, result modelconfig.Detection) (CustomProviderInfo, error) {
	customProviderMu.Lock()
	defer customProviderMu.Unlock()
	data, err := s.load()
	if err != nil {
		return CustomProviderInfo{}, err
	}
	for i, p := range data.Providers {
		if p.ID != before.ID {
			continue
		}
		if p.BaseURL != before.BaseURL || p.APIKey != before.APIKey || p.Protocol != before.Protocol {
			return CustomProviderInfo{}, domain.E(domain.CodeConflict, "服务配置已更改，请重新刷新")
		}
		if len(result.Models) > modelconfig.MaxCatalogModels {
			return CustomProviderInfo{}, domain.E(domain.CodeInvalidArgument, "模型列表最多 1000 项")
		}
		p.Catalog, p.CatalogFetchedAt = result.Models, &result.FetchedAt
		data.Providers[i] = p
		if err := s.write(data); err != nil {
			return CustomProviderInfo{}, err
		}
		return providerInfo(p), nil
	}
	return CustomProviderInfo{}, domain.E(domain.CodeNotFound, "custom provider not found")
}

// ResolveModelID also accepts the original custom:<id> selection indefinitely.
func (s CustomProviderStore) ResolveModelID(id string) (CustomProvider, string, error) {
	raw := strings.TrimPrefix(id, "custom:")
	providerID, model, _ := strings.Cut(raw, "/")
	p, err := s.Get(providerID)
	if err != nil {
		return CustomProvider{}, "", err
	}
	if model == "" {
		return p, p.Model, nil
	}
	for _, enabled := range p.Models {
		if enabled == model {
			return p, model, nil
		}
	}
	return CustomProvider{}, "", domain.E(domain.CodeInvalidArgument, "selected model is not enabled")
}
