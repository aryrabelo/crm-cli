package run_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jdanielnd/crm-cli/internal/column"
	"github.com/jdanielnd/crm-cli/internal/csvio"
	"github.com/jdanielnd/crm-cli/internal/db"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/jdanielnd/crm-cli/internal/run"
	"github.com/jdanielnd/crm-cli/internal/table"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAPI is an httptest provider backend that counts requests.
type fakeAPI struct {
	srv  *httptest.Server
	hits atomic.Int64
}

func newAPI(t *testing.T, handler func(w http.ResponseWriter, q string)) *fakeAPI {
	t.Helper()
	a := &fakeAPI{}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.hits.Add(1)
		handler(w, r.URL.Query().Get("q"))
	}))
	t.Cleanup(a.srv.Close)
	return a
}

type env struct {
	db     *sql.DB
	engine *run.Engine
	tables *table.Store
	ws     *table.Workspace
}

func setup(t *testing.T, rows int) *env {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "crm.db"))
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	lines := []string{"name"}
	for i := range rows {
		lines = append(lines, fmt.Sprintf("user%03d", i))
	}
	parsed, err := csvio.Parse([]byte(strings.Join(lines, "\n") + "\n"))
	require.NoError(t, err)
	ts := table.NewStore(d)
	ctx := context.Background()
	_, err = ts.Import(ctx, table.ImportOptions{Name: "leads", Key: "name"}, parsed)
	require.NoError(t, err)
	ws, err := ts.FindWorkspace(ctx, "leads")
	require.NoError(t, err)
	registry := provider.NewRegistry(&http.Client{Timeout: 5 * time.Second}, func(string) string { return "" })
	return &env{db: d, engine: run.NewEngine(d, registry), tables: ts, ws: ws}
}

func (e *env) provider(t *testing.T, name string, api *fakeAPI, path string, price provider.Cost) {
	t.Helper()
	require.NoError(t, provider.NewStore(e.db).Save(context.Background(), &provider.Config{
		Name: name, Kind: "http", Price: price, Enabled: true,
		Settings: provider.Settings{URL: api.srv.URL + "/?q={{input}}", Path: path},
	}))
}

func (e *env) enrich(t *testing.T, name string, waterfall ...string) {
	t.Helper()
	require.NoError(t, e.tables.AddColumn(context.Background(), &column.Column{
		WorkspaceID: e.ws.ID, Name: name, Type: column.Enrich,
		Config: column.Config{Waterfall: waterfall, From: "name"},
	}))
}

func (e *env) count(t *testing.T, query string) int {
	t.Helper()
	var n int
	require.NoError(t, e.db.QueryRow(query).Scan(&n))
	return n
}

