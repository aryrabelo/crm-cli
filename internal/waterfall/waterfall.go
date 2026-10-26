// Package waterfall runs an ordered list of providers for one input: the
// first non-empty answer wins, failures and empty answers fall through, and
// every answer is cached by (provider, input hash) so a re-run costs nothing.
package waterfall

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/jdanielnd/crm-cli/internal/db/repo"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/jdanielnd/crm-cli/internal/value"
)

// ErrHalt marks errors that must stop the waterfall instead of falling
// through to the next provider (an exhausted budget, for example).
var ErrHalt = errors.New("waterfall halted")

// Step is one provider of a waterfall.
type Step struct {
	Name    string
	Adapter provider.Adapter
}

// CallFunc performs one (possibly retried) provider call and reports the
// number of attempts it made. The run package supplies it to add budget
// reservation, rate limiting and retry/backoff around Adapter.Enrich.
type CallFunc func(ctx context.Context, step Step, in provider.Input) (out provider.Output, cost provider.Cost, attempts int, err error)

// Waterfall is an ordered provider list sharing one cache.
type Waterfall struct {
	Steps []Step
	Cache *Cache
}

// Result is the outcome of running a waterfall for one input.
type Result struct {
	// Value is the answer selected by the field path; nil when nobody answered.
	Value any
	// Provider is the provider that answered ("" when nobody did).
	Provider string
	// CacheHit is true when the outcome came from the cache alone: a cached
	// answer, or every provider cached as empty. No provider was called.
	CacheHit bool
	// Cost is what the calls made for this input cost (cache hits cost 0).
	Cost provider.Cost
	// Calls counts provider calls (not cache hits); Attempts includes retries.
	Calls    int
	Attempts int
	// Errors holds "provider: error" for every provider that failed.
	Errors []string
}

// Answered reports whether some provider produced a non-empty value.
func (r *Result) Answered() bool { return r.Provider != "" }

// selectField decodes out and picks field from it.
func selectField(out provider.Output, field string) (any, error) {
	doc, err := value.Decode(out)
	if err != nil {
		return nil, fmt.Errorf("decode answer: %w", err)
	}
	v, _ := value.Lookup(doc, field)
	return v, nil
}

// Run walks the waterfall. Cached answers are consulted for every provider
// before any money is spent: the first cached non-empty answer wins, and
// providers with a cached empty answer are not called again. Then uncached
// providers are called in order. The returned error is non-nil only when the
// waterfall halted (ErrHalt, context cancellation) or the cache failed; a
// result with no answer and provider errors is reported through Result.
func (w *Waterfall) Run(ctx context.Context, in provider.Input, field string, call CallFunc) (Result, error) {
	var res Result
	keys := make([]provider.Hash, len(w.Steps))
	knownEmpty := make([]bool, len(w.Steps))
	for i, s := range w.Steps {
		keys[i] = s.Adapter.Key(in)
		out, ok, err := w.Cache.Get(ctx, s.Name, keys[i])
		if err != nil {
			return res, err
		}
		if !ok {
			continue
		}
		v, err := selectField(out, field)
		if err != nil || value.Empty(v) {
			knownEmpty[i] = true
			continue
		}
		res.Value, res.Provider, res.CacheHit = v, s.Name, true
		return res, nil
	}
	if len(w.Steps) > 0 && !slices.Contains(knownEmpty, false) {
		res.CacheHit = true // every provider already answered "no data"
		return res, nil
	}

	for i, s := range w.Steps {
		if knownEmpty[i] {
			continue
		}
		out, cost, attempts, err := call(ctx, s, in)
		res.Cost += cost
		res.Attempts += attempts
		if attempts > 0 {
			res.Calls++
		}
		if err != nil {
			if errors.Is(err, ErrHalt) || ctx.Err() != nil {
				return res, err
			}
			res.Errors = append(res.Errors, s.Name+": "+err.Error())
			continue
		}
		v, err := selectField(out, field)
		if err != nil {
			res.Errors = append(res.Errors, s.Name+": "+err.Error())
			continue
		}
		if err := w.Cache.Put(ctx, s.Name, keys[i], out, cost); err != nil {
			return res, err
		}
		if !value.Empty(v) {
			res.Value, res.Provider = v, s.Name
			return res, nil
		}
	}
	return res, nil
}

// Estimate is the projected cost of running a waterfall for one input.
type Estimate struct {
	// CacheHit: the outcome is fully known from the cache (a cached
	// non-empty answer, or every provider cached as empty); nothing is spent.
	CacheHit bool
	// Calls is 1 when at least one provider would be called.
	Calls int
	// Expected is the cost if the first provider to be called answers.
	Expected provider.Cost
	// Worst is the cost if every uncached provider has to be called.
	Worst provider.Cost
}

// Estimate projects what Run would spend for in without calling anyone.
// With known=false the input is not available yet (it comes from a column
// computed earlier in the same run), so the cache cannot be consulted.
func (w *Waterfall) Estimate(ctx context.Context, in provider.Input, field string, known bool) (Estimate, error) {
	var est Estimate
	knownEmpty := make([]bool, len(w.Steps))
	if known {
		for i, s := range w.Steps {
			out, ok, err := w.Cache.Get(ctx, s.Name, s.Adapter.Key(in))
			if err != nil {
				return est, err
			}
			if !ok {
				continue
			}
			if v, err := selectField(out, field); err == nil && !value.Empty(v) {
				return Estimate{CacheHit: true}, nil
			}
			knownEmpty[i] = true
		}
		if len(w.Steps) > 0 && !slices.Contains(knownEmpty, false) {
			return Estimate{CacheHit: true}, nil
		}
	}
	for i, s := range w.Steps {
		if knownEmpty[i] {
			continue
		}
		c := s.Adapter.Estimate(in)
		if est.Calls == 0 {
			est.Calls = 1
			est.Expected = c
		}
		est.Worst += c
	}
	return est, nil
}

// Cache stores provider answers in the enrichment_cache table.
type Cache struct {
	db repo.DBTX
}

// NewCache creates a Cache.
func NewCache(db repo.DBTX) *Cache { return &Cache{db: db} }

// Get returns the cached answer of provider for key.
func (c *Cache) Get(ctx context.Context, providerName string, key provider.Hash) (provider.Output, bool, error) {
	var out string
	err := c.db.QueryRowContext(ctx,
		"SELECT output FROM enrichment_cache WHERE provider = ? AND input_hash = ?", providerName, string(key)).Scan(&out)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read enrichment cache: %w", err)
	}
	return provider.Output(out), true, nil
}

// Put stores (or refreshes) an answer.
func (c *Cache) Put(ctx context.Context, providerName string, key provider.Hash, out provider.Output, cost provider.Cost) error {
	_, err := c.db.ExecContext(ctx,
		`INSERT INTO enrichment_cache (provider, input_hash, output, cost_micros) VALUES (?, ?, ?, ?)
		 ON CONFLICT(provider, input_hash) DO UPDATE SET output = excluded.output,
		   cost_micros = excluded.cost_micros, fetched_at = datetime('now')`,
		providerName, string(key), string(out), cost)
	if err != nil {
		return fmt.Errorf("write enrichment cache: %w", err)
	}
	return nil
}
