// Package provider defines the enrichment adapter contract, the provider
// configuration store and the built-in adapters (exa, jina, github, http).
//
// Adding a provider kind touches: the adapter file (a type implementing
// Adapter), Kinds and Settings in config.go, the kind's checks in
// Config.Validate, a case in Registry.Open (or Registry.OpenLLM), and the
// crm provider set flags in internal/cli.
package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Cost is an amount of money in micro-USD (1 USD = 1_000_000) so budget
// arithmetic is exact.
type Cost int64

// USD returns c in dollars.
func (c Cost) USD() float64 { return float64(c) / 1e6 }

// String formats c as dollars with up to six decimals ("$0.005").
func (c Cost) String() string {
	s := strconv.FormatFloat(c.USD(), 'f', 6, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimSuffix(s, ".")
	if !strings.Contains(s, ".") {
		s += ".00"
	} else if len(s)-strings.Index(s, ".") == 2 {
		s += "0"
	}
	return "$" + s
}

// ParseUSD parses a dollar amount ("2", "0.005", "$1.50") into a Cost.
func ParseUSD(s string) (Cost, error) {
	f, err := strconv.ParseFloat(strings.TrimPrefix(strings.TrimSpace(s), "$"), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid USD amount %q", s)
	}
	c, ok := microUSD(f)
	if !ok {
		return 0, fmt.Errorf("invalid USD amount %q", s)
	}
	return c, nil
}

// maxCost is the largest representable Cost; sums and products saturate at it.
const maxCost = Cost(math.MaxInt64)

// microUSD scales a dollar amount to micro-USD. ok is false for negative,
// non-finite and out-of-range amounts.
func microUSD(f float64) (c Cost, ok bool) {
	scaled := math.Round(f * 1e6)
	// float64(MaxInt64) rounds up to 2^63, which is itself out of range.
	if math.IsNaN(scaled) || scaled < 0 || scaled >= float64(math.MaxInt64) {
		return 0, false
	}
	return Cost(scaled), true
}

// mulCost returns n*c, saturating at maxCost. n and c are non-negative.
func mulCost(n int64, c Cost) Cost {
	if n <= 0 || c <= 0 {
		return 0
	}
	if int64(c) > math.MaxInt64/n {
		return maxCost
	}
	return Cost(n * int64(c))
}

// addCost returns a+b, saturating at maxCost. a and b are non-negative.
func addCost(a, b Cost) Cost {
	if a > maxCost-b {
		return maxCost
	}
	return a + b
}

// Hash identifies an input for caching: same hash, same provider → same answer.
type Hash string

// Input is what a column feeds a provider: the cell value of its source column.
type Input struct {
	Value string
}

// Output is the JSON a provider answered with, already narrowed to the useful
// part (e.g. the results array, not the response envelope). JSON null, "",
// {} and [] mean "no data".
type Output json.RawMessage

// Adapter is one enrichment provider.
type Adapter interface {
	// Enrich calls the provider. cost is what the call actually cost.
	Enrich(ctx context.Context, in Input) (Output, Cost, error)
	// Estimate is the cost Enrich will charge for in; it must not be lower.
	Estimate(in Input) Cost
	// Key identifies in (and the adapter settings that shape the answer).
	Key(in Input) Hash
}

// InputSized is implemented by adapters whose price grows with the size of
// the input (llm: prompt tokens). Their Estimate for an input that is not
// known yet (a column computed earlier in the same run) has no upper bound.
type InputSized interface {
	InputSized() bool
}

// PricedByInputSize reports whether a's estimate grows with the input size.
func PricedByInputSize(a Adapter) bool {
	s, ok := a.(InputSized)
	return ok && s.InputSized()
}

// Error is a provider failure. Retryable errors (429, 5xx, timeouts, network)
// may succeed on a later attempt; RetryAfter is the provider's hint, if any.
type Error struct {
	Status     int
	Retryable  bool
	RetryAfter time.Duration
	Msg        string
	// Sent is set when the request was written but no complete response
	// came back (timeout, connection reset, cancelled while waiting): the
	// provider may have processed, and billed, it.
	Sent bool
	// Idempotent is set when the request method is safe to repeat (GET,
	// HEAD, OPTIONS).
	Idempotent bool
	cause      error
}

func (e *Error) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Msg)
	}
	return e.Msg
}

// Unwrap returns the context error of a call cancelled after it was sent.
func (e *Error) Unwrap() error { return e.cause }

// Retryable reports whether err is worth another attempt and the delay the
// provider asked for (0 when it gave none).
func Retryable(err error) (bool, time.Duration) {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Retryable, pe.RetryAfter
	}
	return false, 0
}

// Unconfirmed reports whether err is a call that was sent without a
// confirmed outcome (see Error.Sent), and whether repeating it is safe.
func Unconfirmed(err error) (sent, idempotent bool) {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Sent, pe.Idempotent
	}
	return false, false
}

// hashKey derives a cache key from the adapter kind, its answer-shaping
// settings and the normalized input.
func hashKey(parts ...string) Hash {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return Hash(hex.EncodeToString(h.Sum(nil)))
}

var nullOutput = Output("null")
