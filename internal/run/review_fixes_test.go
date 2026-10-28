package run_test

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jdanielnd/crm-cli/internal/column"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/jdanielnd/crm-cli/internal/run"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func addShout(t *testing.T, e *env) {
	t.Helper()
	upper := newAPI(t, func(w http.ResponseWriter, q string) { json200(w, `{"v":"U-`+q+`"}`) })
	e.provider(t, "upper", upper, "v", 0)
	require.NoError(t, e.tables.AddColumn(context.Background(), &column.Column{
		WorkspaceID: e.ws.ID, Name: "shout", Type: column.Enrich,
		Config: column.Config{Waterfall: []string{"upper"}, From: "email"},
	}))
}

// Cubic #4380602102: a dependent whose source failed was stored as empty and
// never recomputed on resume.
func TestDependent_SourceFailureKeepsDependentOpen(t *testing.T) {
	e := setup(t, 3)
	var broken atomic.Bool
	broken.Store(true)
	emails := newAPI(t, func(w http.ResponseWriter, q string) {
		if broken.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		json200(w, `{"v":"`+q+`@acme.com"}`)
	})
	e.provider(t, "emails", emails, "v", 0)
	e.enrich(t, "email", "emails")
	addShout(t, e)
	ctx := context.Background()
	opts := run.Options{Table: "leads", Columns: []string{"email", "shout"}, Retry: 0, Backoff: time.Millisecond}

	res, err := e.engine.Execute(ctx, opts)
	require.NoError(t, err)
	assert.Equal(t, 3, res.Run.Stats.Failed+res.Run.Stats.Skipped+res.Run.Stats.Pending-3, "shout items stay open")
	assert.Zero(t, res.Run.Stats.Empty, "an unanswered source is not an empty result")
	assert.Zero(t, res.Run.Stats.OK)

	broken.Store(false)
	res, err = e.engine.Execute(ctx, run.Options{Resume: res.Run.ID, Retry: 0, Backoff: time.Millisecond})
	require.NoError(t, err)
	assert.Equal(t, 6, res.Run.Stats.OK, "resume recomputes both columns")
	rows, err := e.tables.Rows(ctx, e.ws.ID)
	require.NoError(t, err)
	assert.Equal(t, "U-user002@acme.com", rows[2].Data["shout"])
}

// Cubic #4380602138: a dependent listed before its source silently ran on an
// empty input.
func TestPrepare_RejectsDependentListedBeforeSource(t *testing.T) {
	e := setup(t, 1)
	emails := newAPI(t, func(w http.ResponseWriter, q string) { json200(w, `{"v":"x"}`) })
	e.provider(t, "emails", emails, "v", 0)
	e.enrich(t, "email", "emails")
	addShout(t, e)
	_, err := e.engine.Estimate(context.Background(), run.Options{Table: "leads", Columns: []string{"shout", "email"}})
	require.ErrorIs(t, err, model.ErrValidation)
	assert.Contains(t, err.Error(), "list \"email\" before \"shout\"")
}

// Cubic #4380602133: the dry-run ignored the cache for a dependent column
// whose source value is already persisted.
func TestEstimate_DependentUsesCacheWhenSourceIsPersisted(t *testing.T) {
	e := setup(t, 3)
	emails := newAPI(t, func(w http.ResponseWriter, q string) { json200(w, `{"v":"`+q+`@acme.com"}`) })
	e.provider(t, "emails", emails, "v", 1000)
	e.enrich(t, "email", "emails")
	addShout(t, e)
	ctx := context.Background()
	opts := run.Options{Table: "leads", Columns: []string{"email", "shout"}, Budget: 1_000_000, BudgetSet: true}
	_, err := e.engine.Execute(ctx, opts)
	require.NoError(t, err)

	est, err := e.engine.Estimate(ctx, opts)
	require.NoError(t, err)
	assert.Equal(t, 6, est.Total.CacheHits)
	assert.Zero(t, est.Total.Calls)
}

// Cubic #4381117156: Start returned the run without timestamps or stats.
func TestStart_ReturnsPersistedRunWithStats(t *testing.T) {
	e := setup(t, 3)
	api := newAPI(t, func(w http.ResponseWriter, q string) { json200(w, `{"v":"`+q+`"}`) })
	e.provider(t, "p", api, "v", 0)
	e.enrich(t, "v", "p")
	r, err := e.engine.Start(context.Background(), run.Options{Table: "leads", Columns: []string{"v"}})
	require.NoError(t, err)
	assert.Equal(t, 3, r.Stats.Items)
	assert.Equal(t, 3, r.Stats.Pending)
	assert.NotEmpty(t, r.StartedAt)
}

// Cubic #4390624669: a lease below 3ns made the heartbeat ticker panic.
func TestExecute_TinyLeaseTimeoutDoesNotPanic(t *testing.T) {
	e := setup(t, 1)
	api := newAPI(t, func(w http.ResponseWriter, q string) { json200(w, `{"v":"`+q+`"}`) })
	e.provider(t, "p", api, "v", 0)
	e.enrich(t, "v", "p")
	assert.NotPanics(t, func() {
		_, _ = e.engine.Execute(context.Background(), run.Options{Table: "leads", Columns: []string{"v"}, LeaseTimeout: time.Nanosecond})
	})
}

// Cubic #4390624467: Ctrl-C during a paid POST that was sent without a
// response left the item pending, so --resume re-sent it.
func TestUncertain_CancelledPaidPOSTIsNotLeftPending(t *testing.T) {
	srv, hits := slowAPI(t)
	e := timeoutEnv(t, srv, http.MethodPost)
	e.engine = run.NewEngine(e.db, provider.NewRegistry(&http.Client{Timeout: 5 * time.Second}, func(string) string { return "" }))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for hits.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	res, err := e.engine.Execute(ctx, run.Options{Table: "leads", Columns: []string{"company"},
		Retry: 2, Backoff: time.Millisecond, Budget: 1_000_000, BudgetSet: true})
	require.NoError(t, err)
	assert.Zero(t, res.Run.Stats.Pending, "a possibly billed call must not be silently re-sent on resume")
	assert.Equal(t, 1, res.Run.Stats.Failed)
	assert.Equal(t, 1, res.Run.Stats.Uncertain)
}

// Cubic #4390699968: Start after Close registered a goroutine nobody waits for.
func TestTracker_StartAfterCloseIsRejected(t *testing.T) {
	tr := run.NewTracker()
	tr.Close()
	var ran atomic.Bool
	tr.Start(7, func(context.Context) error { ran.Store(true); return nil })
	active, err := tr.State(7)
	assert.False(t, active)
	assert.ErrorIs(t, err, run.ErrTrackerClosed)
	assert.False(t, ran.Load())
}
