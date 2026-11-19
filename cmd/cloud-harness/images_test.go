package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloud-harness/cloud-harness/internal/config"
	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/provider"
)

const cliImageBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC"

func TestChatImageInputReachesModelWithoutPersistingImageBytes(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "chat.db")
	imageBytes, err := base64.StdEncoding.DecodeString(cliImageBase64)
	if err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(workspace, "my shot.png")
	if err := os.WriteFile(imagePath, imageBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	requests := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"I see the screenshot"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	cfg := config.Config{Provider: "openai", OpenAIBaseURL: srv.URL, OpenAIAPIKey: "test", OpenAIModel: "vision-model", DBPath: dbPath}
	mgr, db, err := openManager(ctx, cfg, workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var out, stderr bytes.Buffer
	if code := runChat(ctx, mgr, "image-session", "", strings.NewReader("Analyze this @image:\"my shot.png\"\n"), &out, &stderr); code != 0 {
		t.Fatalf("chat exit=%d out=%s stderr=%s", code, &out, &stderr)
	}
	if !strings.Contains(out.String(), "I see the screenshot") {
		t.Fatalf("chat output: %s", &out)
	}
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(<-requests, &req); err != nil {
		t.Fatal(err)
	}
	var seenUser bool
	for _, msg := range req.Messages {
		if msg.Role != "user" {
			continue
		}
		seenUser = true
		var content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ImageURL struct {
				URL string `json:"url"`
			} `json:"image_url"`
		}
		if err := json.Unmarshal(msg.Content, &content); err != nil {
			t.Fatal(err)
		}
		if len(content) != 2 || content[0].Text != "Analyze this" || content[0].Type != "text" || content[1].Type != "image_url" || content[1].ImageURL.URL != "data:image/png;base64,"+cliImageBase64 {
			t.Fatalf("model did not receive both text and image: %s", msg.Content)
		}
	}
	if !seenUser {
		t.Fatal("model did not receive the user message")
	}
	runs, err := db.ListRuns(ctx, "image-session")
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
	checkpoint, err := db.LatestCheckpoint(ctx, runs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		Messages []provider.Message `json:"messages"`
	}
	if err := json.Unmarshal(checkpoint, &stored); err != nil {
		t.Fatal(err)
	}
	var refs []provider.Image
	for _, message := range stored.Messages {
		refs = append(refs, message.Images...)
	}
	if len(refs) != 1 || refs[0].Path != imagePath || refs[0].MediaType != "image/png" || refs[0].SizeBytes != len(imageBytes) || refs[0].Width != 1 || refs[0].Height != 1 || refs[0].SHA256 == "" || len(refs[0].Data) != 0 {
		t.Fatalf("checkpoint reference metadata: %+v", refs)
	}
	// Check the DB and live WAL before closing; this covers all persisted rows,
	// including turn input, history, events, responses, and checkpoints.
	for _, path := range []string{dbPath, dbPath + "-wal"} {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, imageBytes) || bytes.Contains(data, []byte(cliImageBase64)) || bytes.Contains(data, []byte("data:image/")) {
			t.Fatalf("image payload leaked to %s", path)
		}
	}
}

