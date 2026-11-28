package transport

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/agent"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

func TestProjectMemoryIsolationEditsAndSources(t *testing.T) {
	ctx := t.Context()
	m, h := newServer(t)
	m.Workspace = t.TempDir()
	m.SkipPlanner, m.DisableRepoMap, m.DisableSnapshots = true, true, true
	other := &agent.Manager{Workspace: t.TempDir(), Store: m.Store, Provider: &provider.Mock{}, SkipPlanner: true, DisableRepoMap: true, DisableSnapshots: true}
	hOther := (&Server{Manager: other}).Handler()
	const collection = "/v1/project-memories"
	sourceRun := func(manager *agent.Manager, text string) storage.Run {
		t.Helper()
		session, err := manager.CreateSession(ctx)
		if err != nil {
			t.Fatal(err)
		}
		run, err := manager.RunMessage(ctx, session.ID, text)
		if err != nil || run.Status != domain.RunCompleted {
			t.Fatalf("source run: %v %+v", err, run)
		}
		return run
	}
	source := sourceRun(m, "source includes fixture-only-private-text")
	foreign := sourceRun(other, "another project in the same db")
	body := map[string]string{"source_session_id": source.SessionID, "source_run_id": source.ID, "kind": "decision", "content": " use SQLite "}
	list := func(h http.Handler) []storage.ProjectMemory {
		t.Helper()
		var items []storage.ProjectMemory
		w := modelConfigRequest(t, h, "GET", collection, nil, 200, "")
		if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil {
			t.Fatal(err)
		}
		return items
	}
	if got := list(h); len(got) != 0 {
		t.Fatal("memory saved without confirmation", got)
	}
	modelConfigRequest(t, hOther, "POST", collection, body, 404, "")
	body["source_run_id"] = foreign.ID
	modelConfigRequest(t, h, "POST", collection, body, 404, "")
	body["source_run_id"] = source.ID
	body["source_session_id"] = foreign.SessionID
	modelConfigRequest(t, h, "POST", collection, body, 404, "")
	body["source_session_id"] = source.SessionID
	modelConfigRequest(t, h, "POST", collection, body, 201, "")
	items := list(h)
	if len(items) != 1 || items[0].Content != "use SQLite" || !items[0].SourceAvailable || items[0].SourceSessionID != source.SessionID || items[0].SourceRunID != source.ID {
		t.Fatalf("created memory: %+v", items)
	}
	var linked view
	w := modelConfigRequest(t, h, "GET", "/v1/runs/"+items[0].SourceRunID, nil, 200, "")
	if err := json.Unmarshal(w.Body.Bytes(), &linked); err != nil || linked.SessionID != items[0].SourceSessionID {
		t.Fatalf("invalid source link: %+v, %v", linked, err)
	}
	assertContext := func(manager *agent.Manager, want, absent string) {
		t.Helper()
		sourceRun(manager, "next turn")
		requests := manager.Provider.(*provider.Mock).Requests
		msgs := requests[len(requests)-1].Messages
		found := false
		for _, msg := range msgs {
			if want != "" && strings.Contains(msg.Content, want) {
				found = true
				if msg.Role != "user" || !strings.Contains(msg.Content, "reference data") {
					t.Fatalf("memory role/framing: %+v", msg)
				}
			}
			if strings.Contains(msg.Content, absent) {
				t.Fatalf("unconfirmed, deleted, or out-of-scope data reached the model: %+v", msg)
			}
		}
		if want != "" && !found {
			t.Fatalf("missing %q in %+v", want, msgs)
		}
	}
	assertContext(m, "use SQLite", "fixture-only-private-text")
	assertContext(other, "", "use SQLite")
	if got := list(hOther); len(got) != 0 {
		t.Fatal("cross-project memory list", got)
	}
	path := collection + "/" + items[0].ID
	edit := map[string]string{"kind": "fact", "content": "use PostgreSQL"}
	modelConfigRequest(t, hOther, "PUT", path, edit, 404, "")
	modelConfigRequest(t, hOther, "DELETE", path, nil, 404, "")
	modelConfigRequest(t, h, "PUT", path, map[string]string{"kind": "fact", "content": "  "}, 400, "")
	modelConfigRequest(t, h, "PUT", path, map[string]string{"kind": "fact", "content": strings.Repeat("字", 501)}, 400, "")
	modelConfigRequest(t, h, "PUT", path, edit, 204, "")
	updated := list(h)[0]
	if updated.SourceRunID != source.ID || updated.SourceSessionID != source.SessionID || updated.Kind != "fact" {
		t.Fatal(updated)
	}
	assertContext(m, "use PostgreSQL", "use SQLite")
	if _, err := m.Store.PruneRuns(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	afterPrune := list(h)
	if len(afterPrune) != 1 || afterPrune[0].SourceAvailable || afterPrune[0].SourceRunID != source.ID {
		t.Fatal(afterPrune)
	}
	modelConfigRequest(t, h, "DELETE", path, nil, 204, "")
	modelConfigRequest(t, h, "DELETE", path, nil, 404, "")
	assertContext(m, "", "use PostgreSQL")
}
