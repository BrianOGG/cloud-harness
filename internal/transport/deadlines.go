package transport

import (
	"net/http"
	"time"
)

// holdOpen lifts the connection's read and write deadlines for a handler
// that outlasts them: a run executes inside its request, and an event stream
// stays open. The returned function arms a fresh write deadline; call it
// before writing again.
func (s *Server) holdOpen(w http.ResponseWriter) (rearm func()) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	return func() { _ = rc.SetWriteDeadline(time.Now().Add(s.writeTimeout())) }
}

// streamWriter gives every write of a long-lived response its own write
// deadline, so the stream has no total time limit but a client that stops
// reading is disconnected.
type streamWriter struct {
	http.ResponseWriter
	rearm func()
}

func (w *streamWriter) Write(p []byte) (int, error) {
	w.rearm()
	return w.ResponseWriter.Write(p)
}

func (w *streamWriter) Flush() {
	w.rearm()
	flushSSE(w.ResponseWriter)
}

func (w *streamWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
