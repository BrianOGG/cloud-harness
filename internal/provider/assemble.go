package provider

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

type partialCall struct {
	index int
	id    string
	name  string
	args  strings.Builder
}

type assembler struct {
	id        string
	text      strings.Builder
	reasoning strings.Builder
	calls     map[int]*partialCall
	order     []int
	finish    string
	sawDone   bool
	usage     domain.Usage
	onDelta   func(string)
	deltas    []string
}

func newAssembler() *assembler {
	return &assembler{calls: map[int]*partialCall{}}
}

func (a *assembler) consumeData(line []byte) error {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || bytes.Equal(line, []byte("[DONE]")) {
		if bytes.Equal(line, []byte("[DONE]")) {
			a.sawDone = true
		}
		return nil
	}
	var chunk struct {
		ID      string `json:"id"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Delta        struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
		Usage *chatUsage `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &chunk); err != nil {
		return domain.E(domain.CodeProviderHTTP, "stream chunk was not JSON")
	}
	if chunk.Error != nil && chunk.Error.Message != "" {
		return domain.E(domain.CodeProviderHTTP, chunk.Error.Message)
	}
	if chunk.ID != "" {
		a.id = chunk.ID
	}
	if chunk.Usage != nil {
		a.usage = chunk.Usage.domain()
	}
	for _, ch := range chunk.Choices {
		if ch.FinishReason != "" {
			a.finish = ch.FinishReason
		}
		if ch.Delta.Content != "" && a.onDelta != nil {
			a.deltas = append(a.deltas, ch.Delta.Content)
		}
		a.text.WriteString(ch.Delta.Content)
		a.reasoning.WriteString(ch.Delta.ReasoningContent)
		for _, tc := range ch.Delta.ToolCalls {
			call := a.calls[tc.Index]
			if call == nil {
				call = &partialCall{index: tc.Index}
				a.calls[tc.Index] = call
				a.order = append(a.order, tc.Index)
			}
			if tc.ID != "" {
				call.id = tc.ID
			}
			if tc.Function.Name != "" {
				call.name = tc.Function.Name
			}
			call.args.WriteString(tc.Function.Arguments)
		}
	}
	return nil
}

func (a *assembler) response() domain.ModelResponse {
	text, inlineReasoning, tagged := splitThinkingContent(a.text.String())
	items := make([]domain.ResponseItem, 0, 2+len(a.order))
	add := func(it domain.ResponseItem) {
		it.Ordinal = len(items)
		items = append(items, it)
	}
	if a.reasoning.Len() > 0 {
		add(domain.ResponseItem{
			Kind: domain.ItemOpaque,
			Opaque: &domain.OpaqueItem{
				Provider: "openai",
				Kind:     "reasoning_content",
				Payload:  []byte(a.reasoning.String()),
			},
		})
	}
	if inlineReasoning != "" {
		add(domain.ResponseItem{Kind: domain.ItemOpaque, Opaque: &domain.OpaqueItem{
			Provider: "openai", Kind: "reasoning_content", Payload: []byte(inlineReasoning),
		}})
	}
	if text != "" {
		add(domain.ResponseItem{Kind: domain.ItemText, Text: text})
	}
	for _, idx := range a.order {
		call := a.calls[idx]
		args := strings.TrimSpace(call.args.String())
		if args == "" {
			args = "{}"
		}
		add(domain.ResponseItem{
			Kind: domain.ItemToolCall,
			ToolCall: &domain.ToolCall{
				ProviderCallID: call.id,
				Name:           call.name,
				Arguments:      json.RawMessage(args),
			},
		})
	}
	if hasUsage(a.usage) {
		u := a.usage
		add(domain.ResponseItem{Kind: domain.ItemUsage, Usage: &u})
	}
	complete := a.finish != ""
	// Until the complete content is known, a later orphan </think> can turn
	// any prefix into private reasoning. Do not publish it through OnDelta
	// first: trace streams cannot retract leaked text. Preserve plain-content
	// chunk boundaries, but release them only after classification.
	if complete && a.onDelta != nil {
		if tagged {
			if text != "" {
				a.onDelta(text)
			}
		} else {
			for _, delta := range a.deltas {
				a.onDelta(delta)
			}
		}
	}
	return domain.ModelResponse{
		Provider:           "openai",
		ProviderResponseID: a.id,
		Complete:           complete,
		Items:              items,
	}
}

func parseSSE(r io.Reader, scrubber func(string) string, onDelta ...func(string)) (domain.ModelResponse, error) {
	a := newAssembler()
	if len(onDelta) > 0 {
		a.onDelta = onDelta[0]
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		trim := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trim, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(trim, []byte("data:")))
		if err := a.consumeData(data); err != nil {
			if scrubber != nil {
				return domain.ModelResponse{}, domain.E(domain.CodeProviderHTTP, scrubber(err.Error()))
			}
			return domain.ModelResponse{}, err
		}
		if a.sawDone {
			break
		}
	}
	if err := sc.Err(); err != nil {
		if scrubber != nil {
			return domain.ModelResponse{}, domain.E(domain.CodeProviderHTTP, scrubber(err.Error()))
		}
		return domain.ModelResponse{}, domain.E(domain.CodeProviderHTTP, err.Error())
	}
	return a.response(), nil
}

func parseChatJSON(body []byte, onDelta ...func(string)) (domain.ModelResponse, error) {
	var parsed struct {
		ID      string     `json:"id"`
		Usage   *chatUsage `json:"usage"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          string         `json:"content"`
				ReasoningContent string         `json:"reasoning_content"`
				ToolCalls        []chatToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return domain.ModelResponse{}, domain.E(domain.CodeProviderHTTP, "response was not a chat completion")
	}
	if len(parsed.Choices) == 0 {
		return domain.ModelResponse{}, domain.E(domain.CodeProtocol, "chat completion had no choices")
	}
	msg := parsed.Choices[0].Message
	a := newAssembler()
	if len(onDelta) > 0 {
		a.onDelta = onDelta[0]
	}
	a.id = parsed.ID
	a.finish = parsed.Choices[0].FinishReason
	if a.finish == "" {
		a.finish = "stop"
	}
	if parsed.Usage != nil {
		a.usage = parsed.Usage.domain()
	}
	if msg.Content != "" && a.onDelta != nil {
		a.deltas = append(a.deltas, msg.Content)
	}
	a.text.WriteString(msg.Content)
	a.reasoning.WriteString(msg.ReasoningContent)
	for i, tc := range msg.ToolCalls {
		idx := i
		if tc.Index != nil {
			idx = *tc.Index
		}
		call := &partialCall{index: idx, id: tc.ID, name: tc.Function.Name}
		call.args.WriteString(tc.Function.Arguments)
		a.calls[idx] = call
		a.order = append(a.order, idx)
	}
	return a.response(), nil
}

type chatToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}
