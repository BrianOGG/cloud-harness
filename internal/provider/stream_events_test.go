package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

const streamTestKey = "stream-test-key"

// streamFixtureEvents returns the events of a fixture, each with its
// terminating blank line.
func streamFixtureEvents(t *testing.T, name string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "stream", name))
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	for _, ev := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n\n") {
		if strings.TrimSpace(ev) != "" {
			events = append(events, ev+"\n\n")
		}
	}
	return events
}

// writeStreamFixture sends a fixture one event per write. After each event
// that carries text it waits for the provider to report that text, so a
// provider that reads the whole body before reporting anything fails the test.
func writeStreamFixture(t *testing.T, w http.ResponseWriter, name string, reported <-chan string) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	flusher := w.(http.Flusher)
	for _, ev := range streamFixtureEvents(t, name) {
		_, _ = io.WriteString(w, ev)
		flusher.Flush()
		if reported == nil || !strings.Contains(ev, `"text_delta"`) && !strings.Contains(ev, `"response.output_text.delta"`) && !strings.Contains(ev, `"response.refusal.delta"`) {
			continue
		}
		select {
		case <-reported:
		case <-time.After(5 * time.Second):
			t.Errorf("%s: text was not reported before the next event was sent: %s", name, ev)
			return
		}
	}
}

type streamProviderCase struct {
	name string
	path string
	new  func(baseURL string, client *http.Client) Provider
}

func streamProviderCases() []streamProviderCase {
	return []streamProviderCase{
		{name: "anthropic", path: "/v1/messages", new: func(baseURL string, client *http.Client) Provider {
			return &Anthropic{BaseURL: baseURL, APIKey: streamTestKey, Model: "claude-test", Client: client, RetryBaseDelay: time.Millisecond, IdleTimeout: 200 * time.Millisecond}
		}},
		{name: "responses", path: "/responses", new: func(baseURL string, client *http.Client) Provider {
			return &OpenAIResponses{BaseURL: baseURL, APIKey: streamTestKey, Model: "gpt-test", Client: client, RetryBaseDelay: time.Millisecond, IdleTimeout: 200 * time.Millisecond}
		}},
	}
}

func streamTestRequest(onDelta func(string)) Request {
	return Request{
		Messages: []Message{{Role: "system", Content: "be brief"}, {Role: "user", Content: "read the api doc"}},
		Tools:    []ToolDefinition{{Name: "mcp.files.read", Description: "read", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)}},
		OnDelta:  onDelta,
	}
}

