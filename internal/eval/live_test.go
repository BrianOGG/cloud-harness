package eval

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/config"
	"github.com/BrianOGG/cloud-harness/internal/domain"
)

func TestLiveSuiteRequiresRealProvider(t *testing.T) {
	for _, provider := range []string{"", "mock"} {
		_, err := LiveSuite(context.Background(), config.Config{Provider: provider}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "real provider") {
			t.Fatalf("provider %q: expected real-provider error, got %v", provider, err)
		}
	}
}

func TestScoreLiveBatchThreeReads(t *testing.T) {
	calls := []Call{
		{Name: "read_file", Arguments: json.RawMessage(`{"path":"alpha.txt"}`), Status: string(domain.InvSucceeded), OK: true},
		{Name: "read_file", Arguments: json.RawMessage(`{"path":"beta.txt"}`), Status: string(domain.InvSucceeded), OK: true},
		{Name: "read_file", Arguments: json.RawMessage(`{"path":"gamma.txt"}`), Status: string(domain.InvSucceeded), OK: true},
	}
	if failures := scoreLiveBatchThreeReads(domain.RunCompleted, "alpha-one, beta-two, gamma-three", calls); len(failures) != 0 {
		t.Fatalf("expected pass, got %v", failures)
	}
	calls = calls[:2]
	failures := scoreLiveBatchThreeReads(domain.RunCompleted, "alpha and beta only", calls)
	joined := strings.Join(failures, "\n")
	if !strings.Contains(joined, "gamma.txt") || !strings.Contains(joined, "gamma") {
		t.Fatalf("expected gamma failures, got %v", failures)
	}
}

func TestScoreLiveFileSafeEdit(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "allowed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "forbidden"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "allowed", "config.conf"), []byte(safeEditUpdatedContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "forbidden", "config.conf"), []byte(safeEditForbiddenInit), 0o644); err != nil {
		t.Fatal(err)
	}
	if failures := scoreLiveFileSafeEdit(root, domain.RunCompleted, "", nil); len(failures) != 0 {
		t.Fatalf("expected pass, got %v", failures)
	}
	if err := os.WriteFile(filepath.Join(root, "forbidden", "config.conf"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	failures := scoreLiveFileSafeEdit(root, domain.RunCompleted, "", nil)
	if len(failures) == 0 || !strings.Contains(strings.Join(failures, "\n"), "forbidden/config.conf was modified") {
		t.Fatalf("expected forbidden modification failure, got %v", failures)
	}
}

func TestScoreLiveRetrieveChinese(t *testing.T) {
	calls := []Call{
		{Name: "retrieve_history", Arguments: json.RawMessage(`{"query":"云计算"}`), Status: string(domain.InvSucceeded), OK: true, ResultText: `{"source_id":"note_cloud"}`},
		{Name: "retrieve_history", Arguments: json.RawMessage(`{"query":"检索"}`), Status: string(domain.InvSucceeded), OK: true, ResultText: `{"source_id":"note_search"}`},
	}
	if failures := scoreLiveRetrieveChinese(domain.RunCompleted, "note_search", calls); len(failures) != 0 {
		t.Fatalf("expected pass, got %v", failures)
	}
	calls[1].ResultText = `{"source_id":"note_cloud"}`
	failures := scoreLiveRetrieveChinese(domain.RunCompleted, "note_search", calls)
	if len(failures) == 0 || !strings.Contains(strings.Join(failures, "\n"), "note_search without note_cloud") {
		t.Fatalf("expected retrieval failure, got %v", failures)
	}
}

func TestScoreLiveRetrievePath(t *testing.T) {
	calls := []Call{{
		Name: "retrieve_history", Status: string(domain.InvSucceeded), OK: true,
		ResultText: `{"source_id":"evt_9f3a2c","text":"handler lives in internal/server.go"}`,
	}}
	if failures := scoreLiveRetrievePath(domain.RunCompleted, "server.go event evt_9f3a2c", calls); len(failures) != 0 {
		t.Fatalf("expected pass, got %v", failures)
	}
	failures := scoreLiveRetrievePath(domain.RunCompleted, "server.go", calls)
	if len(failures) == 0 || !strings.Contains(strings.Join(failures, "\n"), "evt_9f3a2c") {
		t.Fatalf("expected missing event id failure, got %v", failures)
	}
}
