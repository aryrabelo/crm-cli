package cli_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMCPServeHelp(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "crm.db")
	stdout, _, code := crm(t, dbPath, "mcp", "serve", "--help")
	assert.Equal(t, 0, code)
	assert.Contains(t, stdout, "MCP server")
}

func TestMCPServe_HTTPFlags(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "crm.db")
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"non-loopback needs a token", []string{"--http", "0.0.0.0:0"}, "listens beyond this machine"},
		{"all interfaces needs a token", []string{"--http", ":0"}, "listens beyond this machine"},
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