func TestRunImageInputAndChatMockMetadata(t *testing.T) {
	workspace := t.TempDir()
	data, _ := base64.StdEncoding.DecodeString(cliImageBase64)
	if err := os.WriteFile(filepath.Join(workspace, "shot.png"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	code := MainWithIO([]string{"run", "--db", filepath.Join(t.TempDir(), "run.db"), "--workspace", workspace, "--provider", "mock", "--message", "inspect @image:shot.png"}, nil, &out, &stderr)
	if code != 0 || !strings.Contains(out.String(), "Mock answer: inspect") || strings.Contains(out.String(), "@image:") {
		t.Fatalf("run exit=%d out=%s stderr=%s", code, &out, &stderr)
	}
	mock := &provider.Mock{}
	mgr, _, root := newChatTestManager(t, mock)
	if err := os.WriteFile(filepath.Join(root, "shot.png"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	stderr.Reset()
	if code := runChat(context.Background(), mgr, "mock-image-session", "inspect @image:shot.png @image:shot.png", nil, &out, &stderr); code != 0 {
		t.Fatalf("chat exit=%d out=%s stderr=%s", code, &out, &stderr)
	}
	if len(mock.Requests) != 1 {
		t.Fatalf("mock requests=%d", len(mock.Requests))
	}
	user := mock.Requests[0].Messages[len(mock.Requests[0].Messages)-1]
	if user.Content != "inspect" || len(user.Images) != 2 || user.Images[0].SizeBytes != len(data) || user.Images[0].Width != 1 || user.Images[0].Height != 1 || len(user.Images[0].Data) != 0 {
		t.Fatalf("mock user message: %+v", user)
	}
}

func TestChatRejectsBadImageWithoutStartingTurnOrRequest(t *testing.T) {
	for _, input := range []string{"inspect @image:", "inspect @image:missing.png", "inspect @image:not-image.png", "inspect @image:shot.png @image:missing.png"} {
		t.Run(input, func(t *testing.T) {
			mock := &provider.Mock{}
			mgr, db, root := newChatTestManager(t, mock)
			if err := os.WriteFile(filepath.Join(root, "not-image.png"), []byte("plain text"), 0o600); err != nil {
				t.Fatal(err)
			}
			data, _ := base64.StdEncoding.DecodeString(cliImageBase64)
			if err := os.WriteFile(filepath.Join(root, "shot.png"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			var out, stderr bytes.Buffer
			if code := runChat(context.Background(), mgr, "invalid-image-session", input, nil, &out, &stderr); code != 1 || stderr.Len() == 0 {
				t.Fatalf("exit=%d out=%s stderr=%s", code, &out, &stderr)
			}
			runs, err := db.ListRuns(context.Background(), "invalid-image-session")
			if err != nil || len(runs) != 0 || len(mock.Requests) != 0 {
				t.Fatalf("invalid image started work: runs=%+v calls=%d err=%v", runs, len(mock.Requests), err)
			}
			if turns, err := db.CountTurns(context.Background(), "invalid-image-session"); err != nil || turns != 0 {
				t.Fatalf("invalid image persisted a turn: count=%d err=%v", turns, err)
			}
		})
	}
}

func TestChatRejectsUnsupportedProviderWithoutCallingIt(t *testing.T) {
	var calls atomic.Int32
	prov := &scriptedProvider{step: func(int, provider.Request) (domain.ModelResponse, error) {
		calls.Add(1)
		return domain.ModelResponse{}, nil
	}}
	mgr, db, root := newChatTestManager(t, prov)
	data, _ := base64.StdEncoding.DecodeString(cliImageBase64)
	if err := os.WriteFile(filepath.Join(root, "shot.png"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := runChat(context.Background(), mgr, "unsupported-image-session", "@image:shot.png", nil, &out, &stderr); code != 1 || !strings.Contains(stderr.String(), "does not support image input") {
		t.Fatalf("exit=%d out=%s stderr=%s", code, &out, &stderr)
	}
	runs, err := db.ListRuns(context.Background(), "unsupported-image-session")
	if err != nil || len(runs) != 0 || calls.Load() != 0 {
		t.Fatalf("unsupported provider started work: runs=%+v calls=%d err=%v", runs, calls.Load(), err)
	}
}

func TestChatAskUserAnswerCanAttachImage(t *testing.T) {
	for _, answer := range []string{"top left @image:shot.png", "@image:"} {
		t.Run(answer, func(t *testing.T) {
			mock := &provider.Mock{Responses: []domain.ModelResponse{{
				Provider: "mock", Complete: true,
				Items: []domain.ResponseItem{{Kind: domain.ItemToolCall, ToolCall: &domain.ToolCall{
					ProviderCallID: "ask", Name: domain.ToolAskUser, Arguments: []byte(`{"question":"Show me the error"}`),
				}}},
			}}}
			mgr, db, root := newChatTestManager(t, mock)
			data, _ := base64.StdEncoding.DecodeString(cliImageBase64)
			if err := os.WriteFile(filepath.Join(root, "shot.png"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			var out, stderr bytes.Buffer
			code := runChat(context.Background(), mgr, "image-answer-session", "inspect the error", strings.NewReader(answer+"\n"), &out, &stderr)
			runs, err := db.ListRuns(context.Background(), "image-answer-session")
			if err != nil || len(runs) != 1 {
				t.Fatalf("runs=%+v err=%v", runs, err)
			}
			if answer == "@image:" {
				if code != 1 || len(mock.Requests) != 1 || runs[0].Status != domain.RunWaiting {
					t.Fatalf("invalid answer resumed work: exit=%d requests=%d run=%+v stderr=%s", code, len(mock.Requests), runs[0], &stderr)
				}
				return
			}
			if code != 0 || len(mock.Requests) != 2 || runs[0].Status != domain.RunCompleted {
				t.Fatalf("valid answer failed: exit=%d requests=%d run=%+v stderr=%s", code, len(mock.Requests), runs[0], &stderr)
			}
			messages := mock.Requests[1].Messages
			user := messages[len(messages)-1]
			if user.Content != "User reply: top left" || len(user.Images) != 1 || user.Images[0].Width != 1 {
				t.Fatalf("answer image did not reach model: %+v", user)
			}
		})
	}
}
