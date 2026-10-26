package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxResponseBytes = 16 << 20

// call performs req and classifies the outcome. A 404 is not an error: it
// returns found=false so adapters can answer "no data" (and still charge).
// 408, 429 and 5xx are retryable, as are transport failures and timeouts.
func call(ctx context.Context, client *http.Client, req *http.Request) (body []byte, found bool, err error) {
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		return nil, false, &Error{Retryable: true, Msg: err.Error()}
	}
	defer resp.Body.Close()

	body, err = io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		return nil, false, &Error{Retryable: true, Msg: "read response: " + err.Error()}
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return body, true, nil
	case resp.StatusCode == http.StatusNotFound:
		return nil, false, nil
	}

	e := &Error{Status: resp.StatusCode, Msg: snippet(body)}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode == http.StatusRequestTimeout,
		resp.StatusCode >= 500:
		e.Retryable = true
		e.RetryAfter = retryAfter(resp.Header)
	case resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0":
		// GitHub signals an exhausted rate limit with 403 + remaining 0.
		e.Retryable = true
		e.RetryAfter = rateLimitReset(resp.Header)
	}
	return nil, false, e
}

func snippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	if s == "" {
		return "empty response"
	}
	return s
}

func retryAfter(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func rateLimitReset(h http.Header) time.Duration {
	secs, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64)
	if err != nil {
		return 0
	}
	if d := time.Until(time.Unix(secs, 0)); d > 0 {
		return d
	}
	return 0
}

// errMissingAuth is returned when a provider requires a credential that is
// not set; it is not retryable, so a waterfall falls through immediately.
func errMissingAuth(name, env string) error {
	return &Error{Msg: fmt.Sprintf("%s: environment variable %s is not set", name, env)}
}
