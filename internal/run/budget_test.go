package run

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestBackoff_HintAboveCapIsHonoured(t *testing.T) {
	old := jitter
	jitter = func() float64 { return 1 }
	t.Cleanup(func() { jitter = old })

	assert.Equal(t, 90*time.Second, backoff(time.Second, 0, 90*time.Second), "Retry-After beyond 30s is not truncated")
	assert.Equal(t, maxBackoff, backoff(time.Second, 20, 0), "the exponential part is capped")
	assert.Equal(t, maxRetryAfter, backoff(time.Second, 0, time.Hour), "absurd hints are bounded")
	assert.Equal(t, 4*time.Second, backoff(time.Second, 2, time.Second))
}

func TestBackoff_JitterStaysWithinBounds(t *testing.T) {
	for range 200 {
		d := backoff(time.Second, 1, 0)
		assert.GreaterOrEqual(t, d, 1600*time.Millisecond)
		assert.LessOrEqual(t, d, 2400*time.Millisecond)
	}
}
