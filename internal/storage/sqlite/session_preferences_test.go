package sqlite

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

func TestSessionPreferencesDefaultsAndPersistence(t *testing.T) {
	ctx := t.Context()
	file := filepath.Join(t.TempDir(), "preferences.db")
	db, err := Open(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(ctx, storage.Session{ID: "one", CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	p, err := db.GetSessionPreferences(ctx, "one")
	if err != nil || p.ModelID != "default" || p.ApprovalMode != "ask" {
		t.Fatalf("defaults: %+v %v", p, err)
	}
	for _, mode := range []string{"safe", "workspace-write", "ask"} {
		if err := db.SaveSessionPreferences(ctx, storage.SessionPreferences{SessionID: "one", ModelID: "custom:example", ApprovalMode: mode}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SaveSessionPreferences(ctx, storage.SessionPreferences{SessionID: "one", ApprovalMode: "unsafe"}); err == nil {
		t.Fatal("invalid mode persisted")
	}
	if err := db.SaveSessionPreferences(ctx, storage.SessionPreferences{SessionID: "missing", ApprovalMode: "safe"}); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("missing session: %v", err)
	}
	if _, err := db.GetSessionPreferences(ctx, "missing"); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("missing session: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	p, err = db.GetSessionPreferences(ctx, "one")
	if err != nil || p.ModelID != "custom:example" || p.ApprovalMode != "ask" || p.UpdatedAt.IsZero() {
		t.Fatalf("reopened: %+v %v", p, err)
	}
}
