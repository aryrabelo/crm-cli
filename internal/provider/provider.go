// Package provider defines the enrichment adapter contract, the provider
// configuration store and the built-in adapters (exa, jina, github, http).
//
// A new provider is one file: a type implementing Adapter plus a case in
// Registry.Open.
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
	if err != nil || f < 0 || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, fmt.Errorf("invalid USD amount %q", s)
	}
	return Cost(math.Round(f * 1e6)), nil
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

// Error is a provider failure. Retryable errors (429, 5xx, timeouts, network)
// may succeed on a later attempt; RetryAfter is the provider's hint, if any.
type Error struct {
	Status     int
	Retryable  bool
	RetryAfter time.Duration
	Msg        string
}

func (e *Error) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Msg)
	}
	return e.Msg
}

// Retryable reports whether err is worth another attempt and the delay the
// provider asked for (0 when it gave none).
func Retryable(err error) (bool, time.Duration) {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Retryable, pe.RetryAfter
	}
	return false, 0
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
