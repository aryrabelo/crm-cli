// Package httpguard protects the local HTTP servers of crm (the MCP
// streamable transport, and any later browser-facing server) against DNS
// rebinding, cross-site requests and oversized or slow bodies.
//
// Every request must:
//   - carry an allowlisted Host (on a loopback bind: 127.0.0.1, localhost or
//     [::1] with the bound port), so a page whose name was rebound to
//     127.0.0.1 is refused;
//   - carry no Origin, the opaque "null" Origin, an allowlisted Origin, or an
//     Origin naming the request's own Host;
//   - carry "Authorization: Bearer <token>" (a token is always required).
//
// Host and Origin failures answer 403, a missing or wrong token 401. Bodies
// are capped at MaxBodyBytes.
package httpguard

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DefaultMaxBodyBytes caps request bodies when Config.MaxBodyBytes is 0.
const DefaultMaxBodyBytes = 16 << 20

// Timeouts applied by NewServer. There is no write timeout: streamable HTTP
// responses may stay open while a tool runs.
const (
	ReadHeaderTimeout = 10 * time.Second
	ReadTimeout       = 60 * time.Second
	IdleTimeout       = 120 * time.Second
)

// Config is what a guarded handler accepts.
type Config struct {
	// Token is the bearer token every request must present. Required.
	Token string
	// Hosts are the accepted Host header values ("127.0.0.1:8765"). Empty
	// accepts any Host (a non-loopback bind, protected by the token).
	Hosts []string
	// Origins are accepted Origin values ("http://localhost:8765") besides
	// the request's own Host.
	Origins []string
	// MaxBodyBytes caps the request body (0: DefaultMaxBodyBytes).
	MaxBodyBytes int64
}

// Loopback is the Config of a server listening on the loopback address addr:
// the Host and Origin allowlists are 127.0.0.1, localhost and [::1] with
// addr's port.
func Loopback(token string, addr net.Addr) Config {
	_, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		port = ""
	}
	cfg := Config{Token: token}
	for _, h := range []string{"127.0.0.1", "localhost", "[::1]"} {
		cfg.Hosts = append(cfg.Hosts, h+":"+port)
		cfg.Origins = append(cfg.Origins, "http://"+h+":"+port)
		if port == "80" {
			cfg.Hosts = append(cfg.Hosts, h)
			cfg.Origins = append(cfg.Origins, "http://"+h)
		}
	}
	return cfg
}

// ForListener is the Config for a server on ln: Loopback when ln is bound to
// a loopback address, otherwise token-only (any Host, same-origin Origins).
func ForListener(token string, ln net.Listener) Config {
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok && tcp.IP.IsLoopback() {
		return Loopback(token, ln.Addr())
	}
	return Config{Token: token}
}

// IsLoopback reports whether addr (HOST:PORT) names only this machine: a
// loopback IP or "localhost".
func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// NewToken returns a random bearer token (256 bits, URL-safe).
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Wrap returns next behind the guard. An empty token is an error: the guard
// never serves unauthenticated requests.
func Wrap(cfg Config, next http.Handler) (http.Handler, error) {
	if cfg.Token == "" {
		return nil, errors.New("httpguard: a bearer token is required")
	}
	limit := cfg.MaxBodyBytes
	if limit <= 0 {
		limit = DefaultMaxBodyBytes
	}
	want := []byte("Bearer " + cfg.Token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(cfg.Hosts) > 0 && !slices.Contains(cfg.Hosts, strings.ToLower(r.Host)) {
			http.Error(w, "forbidden: unexpected Host", http.StatusForbidden)
			return
		}
		if !originAllowed(cfg.Origins, r) {
			http.Error(w, "forbidden: cross-origin request", http.StatusForbidden)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="crm"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.ContentLength > limit {
			http.Error(w, "request body too large (max "+strconv.FormatInt(limit, 10)+" bytes)", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	}), nil
}

// originAllowed accepts requests without Origin (non-browser clients), the
// opaque "null" Origin, allowlisted Origins and same-origin requests.
func originAllowed(allowed []string, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || origin == "null" {
		return true
	}
	if slices.Contains(allowed, strings.ToLower(origin)) {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && strings.EqualFold(u.Host, r.Host)
}

// NewServer is an http.Server for h with the package's timeouts.
func NewServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: ReadHeaderTimeout,
		ReadTimeout:       ReadTimeout,
		IdleTimeout:       IdleTimeout,
	}
}
