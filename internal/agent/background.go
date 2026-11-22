package agent

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

type startedKeyType struct{}

var startedKey startedKeyType

// signalStarted reports that a background execution has recorded its run and
// is about to execute it. It does nothing for a caller that runs in place.
func signalStarted(ctx context.Context, run storage.Run) {
	if fn, ok := ctx.Value(startedKey).(func(context.Context, storage.Run)); ok {
		fn(ctx, run)
	}
}

// DeltaStream receives model text while a response is still being generated.
// The text is a transport payload: it is never persisted, and the stored
// response is the record of what the model said.
type DeltaStream interface {
	// Delta delivers the next piece of runID's current model response.
	Delta(runID, text string)
	// End marks the end of that response. completed is false when the
	// response failed, so its text will not appear in the stored events.
	End(runID string, completed bool)
}

// deltaSink returns the provider callback that reports runID's model text.
func (m *Manager) deltaSink(runID string) func(string) {
	if m.Stream == nil {
		return nil
	}
	return func(text string) { m.Stream.Delta(runID, text) }
}

func (m *Manager) endDeltas(runID string, completed bool) {
	if m.Stream != nil {
		m.Stream.End(runID, completed)
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
	// stopped is set when Cancel stops this execution. Cancel then records
	// the run as CANCELLED, possibly after the execution has returned.
	var stopped atomic.Bool
	runCtx := context.WithValue(context.WithoutCancel(ctx), startedKey, func(execCtx context.Context, run storage.Run) {
		m.bgMu.Lock()
		if m.bgRuns == nil {
			m.bgRuns = map[string]struct{}{}
		}
		runID = run.ID
		m.bgRuns[runID] = struct{}{}
		m.bgMu.Unlock()
		m.onStop(runID, func() { stopped.Store(true) })
		if execCtx.Err() != nil {
			stopped.Store(true)
		}
		select {
		case started <- run:
		default:
		}
	})
	go func() {
		defer m.bgWG.Done()
		ex.result, ex.err = execute(runCtx)
		if ex.err != nil && runID != "" && !stopped.Load() {
			if failed, ok := m.failStranded(runCtx, runID, ex.err); ok {
				ex.result = failed
			}
		}
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

// failStranded records cause on a run whose execution returned an error
// without moving it out of RUNNING. No caller may be waiting to read that
// error, and nothing else would ever move the run.
func (m *Manager) failStranded(ctx context.Context, runID string, cause error) (storage.Run, bool) {
	current, err := m.Store.GetRun(ctx, runID)
	if err != nil || current.Status != domain.RunRunning {
		return storage.Run{}, false
	}
	failed, err := m.finish(ctx, current, domain.EventFatal, "", asDomain(cause))
	return failed, err == nil
}

// shutdownRecheck is how often Shutdown looks for an execution that recorded
// its run after the previous pass.
const shutdownRecheck = 20 * time.Millisecond

// Shutdown refuses new background executions, cancels those still executing,
// and waits for them to unwind or for ctx to end. Runs are cancelled side by
// side, so a slow Stop hook on one does not keep another executing. Shutdown
// returns only after every cancel it began has recorded its run as CANCELLED,
// even when ctx ends first, so none is left RUNNING. A run that waits for a
// person is not executing and is left as it is.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.bgMu.Lock()
	m.bgClosed = true
	m.bgMu.Unlock()

	unwound := make(chan struct{})
	go func() {
		m.bgWG.Wait()
		close(unwound)
	}()
	ticker := time.NewTicker(shutdownRecheck)
	defer ticker.Stop()
	persist := context.WithoutCancel(ctx)
	asked := map[string]bool{}
	var (
		cancels sync.WaitGroup
		errMu   sync.Mutex
		errs    []error
	)
	fail := func(err error) {
		errMu.Lock()
		errs = append(errs, err)
		errMu.Unlock()
	}
	for {
		// An execution accepted just before bgClosed was set may record its
		// run only now, so every pass looks again.
		for _, id := range m.backgroundRunIDs() {
			if asked[id] {
				continue
			}
			run, err := m.Store.GetRun(persist, id)
			if err != nil {
				asked[id] = true
				fail(err)
				continue
			}
			if !executing(run) {
				continue
			}
			asked[id] = true
			cancels.Add(1)
			go func() {
				defer cancels.Done()
				if err := m.cancelExecuting(persist, id); err != nil {
					fail(err)
				}
			}()
		}
		select {
		case <-unwound:
			cancels.Wait()
			return errors.Join(errs...)
		case <-ctx.Done():
			cancels.Wait()
			return errors.Join(append(errs, ctx.Err())...)
		case <-ticker.C:
		}
	}
}

func (m *Manager) backgroundRunIDs() []string {
	m.bgMu.Lock()
	defer m.bgMu.Unlock()
	ids := make([]string, 0, len(m.bgRuns))
	for id := range m.bgRuns {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// executing reports whether a background run is still doing work: it is
// RUNNING, or it waits on a child that runs inside its own execution.
func executing(run storage.Run) bool {
	return run.Status == domain.RunRunning ||
		(run.Status == domain.RunWaiting && run.WaitReason == domain.WaitChild)
}

// cancelExecuting cancels id, which also stops a child it delegated to.
func (m *Manager) cancelExecuting(ctx context.Context, id string) error {
	if _, err := m.Cancel(ctx, id); err != nil {
		// The run may have stopped by itself in the meantime.
		if current, readErr := m.Store.GetRun(ctx, id); readErr != nil || executing(current) {
			return err
		}
	}
	return nil
}
