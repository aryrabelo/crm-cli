package waterfall_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/jdanielnd/crm-cli/internal/db"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/jdanielnd/crm-cli/internal/waterfall"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "crm.db"))
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	return d
}

// age moves every cache entry of providerName back by d.
func age(t *testing.T, d *sql.DB, providerName string, by time.Duration) {
	t.Helper()
	_, err := d.Exec("UPDATE enrichment_cache SET fetched_at = datetime(fetched_at, ?) WHERE provider = ?",
		fmt.Sprintf("-%d seconds", int64(by.Seconds())), providerName)
	require.NoError(t, err)
}

// Design review P1-8: fetched_at was written but never read, so an answer
// (or a "known empty") was reused forever. Entries older than the step's TTL
// are now ignored, and the provider is called again.
func TestRun_ExpiredCacheEntryIsFetchedAgain(t *testing.T) {
	d := openDB(t)
	p := &fake{out: `{"company":"Acme"}`, price: 2}
	wf := &waterfall.Waterfall{Steps: []waterfall.Step{{Name: "p", Adapter: p, TTL: time.Hour}}, Cache: waterfall.NewCache(d)}
	ctx := context.Background()
	in := provider.Input{Value: "x"}

	_, err := wf.Run(ctx, in, "company", direct)
	require.NoError(t, err)
	res, err := wf.Run(ctx, in, "company", direct)
	require.NoError(t, err)
	assert.True(t, res.CacheHit, "fresh entry is reused")
	assert.Equal(t, 1, p.calls)

	age(t, d, "p", 2*time.Hour)
	est, err := wf.Estimate(ctx, in, "company", true)
	require.NoError(t, err)
	assert.False(t, est.CacheHit, "expired entry is not a cache hit in the estimate")
	assert.Equal(t, provider.Cost(2), est.Expected)

	p.out = `{"company":"Acme 2"}`
	res, err = wf.Run(ctx, in, "company", direct)
	require.NoError(t, err)
	assert.False(t, res.CacheHit)
	assert.Equal(t, "Acme 2", res.Value)
	assert.Equal(t, 2, p.calls)

	res, err = wf.Run(ctx, in, "company", direct)
	require.NoError(t, err)
	assert.True(t, res.CacheHit, "the refetch refreshed fetched_at")
	assert.Equal(t, "Acme 2", res.Value)

	// TTL 0 never expires.
	wf.Steps[0].TTL = 0
	age(t, d, "p", 1000*24*time.Hour)
	res, err = wf.Run(ctx, in, "company", direct)
	require.NoError(t, err)
	assert.True(t, res.CacheHit)
	assert.Equal(t, 2, p.calls)
}

func TestCache_Clear(t *testing.T) {
	d := openDB(t)
	c := waterfall.NewCache(d)
	ctx := context.Background()
	for _, p := range []string{"a", "b"} {
		for _, k := range []provider.Hash{"old", "new"} {
			require.NoError(t, c.Put(ctx, p, k, provider.Output(`"v"`), 1))
		}
	}
	_, err := d.Exec("UPDATE enrichment_cache SET fetched_at = datetime('now', '-10 days') WHERE input_hash = 'old'")
	require.NoError(t, err)

	n, err := c.Clear(ctx, "a", 7*24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "only a's entry older than 7 days")
	_, ok, err := c.Get(ctx, "a", "new", 0)
	require.NoError(t, err)
	assert.True(t, ok)

	n, err = c.Clear(ctx, "", 7*24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "b's old entry")

	n, err = c.Clear(ctx, "", 0)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n, "everything left")
}
