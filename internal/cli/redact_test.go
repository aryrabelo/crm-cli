package cli_test

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/jdanielnd/crm-cli/internal/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// codebase-details M4: an http provider with ?key= in its URL template must
// not leak the key through item errors — neither in run_items.error nor in
// crm run show --items — whether the call fails in transport (Go's error
// embeds the full URL) or the provider echoes the key in its error body.
func TestRun_ItemErrorsNeverLeakCredentials(t *testing.T) {
	const secret = "SECRET-KEY-123"
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid key ` + r.URL.Query().Get("key") + `"}`))
	}))
	defer echo.Close()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crm.db")
	path := writeFile(t, dir, "leads.csv", "domain\nacme.io\nbluth.shop\n")
	steps := [][]string{
		{"table", "import", path, "--as", "leads", "--key", "domain"},
		{"provider", "set", "down", "--kind", "http", "--url", "http://127.0.0.1:1/find?key=" + secret + "&q={{input}}"},
		{"provider", "set", "echo", "--kind", "http", "--url", echo.URL + "/find?key=" + secret + "&q={{input}}"},
		{"column", "add", "leads", "email", "--type", "enrich", "--waterfall", "down,echo", "--from", "domain"},
	}
	for _, args := range steps {
		_, stderr, code := crm(t, dbPath, args...)
		require.Equal(t, 0, code, "%v: %s", args, stderr)
	}
	_, stderr, _ := crm(t, dbPath, "run", "leads", "--columns", "email", "--retry", "0")
	assert.NotContains(t, stderr, secret)

	out, stderr, code := crm(t, dbPath, "run", "show", "1", "--items", "-f", "json")
	require.Equal(t, 0, code, stderr)
	items := jsonRows(t, out)
	require.Len(t, items, 2)
	for _, it := range items {
		assert.Equal(t, "failed", it["status"])
		assert.Contains(t, it["error"], "down: ")
		assert.Contains(t, it["error"], "echo: HTTP 401")
	}
	assert.NotContains(t, out, secret)

	d, err := db.Open(dbPath)
	require.NoError(t, err)
	defer d.Close()
	var n int
	require.NoError(t, d.QueryRow("SELECT COUNT(*) FROM run_items WHERE error LIKE ?", "%"+secret+"%").Scan(&n))
	assert.Zero(t, n, "run_items.error holds the key")
	require.NoError(t, d.QueryRow("SELECT COUNT(*) FROM run_items WHERE error LIKE '%<redacted>%'").Scan(&n))
	assert.Equal(t, 2, n)
}
