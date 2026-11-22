package transport

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/agent"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/storage"
	"github.com/BrianOGG/cloud-harness/internal/storage/sqlite"
)

// gatedProvider streams one text response a chunk at a time, each chunk only
// when the test asks for it, and returns only when the test releases it.
type gatedProvider struct {
	chunks  []string
	fail    error
	started chan struct{}
	step    chan struct{}
	release chan struct{}
}

func newGatedProvider(chunks ...string) *gatedProvider {
	return &gatedProvider{
		chunks: chunks, started: make(chan struct{}, 1),
		step: make(chan struct{}), release: make(chan struct{}),
	}
}

func (p *gatedProvider) Name() string { return "gated" }

func (p *gatedProvider) Complete(ctx context.Context, req provider.Request) (domain.ModelResponse, error) {
	select {
	case p.started <- struct{}{}:
	default:
	}
	for _, chunk := range p.chunks {
		select {
		case <-p.step:
		case <-ctx.Done():
			return domain.ModelResponse{}, ctx.Err()
		}
		if req.OnDelta != nil {
			req.OnDelta(chunk)
		}
	}
	select {
	case <-p.release:
	case <-ctx.Done():
		return domain.ModelResponse{}, ctx.Err()
	}
	if p.fail != nil {
		return domain.ModelResponse{}, p.fail
	}
	return domain.ModelResponse{
		Provider: "gated", ProviderResponseID: "gated_resp", Complete: true,
		Items: []domain.ResponseItem{{Kind: domain.ItemText, Text: strings.Join(p.chunks, "")}},
	}, nil
}

func (p *gatedProvider) emit(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case p.step <- struct{}{}:
		case <-time.After(5 * time.Second):
			t.Fatal("provider did not take the next chunk")
		}
	}
}

type streamFixture struct {
	db      *sqlite.DB
	path    string
	manager *agent.Manager
	base    string
	session string
}

// newStreamFixture serves a manager over real HTTP. The poll interval is an
// hour, so a subscriber that sees an event was woken by a notification.
func newStreamFixture(t *testing.T, p *gatedProvider) *streamFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "h.db")
	db, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	m := &agent.Manager{Store: db, Provider: p, OwnerID: "api", SkipPlanner: true, DisableRepoMap: true, DisableSnapshots: true}
	ts := httptest.NewServer((&Server{Manager: m, PollInterval: time.Hour}).Handler())
	t.Cleanup(func() {
		ts.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Shutdown(ctx)
		_ = db.Close()
	})
	session, err := m.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return &streamFixture{db: db, path: path, manager: m, base: ts.URL, session: session.ID}
}

