package run

import (
	"context"
	"sync"
)

// Tracker owns runs executed in the background of a long-lived server (MCP
// wait=false, crm ui): each executes on its own context, derived from one
// that Close cancels. A cancelled run stops at its next item and is left
// interrupted and resumable, like a Ctrl-C of crm run.
type Tracker struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.Mutex
	active map[int64]context.CancelFunc
	errs   map[int64]error
}

// NewTracker creates a Tracker. Close it to stop its runs.
func NewTracker() *Tracker {
	ctx, cancel := context.WithCancel(context.Background())
	return &Tracker{ctx: ctx, cancel: cancel, active: map[int64]context.CancelFunc{}, errs: map[int64]error{}}
}

// Start executes fn for run id on a goroutine (typically Engine.Execute with
// Options.Resume = id); its error is kept for State.
func (t *Tracker) Start(id int64, fn func(context.Context) error) {
	ctx, cancel := context.WithCancel(t.ctx)
	t.mu.Lock()
	t.active[id] = cancel
	delete(t.errs, id)
	t.mu.Unlock()
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		defer cancel()
		err := fn(ctx)
		t.mu.Lock()
		defer t.mu.Unlock()
		delete(t.active, id)
		if err != nil {
			t.errs[id] = err
		}
	}()
}

// State reports whether run id is still executing here, and the error its
// execution ended with (nil while active or on success).
func (t *Tracker) State(id int64) (active bool, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, active = t.active[id]
	return active, t.errs[id]
}

// Cancel stops run id when it executes here; false when it does not (it
// finished, or another process executes it).
func (t *Tracker) Cancel(id int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	cancel, ok := t.active[id]
	if ok {
		cancel()
	}
	return ok
}

// Close cancels every run and waits for them to record their state.
func (t *Tracker) Close() {
	t.cancel()
	t.wg.Wait()
}
