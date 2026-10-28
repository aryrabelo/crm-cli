package cli_test

import (
	"bufio"
	"io"
	"net/http"
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

func TestMCPServeHelp(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "crm.db")
	stdout, _, code := crm(t, dbPath, "mcp", "serve", "--help")
	assert.Equal(t, 0, code)
	assert.Contains(t, stdout, "MCP server")
}

func TestMCPServe_HTTPFlags(t *testing.T) {
	t.Setenv("CRM_MCP_TOKEN", "") // an inherited token would let the non-loopback cases start a server
	dbPath := filepath.Join(t.TempDir(), "crm.db")
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"non-loopback needs a token", []string{"--http", "0.0.0.0:0"}, "listens beyond this machine"},
		{"all interfaces needs a token", []string{"--http", ":0"}, "listens beyond this machine"},
		{"short token beyond loopback", []string{"--http", "0.0.0.0:0", "--token", "x"}, "at least 32 characters"},
		{"token needs http", []string{"--token", "x"}, "--token needs --http"},
		{"bad address", []string{"--http", "8765"}, "want HOST:PORT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, code := crm(t, dbPath, append([]string{"mcp", "serve"}, tc.args...)...)
			assert.Equal(t, 2, code)
			assert.Contains(t, stderr, tc.want)
		})
	}
}

// Design review P0-1: without --token a loopback HTTP server is not open —
// it generates a token, prints it on stderr, and refuses requests without it
// or from a foreign Host/Origin.
func TestMCPServe_HTTPGeneratesToken(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "crm.db")
	cmd := exec.Command(binaryPath, "--db", dbPath, "mcp", "serve", "--http", "127.0.0.1:0")
	cmd.Env = []string{"PATH=/usr/bin:/bin"} // no CRM_MCP_TOKEN
	stderr, err := cmd.StderrPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Signal(syscall.SIGTERM); _ = cmd.Wait() })

	addrRe := regexp.MustCompile(`on http://(127\.0\.0\.1:\d+)/mcp`)
	tokenRe := regexp.MustCompile(`Authorization: Bearer (\S+)`)
	var addr, token string
	lines := bufio.NewScanner(stderr)
	deadline := time.After(10 * time.Second)
	found := make(chan struct{})
	go func() {
		for lines.Scan() {
			if m := addrRe.FindStringSubmatch(lines.Text()); m != nil {
				addr = m[1]
			}
			if m := tokenRe.FindStringSubmatch(lines.Text()); m != nil {
				token = m[1]
				close(found)
				_, _ = io.Copy(io.Discard, stderr)
				return
			}
		}
	}()
	select {
	case <-found:
	case <-deadline:
		t.Fatal("no generated token on stderr")
	}
	require.NotEmpty(t, addr)
	require.GreaterOrEqual(t, len(token), 43)

	_, port, _ := strings.Cut(addr, ":")
	send := func(host, origin, auth string) int {
		req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/mcp", strings.NewReader(
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
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
	assert.Equal(t, http.StatusUnauthorized, send("", "", ""))
	assert.Equal(t, http.StatusForbidden, send("", "http://evil.example", "Bearer "+token))
	assert.Equal(t, http.StatusForbidden, send("evil.example:"+port, "", "Bearer "+token))
	assert.Equal(t, http.StatusOK, send("", "", "Bearer "+token))
}
