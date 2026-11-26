package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestCustomProviderStorePersistsPrivateKeyWithoutEcho(t *testing.T) {
	store := CustomProviderStore{Directory: t.TempDir()}
	input := CustomProvider{ID: "example", Name: "Example", BaseURL: "https://example.invalid/v1/", Model: "one", APIKey: "test-only-provider-key"}
	info, err := store.Save(input)
	if err != nil || !info.HasAPIKey {
		t.Fatalf("save failed: %v", err)
	}
	public, err := json.Marshal(info)
	if err != nil || bytes.Contains(public, []byte(input.APIKey)) {
		t.Fatal("public settings exposed the key")
	}
	reopened := CustomProviderStore{Directory: store.Directory}
	saved, err := reopened.Get(input.ID)
	if err != nil || saved.APIKey != input.APIKey {
		t.Fatal("key did not survive reopening")
	}
	stat, err := os.Stat(filepath.Join(store.Directory, "compatible-providers.json"))
	if err != nil || stat.Mode().Perm() != 0o600 {
		t.Fatal("provider file must have mode 0600")
	}
	input.APIKey, input.Model = "", "two"
	if _, err := reopened.Save(input); err != nil {
		t.Fatal(err)
	}
	saved, err = reopened.Get(input.ID)
	if err != nil || saved.APIKey == "" || saved.Model != "two" {
		t.Fatal("blank edit did not preserve existing key")
	}
	list, err := reopened.List()
	if err != nil || len(list) != 1 || !list[0].HasAPIKey {
		t.Fatal("list lost provider metadata")
	}
	public, _ = json.Marshal(list)
	if bytes.Contains(public, []byte(saved.APIKey)) {
		t.Fatal("listing exposed the key")
	}
}

func TestCustomProviderURLValidation(t *testing.T) {
	for _, endpoint := range []string{"http://example.invalid/v1", "http://127.0.0.1.evil.invalid", "file:///tmp/provider", "https://user:password@example.invalid", "https://example.invalid?key=bad", "https://example.invalid#bad", "//example.invalid", "https:///missing"} {
		store := CustomProviderStore{Directory: t.TempDir()}
		if _, err := store.Save(CustomProvider{ID: "bad", Name: "Bad", Model: "test", BaseURL: endpoint, APIKey: "test-only"}); err == nil {
			t.Errorf("accepted invalid URL %q", endpoint)
		}
	}
	for _, endpoint := range []string{"https://example.invalid/v1", "http://localhost:1234/v1", "http://127.0.0.2:1234", "http://[::1]:1234"} {
		store := CustomProviderStore{Directory: t.TempDir()}
		if _, err := store.Save(CustomProvider{ID: "ok", Name: "OK", Model: "test", BaseURL: endpoint, APIKey: "test-only"}); err != nil {
			t.Errorf("rejected URL %q: %v", endpoint, err)
		}
	}
}

func TestCustomProviderConcurrentSavesPreserveBothRecords(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	for _, id := range []string{"a", "b"} {
		wg.Go(func() {
			if _, err := (CustomProviderStore{Directory: dir}).Save(CustomProvider{ID: id, Name: id, BaseURL: "https://example.invalid", Model: "test", APIKey: "test-only"}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	rows, err := (CustomProviderStore{Directory: dir}).List()
	if err != nil || len(rows) != 2 {
		t.Fatal("concurrent saves lost a record")
	}
}
