package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/config"
	contextpkg "github.com/BrianOGG/cloud-harness/internal/context"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

func TestM11ReferencesAreExpandedAndAudited(t *testing.T) {
	mock := &provider.Mock{}
	m, db, root := testManager(t, mock)
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	session, err := m.CreateSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessage(context.Background(), session.ID, "inspect @file:note.txt#L2-L3")
	if err != nil || run.Status != domain.RunCompleted {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	if len(mock.Requests) == 0 || !strings.Contains(mock.Requests[0].Messages[len(mock.Requests[0].Messages)-1].Content, "note.txt:2 two") || strings.Contains(mock.Requests[0].Messages[len(mock.Requests[0].Messages)-1].Content, "note.txt:1 one") {
		t.Fatalf("reference not expanded in user message: %+v", mock.Requests)
	}
	events, err := db.ListEvents(t.Context(), run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Type != "context.reference" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil || payload["type"] != "file" || payload["bytes"].(float64) == 0 {
			t.Fatalf("reference event payload=%s err=%v", event.Payload, err)
		}
		if strings.Contains(string(event.Payload), "two") {
			t.Fatal("reference event leaked file content")
		}
		found = true
	}
	if !found {
		t.Fatal("missing context.reference event")
	}
}

func TestM11DiffReferenceUsesWorkspaceGitAndReportsUnavailable(t *testing.T) {
	mock := &provider.Mock{}
	m, db, root := testManager(t, mock)
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s (%v)", args, output, err)
		}
	}
	git("init", "-q")
	git("config", "user.email", "m11@example.invalid")
	git("config", "user.name", "M11 Test")
	path := filepath.Join(root, "note.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "note.txt")
	git("commit", "-qm", "initial")
	if err := os.WriteFile(path, []byte("after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	session, _ := m.CreateSession(t.Context())
	run, err := m.RunMessage(t.Context(), session.ID, "show @diff")
	if err != nil || run.Status != domain.RunCompleted {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	if len(mock.Requests) == 0 || !strings.Contains(messageText(mock.Requests[0].Messages), "+after") {
		t.Fatalf("git diff did not enter request: %+v", mock.Requests)
	}
	events, _ := db.ListEvents(t.Context(), run.ID, 0)
	if !hasReferenceEvent(events, "diff", "working-tree") {
		t.Fatalf("missing diff reference event: %+v", events)
	}

	plain, _, _ := testManager(t, &provider.Mock{})
	plainSession, _ := plain.CreateSession(t.Context())
	noGitRun, err := plain.RunMessage(t.Context(), plainSession.ID, "show @diff")
	if err != nil || noGitRun.Status != domain.RunCompleted {
		t.Fatalf("non-git diff reference stopped run: %+v err=%v", noGitRun, err)
	}
	plainMock := plain.Provider.(*provider.Mock)
	if !strings.Contains(messageText(plainMock.Requests[0].Messages), "diff unavailable") {
		t.Fatalf("missing non-git diagnostic: %+v", plainMock.Requests[0].Messages)
	}
}

func TestM11CompactKeepsRecentHistoryAndAuditsReplacement(t *testing.T) {
	m, db, _ := testManager(t, &provider.Mock{})
	session, err := m.CreateSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for i, text := range []string{"old-key-fact-292", "middle-293", "recent-294", "recent-295", "recent-296", "recent-297"} {
		run, runErr := m.RunMessage(t.Context(), session.ID, text)
		if runErr != nil || run.Status != domain.RunCompleted {
			t.Fatalf("run %d=%+v err=%v", i, run, runErr)
		}
	}
	var summaryInput string
	m.Summarizer = providerFunc(func(_ context.Context, req provider.Request) (domain.ModelResponse, error) {
		summaryInput = messageText(req.Messages)
		return textOnly(`{"confirmed_facts":["old-key-fact-292"],"in_progress":["M11-14"],"pending":["acceptance"],"key_files_and_decisions":["internal/context"]}`), nil
	})
	summary, err := m.CompactSession(t.Context(), session.ID)
	if err != nil || !strings.Contains(summary, "old-key-fact-292") || !strings.Contains(summaryInput, "old-key-fact-292") {
		t.Fatalf("summary=%s input contains fact=%v err=%v", summary, strings.Contains(summaryInput, "old-key-fact-292"), err)
	}
	raw, _, err := db.LatestMemory(t.Context(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	mem, err := contextpkg.ParseMemory(raw)
	if err != nil || mem.CompactionSummary != summary || mem.Goal != "" || len(mem.Facts) != 0 {
		t.Fatalf("compacted memory=%+v err=%v", mem, err)
	}
	runs, _ := db.ListRuns(t.Context(), session.ID)
	if _, err := db.GetHistory(t.Context(), runs[0].TurnID); err != nil {
		t.Fatalf("compaction deleted original history: %v", err)
	}
	latest := runs[len(runs)-1]
	events, _ := db.ListEvents(t.Context(), latest.ID, 0)
	found := false
	for _, event := range events {
		if event.Type != "context.compacted" {
			continue
		}
		var payload map[string]int
		if err := json.Unmarshal(event.Payload, &payload); err != nil || payload["before_messages"] < 6 || payload["summary_bytes"] != len(summary) {
			t.Fatalf("compaction event=%s err=%v", event.Payload, err)
		}
		found = true
	}
	if !found {
		t.Fatal("missing context.compacted event")
	}

	after := &provider.Mock{}
	m.Provider = after
	m.SkipPlanner = true
	if _, err := m.RunMessage(t.Context(), session.ID, "new question"); err != nil {
		t.Fatal(err)
	}
	if len(after.Requests) == 0 {
		t.Fatal("post-compaction request missing")
	}
	text := messageText(after.Requests[0].Messages)
	if !strings.Contains(text, summary) || strings.Count(text, "old-key-fact-292") != 1 || !strings.Contains(text, "recent-297") || !strings.Contains(text, "recent-296") {
		t.Fatalf("post-compaction context was not summary plus recent records: %s", text)
	}
}

func hasReferenceEvent(events []storage.Event, typ, rangeName string) bool {
	for _, event := range events {
		if event.Type != "context.reference" {
			continue
		}
		var payload struct {
			Type  string `json:"type"`
			Range string `json:"range"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && payload.Type == typ && payload.Range == rangeName {
			return true
		}
	}
	return false
}

func TestM11CompactionFailureLeavesMemoryAndHistoryUnchanged(t *testing.T) {
	m, db, _ := testManager(t, &provider.Mock{})
	session, err := m.CreateSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.RunMessage(t.Context(), session.ID, "keep this fact"); err != nil {
		t.Fatal(err)
	}
	before, _, err := db.LatestMemory(t.Context(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	m.Summarizer = providerFunc(func(context.Context, provider.Request) (domain.ModelResponse, error) {
		return domain.ModelResponse{}, errors.New("summary unavailable")
	})
	if _, err := m.CompactSession(t.Context(), session.ID); err == nil || !strings.Contains(err.Error(), "summary unavailable") {
		t.Fatalf("compact error=%v", err)
	}
	after, _, err := db.LatestMemory(t.Context(), session.ID)
	if err != nil || before != after {
		t.Fatalf("failed compaction changed memory: before=%s after=%s err=%v", before, after, err)
	}
}

func TestM11CompactionUsesFastProfileWhenConfigured(t *testing.T) {
	m, _, _ := testManager(t, &provider.Mock{})
	session, err := m.CreateSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.RunMessage(t.Context(), session.ID, "fast profile fact"); err != nil {
		t.Fatal(err)
	}
	var routedModel string
	m.Summarizer = providerFunc(func(_ context.Context, req provider.Request) (domain.ModelResponse, error) {
		routedModel = req.Model
		return textOnly(`{"confirmed_facts":["fast profile fact"]}`), nil
	})
	m.ModelRouting = &config.ModelRouting{
		ModelProfiles: map[string]config.ModelProfile{"brief": {Model: "fast-brief"}},
		ModelRoles:    config.ModelRoles{Fast: "brief"},
	}
	if _, err := m.CompactSession(t.Context(), session.ID); err != nil {
		t.Fatal(err)
	}
	if routedModel != "fast-brief" {
		t.Fatalf("compaction model=%q, want fast profile", routedModel)
	}
}