func TestProviderStreamDeltaOrderAndFinalMessage(t *testing.T) {
	wantDeltas := []string{"I'll", " read", " the file", " first.\n"}
	const wantText = "I'll read the file first.\n"
	for _, tc := range []struct {
		streamProviderCase
		fixture      string
		provider     string
		responseID   string
		callID       string
		opaqueKind   string
		opaqueHas    string
		signature    string
		wantUsage    domain.Usage
		wantArgument string
	}{
		{
			streamProviderCase: streamProviderCases()[0], fixture: "anthropic_thinking_text_tool.sse",
			provider: "anthropic", responseID: "msg_01StreamFixture", callID: "toolu_01StreamFixture",
			opaqueKind: "thinking", opaqueHas: `"thinking":"The user wants the file. Read it first."`,
			signature: "EqQBCgIYAhIM1gbcDa9GJwZA2b3hGgxBdjrkzLoky3dl1pk=",
			wantUsage: domain.Usage{InputTokens: 365, OutputTokens: 58, CachedInputTokens: 300, CacheCreationInputTokens: 40, CacheReported: true},
		},
		{
			streamProviderCase: streamProviderCases()[1], fixture: "responses_reasoning_text_tool.sse",
			provider: "openai-responses", responseID: "resp_stream_fixture", callID: "call_stream_1",
			opaqueKind: "reasoning", opaqueHas: `"encrypted_content":"gAAAAABfixture-encrypted=="`,
			wantUsage: domain.Usage{InputTokens: 365, OutputTokens: 58, CachedInputTokens: 300, ReasoningTokens: 32, CacheReported: true, ReasoningReported: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reported := make(chan string, 16)
			var gotPath, gotAccept string
			var body struct {
				Stream bool `json:"stream"`
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath, gotAccept = r.URL.Path, r.Header.Get("Accept")
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				writeStreamFixture(t, w, tc.fixture, reported)
			}))
			defer srv.Close()

			var deltas []string
			resp, err := tc.new(srv.URL, srv.Client()).Complete(context.Background(), streamTestRequest(func(d string) {
				deltas = append(deltas, d)
				reported <- d
			}))
			if err != nil {
				t.Fatal(err)
			}
			if gotPath != tc.path || gotAccept != "text/event-stream" || !body.Stream {
				t.Fatalf("request path=%q accept=%q stream=%v", gotPath, gotAccept, body.Stream)
			}
			if !reflect.DeepEqual(deltas, wantDeltas) {
				t.Fatalf("deltas %q, want %q", deltas, wantDeltas)
			}
			if resp.Provider != tc.provider || resp.ProviderResponseID != tc.responseID || !resp.Complete {
				t.Fatalf("response %+v", resp)
			}
			if len(resp.Items) != 4 {
				t.Fatalf("items %+v", resp.Items)
			}
			opaque, text, call, usage := resp.Items[0], resp.Items[1], resp.Items[2], resp.Items[3]
			if opaque.Kind != domain.ItemOpaque || opaque.Opaque.Kind != tc.opaqueKind || !json.Valid(opaque.Opaque.Payload) ||
				!strings.Contains(string(opaque.Opaque.Payload), tc.opaqueHas) || string(opaque.Opaque.Signature) != tc.signature {
				t.Fatalf("opaque item %+v payload %s", opaque.Opaque, opaque.Opaque.Payload)
			}
			if text.Kind != domain.ItemText || text.Text != wantText || text.Text != strings.Join(deltas, "") {
				t.Fatalf("text item %+v", text)
			}
			if call.Kind != domain.ItemToolCall || call.ToolCall.Name != "mcp.files.read" || call.ToolCall.ProviderCallID != tc.callID {
				t.Fatalf("tool call %+v", call.ToolCall)
			}
			var args struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(call.ToolCall.Arguments, &args); err != nil || args.Path != "docs/api.md" {
				t.Fatalf("tool arguments %s: %v", call.ToolCall.Arguments, err)
			}
			if usage.Kind != domain.ItemUsage || *usage.Usage != tc.wantUsage {
				t.Fatalf("usage %+v", usage.Usage)
			}
			for i, item := range resp.Items {
				if item.Ordinal != i {
					t.Fatalf("item %d has ordinal %d", i, item.Ordinal)
				}
			}
		})
	}
}

// The streamed message must map to the same response as the same message
// returned in one body.
func TestAnthropicStreamMatchesBufferedMessage(t *testing.T) {
	const buffered = `{"id":"msg_01StreamFixture","content":[
		{"type":"thinking","thinking":"The user wants the file. Read it first.","signature":"EqQBCgIYAhIM1gbcDa9GJwZA2b3hGgxBdjrkzLoky3dl1pk="},
		{"type":"text","text":"I'll read the file first.\n"},
		{"type":"tool_use","id":"toolu_01StreamFixture","name":"mcp_files_read","input":{"path":"docs/api.md"}}],
		"stop_reason":"tool_use","usage":{"input_tokens":25,"cache_creation_input_tokens":40,"cache_read_input_tokens":300,"output_tokens":58}}`
	want, err := ParseAnthropicMessage([]byte(buffered))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := parseAnthropicStream(strings.NewReader(strings.Join(streamFixtureEvents(t, "anthropic_thinking_text_tool.sse"), "")), func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseAnthropicMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	// The opaque payload is replayed as JSON, so compare it as JSON.
	var gotPayload, wantPayload any
	if err := json.Unmarshal(got.Items[0].Opaque.Payload, &gotPayload); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want.Items[0].Opaque.Payload, &wantPayload); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotPayload, wantPayload) {
		t.Fatalf("opaque payload %s, want %s", got.Items[0].Opaque.Payload, want.Items[0].Opaque.Payload)
	}
	got.Items[0].Opaque.Payload, want.Items[0].Opaque.Payload = nil, nil
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("streamed %+v\nbuffered %+v", got, want)
	}
}

