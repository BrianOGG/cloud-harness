package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
)

// streamingProvider reports each chunk through OnDelta before it returns the
// whole text, the way the OpenAI-compatible provider does.
func streamingProvider(chunks ...string) *scriptedProvider {
	return &scriptedProvider{step: func(_ int, req provider.Request) (domain.ModelResponse, error) {
		for _, chunk := range chunks {
			if req.OnDelta != nil {
				req.OnDelta(chunk)
			}
		}
		return domain.ModelResponse{
			Provider: "mock", Complete: true,
			Items: []domain.ResponseItem{{Kind: domain.ItemText, Text: strings.Join(chunks, "")}},
		}, nil
	}}
}

// orderWriter records each write, so a test can tell text that was written
// piece by piece from text that was printed in one go.
type orderWriter struct{ writes []string }

func (w *orderWriter) Write(p []byte) (int, error) {
	w.writes = append(w.writes, string(p))
	return len(p), nil
}

func TestChatStreamPrintsTextAsItArrivesAndOnlyOnce(t *testing.T) {
	mgr, _, _ := newChatTestManager(t, streamingProvider("Hel", "lo ", "there"))
	out := &orderWriter{}
	mgr.Stream = newCLIStream(out, mgr.Store)
	var errBuf bytes.Buffer
	if code := runChat(context.Background(), mgr, "", "hi", nil, out, &errBuf); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errBuf.String())
	}
	want := []string{"Hel", "lo ", "there", "\n"}
	if strings.Join(out.writes, "|") != strings.Join(want, "|") {
		t.Fatalf("stdout writes = %q, want %q", out.writes, want)
	}
}

func TestChatWithoutStreamPrintsOnlyTheFinalAnswer(t *testing.T) {
	mgr, _, _ := newChatTestManager(t, streamingProvider("Hel", "lo ", "there"))
	out := &orderWriter{}
	var errBuf bytes.Buffer
	if code := runChat(context.Background(), mgr, "", "hi", nil, out, &errBuf); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errBuf.String())
	}
	if got := strings.Join(out.writes, ""); got != "Hello there\n" || len(out.writes) != 1 {
		t.Fatalf("stdout writes = %q", out.writes)
	}
}

// A turn whose answer was not streamed must print it, even when an earlier
// turn streamed the same words.
func TestChatPrintsUnstreamedAnswerThatRepeatsAnEarlierOne(t *testing.T) {
	prov := &scriptedProvider{step: func(call int, req provider.Request) (domain.ModelResponse, error) {
		if call == 1 {
			req.OnDelta("Yes.")
		}
		// The second turn's response arrives without any streamed text.
		return domain.ModelResponse{
			Provider: "mock", Complete: true,
			Items: []domain.ResponseItem{{Kind: domain.ItemText, Text: "Yes."}},
		}, nil
	}}
	mgr, _, _ := newChatTestManager(t, prov)
	var out, errBuf bytes.Buffer
	mgr.Stream = newCLIStream(&out, mgr.Store)
	session, err := mgr.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for turn := 1; turn <= 2; turn++ {
		if _, code, _ := runChatTurn(context.Background(), mgr, session.ID, "is it?", nil, &out, &errBuf); code != 0 {
			t.Fatalf("turn %d: code=%d stderr=%q", turn, code, errBuf.String())
		}
	}
	if out.String() != "Yes.\nYes.\n" {
		t.Fatalf("stdout = %q", out.String())
	}
}

// A response that fails after streaming some text leaves that text on screen;
// the answer the run finally gives is still printed.
func TestChatStreamPrintsFinalAnswerThatWasNotStreamed(t *testing.T) {
	prov := &scriptedProvider{step: func(call int, req provider.Request) (domain.ModelResponse, error) {
		if call == 1 {
			req.OnDelta("half an ans")
			return domain.ModelResponse{}, errors.New("upstream closed the stream")
		}
		return domain.ModelResponse{}, errors.New("unexpected second call")
	}}
	mgr, _, _ := newChatTestManager(t, prov)
	var out, errBuf bytes.Buffer
	mgr.Stream = newCLIStream(&out, mgr.Store)
	code := runChat(context.Background(), mgr, "", "hi", nil, &out, &errBuf)
	if code == 0 {
		t.Fatalf("a failed run exited 0: stdout=%q", out.String())
	}
	if !strings.HasPrefix(out.String(), "half an ans\n") {
		t.Fatalf("stdout = %q", out.String())
	}
}

// chat streams unless it is told not to. Either way the answer is on stdout
// once.
func TestChatStreamsByDefault(t *testing.T) {
	setDiagnosticEnv(t, "", "")
	for _, tc := range []struct {
		name   string
		extra  []string
		writes []string
	}{
		{name: "default", writes: []string{"Mock ", "answer: ", "say ", "hello", "\n"}},
		{name: "stream=false", extra: []string{"--stream=false"}, writes: []string{"Mock answer: say hello\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"chat", "--workspace", t.TempDir(), "--db", filepath.Join(t.TempDir(), "h.db"), "--provider", "mock", "--message", "say hello"}
			out := &orderWriter{}
			var errBuf bytes.Buffer
			if code := Main(append(args, tc.extra...), out, &errBuf); code != 0 {
				t.Fatalf("code=%d stderr=%q", code, errBuf.String())
			}
			if strings.Join(out.writes, "|") != strings.Join(tc.writes, "|") {
				t.Fatalf("stdout writes = %q, want %q", out.writes, tc.writes)
			}
		})
	}
}

func TestRunStreamWritesTextToStderrAndKeepsStdoutJSON(t *testing.T) {
	setDiagnosticEnv(t, "", "")
	args := []string{"run", "--workspace", t.TempDir(), "--db", filepath.Join(t.TempDir(), "h.db"), "--provider", "mock", "--message", "say hello"}
	for _, tc := range []struct {
		name   string
		extra  []string
		stderr string
	}{
		{name: "default", stderr: ""},
		{name: "stream", extra: []string{"--stream"}, stderr: "Mock answer: say hello\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errBuf bytes.Buffer
			code := Main(append(append([]string{}, args...), tc.extra...), &out, &errBuf)
			var view struct {
				Status      domain.RunStatus `json:"status"`
				FinalAnswer string           `json:"final_answer"`
			}
			if err := json.Unmarshal(out.Bytes(), &view); code != 0 || err != nil {
				t.Fatalf("code=%d err=%v stdout=%q stderr=%q", code, err, out.String(), errBuf.String())
			}
			if view.Status != domain.RunCompleted || view.FinalAnswer != "Mock answer: say hello" {
				t.Fatalf("view = %+v", view)
			}
			if strings.Count(out.String(), "\n") != 1 {
				t.Fatalf("stdout is not one JSON line: %q", out.String())
			}
			if errBuf.String() != tc.stderr {
				t.Fatalf("stderr = %q, want %q", errBuf.String(), tc.stderr)
			}
		})
	}
}
