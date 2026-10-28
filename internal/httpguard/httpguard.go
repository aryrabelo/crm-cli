// Package httpguard protects the local HTTP servers of crm (the MCP
// streamable transport and the crm ui web server) against DNS rebinding,
// cross-site requests and oversized or slow bodies.
//
// Every request must:
//   - carry an allowlisted Host (on a loopback bind: 127.0.0.1, localhost or
//     [::1] with the bound port), so a page whose name was rebound to
//     127.0.0.1 is refused;
//   - carry no Origin, the opaque "null" Origin, an allowlisted Origin, or an
//     Origin naming the request's own Host;
//   - carry "Authorization: Bearer <token>" (Wrap: a token is always
//     required; only WrapPublic, for data-free static files, skips it).
//
// Host and Origin failures answer 403, a missing or wrong token 401. Bodies
// are capped at MaxBodyBytes.
package httpguard

import (
	"context"
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
// addr's port, plus addr's own IP when it is another loopback address
// (127.0.0.2).
func Loopback(token string, addr net.Addr) Config {
	_, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		port = ""
	}
	cfg := Config{Token: token}
	hosts := []string{"127.0.0.1", "localhost", "[::1]"}
	if host, _, err := net.SplitHostPort(addr.String()); err == nil {
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			h := ip.String()
			if ip.To4() == nil {
				h = "[" + h + "]"
			}
			if !slices.Contains(hosts, h) {
				hosts = append(hosts, h)
			}
		}
	}
	for _, h := range hosts {
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
	want := []byte(cfg.Token)
	return guard(cfg, func(w http.ResponseWriter, r *http.Request) bool {
		// The auth scheme is case-insensitive (RFC 7235); the token is not.
		scheme, got, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		if !strings.EqualFold(scheme, "Bearer") || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="crm"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return false
		}
		return true
	}, next), nil
}

// WrapPublic returns next behind the Host/Origin checks and the body cap of
// cfg, without the token. It is for responses that hold no data and that a
// browser must load before it has a token (the static files of a web UI). On
// a loopback bind (Hosts set) a page on a rebound or foreign Host cannot
// load them; on a non-loopback bind the Host is not checked and only a
// cross-origin Origin header is refused.
func WrapPublic(cfg Config, next http.Handler) http.Handler {
	return guard(cfg, nil, next)
}

// guard checks Host and Origin, then auth (when not nil), caps the body and
// calls next.
func guard(cfg Config, auth func(http.ResponseWriter, *http.Request) bool, next http.Handler) http.Handler {
	limit := cfg.MaxBodyBytes
	if limit <= 0 {
		limit = DefaultMaxBodyBytes
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(cfg.Hosts) > 0 && !slices.Contains(cfg.Hosts, strings.ToLower(r.Host)) {
			http.Error(w, "forbidden: unexpected Host", http.StatusForbidden)
			return
		}
		if !originAllowed(cfg.Origins, r) {
			http.Error(w, "forbidden: cross-origin request", http.StatusForbidden)
			return
		}
		if auth != nil && !auth(w, r) {
			return
		}
		if r.ContentLength > limit {
			http.Error(w, "request body too large (max "+strconv.FormatInt(limit, 10)+" bytes)", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
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
	return err == nil && u.Scheme == requestScheme(r) && strings.EqualFold(u.Host, r.Host)
}

// requestScheme is the scheme the client used: https on a TLS connection or,
// when a proxy in front terminated TLS, the X-Forwarded-Proto it set. A
// browser page cannot forge that header cross-origin (it forces a preflight).
func requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return "https"
	}
	return "http"
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

// Serve listens on addr and serves the handler that build returns for the
// listener's Config (ForListener: loopback Host/Origin allowlists on a
// loopback bind) until ctx is cancelled, then shuts down gracefully. token
// is required. ready (optional) receives the bound address once listening.
// Without WithTLS the transport is plain HTTP, so the bearer token crosses
// the network in clear text on a non-loopback bind.
func Serve(ctx context.Context, addr, token string, build func(Config) (http.Handler, error), ready func(net.Addr), opts ...ServeOption) error {
	var so serveOptions
	for _, o := range opts {
		o(&so)
	}
	if token == "" {
		return errors.New("httpguard: a bearer token is required")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	cfg := ForListener(token, ln)
	if so.certFile != "" {
		cfg = cfg.https()
	}
	h, err := build(cfg)
	if err != nil {
		_ = ln.Close()
		return err
	}
	srv := NewServer(h)
	if ready != nil {
		ready(ln.Addr())
	}
	errc := make(chan error, 1)
	go func() {
		if so.certFile != "" {
			errc <- srv.ServeTLS(ln, so.certFile, so.keyFile)
			return
		}
		errc <- srv.Serve(ln)
	}()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			// Graceful shutdown timed out: force-close what is still open
			// (streamable HTTP responses can outlive any deadline).
			_ = srv.Close()
			return err
		}
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// ServeOption configures Serve.
type ServeOption func(*serveOptions)

type serveOptions struct{ certFile, keyFile string }

// WithTLS serves HTTPS with the PEM certificate and key files; loopback
// Origin allowlists become https:// accordingly.
func WithTLS(certFile, keyFile string) ServeOption {
	return func(o *serveOptions) { o.certFile, o.keyFile = certFile, keyFile }
}

// https returns cfg with its allowlisted Origins on the https scheme.
func (c Config) https() Config {
	origins := make([]string, len(c.Origins))
	for i, o := range c.Origins {
		origins[i] = "https://" + strings.TrimPrefix(o, "http://")
	}
	c.Origins = origins
	return c
}