func (f *streamFixture) createRun(t *testing.T, query string) view {
	t.Helper()
	resp, err := http.Post(f.base+"/v1/sessions/"+f.session+"/runs"+query, "application/json", strings.NewReader(`{"message":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create run: status %d", resp.StatusCode)
	}
	var out view
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *streamFixture) waitStatus(t *testing.T, runID string, want domain.RunStatus) storage.Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		run, err := f.db.GetRun(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == want {
			return run
		}
		if time.Now().After(deadline) {
			t.Fatalf("run is %s, want %s", run.Status, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type sseEvent struct {
	id, typ, data string
}

// sseStream reads one event subscription. Events arrive on ch; ch closes when
// the server ends the stream or the test closes it.
type sseStream struct {
	ch     chan sseEvent
	cancel context.CancelFunc
}

func (f *streamFixture) subscribe(t *testing.T, runID, lastEventID string) *sseStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.base+"/v1/runs/"+runID+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("subscribe: status %d", resp.StatusCode)
	}
	s := &sseStream{ch: make(chan sseEvent, 64), cancel: cancel}
	t.Cleanup(cancel)
	go func() {
		defer close(s.ch)
		defer resp.Body.Close()
		reader := bufio.NewReader(resp.Body)
		var ev sseEvent
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\n")
			switch {
			case strings.HasPrefix(line, "id: "):
				ev.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				ev.typ = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			case line == "" && ev.typ != "":
				s.ch <- ev
				ev = sseEvent{}
			}
		}
	}()
	return s
}

// next returns the next event, or ok=false when the stream has ended.
func (s *sseStream) next(t *testing.T) (sseEvent, bool) {
	t.Helper()
	select {
	case ev, ok := <-s.ch:
		return ev, ok
	case <-time.After(5 * time.Second):
		t.Fatal("no event arrived on the stream")
		return sseEvent{}, false
	}
}

func deltaText(t *testing.T, ev sseEvent) string {
	t.Helper()
	if ev.id != "" {
		t.Fatalf("stream.delta carries cursor %q", ev.id)
	}
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(ev.data), &payload); err != nil {
		t.Fatalf("delta payload %q: %v", ev.data, err)
	}
	return payload.Text
}

// A run used to execute on its request's context, so a client that hung up
// cancelled it. The run now outlives the request, even one that asked to wait.
func TestRunKeepsRunningAfterClientDisconnects(t *testing.T) {
	p := newGatedProvider("still ", "here")
	f := newStreamFixture(t, p)

	ctx, hangUp := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.base+"/v1/sessions/"+f.session+"/runs?wait=1", strings.NewReader(`{"message":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	clientErr := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		clientErr <- err
	}()
	select {
	case <-p.started:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not reach the provider")
	}
	hangUp()
	if err := <-clientErr; err == nil {
		t.Fatal("request returned before the run stopped; the test did not disconnect mid-run")
	}

	runs, err := f.db.ListRuns(context.Background(), f.session)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %+v, err = %v", runs, err)
	}
	// Give a cancellation, if one were wired to the request, time to land.
	time.Sleep(100 * time.Millisecond)
	if run := f.waitStatus(t, runs[0].ID, domain.RunRunning); run.Status != domain.RunRunning {
		t.Fatalf("run is %s after the client disconnected", run.Status)
	}

	p.emit(t, 2)
	close(p.release)
	done := f.waitStatus(t, runs[0].ID, domain.RunCompleted)
	if done.FinalAnswer != "still here" {
		t.Fatalf("final answer = %q", done.FinalAnswer)
	}
}

// Model text reaches a subscriber as it is produced, in order, without a
// cursor, and never lands in the event log. A subscriber that drops and
// reconnects with Last-Event-ID gets every persisted event exactly once.
func TestDeltasStreamInOrderAndReconnectLosesNoEvent(t *testing.T) {
	chunks := []string{"The ", "quick ", "brown ", "fox ", "jumps."}
	p := newGatedProvider(chunks...)
	f := newStreamFixture(t, p)

	created := f.createRun(t, "?wait=0")
	if created.Status != domain.RunRunning {
		t.Fatalf("wait=0 returned a run that is %s", created.Status)
	}
	// The first chunk is produced before anyone listens.
	p.emit(t, 1)

	first := f.subscribe(t, created.ID, "")
	var seen []string
	var streamed string
	for streamed == "" {
		ev, ok := first.next(t)
		if !ok {
			t.Fatal("stream ended while the run was still running")
		}
		if ev.typ == "stream.delta" {
			streamed += deltaText(t, ev)
			continue
		}
		seen = append(seen, ev.id)
	}
	if len(seen) == 0 {
		t.Fatal("no persisted event was replayed before the first delta")
	}
	for _, chunk := range chunks[1:3] {
		p.emit(t, 1)
		ev, ok := first.next(t)
		if !ok || ev.typ != "stream.delta" {
			t.Fatalf("expected a delta, got %+v (open=%v)", ev, ok)
		}
		if got := deltaText(t, ev); got != chunk {
			t.Fatalf("delta = %q, want %q", got, chunk)
		}
		streamed += chunk
	}
	if want := strings.Join(chunks[:3], ""); streamed != want {
		t.Fatalf("streamed %q, want %q", streamed, want)
	}

	// Drop the connection, let the run finish, then resume from the cursor.
	first.cancel()
	p.emit(t, 2)
	close(p.release)
	done := f.waitStatus(t, created.ID, domain.RunCompleted)
	if want := strings.Join(chunks, ""); done.FinalAnswer != want {
		t.Fatalf("final answer = %q, want %q", done.FinalAnswer, want)
	}

	second := f.subscribe(t, created.ID, seen[len(seen)-1])
	var types []string
	for {
		ev, ok := second.next(t)
		if !ok {
			break
		}
		if ev.typ == "stream.delta" {
			t.Fatalf("a finished run replayed a delta: %+v", ev)
		}
		seen = append(seen, ev.id)
		types = append(types, ev.typ)
	}
	for _, want := range []string{"model.completed", "run.completed"} {
		if !strings.Contains(strings.Join(types, " "), want) {
			t.Fatalf("reconnect missed %s: %v", want, types)
		}
	}

	stored, err := f.db.ListEvents(context.Background(), created.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != len(seen) {
		t.Fatalf("store has %d events, the two connections saw %d: %v", len(stored), len(seen), seen)
	}
	for i, ev := range stored {
		if ev.Type == "stream.delta" {
			t.Fatal("a delta was persisted")
		}
		if want := seen[i]; want != jsonNumber(ev.Seq) {
			t.Fatalf("event %d: saw cursor %s, store has %d", i, want, ev.Seq)
		}
	}
}

// A subscriber is told about a new event when it commits. Before, it found
// out by reading the store every 25ms.
func TestSubscriberWakesOnCommitWithoutPolling(t *testing.T) {
	p := newGatedProvider("done")
	f := newStreamFixture(t, p)
	created := f.createRun(t, "?wait=0")
	sub := f.subscribe(t, created.ID, "")
	if ev, ok := sub.next(t); !ok || ev.typ != "run.created" {
		t.Fatalf("first event = %+v", ev)
	}
	p.emit(t, 1)
	close(p.release)
	completed := false
	for {
		ev, ok := sub.next(t)
		if !ok {
			break
		}
		completed = completed || ev.typ == "run.completed"
	}
	if !completed {
		t.Fatal("stream ended without run.completed with polling disabled")
	}
}

// Text of a response that then fails never reaches the event log, so the
// subscriber is told to discard it.
func TestFailedResponseResetsStreamedText(t *testing.T) {
	p := newGatedProvider("half an ans")
	p.fail = errors.New("upstream closed the stream")
	f := newStreamFixture(t, p)
	created := f.createRun(t, "?wait=0")
	sub := f.subscribe(t, created.ID, "")
	p.emit(t, 1)
	close(p.release)

	var live []string
	for {
		ev, ok := sub.next(t)
		if !ok {
			break
		}
		if ev.id == "" {
			live = append(live, ev.typ)
		}
	}
	if got := strings.Join(live, " "); got != "stream.delta stream.reset" {
		t.Fatalf("unstored events = %q, want a delta then a reset", got)
	}
	f.waitStatus(t, created.ID, domain.RunFailed)
}

// Without ?wait= the request answers once the run has stopped executing, as
// it always has. An unreadable value is refused before any run is created.
func TestWaitParameter(t *testing.T) {
	p := newGatedProvider("done")
	f := newStreamFixture(t, p)
	go func() {
		<-p.started
		p.step <- struct{}{}
		close(p.release)
	}()
	if got := f.createRun(t, ""); got.Status != domain.RunCompleted || got.FinalAnswer != "done" {
		t.Fatalf("default create returned %s %q", got.Status, got.FinalAnswer)
	}

	resp, err := http.Post(f.base+"/v1/sessions/"+f.session+"/runs?wait=later", "application/json", strings.NewReader(`{"message":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("wait=later: status %d", resp.StatusCode)
	}
	runs, err := f.db.ListRuns(context.Background(), f.session)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %d, err = %v", len(runs), err)
	}
}

// Shutdown is what serve runs on SIGINT and SIGTERM: a run still executing is
// recorded as CANCELLED, and no new run is accepted.
func TestShutdownCancelsExecutingRuns(t *testing.T) {
	p := newGatedProvider("never ", "finished")
	f := newStreamFixture(t, p)
	created := f.createRun(t, "?wait=0")
	sub := f.subscribe(t, created.ID, "")
	p.emit(t, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.manager.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	run, err := f.db.GetRun(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != domain.RunCancelled {
		t.Fatalf("run is %s after shutdown", run.Status)
	}
	var last string
	for {
		ev, ok := sub.next(t)
		if !ok {
			break
		}
		if ev.id != "" {
			last = ev.typ
		}
	}
	if last != "run.cancelled" {
		t.Fatalf("subscriber's last persisted event = %q", last)
	}

	resp, err := http.Post(f.base+"/v1/sessions/"+f.session+"/runs", "application/json", strings.NewReader(`{"message":"late"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("run created during shutdown: status %d", resp.StatusCode)
	}
}

func jsonNumber(n int64) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

// seedRun stores a RUNNING run that nothing executes, for events to hang on.
func (f *streamFixture) seedRun(t *testing.T) string {
	t.Helper()
	ctx, now := context.Background(), time.Now()
	turn := storage.Turn{ID: domain.NewID("trn_"), SessionID: f.session, UserInput: "hello", Status: "completed", CreatedAt: now}
	if err := f.db.CreateTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	run := storage.Run{ID: domain.NewID("run_"), SessionID: f.session, TurnID: turn.ID, Status: domain.RunRunning, Version: 1, CreatedAt: now, UpdatedAt: now}
	if err := f.db.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	return run.ID
}

// liveOutput lists what a liveStream wrote: an event type, or "delta:" and
// the text.
func liveOutput(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	var out []string
	for _, block := range strings.Split(strings.TrimSpace(rec.Body.String()), "\n\n") {
		var typ, data string
		for _, line := range strings.Split(block, "\n") {
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				typ = v
			}
			if v, ok := strings.CutPrefix(line, "data: "); ok {
				data = v
			}
		}
		if typ == "stream.delta" {
			var body struct{ Text string }
			if err := json.Unmarshal([]byte(data), &body); err != nil {
				t.Fatal(err)
			}
			typ = "delta:" + body.Text
		}
		if typ != "" {
			out = append(out, typ)
		}
	}
	return out
}

// A subscriber that falls behind finds two finished responses in its queue
// while the store already holds every event. Each response's text must still
// be written before the event that completes it, on both the wake-up and the
// fallback path.
func TestQueuedTextKeepsItsPlaceAmongStoredEvents(t *testing.T) {
	want := []string{"model.started", "delta:first", "model.completed", "model.started", "delta:second", "model.completed"}
	paths := map[string]func(*liveStream) error{
		"wake": func(l *liveStream) error {
			_, _, err := l.writeQueued(context.Background())
			return err
		},
		"fallback": func(l *liveStream) error { return l.catchUp(context.Background()) },
	}
	for name, send := range paths {
		t.Run(name, func(t *testing.T) {
			f := newStreamFixture(t, newGatedProvider())
			srv := &Server{Manager: f.manager}
			hub := srv.stream()
			runID := f.seedRun(t)
			sub := hub.Subscribe(runID)
			defer sub.Close()
			commit := func(typ string) {
				t.Helper()
				if _, err := f.db.AppendEvent(context.Background(), storage.Event{RunID: runID, Type: typ, Persistent: true, CreatedAt: time.Now()}); err != nil {
					t.Fatal(err)
				}
			}
			for _, text := range []string{"first", "second"} {
				commit("model.started")
				hub.Delta(runID, text)
				hub.End(runID, true)
				commit("model.completed")
			}

			rec := httptest.NewRecorder()
			live := &liveStream{store: f.db, w: rec, sub: sub, runID: runID}
			if err := send(live); err != nil {
				t.Fatal(err)
			}
			if got := liveOutput(t, rec); strings.Join(got, " ") != strings.Join(want, " ") {
				t.Fatalf("wrote %v\nwant  %v", got, want)
			}
			if live.after != 4 {
				t.Fatalf("after = %d", live.after)
			}
		})
	}
}

// Another process, such as `runs resume`, commits through its own handle, so
// the hub never hears of it. The fallback read finds the event.
func TestFallbackReadFindsEventsFromAnotherProcess(t *testing.T) {
	f := newStreamFixture(t, newGatedProvider())
	srv := &Server{Manager: f.manager}
	runID := f.seedRun(t)
	sub := srv.stream().Subscribe(runID)
	defer sub.Close()
	other, err := sqlite.Open(context.Background(), f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.AppendEvent(context.Background(), storage.Event{RunID: runID, Type: "run.resumed", Persistent: true, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	live := &liveStream{store: f.db, w: rec, sub: sub, runID: runID}
	if committed, _, err := live.writeQueued(context.Background()); err != nil || committed || rec.Body.Len() != 0 {
		t.Fatalf("queue held a commit from another process: %v %v %q", committed, err, rec.Body.String())
	}
	if err := live.catchUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := liveOutput(t, rec); len(got) != 1 || got[0] != "run.resumed" {
		t.Fatalf("wrote %v", got)
	}
}
