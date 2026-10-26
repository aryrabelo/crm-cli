package run

import (
	"context"
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

const maxBackoff = 30 * time.Second

// backoff is the delay before retry number attempt (0-based): base·2^attempt,
// raised to the provider's Retry-After hint, capped at maxBackoff.
func backoff(base time.Duration, attempt int, hint time.Duration) time.Duration {
	d := base
	for range attempt {
		d *= 2
		if d >= maxBackoff {
			break
		}
	}
	d = max(d, hint)
	return min(d, maxBackoff)
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
