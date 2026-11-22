package agent

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
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
