package run

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/jdanielnd/crm-cli/internal/provider"
)

// budget is the hard spending cap of a run. A call reserves its estimate
// before it is made and settles with the actual cost afterwards, so
// spent + reserved never exceeds limit. Safe for concurrent use.
type budget struct {
	mu       sync.Mutex
	limit    provider.Cost
	spent    provider.Cost
	reserved provider.Cost
}

func newBudget(limit, spent provider.Cost) *budget {
	return &budget{limit: limit, spent: spent}
}

// reserve claims c; false means the call would break the budget.
func (b *budget) reserve(c provider.Cost) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.spent+b.reserved+c > b.limit {
		return false
	}
	b.reserved += c
	return true
}

// settle releases a reservation and records what the call actually cost.
func (b *budget) settle(reserved, actual provider.Cost) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reserved -= reserved
	b.spent += actual
}

const (
	maxBackoff = 30 * time.Second
	// maxRetryAfter bounds a provider's Retry-After hint, which is honoured
	// beyond maxBackoff so a 429 is never retried before the server allows.
	maxRetryAfter = 10 * time.Minute
	jitterFrac    = 0.2
)

// jitter returns a factor in [1-jitterFrac, 1+jitterFrac]; a variable so
// tests can pin it.
var jitter = func() float64 { return 1 + jitterFrac*(2*rand.Float64()-1) }

// backoff is the delay before retry number attempt (0-based): base·2^attempt
// with ±20% jitter (so workers that hit the same 429 do not retry in
// lockstep), capped at maxBackoff, then raised to the provider's Retry-After
// hint (itself capped at maxRetryAfter).
func backoff(base time.Duration, attempt int, hint time.Duration) time.Duration {
	d := base
	for range attempt {
		d *= 2
		if d >= maxBackoff {
			break
		}
	}
	d = min(time.Duration(float64(d)*jitter()), maxBackoff)
	return max(d, min(hint, maxRetryAfter))
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
