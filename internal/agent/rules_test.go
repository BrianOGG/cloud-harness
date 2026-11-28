package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	contextpkg "github.com/BrianOGG/cloud-harness/internal/context"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/rules"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

func isolateRuleConfig(t *testing.T) string {
	t.Helper()
	t.Setenv("CLOUD_HARNESS_CONFIG", "")
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", base)
	return filepath.Join(base, "cloud-harness")
}

func writeAgentRule(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProjectRulesFrozenAcrossCompactionAndRestartedResume(t *testing.T) {
	global := isolateRuleConfig(t)
	ctx := context.Background()
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "read", Name: "read_file", Arguments: []byte(`{"path":"note.txt"}`)}),
		toolResp(domain.ToolCall{ProviderCallID: "ask", Name: "ask_user", Arguments: []byte(`{"question":"Continue?"}`)}),
	}}
	m, db, root := testManager(t, mock)
	m.SkipPlanner = true
	m.ContextTokenLimit = 1
	// A frozen clock also catches checkpoint ordering regressions.
	now := time.Now().UTC()
	m.Now = func() time.Time { return now }
	paths := []string{
		filepath.Join(global, "AGENTS.md"), filepath.Join(root, "AGENTS.md"),
		filepath.Join(root, "z/AGENTS.md"), filepath.Join(root, "a/b/AGENTS.md"),
	}
	bodies := []string{"global rule", strings.Repeat("root rule ", 100), "shallow rule", "deep rule"}
	for i, path := range paths {
		writeAgentRule(t, path, bodies[i])
	}
	if err := os.MkdirAll(filepath.Join(root, "alias"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".././AGENTS.md", filepath.Join(root, "alias/AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	writeAgentRule(t, filepath.Join(root, "note.txt"), "read me")
	images := writeAgentImage(t, root)
	session, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mem := contextpkg.Memory{Goal: "existing memory"}
	if err := db.SaveMemory(ctx, session.ID, mem.JSON(), "[]"); err != nil {
		t.Fatal(err)
	}
	user := strings.Repeat("preserve current user ", 20)
	run, err := m.RunMessageWithImages(ctx, session.ID, user, images)
	if err != nil || run.Status != domain.RunWaiting || run.WaitReason != domain.WaitUser {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	if len(mock.Requests) != 2 {
		t.Fatalf("requests=%d", len(mock.Requests))
	}
	first := mock.Requests[0]
	if len(first.Prefix) != 3 || first.Prefix[0].Content != defaultSystem || first.Prefix[1].Content != toolDigest("ask") || first.Prefix[2].Role != "system" {
		t.Fatalf("project rules changed static prefix: %+v", first.Prefix)
	}
	if !strings.Contains(first.Messages[3].Content, "existing memory") || first.Messages[4].Content != user {
		t.Fatal("rules were inserted after memory/history or shifted the protected user index")
	}
	ruleText := first.Prefix[2].Content
	previous := -1
	for _, source := range []string{"global", "AGENTS.md"} {
		marker := "<!-- AGENTS.md: " + source + " -->"
		at := strings.Index(ruleText, marker)
		if at <= previous || strings.Count(ruleText, marker) != 1 {
			t.Fatalf("source order/dedup failed for %q: %s", source, ruleText)
		}
		previous = at
	}
	if strings.Contains(ruleText, "shallow rule") || strings.Contains(ruleText, "deep rule") {
		t.Fatal("unrelated directory injected at startup")
	}
	wantPrefix, err := json.Marshal(first.Prefix)
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range mock.Requests {
		got, _ := json.Marshal(request.Prefix)
		if !bytes.Equal(got, wantPrefix) || request.Messages[4].Content != user {
			t.Fatal("model loop/compaction rewrote the frozen rules or current user")
		}
		assertAgentImage(t, request, images[0])
	}
	snap, err := m.loadCheckpoint(ctx, run.ID)
	if err != nil || snap.PrefixN != 3 || snap.UserAt != 4 || snap.Messages[2].Content != ruleText {
		t.Fatalf("rules missing from wait checkpoint: %+v err=%v", snap, err)
	}
	for _, path := range paths {
		writeAgentRule(t, path, "changed after run startup")
	}
	writeAgentRule(t, filepath.Join(root, "new/AGENTS.md"), "newly discovered rule")
	restartedProvider := &provider.Mock{Responses: []domain.ModelResponse{textOnly("resumed")}}
	restarted := &Manager{
		Store: db, Provider: restartedProvider, Broker: m.Broker, Workspace: root,
		OwnerID: m.OwnerID, SkipPlanner: true, ContextTokenLimit: 1,
	}
	run, err = restarted.Resume(ctx, run.ID, ResumeInput{Answer: "continue"})
	if err != nil || run.Status != domain.RunCompleted || len(restartedProvider.Requests) != 1 {
		t.Fatalf("resumed=%+v err=%v requests=%d", run, err, len(restartedProvider.Requests))
	}
	resumedPrefix, _ := json.Marshal(restartedProvider.Requests[0].Prefix)
	if !bytes.Equal(resumedPrefix, wantPrefix) {
		t.Fatal("restarted resume reread rules from disk")
	}
	assertAgentImage(t, restartedProvider.Requests[0], images[0])
	events, err := db.ListEvents(ctx, run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	loaded := assertRulesLoadedEvent(t, events, 2)
	for i, file := range loaded {
		if file.Path != paths[i] || file.Bytes != int64(len(bodies[i])) {
			t.Fatalf("loaded source %d=%+v", i, file)
		}
	}
	// A new run in the same session takes a fresh snapshot of project rules.
	if next, err := restarted.RunMessage(ctx, session.ID, "next turn"); err != nil || next.Status != domain.RunCompleted {
		t.Fatalf("next run=%+v err=%v", next, err)
	}
	if nextPrefix := messageText(restartedProvider.Requests[1].Prefix); !strings.Contains(nextPrefix, "changed after run startup") || strings.Contains(nextPrefix, "newly discovered rule") {
		t.Fatalf("new run did not refresh rules: %s", nextPrefix)
	}
}

func assertRulesLoadedEvent(t *testing.T, events []storage.Event, wantFiles int) []rules.File {
	t.Helper()
	count := 0
	var files []rules.File
	for _, event := range events {
		if event.Type != "rules.loaded" {
			continue
		}
		count++
		var payload map[string][]map[string]json.RawMessage
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload) != 1 || len(payload["files"]) != wantFiles {
			t.Fatalf("loaded payload=%s", event.Payload)
		}
		for _, file := range payload["files"] {
			if len(file) != 3 || file["path"] == nil || file["bytes"] == nil || file["source"] == nil {
				t.Fatalf("rules.loaded invalid source metadata: %s", event.Payload)
			}
		}
		var decoded struct {
			Files []rules.File `json:"files"`
		}
		if err := json.Unmarshal(event.Payload, &decoded); err != nil {
			t.Fatal(err)
		}
		files = decoded.Files
	}
	if count != 1 {
		t.Fatalf("rules loaded %d times, want once", count)
	}
	return files
}

func TestNoProjectRulesRunByteForByteAndNoLateInjection(t *testing.T) {
	isolateRuleConfig(t)
	ctx := context.Background()
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "ask", Name: "ask_user", Arguments: []byte(`{"question":"Continue?"}`)}),
		textOnly("done"),
	}}
	m, db, root := testManager(t, mock)
	session, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessage(ctx, session.ID, "hello")
	if err != nil || run.Status != domain.RunWaiting {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	wantMessages, _ := json.Marshal([]provider.Message{
		{Role: "system", Content: defaultSystem}, {Role: "system", Content: toolDigest("ask")}, {Role: "user", Content: "hello"},
	})
	gotMessages, _ := json.Marshal(mock.Requests[0].Messages)
	wantPrefix, _ := json.Marshal([]provider.Message{{Role: "system", Content: defaultSystem}, {Role: "system", Content: toolDigest("ask")}})
	gotPrefix, _ := json.Marshal(mock.Requests[0].Prefix)
	if !bytes.Equal(gotMessages, wantMessages) || !bytes.Equal(gotPrefix, wantPrefix) {
		t.Fatalf("no-rules request changed:\nmessages=%s\nprefix=%s", gotMessages, gotPrefix)
	}
	writeAgentRule(t, filepath.Join(root, "AGENTS.md"), "must wait until next run")
	run, err = m.Resume(ctx, run.ID, ResumeInput{Answer: "yes"})
	if err != nil || run.Status != domain.RunCompleted {
		t.Fatalf("resumed=%+v err=%v", run, err)
	}
	gotPrefix, _ = json.Marshal(mock.Requests[1].Prefix)
	if !bytes.Equal(gotPrefix, wantPrefix) || strings.Contains(messageText(mock.Requests[1].Messages), "must wait until next run") {
		t.Fatal("resume injected rules that were absent at startup")
	}
	events, err := db.ListEvents(ctx, run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if strings.HasPrefix(event.Type, "rules.") {
			t.Fatalf("empty workspace added a rules event: %+v", event)
		}
	}
}

