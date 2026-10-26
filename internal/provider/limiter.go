package provider

import (
	"context"
	"sync"
	"time"
)

// Limiter spaces calls to one provider so they never exceed rate per second.
// A zero rate means unlimited. Safe for concurrent use.
type Limiter struct {
	interval time.Duration
	mu       sync.Mutex
	next     time.Time
}

// NewLimiter creates a Limiter allowing rate calls per second.
func NewLimiter(rate float64) *Limiter {
	if rate <= 0 {
		return &Limiter{}
	}
	return &Limiter{interval: time.Duration(float64(time.Second) / rate)}
}

// Wait blocks until the next call slot or until ctx is done.
func (l *Limiter) Wait(ctx context.Context) error {
	if l.interval == 0 {
		return ctx.Err()
	}
	l.mu.Lock()
	now := time.Now()
	slot := l.next
	if slot.Before(now) {
		slot = now
	}
	l.next = slot.Add(l.interval)
	l.mu.Unlock()

	d := time.Until(slot)
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
