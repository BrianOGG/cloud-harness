package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/security"
)

// CustomProvider is the input and private on-disk record. Only CustomProviderInfo may be returned by the API.
type CustomProvider struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	Model   string `json:"model"`
	APIKey  string `json:"api_key,omitempty"`
}

type CustomProviderInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	BaseURL   string `json:"base_url"`
	Model     string `json:"model"`
	HasAPIKey bool   `json:"has_api_key"`
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
	return data, nil
}

func (s CustomProviderStore) List() ([]CustomProviderInfo, error) {
	data, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]CustomProviderInfo, 0, len(data.Providers))
	for _, p := range data.Providers {
		out = append(out, CustomProviderInfo{ID: p.ID, Name: p.Name, BaseURL: p.BaseURL, Model: p.Model, HasAPIKey: p.APIKey != ""})
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
	u, err := url.Parse(input.BaseURL)
	loopback := false
	if err == nil {
		ip := net.ParseIP(u.Hostname())
		loopback = u.Hostname() == "localhost" || ip != nil && ip.IsLoopback()
	}
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !(u.Scheme == "https" || u.Scheme == "http" && loopback) {
		return CustomProviderInfo{}, domain.E(domain.CodeInvalidArgument, "base URL must be HTTPS (HTTP is allowed for loopback test endpoints)")
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
	return CustomProviderInfo{ID: input.ID, Name: input.Name, BaseURL: input.BaseURL, Model: input.Model, HasAPIKey: true}, nil
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
