package agent

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/imageinput"
	"github.com/BrianOGG/cloud-harness/internal/provider"
)

func TestImagesSurviveToolCallsCompactionAndCheckpointResume(t *testing.T) {
	ctx := context.Background()
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "read", Name: "read_file", Arguments: []byte(`{"path":"large.txt"}`)}),
		toolResp(domain.ToolCall{ProviderCallID: "ask", Name: "ask_user", Arguments: []byte(`{"question":"Which part?"}`)}),
	}}
	m, db, root := testManager(t, mock)
	m.ContextTokenLimit = 1
	m.SkipPlanner = true
	structuralCompactionOnly(m)
	if err := os.WriteFile(filepath.Join(root, "large.txt"), []byte(strings.Repeat("large tool result ", 500)), 0o600); err != nil {
		t.Fatal(err)
	}
	images := writeAgentImage(t, root)
	session, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessageWithImages(ctx, session.ID, "inspect image", images)
	if err != nil || run.Status != domain.RunWaiting || run.WaitReason != domain.WaitUser {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	if len(mock.Requests) != 2 {
		t.Fatalf("provider requests=%d", len(mock.Requests))
	}
	for _, request := range mock.Requests {
		assertAgentImage(t, request, images[0])
	}
	raw, err := db.LatestCheckpoint(ctx, run.ID)
	if err != nil || strings.Contains(string(raw), "data:image/") || strings.Contains(string(raw), base64.StdEncoding.EncodeToString(images[0].Data)) {
		t.Fatalf("checkpoint leaked bytes: %s err=%v", raw, err)
	}
	// A fresh provider must receive reloaded bytes, otherwise Mock's own input
	// validation rejects the reference-only image loaded from the checkpoint.
	restarted := &provider.Mock{}
	m.Provider = restarted
	run, err = m.Resume(ctx, run.ID, ResumeInput{Answer: "top left"})
	if err != nil || run.Status != domain.RunCompleted {
		t.Fatalf("resumed=%+v err=%v", run, err)
	}
	if len(restarted.Requests) != 1 {
		t.Fatalf("resumed requests=%d", len(restarted.Requests))
	}
	assertAgentImage(t, restarted.Requests[0], images[0])
}

func TestChangedImageReferenceFailsResumeBeforeModelRequest(t *testing.T) {
	ctx := context.Background()
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "ask", Name: "ask_user", Arguments: []byte(`{"question":"Which part?"}`)}),
	}}
	m, _, root := testManager(t, mock)
	images := writeAgentImage(t, root)
	session, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessageWithImages(ctx, session.ID, "inspect", images)
	if err != nil || run.Status != domain.RunWaiting {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	if err := os.WriteFile(images[0].Path, append(images[0].Data, 0), 0o600); err != nil {
		t.Fatal(err)
	}
	run, err = m.Resume(ctx, run.ID, ResumeInput{Answer: "top left"})
	if err != nil || run.Status != domain.RunFailed || run.Error == nil || !strings.Contains(run.Error.Message, "changed since") || len(mock.Requests) != 1 {
		t.Fatalf("resume run=%+v err=%v model calls=%d", run, err, len(mock.Requests))
	}
}

func writeAgentImage(t *testing.T, workspace string) []provider.Image {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "shot.png"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, images, err := imageinput.ParseAndLoad("@image:shot.png", workspace)
	if err != nil {
		t.Fatal(err)
	}
	return images
}

func assertAgentImage(t *testing.T, request provider.Request, want provider.Image) {
	t.Helper()
	var images []provider.Image
	for _, message := range request.Messages {
		images = append(images, message.Images...)
	}
	if len(images) != 1 || images[0].Path != want.Path || images[0].SHA256 != want.SHA256 || images[0].Width != want.Width || images[0].Height != want.Height || images[0].SizeBytes != len(want.Data) {
		t.Fatalf("image lost or changed: %+v", images)
	}
}
