package httpguard_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestWrap_BodyLimitChunked(t *testing.T) {
	hs := serve(t, func(a net.Addr) httpguard.Config {
		c := httpguard.Loopback("tok", a)
		c.MaxBodyBytes = 10
		return c
	})
	// An io.MultiReader has no known length, so the client sends the body
	// chunked and only the MaxBytesReader can stop it.
	post := func(body string) int {
		req, err := http.NewRequest(http.MethodPost, hs.URL, io.MultiReader(strings.NewReader(body)))
		require.NoError(t, err)
		require.EqualValues(t, 0, req.ContentLength)
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		return resp.StatusCode
	}
	assert.Equal(t, http.StatusNoContent, post("0123456789"))
	assert.Equal(t, http.StatusRequestEntityTooLarge, post("0123456789A"))
}

func TestWrap_BearerSchemeIsCaseInsensitive(t *testing.T) {
	hs := serve(t, func(a net.Addr) httpguard.Config { return httpguard.Loopback("tok", a) })
	assert.Equal(t, http.StatusNoContent, do(t, hs, "", "", "bearer tok", "{}"))
	assert.Equal(t, http.StatusNoContent, do(t, hs, "", "", "BEARER tok", "{}"))
	assert.Equal(t, http.StatusUnauthorized, do(t, hs, "", "", "Bearer TOK", "{}"), "the token is case-sensitive")
	assert.Equal(t, http.StatusUnauthorized, do(t, hs, "", "", "Basic tok", "{}"))
	assert.Equal(t, http.StatusUnauthorized, do(t, hs, "", "", "tok", "{}"))
}

func TestWrap_UnauthorizedChallenge(t *testing.T) {
	hs := serve(t, func(a net.Addr) httpguard.Config { return httpguard.Loopback("tok", a) })
	for _, auth := range []string{"", "Bearer nope"} {
		req, err := http.NewRequest(http.MethodPost, hs.URL, strings.NewReader("{}"))
		require.NoError(t, err)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.True(t, strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Bearer"), auth)
	}
}

func TestLoopback_OtherLoopbackAddressIsAccepted(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skipf("127.0.0.2 not bindable: %v", err)
	}
	hs := httptest.NewUnstartedServer(nil)
	hs.Listener.Close()
	hs.Listener = ln
	h, err := httpguard.Wrap(httpguard.ForListener("tok", ln), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	require.NoError(t, err)
	hs.Config.Handler = h
	hs.Start()
	t.Cleanup(hs.Close)
	assert.Equal(t, http.StatusNoContent, do(t, hs, "", "http://"+ln.Addr().String(), "Bearer tok", "{}"))
}

func TestWrap_SameHostOriginMustMatchScheme(t *testing.T) {
	hs := serve(t, func(net.Addr) httpguard.Config { return httpguard.Config{Token: "tok"} })
	ok := "Bearer tok"
	assert.Equal(t, http.StatusForbidden, do(t, hs, "crm.lan:8765", "https://crm.lan:8765", ok, "{}"), "plain listener, https origin")
}

func TestWrapPublic_ChecksHostAndOriginButNoToken(t *testing.T) {
	hs := httptest.NewUnstartedServer(nil)
	hs.Config.Handler = httpguard.WrapPublic(httpguard.Loopback("tok", hs.Listener.Addr()),
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	hs.Start()
	t.Cleanup(hs.Close)
	_, port, _ := net.SplitHostPort(hs.Listener.Addr().String())
	assert.Equal(t, http.StatusNoContent, do(t, hs, "", "", "", ""))
	assert.Equal(t, http.StatusNoContent, do(t, hs, "localhost:"+port, "", "", ""))
	assert.Equal(t, http.StatusForbidden, do(t, hs, "attacker:"+port, "", "", ""))
	assert.Equal(t, http.StatusForbidden, do(t, hs, "", "https://evil.example", "", ""))
}

func TestNewServer_Timeouts(t *testing.T) {
	s := httpguard.NewServer(http.NotFoundHandler())
	assert.Equal(t, httpguard.ReadHeaderTimeout, s.ReadHeaderTimeout)
	assert.Equal(t, httpguard.ReadTimeout, s.ReadTimeout)
	assert.Equal(t, httpguard.IdleTimeout, s.IdleTimeout)
}

func selfSigned(t *testing.T) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	pool = x509.NewCertPool()
	pool.AddCert(leaf)
	return certFile, keyFile, pool
}

func TestServe_WithTLS(t *testing.T) {
	certFile, keyFile, pool := selfSigned(t)
	ctx, cancel := context.WithCancel(context.Background())
	addrc := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() {
		done <- httpguard.Serve(ctx, "127.0.0.1:0", "tok", func(cfg httpguard.Config) (http.Handler, error) {
			return httpguard.Wrap(cfg, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
		}, func(a net.Addr) { addrc <- a }, httpguard.WithTLS(certFile, keyFile))
	}()
	addr := <-addrc
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	req, err := http.NewRequest(http.MethodPost, "https://"+addr.String(), strings.NewReader("{}"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Origin", "https://"+addr.String())
	resp, err := client.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	cancel()
	require.NoError(t, <-done)
}