func TestProviderStreamTruncationAndRefusal(t *testing.T) {
	cases := streamProviderCases()
	for _, tc := range []struct {
		streamProviderCase
		name       string
		fixture    string
		wantDeltas []string
		complete   bool
		output     int
	}{
		{cases[0], "anthropic max_tokens", "anthropic_max_tokens.sse", []string{"The first three", " steps are"}, false, 6},
		{cases[0], "anthropic refusal", "anthropic_refusal.sse", []string{"I can't", " help with that."}, false, 9},
		{cases[1], "responses incomplete", "responses_incomplete.sse", []string{"The first three", " steps are"}, false, 6},
		{cases[1], "responses refusal", "responses_refusal.sse", []string{"I can't", " help with that."}, true, 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reported := make(chan string, 16)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeStreamFixture(t, w, tc.fixture, reported)
			}))
			defer srv.Close()
			var deltas []string
			resp, err := tc.new(srv.URL, srv.Client()).Complete(context.Background(), streamTestRequest(func(d string) {
				deltas = append(deltas, d)
				reported <- d
			}))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(deltas, tc.wantDeltas) {
				t.Fatalf("deltas %q, want %q", deltas, tc.wantDeltas)
			}
			if resp.Complete != tc.complete {
				t.Fatalf("complete = %v, want %v", resp.Complete, tc.complete)
			}
			if len(resp.Items) != 2 || resp.Items[0].Kind != domain.ItemText || resp.Items[0].Text != strings.Join(tc.wantDeltas, "") {
				t.Fatalf("items %+v", resp.Items)
			}
			if resp.Items[1].Kind != domain.ItemUsage || resp.Items[1].Usage.OutputTokens != tc.output {
				t.Fatalf("usage %+v", resp.Items[1])
			}
		})
	}
}

func streamRetryFixtures(provider string) (failBeforeText, failAfterText, ok string, status int) {
	if provider == "anthropic" {
		return "anthropic_overloaded.sse", "anthropic_error_after_text.sse", "anthropic_ok.sse", 529
	}
	return "responses_server_error.sse", "responses_error_after_text.sse", "responses_ok.sse", http.StatusInternalServerError
}

func TestProviderStreamRetriesBeforeAnyText(t *testing.T) {
	for _, tc := range streamProviderCases() {
		t.Run(tc.name, func(t *testing.T) {
			failBeforeText, _, ok, _ := streamRetryFixtures(tc.name)
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch calls.Add(1) {
				case 1:
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(w, `{"error":{"message":"slow down"}}`)
				case 2:
					writeStreamFixture(t, w, failBeforeText, nil)
				default:
					writeStreamFixture(t, w, ok, nil)
				}
			}))
			defer srv.Close()
			var deltas []string
			resp, err := tc.new(srv.URL, srv.Client()).Complete(context.Background(), streamTestRequest(func(d string) { deltas = append(deltas, d) }))
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 3 {
				t.Fatalf("calls = %d, want 3", calls.Load())
			}
			if !reflect.DeepEqual(deltas, []string{"po", "ng"}) || resp.Text() != "pong" || !resp.Complete {
				t.Fatalf("deltas %q response %+v", deltas, resp)
			}
		})
	}
}

func TestProviderStreamSingleAttemptReportsStatus(t *testing.T) {
	for _, tc := range streamProviderCases() {
		t.Run(tc.name, func(t *testing.T) {
			failBeforeText, _, _, status := streamRetryFixtures(tc.name)
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				writeStreamFixture(t, w, failBeforeText, nil)
			}))
			defer srv.Close()
			req := streamTestRequest(func(d string) { t.Errorf("unexpected delta %q", d) })
			req.singleAttempt = true
			_, err := tc.new(srv.URL, srv.Client()).Complete(context.Background(), req)
			var statusErr *HTTPStatusError
			if !errors.As(err, &statusErr) || statusErr.StatusCode != status || retryReason(err) != "upstream_5xx" {
				t.Fatalf("err = %v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("calls = %d, want 1", calls.Load())
			}
		})
	}
}

func TestProviderStreamErrorAfterTextIsNotRetried(t *testing.T) {
	for _, tc := range streamProviderCases() {
		t.Run(tc.name, func(t *testing.T) {
			_, failAfterText, _, _ := streamRetryFixtures(tc.name)
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				writeStreamFixture(t, w, failAfterText, nil)
			}))
			defer srv.Close()
			var deltas []string
			resp, err := tc.new(srv.URL, srv.Client()).Complete(context.Background(), streamTestRequest(func(d string) { deltas = append(deltas, d) }))
			if !domain.IsCode(err, domain.CodeProviderHTTP) {
				t.Fatalf("err = %v", err)
			}
			var statusErr *HTTPStatusError
			if errors.As(err, &statusErr) {
				t.Fatalf("error after text must not be retryable: %v", err)
			}
			if calls.Load() != 1 || len(resp.Items) != 0 || !reflect.DeepEqual(deltas, []string{"Half an"}) {
				t.Fatalf("calls=%d items=%+v deltas=%q", calls.Load(), resp.Items, deltas)
			}
		})
	}
}

