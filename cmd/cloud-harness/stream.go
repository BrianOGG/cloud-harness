package main

import (
	"context"
	"io"
	"strings"
	"sync"
	"unicode"

	"github.com/BrianOGG/cloud-harness/internal/agent"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

// cliStream writes a top-level run's model text to w while the model is still
// producing it. Child runs execute concurrently, so their text is left out
// rather than interleaved.
type cliStream struct {
	w     io.Writer
	store storage.Store

	mu       sync.Mutex
	topLevel map[string]bool
	current  strings.Builder
	midLine  bool
	last     string
}

// terminalText returns model text with control characters removed, so it can
// only add characters to the terminal: no escape sequence, carriage return or
// backspace reaches it to move the cursor, recolor or overwrite what is on
// screen. Newlines and tabs stay. Dropping one character at a time gives the
// same result however the text is split into deltas.
func terminalText(text string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, text)
}

func newCLIStream(w io.Writer, store storage.Store) *cliStream {
	return &cliStream{w: w, store: store, topLevel: map[string]bool{}}
}

func (s *cliStream) Delta(runID, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.isTopLevel(runID) {
		return
	}
	s.current.WriteString(text)
	shown := terminalText(text)
	if shown == "" {
		return
	}
	s.midLine = !strings.HasSuffix(shown, "\n")
	_, _ = io.WriteString(s.w, shown)
}

// End closes the line of the response that just ended. The text of a response
// that failed stays on screen, because a terminal cannot take it back.
func (s *cliStream) End(runID string, completed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.isTopLevel(runID) {
		return
	}
	text := s.current.String()
	s.current.Reset()
	// What the caller may skip printing is the text of this response only. A
	// response that streamed nothing leaves nothing to skip, whatever an
	// earlier response wrote.
	s.last = ""
	if text == "" {
		return
	}
	if s.midLine {
		_, _ = io.WriteString(s.w, "\n")
		s.midLine = false
	}
	if completed {
		s.last = text
	}
}

// showed reports whether answer is the response that was streamed last, so
// the caller does not print it a second time.
func (s *cliStream) showed(answer string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last != "" && strings.TrimSpace(s.last) == strings.TrimSpace(answer)
}

func (s *cliStream) isTopLevel(runID string) bool {
	top, known := s.topLevel[runID]
	if !known {
		run, err := s.store.GetRun(context.Background(), runID)
		if err != nil {
			// Not cached: the final answer is still printed when the run ends.
			return false
		}
		top = run.ParentRunID == ""
		s.topLevel[runID] = top
	}
	return top
}

// answerStreamed reports whether mgr's CLI stream already wrote answer.
func answerStreamed(mgr *agent.Manager, answer string) bool {
	s, ok := mgr.Stream.(*cliStream)
	return ok && s.showed(answer)
}
