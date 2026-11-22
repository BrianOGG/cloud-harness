package trace

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/storage"
)

func TestSSEIDsOnlyForPersistentEvents(t *testing.T) {
	var buf bytes.Buffer
	ev, err := Persistent("run", "run.completed", map[string]string{"answer": "ok"}, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	ev.Seq = 4
	if err := WriteSSE(&buf, ev); err != nil {
		t.Fatal(err)
	}
	delta := storage.Event{Type: "stream.delta", Payload: []byte(`{"text":"hi"}`), Persistent: false}
	if err := WriteSSE(&buf, delta); err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	if !strings.Contains(body, "id: 4\n") {
		t.Fatalf("missing cursor:\n%s", body)
	}
	if strings.Count(body, "id: ") != 1 {
		t.Fatalf("ephemeral event was given a cursor:\n%s", body)
	}
	if !strings.Contains(body, "event: stream.delta\n") {
		t.Fatalf("delta missing:\n%s", body)
	}
}
