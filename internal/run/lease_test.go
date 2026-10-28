package run_test

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/jdanielnd/crm-cli/internal/run"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Design review P1-1: two executors on the same run (MCP wait=false + CLI
// --resume) used to process the same items with a budget each — up to 2×
// the spend and double-counted stats. Exactly one may hold the run.
func TestLease_ConcurrentExecutorsOneRefused(t *testing.T) {
	e := setup(t, 4)
	release := make(chan struct{})
	calling := make(chan struct{}, 64)
	api := newAPI(t, func(w http.ResponseWriter, q string) {
		calling <- struct{}{}
		<-release
		json200(w, `{"v":"`+q+`"}`)
	})
	e.provider(t, "slow", api, "v", 1000)
	e.enrich(t, "v", "slow")
	ctx := context.Background()
	budget := provider.Cost(4000)
	r, err := e.engine.Start(ctx, run.Options{Table: "leads", Columns: []string{"v"}, Budget: budget, BudgetSet: true})
	require.NoError(t, err)

	type outcome struct {
		res *run.Result
		err error
	}
	results := make(chan outcome, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for range 2 {
		go func() {
			ready.Done()
			ready.Wait() // both start together
			res, err := e.engine.Execute(ctx, run.Options{Resume: r.ID, Concurrency: 2, Backoff: time.Millisecond})
			results <- outcome{res, err}
		}()
	}
	// The refused executor returns while the holder is still calling.
	var refused outcome
	select {
	case refused = <-results:
	case <-time.After(10 * time.Second):
		t.Fatal("no executor was refused")
	}
	require.Error(t, refused.err)
	require.ErrorIs(t, refused.err, model.ErrConflict)
	assert.Contains(t, refused.err.Error(), "is being executed by")
	assert.Equal(t, 4, model.ExitCode(refused.err))

	close(release)
	held := <-results
	require.NoError(t, held.err)
	got := held.res.Run
	assert.Equal(t, run.StatusCompleted, got.Status)
	assert.LessOrEqual(t, got.Spent, budget)
	assert.Equal(t, provider.Cost(4000), got.Spent)
	assert.EqualValues(t, 4, api.hits.Load(), "each item called once")
	assert.Equal(t, 4, got.Stats.Calls)
	assert.Equal(t, 4, got.Stats.OK)
	assert.Equal(t, 4, e.count(t, "SELECT SUM(attempts) FROM run_items"))
	assert.Equal(t, 0, e.count(t, "SELECT COUNT(*) FROM runs WHERE owner IS NOT NULL"), "lease released")
}

// A run whose executor died (heartbeat stale) can be resumed; a live one
// cannot.
func TestLease_StaleTakeover(t *testing.T) {
	e := setup(t, 2)
	api := newAPI(t, func(w http.ResponseWriter, q string) { json200(w, `{"v":"`+q+`"}`) })
	e.provider(t, "fast", api, "v", 1000)
	e.enrich(t, "v", "fast")
	ctx := context.Background()
	r, err := e.engine.Start(ctx, run.Options{Table: "leads", Columns: []string{"v"}, Budget: 10_000, BudgetSet: true})
	require.NoError(t, err)
	opts := run.Options{Resume: r.ID, LeaseTimeout: time.Second}

	// A live holder (fresh heartbeat) refuses the resume.
	_, err = e.db.Exec("UPDATE runs SET owner = 'ghost:1:abc', heartbeat_at = ? WHERE id = ?", time.Now().UnixMilli(), r.ID)
	require.NoError(t, err)
	_, err = e.engine.Execute(ctx, opts)
	require.ErrorIs(t, err, model.ErrConflict)
	assert.Contains(t, err.Error(), "ghost:1:abc")
	assert.EqualValues(t, 0, api.hits.Load())

	// The holder crashed: once its heartbeat is older than the lease
	// timeout, the run is taken over and completes.
	_, err = e.db.Exec("UPDATE runs SET heartbeat_at = ? WHERE id = ?", time.Now().Add(-2*time.Second).UnixMilli(), r.ID)
	require.NoError(t, err)
	res, err := e.engine.Execute(ctx, opts)
	require.NoError(t, err)
	assert.Equal(t, run.StatusCompleted, res.Run.Status)
	assert.EqualValues(t, 2, api.hits.Load())
}

// An executor whose lease was taken over stops and writes nothing more.
func TestLease_LostLeaseStopsTheRun(t *testing.T) {
	e := setup(t, 3)
	calls := 0
	var mu sync.Mutex
	var db = e.db
	api := newAPI(t, func(w http.ResponseWriter, q string) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first { // another executor takes the run over mid-call
			_, err := db.Exec("UPDATE runs SET owner = 'thief:1:x', heartbeat_at = ?", time.Now().UnixMilli())
			if err != nil {
				panic(err)
			}
		}
		json200(w, `{"v":"`+q+`"}`)
	})
	e.provider(t, "p", api, "v", 1000)
	e.enrich(t, "v", "p")
	ctx := context.Background()
	_, err := e.engine.Execute(ctx, run.Options{Table: "leads", Columns: []string{"v"}, Concurrency: 1, Budget: 10_000, BudgetSet: true})
	require.ErrorIs(t, err, run.ErrLeaseLost)
	assert.Equal(t, 0, e.count(t, "SELECT COUNT(*) FROM run_items WHERE status != 'pending'"), "no checkpoint after the takeover")
	assert.Equal(t, 0, e.count(t, "SELECT spent_micros FROM runs"))
	assert.Equal(t, 1, e.count(t, "SELECT COUNT(*) FROM runs WHERE owner = 'thief:1:x'"), "the new holder keeps the run")
}

// Cubic #4380602357: a fatal error mid-run names the interrupted run and the
// --resume command, and still unwraps to the cause.
func TestExecute_FatalErrorNamesRunToResume(t *testing.T) {
	e := setup(t, 2)
	api := newAPI(t, func(w http.ResponseWriter, q string) { json200(w, `{"v":"`+q+`"}`) })
	e.provider(t, "p", api, "v", 1000)
	e.enrich(t, "v", "p")
	ctx := context.Background()
	r, err := e.engine.Start(ctx, run.Options{Table: "leads", Columns: []string{"v"}, Budget: 10_000, BudgetSet: true})
	require.NoError(t, err)
	_, err = e.db.Exec("CREATE TRIGGER boom BEFORE UPDATE ON run_items BEGIN SELECT RAISE(ABORT, 'disk on fire'); END")
	require.NoError(t, err)

	_, err = e.engine.Execute(ctx, run.Options{Resume: r.ID, Concurrency: 1})
	require.Error(t, err)
	var re *run.RunError
	require.ErrorAs(t, err, &re)
	assert.Equal(t, r.ID, re.ID)
	assert.Contains(t, err.Error(), "disk on fire")
	assert.Contains(t, err.Error(), fmt.Sprintf("crm run --resume %d", r.ID))
	assert.Equal(t, 1, e.count(t, "SELECT COUNT(*) FROM runs WHERE status = 'interrupted'"))
}
