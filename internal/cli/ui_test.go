package cli_test

import (
	"bufio"
	"io"
	"net/http"
	"net/url"
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
