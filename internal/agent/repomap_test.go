package agent

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
)

type repoMapMutatingProvider struct {
	inner *provider.Mock
	path  string
	calls int
}

func (p *repoMapMutatingProvider) Name() string { return p.inner.Name() }

func (p *repoMapMutatingProvider) SupportsImages() bool { return true }

func (p *repoMapMutatingProvider) Complete(ctx context.Context, req provider.Request) (domain.ModelResponse, error) {
	resp, err := p.inner.Complete(ctx, req)
	p.calls++
	if p.calls == 1 {
		if writeErr := os.WriteFile(p.path, []byte("package sample\nfunc NewSymbol() {}\n"), 0o644); writeErr != nil {
			return domain.ModelResponse{}, writeErr
		}
	}
	return resp, err
}

func TestRepoMapRefreshesForEachModelRequest(t *testing.T) {
	ctx := context.Background()
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "read", Name: "read_file", Arguments: []byte(`{"path":"main.go"}`)}),
		textOnly("done"),
	}}
	m, _, root := testManager(t, mock)
	m.DisableSnapshots = true
	path := filepath.Join(root, "main.go")
	if err := os.WriteFile(path, []byte("package sample\nfunc OldSymbol() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.Provider = &repoMapMutatingProvider{inner: mock, path: path}
	session, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessage(ctx, session.ID, "read the file")
	if err != nil || run.Status != domain.RunCompleted {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	if len(mock.Requests) != 2 {
		t.Fatalf("requests=%d, want 2", len(mock.Requests))
	}
	first := repoMapMessage(t, mock.Requests[0].Messages)
	second := repoMapMessage(t, mock.Requests[1].Messages)
	if !strings.Contains(first, "func:OldSymbol") || strings.Contains(first, "func:NewSymbol") {
		t.Fatalf("first request has wrong map: %s", first)
	}
	if !strings.Contains(second, "func:NewSymbol") || strings.Contains(second, "func:OldSymbol") {
		t.Fatalf("second request did not refresh map: %s", second)
	}
	for i, req := range mock.Requests {
		if len(req.Prefix) != 2 || strings.Contains(req.Prefix[0].Content+req.Prefix[1].Content, "Repo-Map:") {
			t.Fatalf("request %d put Repo-Map in stable prefix: %+v", i, req.Prefix)
		}
	}
}

func TestRepoMapPreservesImagesInDynamicMessages(t *testing.T) {
	ctx := context.Background()
	mock := &provider.Mock{}
	m, _, root := testManager(t, mock)
	m.DisableSnapshots = true
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc Main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	images := writeAgentImage(t, root)
	session, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessageWithImages(ctx, session.ID, "look at this", images)
	if err != nil || run.Status != domain.RunCompleted {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	if len(mock.Requests) != 1 {
		t.Fatalf("requests=%d", len(mock.Requests))
	}
	assertAgentImage(t, mock.Requests[0], images[0])
	if repoMapMessage(t, mock.Requests[0].Messages) == "" {
		t.Fatal("Repo-Map missing from request with image input")
	}
}

func TestRepoMapDisabledAndEmptyWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name       string
		disable    bool
		withSource bool
	}{
		{name: "disabled", disable: true, withSource: true},
		{name: "empty map", withSource: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			mock := &provider.Mock{}
			m, db, root := testManager(t, mock)
			m.DisableSnapshots = true
			m.DisableRepoMap = tc.disable
			if tc.withSource {
				if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc Main() {}\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			session, err := m.CreateSession(ctx)
			if err != nil {
				t.Fatal(err)
			}
			run, err := m.RunMessage(ctx, session.ID, "hello")
			if err != nil || run.Status != domain.RunCompleted {
				t.Fatalf("run=%+v err=%v", run, err)
			}
			if len(mock.Requests) != 1 {
				t.Fatalf("requests=%d", len(mock.Requests))
			}
			if strings.Contains(messageText(mock.Requests[0].Messages), "Repo-Map:") {
				t.Fatal("Repo-Map unexpectedly injected")
			}
			if tc.disable {
				want := []provider.Message{
					{Role: "system", Content: defaultSystem},
					{Role: "system", Content: defaultTools},
					{Role: "user", Content: "hello"},
				}
				if !reflect.DeepEqual(mock.Requests[0].Messages, want) {
					t.Fatalf("disabled Repo-Map changed the pre-feature request: got=%+v want=%+v", mock.Requests[0].Messages, want)
				}
			}
			events, err := db.ListEvents(ctx, run.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if strings.HasPrefix(event.Type, "repomap.") {
					t.Fatalf("unexpected Repo-Map diagnostic in %s case: %+v", tc.name, event)
				}
			}
		})
	}
}

func TestRepoMapScanFailureIsDiagnosticAndDoesNotFailRun(t *testing.T) {
	ctx := context.Background()
	mock := &provider.Mock{}
	m, db, root := testManager(t, mock)
	m.DisableSnapshots = true
	m.Workspace = filepath.Join(root, "missing")
	session, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessage(ctx, session.ID, "hello")
	if err != nil || run.Status != domain.RunCompleted {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	if len(mock.Requests) != 1 || strings.Contains(messageText(mock.Requests[0].Messages), "Repo-Map:") {
		t.Fatalf("failed map was not skipped: requests=%+v", mock.Requests)
	}
	events, err := db.ListEvents(ctx, run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == "repomap.skipped" && len(event.Payload) > 0 {
			return
		}
	}
	t.Fatalf("missing Repo-Map scan diagnostic: %+v", events)
}

func repoMapMessage(t *testing.T, messages []provider.Message) string {
	t.Helper()
	for _, message := range messages {
		if message.Role == "system" && strings.HasPrefix(message.Content, "Repo-Map:\n") {
			return message.Content
		}
	}
	t.Fatalf("request has no Repo-Map section: %+v", messages)
	return ""
}
