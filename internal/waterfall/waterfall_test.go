package waterfall_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jdanielnd/crm-cli/internal/db"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/jdanielnd/crm-cli/internal/waterfall"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fake answers with out (or fails with err) and counts calls.
type fake struct {
	out   string
	err   error
	price provider.Cost
	calls int
}

func (f *fake) Enrich(context.Context, provider.Input) (provider.Output, provider.Cost, error) {
	f.calls++
	if f.err != nil {
		return nil, 0, f.err
	}
	return provider.Output(f.out), f.price, nil
}
func (f *fake) Estimate(provider.Input) provider.Cost { return f.price }
func (f *fake) Key(in provider.Input) provider.Hash   { return provider.Hash(in.Value) }

func direct(ctx context.Context, s waterfall.Step, in provider.Input) (provider.Output, provider.Cost, int, error) {
	out, cost, err := s.Adapter.Enrich(ctx, in)
	return out, cost, 1, err
}

func newWaterfall(t *testing.T, steps ...waterfall.Step) *waterfall.Waterfall {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "crm.db"))
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	return &waterfall.Waterfall{Steps: steps, Cache: waterfall.NewCache(d)}
}

func TestRun_FallsThroughFailuresAndEmptyAnswers(t *testing.T) {
	bad := &fake{err: &provider.Error{Status: 500, Msg: "boom"}}
	empty := &fake{out: `{"company":null}`, price: 1}
	good := &fake{out: `{"company":"Acme"}`, price: 2}
	never := &fake{out: `{"company":"Other"}`, price: 4}
	wf := newWaterfall(t,
		waterfall.Step{Name: "bad", Adapter: bad},
		waterfall.Step{Name: "empty", Adapter: empty},
		waterfall.Step{Name: "good", Adapter: good},
		waterfall.Step{Name: "never", Adapter: never})
	ctx := context.Background()

	res, err := wf.Run(ctx, provider.Input{Value: "x"}, "company", direct)
	require.NoError(t, err)
	assert.Equal(t, "Acme", res.Value)
	assert.Equal(t, "good", res.Provider)
	assert.Equal(t, provider.Cost(3), res.Cost)
	assert.Equal(t, 3, res.Calls)
	assert.Equal(t, []string{"bad: HTTP 500: boom"}, res.Errors)
	assert.Zero(t, never.calls)

	// Re-run: the cached answer wins before anyone is called, even the
	// failing provider in front of it.
	res, err = wf.Run(ctx, provider.Input{Value: "x"}, "company", direct)
	require.NoError(t, err)
	assert.True(t, res.CacheHit)
	assert.Equal(t, "good", res.Provider)
	assert.Zero(t, res.Cost)
	assert.Equal(t, 1, bad.calls)
	assert.Equal(t, 1, empty.calls)
	assert.Equal(t, 1, good.calls)

	est, err := wf.Estimate(ctx, provider.Input{Value: "x"}, "company", true)
	require.NoError(t, err)
	assert.Equal(t, waterfall.Estimate{CacheHit: true}, est)
	est, err = wf.Estimate(ctx, provider.Input{Value: "y"}, "company", true)
	require.NoError(t, err)
	assert.Equal(t, waterfall.Estimate{Calls: 1, Expected: 0, Worst: 7}, est)
}

func TestRun_KnownEmptyIsNotCalledAgain(t *testing.T) {
	a := &fake{out: `[]`, price: 5}
	wf := newWaterfall(t, waterfall.Step{Name: "a", Adapter: a})
	for i := range 2 {
		res, err := wf.Run(context.Background(), provider.Input{Value: "x"}, "", direct)
		require.NoError(t, err)
		assert.False(t, res.Answered())
		assert.Empty(t, res.Errors)
		assert.Equal(t, i == 1, res.CacheHit, "second run knows the answer is empty from the cache")
	}
	assert.Equal(t, 1, a.calls)
	est, err := wf.Estimate(context.Background(), provider.Input{Value: "x"}, "", true)
	require.NoError(t, err)
	assert.Equal(t, waterfall.Estimate{CacheHit: true}, est, "nothing left to call")
}

func TestRun_HaltStopsFallthrough(t *testing.T) {
	a := &fake{out: `"x"`}
	b := &fake{out: `"y"`}
	wf := newWaterfall(t, waterfall.Step{Name: "a", Adapter: a}, waterfall.Step{Name: "b", Adapter: b})
	halt := fmt.Errorf("budget: %w", waterfall.ErrHalt)
	_, err := wf.Run(context.Background(), provider.Input{Value: "x"}, "",
		func(context.Context, waterfall.Step, provider.Input) (provider.Output, provider.Cost, int, error) {
			return nil, 0, 0, halt
		})
	assert.True(t, errors.Is(err, waterfall.ErrHalt))
	assert.Zero(t, a.calls+b.calls)
}

// Concurrent rows with the same input must pay for one call.
func TestRun_ConcurrentIdenticalInputsCallOnce(t *testing.T) {
	var calls atomic.Int32
	a := &gated{out: `{"company":"Acme"}`, calls: &calls, release: make(chan struct{})}
	wf := newWaterfall(t, waterfall.Step{Name: "a", Adapter: a})
	const n = 8
	var wg sync.WaitGroup
	results := make([]waterfall.Result, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := wf.Run(context.Background(), provider.Input{Value: "x"}, "company", direct)
			assert.NoError(t, err)
			results[i] = res
		}()
	}
	time.Sleep(100 * time.Millisecond) // let every row miss the cache and queue
	close(a.release)
	wg.Wait()
	assert.EqualValues(t, 1, calls.Load())
	paid := 0
	for _, r := range results {
		assert.Equal(t, "Acme", r.Value)
		if r.Cost > 0 {
			paid++
		}
	}
	assert.Equal(t, 1, paid)
}

// An interrupt after a paid call must not drop its answer from the cache.
func TestRun_PaidAnswerIsCachedDespiteCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := &fake{out: `{"company":"Acme"}`, price: 5}
	wf := newWaterfall(t, waterfall.Step{Name: "a", Adapter: a})
	res, err := wf.Run(ctx, provider.Input{Value: "x"}, "company",
		func(ctx context.Context, s waterfall.Step, in provider.Input) (provider.Output, provider.Cost, int, error) {
			out, cost, err := s.Adapter.Enrich(ctx, in)
			cancel() // interrupt lands right after the paid answer
			return out, cost, 1, err
		})
	require.NoError(t, err)
	assert.Equal(t, "Acme", res.Value)

	res, err = wf.Run(context.Background(), provider.Input{Value: "x"}, "company", direct)
	require.NoError(t, err)
	assert.True(t, res.CacheHit)
	assert.Equal(t, 1, a.calls)
}

type gated struct {
	out     string
	calls   *atomic.Int32
	release chan struct{}
}

func (g *gated) Enrich(context.Context, provider.Input) (provider.Output, provider.Cost, error) {
	g.calls.Add(1)
	<-g.release
	return provider.Output(g.out), 3, nil
}
func (g *gated) Estimate(provider.Input) provider.Cost { return 3 }
func (g *gated) Key(in provider.Input) provider.Hash   { return provider.Hash(in.Value) }
