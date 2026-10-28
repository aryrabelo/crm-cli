package cli_test

import (
	"bufio"
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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUI_AddrFlags(t *testing.T) {
	t.Setenv("CRM_UI_TOKEN", "") // an inherited token would start a server in the non-loopback cases
	dbPath := filepath.Join(t.TempDir(), "crm.db")
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"non-loopback needs a token", []string{"--addr", "0.0.0.0:8787"}, "listens beyond this machine"},
		{"all interfaces needs a token", []string{"--addr", ":0"}, "listens beyond this machine"},
		{"short token beyond loopback", []string{"--addr", "0.0.0.0:8787", "--token", "x"}, "at least 32 characters"},
		{"bad address", []string{"--addr", "8787"}, "want HOST:PORT"},
		{"plain http beyond loopback", []string{"--addr", "0.0.0.0:0", "--token", strings.Repeat("a", 32)}, "clear text"},
		{"key without cert", []string{"--tls-key", "k.pem"}, "go together"},
		{"unreadable tls files", []string{"--tls-cert", "nope.pem", "--tls-key", "nope.key"}, "--tls-cert/--tls-key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, code := crm(t, dbPath, append([]string{"ui"}, tc.args...)...)
			assert.Equal(t, 2, code)
			assert.Contains(t, stderr, tc.want)
		})
	}
}

// crm ui on loopback without --token generates one and prints it once, in
// the fragment of the page URL; the page loads without it, the API does not.
func TestUI_GeneratesTokenAndPrintsURL(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "crm.db")
	cmd := exec.Command(binaryPath, "--db", dbPath, "ui", "--addr", "127.0.0.1:0")
	cmd.Env = []string{"PATH=/usr/bin:/bin"} // no CRM_UI_TOKEN
	stderr, err := cmd.StderrPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Signal(syscall.SIGTERM); _ = cmd.Wait() })

	urlRe := regexp.MustCompile(`(http://127\.0\.0\.1:\d+)/#token=(\S+)`)
	found := make(chan []string, 1)
	go func() {
		lines := bufio.NewScanner(stderr)
		for lines.Scan() {
			if m := urlRe.FindStringSubmatch(lines.Text()); m != nil {
				found <- m
				_, _ = io.Copy(io.Discard, stderr)
				return
			}
		}
	}()
	var m []string
	select {
	case m = <-found:
	case <-time.After(10 * time.Second):
		t.Fatal("no page URL on stderr")
	}
	base := m[1]
	token, err := url.QueryUnescape(m[2])
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(token), 43)

	get := func(path, auth string) int {
		req, err := http.NewRequest(http.MethodGet, base+path, nil)
		require.NoError(t, err)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		return resp.StatusCode
	}
	assert.Equal(t, http.StatusOK, get("/", ""))
	assert.Equal(t, http.StatusUnauthorized, get("/api/tables", ""))
	assert.Equal(t, http.StatusOK, get("/api/tables", "Bearer "+token))
}

// A custom token with a space is printed as %20, which the page's
// decodeURIComponent turns back into a space ("+" would stay a plus).
func TestUI_PageURLEncodesSpacesAsPercent20(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "crm.db")
	cmd := exec.Command(binaryPath, "--db", dbPath, "ui", "--addr", "127.0.0.1:0", "--token", "my secret+token")
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	stderr, err := cmd.StderrPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Signal(syscall.SIGTERM); _ = cmd.Wait() })

	found := make(chan string, 1)
	go func() {
		lines := bufio.NewScanner(stderr)
		for lines.Scan() {
			if _, tok, ok := strings.Cut(lines.Text(), "/#token="); ok {
				found <- tok
				_, _ = io.Copy(io.Discard, stderr)
				return
			}
		}
	}()
	select {
	case tok := <-found:
		assert.Equal(t, "my%20secret%2Btoken", strings.TrimSpace(tok))
	case <-time.After(10 * time.Second):
		t.Fatal("no page URL on stderr")
	}
}

// Cubic #4381117047: --tls-cert/--tls-key serve the page over HTTPS.
func TestUI_ServesHTTPSWithCertificate(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := selfSignedCert(t, dir)
	dbPath := filepath.Join(dir, "crm.db")
	cmd := exec.Command(binaryPath, "--db", dbPath, "ui", "--addr", "127.0.0.1:0", "--tls-cert", certPath, "--tls-key", keyPath)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	stderr, err := cmd.StderrPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Signal(syscall.SIGTERM); _ = cmd.Wait() })

	urlRe := regexp.MustCompile(`(https://127\.0\.0\.1:\d+)/#token=(\S+)`)
	found := make(chan []string, 1)
	go func() {
		lines := bufio.NewScanner(stderr)
		for lines.Scan() {
			if m := urlRe.FindStringSubmatch(lines.Text()); m != nil {
				found <- m
				_, _ = io.Copy(io.Discard, stderr)
				return
			}
		}
	}()
	var m []string
	select {
	case m = <-found:
	case <-time.After(10 * time.Second):
		t.Fatal("no https page URL on stderr")
	}
	token, err := url.QueryUnescape(m[2])
	require.NoError(t, err)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // self-signed test certificate
	req, err := http.NewRequest(http.MethodGet, m[1]+"/api/tables", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// selfSignedCert writes a throwaway certificate for 127.0.0.1 and its key.
func selfSignedCert(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "crm-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	certPath, keyPath = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
	return certPath, keyPath
}
