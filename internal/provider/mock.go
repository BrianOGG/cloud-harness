package provider

import (
	"context"
	"sync"

	"github.com/cloud-harness/cloud-harness/internal/domain"
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

func (m *Mock) Complete(_ context.Context, req Request) (domain.ModelResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
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
		return domain.ModelResponse{
			Provider:           "mock",
			ProviderResponseID: "mock_resp",
			Complete:           true,
			Items: []domain.ResponseItem{{
				Ordinal: 0,
				Kind:    domain.ItemText,
				Text:    "Mock answer: " + text,
			}},
		}, nil
	}
	resp := m.Responses[0]
	m.Responses = m.Responses[1:]
	if resp.Provider == "" {
		resp.Provider = "mock"
	}
	resp.Items = domain.CloneItems(resp.Items)
	return resp, nil
}

func cloneReq(req Request) Request {
	out := req
	out.Messages = append([]Message(nil), req.Messages...)
	out.Prefix = append([]Message(nil), req.Prefix...)
	out.Continuation = domain.CloneItems(req.Continuation)
	return out
}