func json200(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

// PLANO S1 aceite: 200 rows, 2 enrich columns, one provider always failing →
// the run finishes, the failing provider falls through, cost ≤ budget, and an
// identical second run spends exactly 0 (all cache hits).
func TestAcceptance_WaterfallCacheBudget(t *testing.T) {
	e := setup(t, 200)
	failing := newAPI(t, func(w http.ResponseWriter, _ string) { w.WriteHeader(http.StatusInternalServerError) })
	emails := newAPI(t, func(w http.ResponseWriter, q string) { json200(w, `{"email":"`+q+`@acme.com"}`) })
	domains := newAPI(t, func(w http.ResponseWriter, q string) { json200(w, `{"data":{"domain":"`+q+`.dev"}}`) })
	e.provider(t, "flaky", failing, "", 3000)
	e.provider(t, "emails", emails, "email", 1000)
	e.provider(t, "domains", domains, "data.domain", 2000)
	e.enrich(t, "email", "flaky", "emails")
	e.enrich(t, "domain", "domains")

	budget := provider.Cost(1_000_000)
	opts := run.Options{Table: "leads", Columns: []string{"email", "domain"}, Concurrency: 8,
		Retry: 1, Backoff: time.Millisecond, Budget: budget, BudgetSet: true}
	ctx := context.Background()

	first, err := e.engine.Execute(ctx, opts)
	require.NoError(t, err)
	r := first.Run
	assert.Equal(t, run.StatusCompleted, r.Status)
	assert.Equal(t, 400, r.Stats.Items)
	assert.Equal(t, 400, r.Stats.OK)
	assert.Zero(t, r.Stats.Failed+r.Stats.Skipped+r.Stats.Pending)
	assert.Equal(t, provider.Cost(200*1000+200*2000), r.Spent, "failed calls cost nothing")
	assert.LessOrEqual(t, r.Spent, budget)
	assert.Equal(t, map[string]int{"emails": 200, "domains": 200}, r.Stats.Providers)
	assert.EqualValues(t, 400, failing.hits.Load(), "200 rows × (1 call + 1 retry)")
	assert.EqualValues(t, 200, emails.hits.Load())
	assert.EqualValues(t, 200, domains.hits.Load())

	rows, err := e.tables.Rows(ctx, e.ws.ID)
	require.NoError(t, err)
	assert.Equal(t, "user000@acme.com", rows[0].Data["email"])
	assert.Equal(t, "user199.dev", rows[199].Data["domain"])

	second, err := e.engine.Execute(ctx, opts)
	require.NoError(t, err)
	r2 := second.Run
	assert.NotEqual(t, r.ID, r2.ID)
	assert.Equal(t, run.StatusCompleted, r2.Status)
	assert.Equal(t, provider.Cost(0), r2.Spent)
	assert.Equal(t, 400, r2.Stats.CacheHits)
	assert.Equal(t, 400, r2.Stats.OK)
	assert.Zero(t, r2.Stats.Calls)
	assert.EqualValues(t, 400, failing.hits.Load(), "cache answers before the failing provider is tried")
	assert.EqualValues(t, 200, emails.hits.Load())
	assert.EqualValues(t, 200, domains.hits.Load())
}

// Budget smaller than needed: the run stops before exceeding it, the rest is
// skipped, and --resume with a larger budget completes exactly those items.
func TestBudget_StopsThenResumeCompletes(t *testing.T) {
	e := setup(t, 100)
	api := newAPI(t, func(w http.ResponseWriter, q string) { json200(w, `{"v":"`+q+`"}`) })
	e.provider(t, "paid", api, "v", 1000)
	e.enrich(t, "v", "paid")
	ctx := context.Background()

	res, err := e.engine.Execute(ctx, run.Options{Table: "leads", Columns: []string{"v"}, Concurrency: 8,
		Budget: 50_000, BudgetSet: true})
	require.NoError(t, err)
	r := res.Run
	assert.Equal(t, run.StatusBudgetExhausted, r.Status)
	assert.Equal(t, provider.Cost(50_000), r.Spent)
	assert.LessOrEqual(t, r.Spent, r.Budget)
	assert.Equal(t, 50, r.Stats.OK)
	assert.Equal(t, 50, r.Stats.Skipped)
	assert.EqualValues(t, 50, api.hits.Load(), "no call is made without budget")

	res, err = e.engine.Execute(ctx, run.Options{Resume: r.ID, Concurrency: 8, Budget: 200_000, BudgetSet: true})
	require.NoError(t, err)
	r = res.Run
	assert.Equal(t, run.StatusCompleted, r.Status)
	assert.Equal(t, 100, r.Stats.OK)
	assert.Zero(t, r.Stats.Skipped)
	assert.Equal(t, provider.Cost(100_000), r.Spent)
	assert.EqualValues(t, 100, api.hits.Load(), "done items are not redone")
	assert.Equal(t, 1, e.count(t, "SELECT COUNT(*) FROM runs"))
}

func TestBudget_RequiredWhenRunMaySpend(t *testing.T) {
	e := setup(t, 3)
	api := newAPI(t, func(w http.ResponseWriter, q string) { json200(w, `{"v":"`+q+`"}`) })
	e.provider(t, "paid", api, "v", 1000)
	e.enrich(t, "v", "paid")
	_, err := e.engine.Execute(context.Background(), run.Options{Table: "leads", Columns: []string{"v"}})
	require.ErrorContains(t, err, "set --budget")
	assert.Zero(t, e.count(t, "SELECT COUNT(*) FROM runs"))
	assert.Zero(t, api.hits.Load())
}

func TestDryRun_EstimatesAndWritesNothing(t *testing.T) {
	e := setup(t, 10)
	api := newAPI(t, func(w http.ResponseWriter, q string) { json200(w, `{"v":"`+q+`"}`) })
	e.provider(t, "a", api, "v", 1000)
	e.provider(t, "b", api, "v", 2000)
	e.enrich(t, "v", "a", "b")
	ctx := context.Background()

	// Warm the cache for 4 rows with a real run limited by budget.
	_, err := e.engine.Execute(ctx, run.Options{Table: "leads", Columns: []string{"v"}, Concurrency: 1,
		Budget: 4000, BudgetSet: true})
	require.NoError(t, err)
	hits := api.hits.Load()
	before := []int{
		e.count(t, "SELECT COUNT(*) FROM runs"),
		e.count(t, "SELECT COUNT(*) FROM run_items"),
		e.count(t, "SELECT COUNT(*) FROM enrichment_cache"),
		e.count(t, "SELECT COUNT(*) FROM workspace_rows WHERE data LIKE '%\"v\"%'"),
	}

	est, err := e.engine.Estimate(ctx, run.Options{Table: "leads", Columns: []string{"v"}})
	require.NoError(t, err)
	assert.Equal(t, 10, est.Total.Items)
	assert.Equal(t, 4, est.Total.CacheHits)
	assert.Equal(t, 6, est.Total.Calls)
	assert.Equal(t, provider.Cost(6*1000), est.Total.Expected)
	assert.Equal(t, provider.Cost(6*3000), est.Total.Worst)

	after := []int{
		e.count(t, "SELECT COUNT(*) FROM runs"),
		e.count(t, "SELECT COUNT(*) FROM run_items"),
		e.count(t, "SELECT COUNT(*) FROM enrichment_cache"),
		e.count(t, "SELECT COUNT(*) FROM workspace_rows WHERE data LIKE '%\"v\"%'"),
	}
	assert.Equal(t, before, after)
	assert.Equal(t, hits, api.hits.Load(), "dry run calls nobody")
}

func TestRetry_BacksOffOn429(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	api := newAPI(t, func(w http.ResponseWriter, q string) {
		mu.Lock()
		seen[q]++
		n := seen[q]
		mu.Unlock()
		if n <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		json200(w, `{"v":"`+q+`"}`)
	})
	e := setup(t, 5)
	e.provider(t, "limited", api, "v", 1000)
	e.enrich(t, "v", "limited")

	res, err := e.engine.Execute(context.Background(), run.Options{Table: "leads", Columns: []string{"v"},
		Concurrency: 5, Retry: 2, Backoff: 5 * time.Millisecond, Budget: 1_000_000, BudgetSet: true})
	require.NoError(t, err)
	assert.Equal(t, 5, res.Run.Stats.OK)
	assert.Equal(t, provider.Cost(5000), res.Run.Spent, "only the successful attempt is charged")
	items, err := run.NewStore(e.db).Items(context.Background(), res.Run.ID, false)
	require.NoError(t, err)
	for _, it := range items {
		assert.Equal(t, 3, it.Attempts)
	}

	// With fewer retries than the provider needs, items fail and stay resumable.
	e2 := setup(t, 2)
	mu.Lock()
	clear(seen)
	mu.Unlock()
	e2.provider(t, "limited", api, "v", 1000)
	e2.enrich(t, "v", "limited")
	res, err = e2.engine.Execute(context.Background(), run.Options{Table: "leads", Columns: []string{"v"},
		Concurrency: 2, Retry: 1, Backoff: time.Millisecond, Budget: 1_000_000, BudgetSet: true})
	require.NoError(t, err)
	assert.Equal(t, 2, res.Run.Stats.Failed)
	res, err = e2.engine.Execute(context.Background(), run.Options{Resume: res.Run.ID, Retry: 1, Backoff: time.Millisecond})
	require.NoError(t, err)
	assert.Equal(t, 2, res.Run.Stats.OK, "the third attempt (on resume) succeeds")
}

func TestInterrupt_LeavesPendingItemsForResume(t *testing.T) {
	e := setup(t, 40)
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int64
	api := newAPI(t, func(w http.ResponseWriter, q string) {
		if calls.Add(1) == 10 {
			cancel()
		}
		json200(w, `{"v":"`+q+`"}`)
	})
	e.provider(t, "p", api, "v", 0)
	e.enrich(t, "v", "p")

	res, err := e.engine.Execute(ctx, run.Options{Table: "leads", Columns: []string{"v"}, Concurrency: 2})
	require.NoError(t, err)
	assert.Equal(t, run.StatusInterrupted, res.Run.Status)
	assert.Positive(t, res.Run.Stats.Pending)
	assert.Equal(t, 40, res.Run.Stats.OK+res.Run.Stats.Pending)

	res, err = e.engine.Execute(context.Background(), run.Options{Resume: res.Run.ID, Concurrency: 4})
	require.NoError(t, err)
	assert.Equal(t, run.StatusCompleted, res.Run.Status)
	assert.Equal(t, 40, res.Run.Stats.OK)
}

func TestDependentColumns_UseValueComputedEarlierInRow(t *testing.T) {
	e := setup(t, 3)
	emails := newAPI(t, func(w http.ResponseWriter, q string) { json200(w, `{"v":"`+q+`@acme.com"}`) })
	upper := newAPI(t, func(w http.ResponseWriter, q string) { json200(w, `{"v":"`+strings.ToUpper(q)+`"}`) })
	e.provider(t, "emails", emails, "v", 0)
	e.provider(t, "upper", upper, "v", 0)
	e.enrich(t, "email", "emails")
	require.NoError(t, e.tables.AddColumn(context.Background(), &column.Column{
		WorkspaceID: e.ws.ID, Name: "shout", Type: column.Enrich,
		Config: column.Config{Waterfall: []string{"upper"}, From: "email"},
	}))
	res, err := e.engine.Execute(context.Background(), run.Options{Table: "leads", Columns: []string{"email", "shout"}})
	require.NoError(t, err)
	assert.Equal(t, 6, res.Run.Stats.OK)
	rows, err := e.tables.Rows(context.Background(), e.ws.ID)
	require.NoError(t, err)
	assert.Equal(t, "USER002@ACME.COM", rows[2].Data["shout"])
}
