package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/modelconfig"
)

func TestLegacyCustomProviderAndMultipleModelIDs(t *testing.T) {
	store := CustomProviderStore{Directory: t.TempDir()}
	const key = "legacy-config-fixture"
	raw := `{"providers":[{"id":"legacy","name":"Legacy","base_url":"https://example.invalid/v1","model":"original","api_key":"legacy-config-fixture"}]}`
	if err := os.WriteFile(filepath.Join(store.Directory, "compatible-providers.json"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := store.Get("legacy")
	if err != nil || p.Protocol != "openai" || p.ProtocolSource != "manual" || !reflect.DeepEqual(p.Models, []string{"original"}) {
		t.Fatalf("legacy defaults: %+v %v", p, err)
	}
	p.Models = []string{"second/variant", "second/variant"}
	p.Catalog = []modelconfig.Model{{ID: "original", Thinking: "unknown"}, {ID: "second/variant", Thinking: "unsupported"}}
	info, err := store.Save(p)
	if err != nil || !reflect.DeepEqual(info.Models, []string{"original", "second/variant"}) {
		t.Fatalf("enabled=%v err=%v", info.Models, err)
	}
	for _, tc := range []struct {
		id, model string
		ok        bool
	}{{"custom:legacy", "original", true}, {"custom:legacy/original", "original", true}, {"custom:legacy/second/variant", "second/variant", true}, {"custom:legacy/missing", "", false}, {"custom:missing", "", false}} {
		_, model, err := store.ResolveModelID(tc.id)
		if (err == nil) != tc.ok || model != tc.model {
			t.Errorf("selection=%s model=%q err=%v", tc.id, model, err)
		}
	}
	public, _ := json.Marshal(info)
	if strings.Contains(string(public), key) || strings.Contains(string(public), "\"api_key\":") {
		t.Fatal("public config exposed key")
	}
	p.ID = "invalid/id"
	if _, err := store.Save(p); err == nil {
		t.Fatal("provider ID accepted slash")
	}
}

func TestUpdateCatalogRetainsConfigurationAndRejectsStaleRefresh(t *testing.T) {
	store := CustomProviderStore{Directory: t.TempDir()}
	original := CustomProvider{ID: "p", Name: "P", BaseURL: "https://example.invalid", Model: "default", APIKey: "catalog-update-fixture", Protocol: "anthropic", Models: []string{"second"}}
	if _, err := store.Save(original); err != nil {
		t.Fatal(err)
	}
	p, _ := store.Get("p")
	now := time.Now().UTC()
	result := modelconfig.Detection{Protocol: "anthropic", Models: []modelconfig.Model{{ID: "discovered", Thinking: "supported"}}, FetchedAt: now}
	info, err := store.UpdateCatalog(p, result)
	if err != nil || info.CatalogFetchedAt == nil || !info.CatalogFetchedAt.Equal(now) || !reflect.DeepEqual(info.Models, []string{"default", "second"}) {
		t.Fatalf("cache=%+v err=%v", info, err)
	}
	p.Name = "renamed"
	if _, err := store.Save(p); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateCatalog(p, result); err != nil {
		t.Fatal("refresh should preserve concurrent unrelated edits")
	}
	changed, _ := store.Get("p")
	if changed.Name != "renamed" {
		t.Fatal("refresh overwrote edit")
	}
	changed.BaseURL = "https://other.invalid"
	if _, err := store.Save(changed); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateCatalog(p, result); err == nil {
		t.Fatal("stale endpoint refresh applied")
	}
	huge := make([]modelconfig.Model, 1001)
	p.Catalog = huge
	if _, err := store.Save(p); err == nil {
		t.Fatal("accepted oversized catalog")
	}
}
