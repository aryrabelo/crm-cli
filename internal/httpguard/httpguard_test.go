package httpguard_test

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jdanielnd/crm-cli/internal/httpguard"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8765": true, "localhost:8765": true, "[::1]:8765": true,
		":8765": false, "0.0.0.0:8765": false, "192.168.0.10:8765": false, "example.com:80": false, "garbage": false,
	} {
		assert.Equal(t, want, httpguard.IsLoopback(addr), addr)
	}
}

func TestNewToken_RandomAndLong(t *testing.T) {
	a, err := httpguard.NewToken()
	require.NoError(t, err)
	b, err := httpguard.NewToken()
	require.NoError(t, err)
	assert.NotEqual(t, a, b)
	assert.GreaterOrEqual(t, len(a), 43)
}

func TestWrap_RequiresToken(t *testing.T) {
	_, err := httpguard.Wrap(httpguard.Config{}, http.NotFoundHandler())
	require.Error(t, err)
}

func serve(t *testing.T, cfg func(net.Addr) httpguard.Config) *httptest.Server {
	t.Helper()
	hs := httptest.NewUnstartedServer(nil)
	h, err := httpguard.Wrap(cfg(hs.Listener.Addr()), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	require.NoError(t, err)
	hs.Config.Handler = h
	hs.Start()
	t.Cleanup(hs.Close)
	return hs
}

func do(t *testing.T, hs *httptest.Server, host, origin, auth, body string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, hs.URL, strings.NewReader(body))
	require.NoError(t, err)
	if host != "" {
		req.Host = host
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	return resp.StatusCode
}

func TestWrap_Loopback(t *testing.T) {
	hs := serve(t, func(a net.Addr) httpguard.Config { return httpguard.Loopback("tok", a) })
	_, port, _ := net.SplitHostPort(hs.Listener.Addr().String())
	ok := "Bearer tok"
	assert.Equal(t, http.StatusNoContent, do(t, hs, "", "", ok, "{}"))
	assert.Equal(t, http.StatusNoContent, do(t, hs, "LOCALHOST:"+port, "http://[::1]:"+port, ok, "{}"))
	assert.Equal(t, http.StatusUnauthorized, do(t, hs, "", "", "", "{}"))
	assert.Equal(t, http.StatusForbidden, do(t, hs, "evil.example:"+port, "", ok, "{}"))
	assert.Equal(t, http.StatusForbidden, do(t, hs, "127.0.0.1:1", "", ok, "{}"), "another port")
	assert.Equal(t, http.StatusForbidden, do(t, hs, "", "http://evil.example", ok, "{}"))
	assert.Equal(t, http.StatusForbidden, do(t, hs, "", "https://127.0.0.1:"+port+".evil.example", ok, "{}"))
}

func TestWrap_NonLoopbackAcceptsAnyHostButOnlySameOrigin(t *testing.T) {
	hs := serve(t, func(net.Addr) httpguard.Config { return httpguard.Config{Token: "tok"} })
	ok := "Bearer tok"
	assert.Equal(t, http.StatusNoContent, do(t, hs, "crm.lan:8765", "http://crm.lan:8765", ok, "{}"))
	assert.Equal(t, http.StatusForbidden, do(t, hs, "crm.lan:8765", "http://evil.example", ok, "{}"))
	assert.Equal(t, http.StatusUnauthorized, do(t, hs, "crm.lan:8765", "", "", "{}"))
}

func TestWrap_BodyLimit(t *testing.T) {
	hs := serve(t, func(a net.Addr) httpguard.Config {
		c := httpguard.Loopback("tok", a)
		c.MaxBodyBytes = 10
		return c
	})
	assert.Equal(t, http.StatusNoContent, do(t, hs, "", "", "Bearer tok", "0123456789"))
	assert.Equal(t, http.StatusRequestEntityTooLarge, do(t, hs, "", "", "Bearer tok", "0123456789A"))
}

func TestNewServer_Timeouts(t *testing.T) {
	s := httpguard.NewServer(http.NotFoundHandler())
	assert.Equal(t, httpguard.ReadHeaderTimeout, s.ReadHeaderTimeout)
	assert.Equal(t, httpguard.ReadTimeout, s.ReadTimeout)
	assert.Equal(t, httpguard.IdleTimeout, s.IdleTimeout)
}