func TestUnreadableProjectRulesDoNotFailRun(t *testing.T) {
	isolateRuleConfig(t)
	ctx := context.Background()
	mock := &provider.Mock{Responses: []domain.ModelResponse{textOnly("done")}}
	m, db, root := testManager(t, mock)
	unreadable := filepath.Join(root, "AGENTS.md")
	writeAgentRule(t, unreadable, "unreadable rule body")
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o644) })
	if file, err := os.Open(unreadable); err == nil {
		_ = file.Close()
		t.Skip("this user can bypass filesystem read permissions; deterministic permission-error coverage is in internal/rules")
	}
	writeAgentRule(t, filepath.Join(root, "src/AGENTS.md"), "readable rule")
	writeAgentRule(t, filepath.Join(root, "src/file.txt"), "data")
	session, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessage(ctx, session.ID, "@file:src/file.txt")
	if err != nil || run.Status != domain.RunCompleted {
		t.Fatalf("unreadable rule failed run=%+v err=%v", run, err)
	}
	if text := messageText(mock.Requests[0].Prefix); strings.Contains(text, "unreadable rule body") || !strings.Contains(text, "readable rule") {
		t.Fatalf("wrong rules injected: %s", text)
	}
	events, err := db.ListEvents(ctx, run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertRulesLoadedEvent(t, events, 1)
	for _, event := range events {
		if event.Type == "rules.skipped" && strings.Contains(string(event.Payload), unreadable) && !strings.Contains(string(event.Payload), "unreadable rule body") {
			return
		}
	}
	t.Fatal("missing skipped-file diagnostic")
}

