package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

func TestMockRecordsImageMetadataWithoutPayload(t *testing.T) {
	image := testProviderImage()
	mock := &Mock{}
	_, err := mock.Complete(context.Background(), Request{Messages: []Message{{
		Role: "user", Content: "check this", Images: []Image{image},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(mock.Requests) != 1 || len(mock.Requests[0].Messages[0].Images) != 1 {
		t.Fatalf("mock request image count: %+v", mock.Requests)
	}
	got := mock.Requests[0].Messages[0].Images[0]
	if got.Width != 1 || got.Height != 1 || got.MediaType != "image/png" || got.SizeBytes != len(image.Data) || len(got.Data) != 0 {
		t.Fatalf("mock recorded payload or lost metadata: %+v", got)
	}
}

func TestOpenAIEncodesImageAsContentBlock(t *testing.T) {
	image := testProviderImage()
	var gotContent json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		gotContent = request.Messages[0].Content
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"seen"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	p := &OpenAICompatible{BaseURL: srv.URL, APIKey: "key", Model: "vision-model", Client: srv.Client()}
	resp, err := p.Complete(context.Background(), Request{Messages: []Message{{
		Role: "user", Content: "What is this?", Images: []Image{image},
	}}})
	if err != nil || resp.Text() != "seen" {
		t.Fatalf("response=%+v err=%v", resp, err)
	}
	var blocks []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL    string `json:"url"`
			Detail string `json:"detail"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(gotContent, &blocks); err != nil {
		t.Fatalf("content is not block array: %s (%v)", gotContent, err)
	}
	wantURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(image.Data)
	if len(blocks) != 2 || blocks[0].Type != "text" || blocks[0].Text != "What is this?" || blocks[1].Type != "image_url" || blocks[1].ImageURL.URL != wantURL || blocks[1].ImageURL.Detail != "auto" {
		t.Fatalf("content blocks: %+v", blocks)
	}
}

func TestOpenAIRejectsInvalidImagesBeforeRequest(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"unexpected"}}]}`)
	}))
	defer srv.Close()
	p := &OpenAICompatible{BaseURL: srv.URL, APIKey: "key", Model: "vision-model", Client: srv.Client()}
	oversized := testProviderImage()
	oversized.Data = append(oversized.Data, make([]byte, MaxImageBytes+1-len(oversized.Data))...)
	unsupported := Image{MediaType: "image/png", Data: []byte("not an image")}
	for name, image := range map[string]Image{"oversized": oversized, "unsupported": unsupported} {
		t.Run(name, func(t *testing.T) {
			_, err := p.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Images: []Image{image}}}})
			if err == nil || !domain.IsCode(err, domain.CodeInvalidArgument) {
				t.Fatalf("expected image validation error, got %v", err)
			}
		})
	}
	for name, p := range map[string]*OpenAICompatible{
		"disabled endpoint": {BaseURL: srv.URL, APIKey: "key", Model: "custom", DisableImages: true},
		"text-only model":   {BaseURL: srv.URL, APIKey: "key", Model: "gpt-3.5-turbo"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := p.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Images: []Image{testProviderImage()}}}})
			if err == nil || !strings.Contains(err.Error(), "does not support image input") {
				t.Fatalf("expected capability error, got %v", err)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid images triggered %d provider requests", calls.Load())
	}
}

func TestOpenAIRejectsAggregateImageLimitBeforeRequest(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer srv.Close()
	image := testProviderImage()
	image.Data = append(image.Data, make([]byte, 7<<20-len(image.Data))...)
	p := &OpenAICompatible{BaseURL: srv.URL, APIKey: "key", Model: "vision-model"}
	_, err := p.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Images: []Image{image, image, image}}}})
	if err == nil || !strings.Contains(err.Error(), "20 MB") || calls.Load() != 0 {
		t.Fatalf("aggregate validation error=%v calls=%d", err, calls.Load())
	}
}

func TestOpenAIReportsModelImageRejection(t *testing.T) {
	image := testProviderImage()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model does not support image input: data:image/png;base64,"+base64.StdEncoding.EncodeToString(image.Data), http.StatusBadRequest)
	}))
	defer srv.Close()
	p := &OpenAICompatible{BaseURL: srv.URL, APIKey: "key", Model: "text-only", Client: srv.Client(), MaxRetries: 1}
	_, err := p.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Images: []Image{image}}}})
	if err == nil || !strings.Contains(err.Error(), "provider/model rejected image input") || strings.Contains(err.Error(), base64.StdEncoding.EncodeToString(image.Data)) {
		t.Fatalf("image rejection error: %v", err)
	}
}

func testProviderImage() Image {
	data, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC")
	if err != nil {
		panic(err)
	}
	return Image{MediaType: "image/png", Width: 1, Height: 1, Data: data}
}
