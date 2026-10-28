package run_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/jdanielnd/crm-cli/internal/run"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// slowAPI answers only after the client has given up: every request is
// received (and, for a real provider, possibly billed) but never answered.
func slowAPI(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// timeoutEnv is a one-row table with column company enriched by provider p
// (price $0.01, method) on an engine whose HTTP client gives up after 100ms.
func timeoutEnv(t *testing.T, srv *httptest.Server, method string) *env {
	t.Helper()
	e := setup(t, 1)
	e.engine = run.NewEngine(e.db, provider.NewRegistry(&http.Client{Timeout: 100 * time.Millisecond}, func(string) string { return "" }))
	s := provider.Settings{URL: srv.URL + "/?q={{input}}", Path: "company"}
	if method == http.MethodPost {
		s.Body = `{"q":"{{input}}"}`
	}
	require.NoError(t, provider.NewStore(e.db).Save(context.Background(), &provider.Config{
		Name: "p", Kind: "http", Price: 10_000, Enabled: true, Settings: s}))
	e.enrich(t, "company", "p")
	return e
}

func runCompany(t *testing.T, e *env, retryUncertain bool) *run.Run {
	t.Helper()
	res, err := e.engine.Execute(context.Background(), run.Options{Table: "leads", Columns: []string{"company"},
		Retry: 2, Backoff: time.Millisecond, Budget: 1_000_000, BudgetSet: true, RetryUncertain: retryUncertain})
	require.NoError(t, err)
	return res.Run
}

// Design review P1-2: a call that timed out after the request was sent was
// recorded at $0 and retried twice, so up to 3 billed calls showed as $0.
func TestUncertain_PaidPOSTTimeoutChargedAtEstimateAndNotRetried(t *testing.T) {
	srv, hits := slowAPI(t)
	e := timeoutEnv(t, srv, http.MethodPost)

	r := runCompany(t, e, false)
	assert.Equal(t, int64(1), hits.Load(), "a paid POST that may have been billed is not retried")
	assert.Equal(t, provider.Cost(10_000), r.Spent, "charged at the estimate, not $0")
	assert.Equal(t, 1, r.Stats.Failed)
	assert.Equal(t, 1, r.Stats.Uncertain)
	assert.Equal(t, int64(10_000), r.Stats.UncertainMicros)

	items, err := run.NewStore(e.db).Items(context.Background(), r.ID, false)
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, provider.Cost(10_000), items[0].Cost)
	assert.Equal(t, provider.Cost(10_000), items[0].Uncertain)
	require.NotNil(t, items[0].Error)
	assert.Contains(t, *items[0].Error, "not retried; --retry-uncertain retries")
}

func TestUncertain_RetryUncertainOptIn(t *testing.T) {
	srv, hits := slowAPI(t)
	e := timeoutEnv(t, srv, http.MethodPost)

	r := runCompany(t, e, true)
	assert.Equal(t, int64(3), hits.Load(), "opted in: 1 call + 2 retries")
	assert.Equal(t, provider.Cost(30_000), r.Spent, "every sent attempt is counted")
	assert.Equal(t, int64(30_000), r.Stats.UncertainMicros)
}

func TestUncertain_IdempotentGETIsRetriedAndCounted(t *testing.T) {
	srv, hits := slowAPI(t)
	e := timeoutEnv(t, srv, http.MethodGet)

	r := runCompany(t, e, false)
	assert.Equal(t, int64(3), hits.Load())
	assert.Equal(t, provider.Cost(30_000), r.Spent)
	assert.Equal(t, 1, r.Stats.Uncertain)
}

func TestUncertain_RefusedConnectionCostsNothing(t *testing.T) {
	srv, _ := slowAPI(t)
	e := timeoutEnv(t, srv, http.MethodPost)
	srv.Close() // nothing listens: the request is never sent

	r := runCompany(t, e, false)
	assert.Zero(t, r.Spent)
	assert.Zero(t, r.Stats.Uncertain)
	assert.Equal(t, 1, r.Stats.Failed)
}
