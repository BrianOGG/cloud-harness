package provider

import (
	"context"
	"strings"
	"sync"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

// Mock is a scripted provider. With no scripted response it returns a text answer.
// It records every request so continuation tests can see opaque bytes unchanged.
type Mock struct {
	mu        sync.Mutex
	Responses []domain.ModelResponse
	Requests  []Request
	Err       error
}

func (m *Mock) Name() string { return "mock" }

func (m *Mock) SupportsImages() bool { return true }

func (m *Mock) Complete(_ context.Context, req Request) (domain.ModelResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var images []Image
	for _, msg := range req.Messages {
		images = append(images, msg.Images...)
	}
	if err := ValidateImages(images); err != nil {
		return domain.ModelResponse{}, err
	}
	m.Requests = append(m.Requests, cloneReq(req))
	if m.Err != nil {
		return domain.ModelResponse{}, m.Err
	}
	if len(m.Responses) == 0 {
		text := ""
		for i := len(req.Messages) - 1; i >= 0; i-- {
			if req.Messages[i].Role == "user" {
				text = req.Messages[i].Content
				break
			}
		}
		resp := domain.ModelResponse{
			Provider:           "mock",
			ProviderResponseID: "mock_resp",
			Complete:           true,
			Items: []domain.ResponseItem{{
				Ordinal: 0,
				Kind:    domain.ItemText,
				Text:    "Mock answer: " + text,
			}},
		}
		emitTextDeltas(resp, req.OnDelta)
		return resp, nil
	}
	resp := m.Responses[0]
	m.Responses = m.Responses[1:]
	if resp.Provider == "" {
		resp.Provider = "mock"
	}
	resp.Items = domain.CloneItems(resp.Items)
	emitTextDeltas(resp, req.OnDelta)
	return resp, nil
}

// emitTextDeltas replays a response's text one word at a time, the way a
// streaming provider reports it before the response is complete.
func emitTextDeltas(resp domain.ModelResponse, onDelta func(string)) {
	if onDelta == nil {
		return
	}
	for _, item := range resp.Items {
		if item.Kind != domain.ItemText {
			continue
		}
		for _, word := range strings.SplitAfter(item.Text, " ") {
			if word != "" {
				onDelta(word)
			}
		}
	}
}

func cloneReq(req Request) Request {
	out := req
	out.Messages = cloneMessages(req.Messages)
	out.Prefix = cloneMessages(req.Prefix)
	out.Continuation = domain.CloneItems(req.Continuation)
	return out
}

func cloneMessages(messages []Message) []Message {
	out := append([]Message(nil), messages...)
	for i := range out {
		out[i].Images = append([]Image(nil), messages[i].Images...)
		for j := range out[i].Images {
			out[i].Images[j].SizeBytes = len(messages[i].Images[j].Data)
			out[i].Images[j].Data = nil
		}
	}
	return out
}
