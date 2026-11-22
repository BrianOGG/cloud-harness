// Package trace separates durable run facts from disposable stream deltas.
// Only persistent events receive a cursor. Deltas are written to the response
// and then dropped.
package trace

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/storage"
)

// Persistent builds a durable event. The store assigns the sequence.
func Persistent(runID, typ string, payload any, now time.Time) (storage.Event, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return storage.Event{}, err
	}
	return storage.Event{RunID: runID, Type: typ, Payload: raw, CreatedAt: now, Persistent: true}, nil
}

// WriteSSE writes one Server-Sent Event. Persistent events include an id so a
// client can resume with Last-Event-ID. Ephemeral events have no id and must
// not be replayed as facts.
func WriteSSE(w io.Writer, ev storage.Event) error {
	if ev.Persistent && ev.Seq > 0 {
		if _, err := fmt.Fprintf(w, "id: %d\n", ev.Seq); err != nil {
			return err
		}
	}
	payload := ev.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, payload)
	return err
}