func TestProviderStreamCutShortIsAnError(t *testing.T) {
	for _, tc := range streamProviderCases() {
		t.Run(tc.name, func(t *testing.T) {
			_, _, ok, _ := streamRetryFixtures(tc.name)
			events := streamFixtureEvents(t, ok)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				// Everything but the terminal event.
				_, _ = io.WriteString(w, strings.Join(events[:len(events)-2], ""))
			}))
			defer srv.Close()
			resp, err := tc.new(srv.URL, srv.Client()).Complete(context.Background(), streamTestRequest(func(string) {}))
			if !domain.IsCode(err, domain.CodeProviderHTTP) || len(resp.Items) != 0 {
				t.Fatalf("resp=%+v err=%v", resp, err)
			}
		})
	}
}

func TestProviderStreamIdleTimeout(t *testing.T) {
	for _, tc := range streamProviderCases() {
		t.Run(tc.name, func(t *testing.T) {
			_, _, ok, _ := streamRetryFixtures(tc.name)
			events := streamFixtureEvents(t, ok)
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, events[0])
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
				}
			}))
			defer srv.Close()
			defer close(release)
			start := time.Now()
			_, err := tc.new(srv.URL, srv.Client()).Complete(context.Background(), streamTestRequest(func(string) {}))
			if err == nil || !strings.Contains(err.Error(), "idle timeout") {
				t.Fatalf("err = %v", err)
			}
			if time.Since(start) > 5*time.Second {
				t.Fatalf("stalled stream took %s", time.Since(start))
			}
		})
	}
}

// The header timeout covers the wait for the response to start, not the
// stream that follows it.
func TestProviderStreamHeaderTimeout(t *testing.T) {
	for _, tc := range streamProviderCases() {
		t.Run(tc.name, func(t *testing.T) {
			_, _, ok, _ := streamRetryFixtures(tc.name)
			events := streamFixtureEvents(t, ok)
			var calls atomic.Int32
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					select {
					case <-release:
					case <-r.Context().Done():
					}
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				for _, ev := range events {
					_, _ = io.WriteString(w, ev)
					w.(http.Flusher).Flush()
					time.Sleep(40 * time.Millisecond)
				}
			}))
			defer srv.Close()
			defer close(release)
			p := tc.new(srv.URL, srv.Client())
			switch p := p.(type) {
			case *Anthropic:
				p.ResponseHeaderTimeout = 60 * time.Millisecond
			case *OpenAIResponses:
				p.ResponseHeaderTimeout = 60 * time.Millisecond
			}
			_, err := p.Complete(context.Background(), streamTestRequest(func(string) {}))
			if err == nil || !strings.Contains(err.Error(), "response header timeout") {
				t.Fatalf("stalled headers: err = %v", err)
			}
			resp, err := p.Complete(context.Background(), streamTestRequest(func(string) {}))
			if err != nil || resp.Text() != "pong" {
				t.Fatalf("stream longer than the header timeout: resp=%+v err=%v", resp, err)
			}
		})
	}
}

// A rejected stream request whose error body stalls is cut by the idle
// timeout and retried like any other rejected request.
func TestProviderStreamRejectedBodyStall(t *testing.T) {
	for _, tc := range streamProviderCases() {
		t.Run(tc.name, func(t *testing.T) {
			_, _, ok, _ := streamRetryFixtures(tc.name)
			var calls atomic.Int32
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) > 1 {
					writeStreamFixture(t, w, ok, nil)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"error":{"message":"slow`)
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
				}
			}))
			defer srv.Close()
			defer close(release)
			start := time.Now()
			resp, err := tc.new(srv.URL, srv.Client()).Complete(context.Background(), streamTestRequest(func(string) {}))
			if err != nil || resp.Text() != "pong" || calls.Load() != 2 {
				t.Fatalf("calls=%d resp=%+v err=%v", calls.Load(), resp, err)
			}
			if time.Since(start) > 5*time.Second {
				t.Fatalf("stalled error body took %s", time.Since(start))
			}
		})
	}
}

