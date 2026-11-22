package agent

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/hooks"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

// blockingProvider answers each call from respond. A call for which respond
// returns nil blocks until its context is cancelled.
type blockingProvider struct {
	respond func(call int) *domain.ModelResponse

	mu      sync.Mutex
	calls   int
	blocked chan struct{}
}

func newBlockingProvider(respond func(call int) *domain.ModelResponse) *blockingProvider {
	return &blockingProvider{respond: respond, blocked: make(chan struct{}, 8)}
}

func (p *blockingProvider) Name() string { return "blocking" }

func (p *blockingProvider) Complete(ctx context.Context, _ provider.Request) (domain.ModelResponse, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	if p.respond != nil {
		if resp := p.respond(call); resp != nil {
			return *resp, nil
		}
	}
	p.blocked <- struct{}{}
	<-ctx.Done()
	return domain.ModelResponse{}, ctx.Err()
}

func backgroundManager(t *testing.T, prov provider.Provider) (*Manager, storage.Session) {
	t.Helper()
	isolatedPolicyConfig(t)
	m, _, _ := testManager(t, nil)
	m.Provider = prov
	m.DisableSnapshots = true
	session, err := m.CreateSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return m, session
}

func waitDone(t *testing.T, ex *Execution) {
	t.Helper()
	select {
	case <-ex.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("execution did not stop")
	}
}

