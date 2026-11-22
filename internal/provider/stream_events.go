package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

// maxStreamEventBytes bounds one server-sent event. The last event of a
// Responses stream repeats the whole response, so it matches the limit on a
// non-streaming body.
const maxStreamEventBytes = 8 << 20

// isEventStream reports whether the upstream answered a stream request with
// server-sent events. A server that ignores "stream" answers with one JSON body.
func isEventStream(res *http.Response) bool {
	return strings.Contains(strings.ToLower(res.Header.Get("Content-Type")), "text/event-stream")
}

// readStreamEvents calls handle with the data of each server-sent event until
// handle reports the last one, the stream ends, or either fails. Both APIs
// repeat the event name as "type" in the data, so "event:" lines are skipped.
func readStreamEvents(r io.Reader, handle func(data []byte) (done bool, err error)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxStreamEventBytes)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		done, err := handle(data)
		if err != nil || done {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		var coded *domain.Error
		if errors.As(err, &coded) {
			return coded
		}
		return domain.E(domain.CodeProviderHTTP, err.Error())
	}
	return nil
}

// postDeltaStream posts a stream request and returns the body the API would
// have sent without streaming. streamed is false when the upstream ignored
// "stream" and answered with that body directly, so no text was reported.
func postDeltaStream(ctx context.Context, cfg postJSONConfig, onDelta func(string), parse func(io.Reader, func(string)) ([]byte, error)) (raw []byte, streamed bool, err error) {
	err = postStreamWithRetry(ctx, cfg, func(res *http.Response, body io.Reader) error {
		var err error
		if streamed = isEventStream(res); streamed {
			raw, err = parse(body, onDelta)
			return scrubProviderError(err, cfg.APIKey)
		}
		raw, err = io.ReadAll(io.LimitReader(body, maxStreamEventBytes))
		if err != nil {
			return domain.E(domain.CodeProviderHTTP, scrubProviderKey(err.Error(), cfg.APIKey))
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return raw, streamed, nil
}

// scrubProviderError removes the API key from a stream error's message.
func scrubProviderError(err error, key string) error {
	var status *HTTPStatusError
	if errors.As(err, &status) {
		return NewHTTPStatusError(status.StatusCode, scrubProviderKey(status.cause.Message, key))
	}
	var coded *domain.Error
	if errors.As(err, &coded) {
		return domain.E(coded.Code, scrubProviderKey(coded.Message, key))
	}
	return err
}

// streamFailure is an error the upstream reported inside a 2xx stream. Before
// any text reached the caller it carries the status the same failure has
// without streaming, so the retry and fallback rules for a rejected request
// apply. Afterwards it is an ordinary error: the text cannot be taken back.
func streamFailure(status int, message string, emitted bool) error {
	message = truncateProviderMessage(message, 400)
	if message == "" {
		message = "provider reported an error in the response stream"
	}
	if status == 0 || emitted {
		return domain.E(domain.CodeProviderHTTP, message)
	}
	return NewHTTPStatusError(status, message)
}

// emitResponseText reports the text of a response that arrived in one piece.
func emitResponseText(resp domain.ModelResponse, onDelta func(string)) {
	for _, item := range resp.Items {
		if item.Kind == domain.ItemText && item.Text != "" {
			onDelta(item.Text)
		}
	}
}

// anthropicStream folds Messages API stream events back into the body the
// API returns without streaming, so ParseAnthropicMessage maps both the same
// way. Text is handed to onDelta as it arrives.
type anthropicStream struct {
	onDelta func(string)
	emitted bool
	stopped bool

	id         string
	stopReason string
	usage      map[string]json.RawMessage
	blocks     []*anthropicStreamBlock
	byIndex    map[int]*anthropicStreamBlock
}

type anthropicStreamBlock struct {
	kind      string
	start     map[string]json.RawMessage
	text      strings.Builder
	signature strings.Builder
	inputJSON strings.Builder
}

func parseAnthropicStream(r io.Reader, onDelta func(string)) ([]byte, error) {
	s := &anthropicStream{onDelta: onDelta, usage: map[string]json.RawMessage{}, byIndex: map[int]*anthropicStreamBlock{}}
	if err := readStreamEvents(r, s.event); err != nil {
		return nil, err
	}
	if !s.stopped {
		return nil, domain.E(domain.CodeProviderHTTP, "anthropic stream ended before message_stop")
	}
	return s.message()
}

func (s *anthropicStream) event(data []byte) (bool, error) {
	var ev struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message struct {
			ID    string                     `json:"id"`
			Usage map[string]json.RawMessage `json:"usage"`
		} `json:"message"`
		ContentBlock json.RawMessage `json:"content_block"`
		Delta        struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			Signature   string `json:"signature"`
			PartialJSON string `json:"partial_json"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Usage map[string]json.RawMessage `json:"usage"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return false, domain.E(domain.CodeProviderHTTP, "anthropic stream event was not JSON")
	}
	switch ev.Type {
	case "message_start":
		s.id = ev.Message.ID
		s.mergeUsage(ev.Message.Usage)
	case "content_block_start":
		block := &anthropicStreamBlock{}
		if err := json.Unmarshal(ev.ContentBlock, &block.start); err != nil || block.start == nil {
			return false, domain.E(domain.CodeProviderHTTP, "anthropic stream content block was not JSON")
		}
		_ = json.Unmarshal(block.start["type"], &block.kind)
		var initial string
		switch block.kind {
		case "text":
			_ = json.Unmarshal(block.start["text"], &initial)
			s.emit(initial)
		case "thinking":
			_ = json.Unmarshal(block.start["thinking"], &initial)
			var signature string
			_ = json.Unmarshal(block.start["signature"], &signature)
			block.signature.WriteString(signature)
		}
		block.text.WriteString(initial)
		s.blocks = append(s.blocks, block)
		s.byIndex[ev.Index] = block
	case "content_block_delta":
		block := s.byIndex[ev.Index]
		if block == nil {
			return false, domain.E(domain.CodeProviderHTTP, "anthropic stream delta had no content block")
		}
		switch ev.Delta.Type {
		case "text_delta":
			block.text.WriteString(ev.Delta.Text)
			s.emit(ev.Delta.Text)
		case "thinking_delta":
			block.text.WriteString(ev.Delta.Thinking)
		case "signature_delta":
			block.signature.WriteString(ev.Delta.Signature)
		case "input_json_delta":
			block.inputJSON.WriteString(ev.Delta.PartialJSON)
		}
	case "message_delta":
		if ev.Delta.StopReason != "" {
			s.stopReason = ev.Delta.StopReason
		}
		s.mergeUsage(ev.Usage)
	case "message_stop":
		s.stopped = true
		return true, nil
	case "error":
		return false, streamFailure(anthropicStreamErrorStatus(ev.Error.Type), strings.TrimSpace(ev.Error.Type+": "+ev.Error.Message), s.emitted)
	}
	return false, nil
}

func (s *anthropicStream) emit(text string) {
	if text == "" {
		return
	}
	s.emitted = true
	s.onDelta(text)
}

// mergeUsage keeps the latest value of each counter: message_start has the
// input side and message_delta the cumulative output.
func (s *anthropicStream) mergeUsage(usage map[string]json.RawMessage) {
	for key, value := range usage {
		if !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			s.usage[key] = value
		}
	}
}

func (s *anthropicStream) message() ([]byte, error) {
	content := make([]map[string]json.RawMessage, 0, len(s.blocks))
	for _, block := range s.blocks {
		out := block.start
		switch block.kind {
		case "text":
			out["text"] = jsonString(block.text.String())
		case "thinking":
			out["thinking"] = jsonString(block.text.String())
			out["signature"] = jsonString(block.signature.String())
		case "tool_use":
			if input := strings.TrimSpace(block.inputJSON.String()); input != "" {
				if !json.Valid([]byte(input)) {
					return nil, domain.E(domain.CodeProtocol, "anthropic stream tool input was not JSON")
				}
				out["input"] = json.RawMessage(input)
			}
		}
		content = append(content, out)
	}
	msg := map[string]any{"id": s.id, "content": content, "stop_reason": s.stopReason}
	if len(s.usage) > 0 {
		msg["usage"] = s.usage
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return nil, domain.E(domain.CodeProtocol, "anthropic stream message was not JSON")
	}
	return raw, nil
}

func jsonString(s string) json.RawMessage {
	raw, _ := json.Marshal(s)
	return raw
}

// anthropicStreamErrorStatus is the HTTP status Anthropic documents for an
// error type when the request is rejected up front.
func anthropicStreamErrorStatus(kind string) int {
	switch kind {
	case "rate_limit_error":
		return http.StatusTooManyRequests
	case "api_error":
		return http.StatusInternalServerError
	case "overloaded_error":
		return 529
	default:
		return 0
	}
}

// parseOpenAIResponsesStream hands text to onDelta as it arrives and returns
// the response object of the terminal event, the body the API returns without
// streaming, for ParseOpenAIResponses.
func parseOpenAIResponsesStream(r io.Reader, onDelta func(string)) ([]byte, error) {
	var final json.RawMessage
	var items []json.RawMessage
	emitted := false
	err := readStreamEvents(r, func(data []byte) (bool, error) {
		var ev struct {
			Type     string          `json:"type"`
			Delta    string          `json:"delta"`
			Item     json.RawMessage `json:"item"`
			Response json.RawMessage `json:"response"`
			Code     string          `json:"code"`
			Message  string          `json:"message"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return false, domain.E(domain.CodeProviderHTTP, "openai response stream event was not JSON")
		}
		switch ev.Type {
		case "response.output_text.delta", "response.refusal.delta":
			if ev.Delta != "" {
				emitted = true
				onDelta(ev.Delta)
			}
		case "response.output_item.done":
			if len(ev.Item) > 0 {
				items = append(items, ev.Item)
			}
		case "response.completed", "response.incomplete":
			final = ev.Response
			return true, nil
		case "response.failed":
			var failed struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			_ = json.Unmarshal(ev.Response, &failed)
			return false, streamFailure(openAIStreamErrorStatus(failed.Error.Code), strings.TrimSpace(failed.Error.Code+": "+failed.Error.Message), emitted)
		case "error":
			return false, streamFailure(openAIStreamErrorStatus(ev.Code), strings.TrimSpace(ev.Code+": "+ev.Message), emitted)
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	if len(final) == 0 {
		return nil, domain.E(domain.CodeProviderHTTP, "openai response stream ended before the response completed")
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(final, &env); err != nil || env == nil {
		return nil, domain.E(domain.CodeProtocol, "openai response was not JSON")
	}
	// The terminal event carries the whole output. For a compatible server that
	// leaves it out, use the items the stream finished one by one.
	var output []json.RawMessage
	if err := json.Unmarshal(env["output"], &output); err != nil || len(output) == 0 {
		if len(items) == 0 {
			return final, nil
		}
		raw, err := json.Marshal(items)
		if err != nil {
			return nil, domain.E(domain.CodeProtocol, "openai response was not JSON")
		}
		env["output"] = raw
		if final, err = json.Marshal(env); err != nil {
			return nil, domain.E(domain.CodeProtocol, "openai response was not JSON")
		}
	}
	return final, nil
}

func openAIStreamErrorStatus(code string) int {
	switch code {
	case "rate_limit_exceeded":
		return http.StatusTooManyRequests
	case "server_error":
		return http.StatusInternalServerError
	default:
		return 0
	}
}
