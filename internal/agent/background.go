package agent

import (
	"context"
	"errors"
	"sort"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

type startedKeyType struct{}

var startedKey startedKeyType

// signalStarted reports that a background execution has recorded its run and
// is about to execute it. It does nothing for a caller that runs in place.
func signalStarted(ctx context.Context, run storage.Run) {
	if fn, ok := ctx.Value(startedKey).(func(storage.Run)); ok {
		fn(run)
	}
}

// DeltaStream receives model text while a response is still being generated.
// The text is a transport payload: it is never persisted, and the stored
// response is the record of what the model said.
type DeltaStream interface {
	// Delta delivers the next piece of runID's current model response.
	Delta(runID, text string)
	// End marks the end of that response, whether it completed or failed.
	End(runID string)
}

// deltaSink returns the provider callback that reports runID's model text.
func (m *Manager) deltaSink(runID string) func(string) {
	if m.Stream == nil {
		return nil
	}
	return func(text string) { m.Stream.Delta(runID, text) }
}

func (m *Manager) endDeltas(runID string) {
	if m.Stream != nil {
		m.Stream.End(runID)
	}
}

// Execution is a run that executes in the background. Its lifetime is not
// tied to the context of the caller that started it: only Cancel or Shutdown
// stops it early.
type Execution struct {
	// Run is the run as recorded when execution began.
	Run storage.Run

	done   chan struct{}
	result storage.Run
	err    error
}

// Done is closed when the run stops executing: it finished or is waiting.
func (e *Execution) Done() <-chan struct{} { return e.done }

// Wait blocks until the run stops executing or ctx ends. When ctx ends first
// the run keeps executing.
func (e *Execution) Wait(ctx context.Context) (storage.Run, error) {
	select {
	case <-e.done:
		return e.result, e.err
	case <-ctx.Done():
		return e.Run, ctx.Err()
	}
}

// StartMessage records a run for message and executes it in the background.
// It returns once the run is recorded. A failure before that point, such as an
// unknown session, is returned and no run executes.
func (m *Manager) StartMessage(ctx context.Context, sessionID, message string, opts RunOptions) (*Execution, error) {
	return m.start(ctx, func(ctx context.Context) (storage.Run, error) {
		return m.RunMessageWithOptions(ctx, sessionID, message, opts)
	})
}

// StartResume resumes a waiting run and executes it in the background. It
// returns once the run is RUNNING again. A rejected resume, such as a run that
// is not waiting, is returned and nothing executes.
func (m *Manager) StartResume(ctx context.Context, runID string, in ResumeInput) (*Execution, error) {
	return m.start(ctx, func(ctx context.Context) (storage.Run, error) {
		return m.Resume(ctx, runID, in)
	})
}

func (m *Manager) start(ctx context.Context, execute func(context.Context) (storage.Run, error)) (*Execution, error) {
	m.bgMu.Lock()
	if m.bgClosed {
		m.bgMu.Unlock()
		return nil, domain.E(domain.CodeConflict, "runtime is shutting down")
	}
	m.bgWG.Add(1)
	m.bgMu.Unlock()

	ex := &Execution{done: make(chan struct{})}
	started := make(chan storage.Run, 1)
	var runID string
	runCtx := context.WithValue(context.WithoutCancel(ctx), startedKey, func(run storage.Run) {
		m.bgMu.Lock()
		if m.bgRuns == nil {
			m.bgRuns = map[string]struct{}{}
		}
		runID = run.ID
		m.bgRuns[runID] = struct{}{}
		m.bgMu.Unlock()
		select {
		case started <- run:
		default:
		}
	})
	go func() {
		defer m.bgWG.Done()
		ex.result, ex.err = execute(runCtx)
		m.bgMu.Lock()
		delete(m.bgRuns, runID)
		m.bgMu.Unlock()
		close(ex.done)
	}()

	select {
	case ex.Run = <-started:
		return ex, nil
	case <-ex.done:
	}
	select {
	case ex.Run = <-started:
		return ex, nil
	default:
	}
	// The run stopped before it began to execute.
	if ex.err != nil {
		return nil, ex.err
	}
	ex.Run = ex.result
	return ex, nil
}

// Shutdown refuses new background executions, cancels those still RUNNING,
// and waits for them to unwind or for ctx to end. A cancelled run is recorded
// as CANCELLED before Shutdown waits, so none is left RUNNING.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.bgMu.Lock()
	m.bgClosed = true
	ids := make([]string, 0, len(m.bgRuns))
	for id := range m.bgRuns {
		ids = append(ids, id)
	}
	m.bgMu.Unlock()
	sort.Strings(ids)

	persist := context.WithoutCancel(ctx)
	var errs []error
	for _, id := range ids {
		run, err := m.Store.GetRun(persist, id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if run.Status != domain.RunRunning {
			continue
		}
		if _, err := m.Cancel(persist, id); err != nil {
			// The run may have stopped by itself in the meantime.
			if current, readErr := m.Store.GetRun(persist, id); readErr != nil || current.Status == domain.RunRunning {
				errs = append(errs, err)
			}
		}
	}

	unwound := make(chan struct{})
	go func() {
		m.bgWG.Wait()
		close(unwound)
	}()
	select {
	case <-unwound:
	case <-ctx.Done():
		errs = append(errs, ctx.Err())
	}
	return errors.Join(errs...)
}