func TestProviderStreamErrorScrubsAPIKey(t *testing.T) {
	bodies := map[string]string{
		"anthropic": `event: error` + "\n" + `data: {"type":"error","error":{"type":"invalid_request_error","message":"bad key ` + streamTestKey + `"}}` + "\n\n",
		"responses": `event: error` + "\n" + `data: {"type":"error","code":"invalid_api_key","message":"bad key ` + streamTestKey + `"}` + "\n\n",
	}
	for _, tc := range streamProviderCases() {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, bodies[tc.name])
			}))
			defer srv.Close()
			_, err := tc.new(srv.URL, srv.Client()).Complete(context.Background(), streamTestRequest(func(string) {}))
			if !domain.IsCode(err, domain.CodeProviderHTTP) || strings.Contains(err.Error(), streamTestKey) || !strings.Contains(err.Error(), "bad key") {
				t.Fatalf("err = %v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("calls = %d, want 1: a request error is not retried", calls.Load())
			}
		})
	}
}

// A server that ignores "stream" answers with one JSON body. Its text is
// reported once and the response is the same.
func TestProviderStreamRequestAnsweredWithJSON(t *testing.T) {
	bodies := map[string]string{
		"anthropic": `{"id":"msg_1","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`,
		"responses": `{"id":"resp_1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"pong"}]}],"usage":{"input_tokens":5,"output_tokens":2}}`,
	}
	for _, tc := range streamProviderCases() {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, bodies[tc.name])
			}))
			defer srv.Close()
			var deltas []string
			resp, err := tc.new(srv.URL, srv.Client()).Complete(context.Background(), streamTestRequest(func(d string) { deltas = append(deltas, d) }))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(deltas, []string{"pong"}) || resp.Text() != "pong" || !resp.Complete {
				t.Fatalf("deltas %q response %+v", deltas, resp)
			}
		})
	}
}

// Without a delta callback, and for an oracle consultation, the request is
// the non-streaming one it was before.
func TestProviderDoesNotStreamWithoutDeltaCallback(t *testing.T) {
	bodies := map[string]string{
		"anthropic": `{"id":"msg_1","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn"}`,
		"responses": `{"id":"resp_1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"pong"}]}]}`,
	}
	for _, tc := range streamProviderCases() {
		t.Run(tc.name, func(t *testing.T) {
			var raw []byte
			var accept string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ = io.ReadAll(r.Body)
				accept = r.Header.Get("Accept")
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, bodies[tc.name])
			}))
			defer srv.Close()
			p := tc.new(srv.URL, srv.Client())
			oracle := OracleRequest("which approach?")
			oracle.OnDelta = func(d string) { t.Errorf("oracle text reached the delta callback: %q", d) }
			for name, req := range map[string]Request{"no callback": streamTestRequest(nil), "oracle": oracle} {
				resp, err := p.Complete(context.Background(), req)
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if strings.Contains(string(raw), `"stream"`) || accept != "" || resp.Text() != "pong" {
					t.Fatalf("%s: accept=%q body=%s response=%+v", name, accept, raw, resp)
				}
			}
		})
	}
}

// A compatible server may leave "output" out of the terminal event. The
// items the stream finished one by one are used instead.
func TestOpenAIResponsesStreamWithoutFinalOutput(t *testing.T) {
	var b strings.Builder
	for _, ev := range streamFixtureEvents(t, "responses_reasoning_text_tool.sse") {
		if strings.Contains(ev, `"type":"response.completed"`) {
			ev = "event: response.completed\n" + `data: {"type":"response.completed","response":{"id":"resp_stream_fixture","status":"completed","usage":{"input_tokens":365,"output_tokens":58}}}` + "\n\n"
		}
		b.WriteString(ev)
	}
	raw, err := parseOpenAIResponsesStream(strings.NewReader(b.String()), func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ParseOpenAIResponses(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 4 || resp.Items[0].Kind != domain.ItemOpaque || resp.Text() != "I'll read the file first.\n" ||
		len(resp.ToolCalls()) != 1 || resp.Items[3].Usage.OutputTokens != 58 {
		t.Fatalf("items %+v", resp.Items)
	}
}