func TestProjectRulesTruncationEventAndBudgetResume(t *testing.T) {
	isolateRuleConfig(t)
	ctx := context.Background()
	mock := &provider.Mock{Responses: []domain.ModelResponse{textOnly("done")}}
	m, db, root := testManager(t, mock)
	m.UseBudget = true
	m.InitialBudget = domain.Budget{MaxModelCalls: 0, MaxToolCalls: 1}
	m.ContextTokenLimit = 1
	writeAgentRule(t, filepath.Join(root, "AGENTS.md"), strings.Repeat("rule ", 10*1024))
	writeAgentRule(t, filepath.Join(root, "src/AGENTS.md"), "omitted rule body")
	writeAgentRule(t, filepath.Join(root, "src/file.txt"), "data")
	session, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessage(ctx, session.ID, "@file:src/file.txt")
	if err != nil || run.WaitReason != domain.WaitBudget || len(mock.Requests) != 0 {
		t.Fatalf("initial budget wait=%+v err=%v", run, err)
	}
	snap, err := m.loadCheckpoint(ctx, run.ID)
	if err != nil || len(snap.Messages) != 5 || len(snap.Messages[2].Content) > rules.MaxBytes || !strings.Contains(snap.Messages[2].Content, "AGENTS.md truncated") || !strings.Contains(snap.Messages[2].Content, "src/AGENTS.md") {
		t.Fatalf("truncated rules not checkpointed: err=%v", err)
	}
	writeAgentRule(t, filepath.Join(root, "AGENTS.md"), "changed while waiting for budget")
	run, err = m.Resume(ctx, run.ID, ResumeInput{ExtraModelCalls: 1})
	if err != nil || run.Status != domain.RunCompleted || mock.Requests[0].Prefix[2].Content != snap.Messages[2].Content {
		t.Fatalf("budget resume changed frozen rules: run=%+v err=%v", run, err)
	}
	events, err := db.ListEvents(ctx, run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	files := assertRulesLoadedEvent(t, events, 1)
	if files[0].Bytes <= 0 || files[0].Bytes >= 32*1024 {
		t.Fatalf("loaded byte count did not reflect truncation: %+v", files[0])
	}
	for _, event := range events {
		if event.Type == "rules.truncated" && strings.Contains(string(event.Payload), "src/AGENTS.md") && !strings.Contains(string(event.Payload), "omitted rule body") {
			return
		}
	}
	t.Fatal("missing truncated-source diagnostic")
}

func TestNewDirectoryRulesReachModelBeforeWrite(t *testing.T) {
	isolateRuleConfig(t)
	ctx := context.Background()
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "first", Name: "write_file", Arguments: []byte(`{"path":"web/app.txt","content":"wrong"}`)}),
		toolResp(domain.ToolCall{ProviderCallID: "confirm", Name: "ask_user", Arguments: []byte(`{"question":"Continue?"}`)}),
		textOnly("read the rule"),
	}}
	m, _, root := testManager(t, mock)
	m.SkipPlanner = true
	writeAgentRule(t, filepath.Join(root, "web/AGENTS.md"), "consult design before writing")
	writeAgentRule(t, filepath.Join(root, "api/AGENTS.md"), "backend only")
	session, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessage(ctx, session.ID, "update the app")
	if err != nil || run.WaitReason != domain.WaitUser || len(mock.Requests) != 2 {
		t.Fatalf("run=%+v err=%v requests=%d", run, err, len(mock.Requests))
	}
	if _, err := os.Stat(filepath.Join(root, "web/app.txt")); !os.IsNotExist(err) {
		t.Fatalf("unexpected write or stat failure: %v", err)
	}
	before, after := messageText(mock.Requests[0].Prefix), messageText(mock.Requests[1].Prefix)
	if strings.Contains(before, "consult design") || !strings.Contains(after, "consult design") || strings.Contains(after, "backend only") || len(mock.Requests[1].Continuation) > 0 {
		t.Fatalf("wrong directory scope before/after replan: %s", after)
	}
	writeAgentRule(t, filepath.Join(root, "web/AGENTS.md"), "changed on disk")
	run, err = m.Resume(ctx, run.ID, ResumeInput{Answer: "yes"})
	if err != nil || run.Status != domain.RunCompleted || messageText(mock.Requests[2].Prefix) != after {
		t.Fatalf("resumed run changed activated rules: %+v err=%v", run, err)
	}
}