func statusOf(t *testing.T, m *Manager, runID string) storage.Run {
	t.Helper()
	run, err := m.Store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// A caller learns the run id when StartMessage returns. A cancel sent right
// then must reach the execution, not only the stored status.
func TestCancelRightAfterStartStopsExecution(t *testing.T) {
	m, session := backgroundManager(t, newBlockingProvider(nil))
	ex, err := m.StartMessage(t.Context(), session.ID, "hello", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Cancel(t.Context(), ex.Run.ID); err != nil {
		t.Fatal(err)
	}
	waitDone(t, ex)
	if got := statusOf(t, m, ex.Run.ID); got.Status != domain.RunCancelled {
		t.Fatalf("status = %s", got.Status)
	}
}

// Nobody is waiting on a background execution to read its error, so an error
// that left the run RUNNING is recorded on the run.
func TestBackgroundExecutionErrorFailsRun(t *testing.T) {
	malformed := toolResp(domain.ToolCall{ProviderCallID: "c1", Name: "read_file", Arguments: json.RawMessage(`{"path":`)})
	m, session := backgroundManager(t, newBlockingProvider(func(int) *domain.ModelResponse { return &malformed }))
	ex, err := m.StartMessage(t.Context(), session.ID, "hello", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, ex)
	if _, err := ex.Wait(t.Context()); err == nil {
		t.Fatal("execution reported no error")
	}
	got := statusOf(t, m, ex.Run.ID)
	if got.Status != domain.RunFailed || got.Error == nil {
		t.Fatalf("status = %s, error = %v", got.Status, got.Error)
	}
}

// Stop hooks run before every terminal transition, including this one.
func TestBackgroundExecutionErrorRunsStopHooks(t *testing.T) {
	malformed := toolResp(domain.ToolCall{ProviderCallID: "c1", Name: "read_file", Arguments: json.RawMessage(`{"path":`)})
	m, session := backgroundManager(t, newBlockingProvider(func(int) *domain.ModelResponse { return &malformed }))
	putAgentHooks(t, m.Workspace, map[hooks.Point][]hooks.Action{hooks.Stop: {{Type: "command", Command: "echo stopped"}}}, false)
	ex, err := m.StartMessage(t.Context(), session.ID, "hello", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, ex)
	if got := statusOf(t, m, ex.Run.ID); got.Status != domain.RunFailed {
		t.Fatalf("status = %s", got.Status)
	}
	events := hookEvents(t, m.Store, ex.Run.ID)
	if len(events) != 1 || events[0].Point != hooks.Stop || events[0].ExitCode != 0 {
		t.Fatalf("hook events = %+v", events)
	}
}

// Cancel runs Stop hooks before it records CANCELLED, so the execution can
// return its context error while the run is still RUNNING. That is a cancel
// in progress, not a failure.
func TestCancelWithSlowStopHookEndsCancelled(t *testing.T) {
	prov := newBlockingProvider(nil)
	m, session := backgroundManager(t, prov)
	putAgentHooks(t, m.Workspace, map[hooks.Point][]hooks.Action{hooks.Stop: {{Type: "command", Command: "sleep 0.3"}}}, false)
	ex, err := m.StartMessage(t.Context(), session.ID, "hello", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-prov.blocked
	cancelled, err := m.Cancel(t.Context(), ex.Run.ID)
	if err != nil || cancelled.Status != domain.RunCancelled {
		t.Fatalf("cancel: %s, %v", cancelled.Status, err)
	}
	waitDone(t, ex)
	if got := statusOf(t, m, ex.Run.ID); got.Status != domain.RunCancelled {
		t.Fatalf("status = %s", got.Status)
	}
}

// While a parent delegates it is WAITING(CHILD) and its child is RUNNING
// inside the parent's execution. Shutdown stops both.
func TestShutdownCancelsParentWaitingOnChild(t *testing.T) {
	delegate := toolResp(domain.ToolCall{ProviderCallID: "d1", Name: domain.ToolDelegateTask, Arguments: json.RawMessage(`{"goal":"look around"}`)})
	prov := newBlockingProvider(func(call int) *domain.ModelResponse {
		if call == 1 {
			return &delegate
		}
		return nil
	})
	m, session := backgroundManager(t, prov)
	ex, err := m.StartMessage(t.Context(), session.ID, "hello", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-prov.blocked:
	case <-time.After(10 * time.Second):
		t.Fatal("child did not reach the model")
	}
	if got := statusOf(t, m, ex.Run.ID); got.Status != domain.RunWaiting || got.WaitReason != domain.WaitChild {
		t.Fatalf("parent = %s(%s)", got.Status, got.WaitReason)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	runs, err := m.Store.ListRuns(context.Background(), session.ID)
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs = %d, err = %v", len(runs), err)
	}
	for _, run := range runs {
		if run.Status != domain.RunCancelled {
			t.Errorf("run %s (parent %q) = %s(%s)", run.ID, run.ParentRunID, run.Status, run.WaitReason)
		}
	}
}

// A run waiting for a person is not executing. Shutdown leaves it resumable.
func TestShutdownLeavesWaitingRunResumable(t *testing.T) {
	ask := toolResp(domain.ToolCall{ProviderCallID: "a1", Name: domain.ToolAskUser, Arguments: json.RawMessage(`{"question":"continue?"}`)})
	m, session := backgroundManager(t, newBlockingProvider(func(int) *domain.ModelResponse { return &ask }))
	ex, err := m.StartMessage(t.Context(), session.ID, "hello", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, ex)
	if err := m.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, m, ex.Run.ID); got.Status != domain.RunWaiting || got.WaitReason != domain.WaitUser {
		t.Fatalf("run = %s(%s)", got.Status, got.WaitReason)
	}
}

// Each Stop hook here exits only once both have started, which a Shutdown
// that cancels one run at a time never lets happen.
func TestShutdownCancelsRunsTogether(t *testing.T) {
	prov := newBlockingProvider(nil)
	m, session := backgroundManager(t, prov)
	both := `touch "stop-$$"; for i in $(seq 100); do [ "$(ls stop-* | wc -l)" -ge 2 ] && exit 0; sleep 0.05; done; exit 1`
	putAgentHooks(t, m.Workspace, map[hooks.Point][]hooks.Action{hooks.Stop: {{Type: "command", Command: both}}}, false)
	second, err := m.CreateSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var runs []*Execution
	for _, id := range []string{session.ID, second.ID} {
		ex, err := m.StartMessage(t.Context(), id, "hello", RunOptions{})
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, ex)
		<-prov.blocked
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := m.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	for _, ex := range runs {
		if got := statusOf(t, m, ex.Run.ID); got.Status != domain.RunCancelled {
			t.Errorf("run %s = %s", ex.Run.ID, got.Status)
		}
		var passed bool
		for _, ev := range hookEvents(t, m.Store, ex.Run.ID) {
			passed = passed || (ev.Point == hooks.Stop && ev.ExitCode == 0)
		}
		if !passed {
			t.Errorf("run %s: Stop hook did not see the other run's hook: %+v", ex.Run.ID, hookEvents(t, m.Store, ex.Run.ID))
		}
	}
}