func TestRuleTargetsExactCommandUsesWorkingDirectory(t *testing.T) {
	m, _, root := testManager(t, &provider.Mock{})
	work := filepath.Join(root, "nested")
	if err := os.Mkdir(work, 0755); err != nil {
		t.Fatal(err)
	}
	args := json.RawMessage(`{"cwd":"nested","command":"printf ready | cat"}`)
	targets := m.ruleTargets(domain.Batch{Invocations: []domain.Invocation{{Name: "run_command", Arguments: args}}}, &loopState{approvalMode: "full-auto"})
	if len(targets) != 1 || targets[0] != work {
		t.Fatalf("rule targets = %v, want %s", targets, work)
	}
}

func TestRuleTargetsPollHonorsOuterAndInnerToolAccess(t *testing.T) {
	m, _, root := testManager(t, &provider.Mock{})
	m.Depth = 1
	batch := domain.Batch{Invocations: []domain.Invocation{{Name: "poll_tool", Arguments: json.RawMessage(`{"target_tool":"read_file","target_args":{"path":"web/page.txt"},"max_attempts":1}`)}}}
	for _, allowed := range [][]string{{"read_file"}, {"poll_tool"}, {"poll_tool", "read_file"}} {
		m.AllowedTools = allowed
		paths := m.ruleTargets(batch, &loopState{})
		if len(allowed) == 2 {
			if len(paths) != 1 || paths[0] != filepath.Join(root, "web/page.txt") {
				t.Errorf("poll targets = %v", paths)
			}
		} else if len(paths) != 0 {
			t.Errorf("restricted poll loaded %v", paths)
		}
	}
}

func TestExplicitFileReferenceUsesTargetDirectoryRules(t *testing.T) {
	isolateRuleConfig(t)
	mock := &provider.Mock{Responses: []domain.ModelResponse{textOnly("done")}}
	m, _, root := testManager(t, mock)
	writeAgentRule(t, filepath.Join(root, "api/AGENTS.md"), "API formatting rule")
	writeAgentRule(t, filepath.Join(root, "api/version.txt"), "release number 9")
	if err := os.Symlink("api/version.txt", filepath.Join(root, "version")); err != nil {
		t.Fatal(err)
	}
	session, err := m.CreateSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessage(t.Context(), session.ID, "@file:version")
	if err != nil || run.Status != domain.RunCompleted {
		t.Fatalf("run=%v err=%v", run.Status, err)
	}
	req := mock.Requests[0]
	if !strings.Contains(messageText(req.Prefix), "API formatting rule") || !strings.Contains(messageText(req.Messages), "release number 9") {
		t.Fatal("file reference missing target rule or content")
	}
}
