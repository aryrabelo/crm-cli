package cli_test

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/jdanielnd/crm-cli/internal/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Design review P1-8: an API answering HTTP 200 with an error body had that
// body cached as the answer, and nothing could ever expire or clear it.
func TestRun_ErrorBodyIsNotCachedAndCacheClears(t *testing.T) {
	var broken atomic.Bool
	broken.Store(true)
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if broken.Load() {
			_, _ = w.Write([]byte(`{"error":"quota exceeded"}`))
			return
		}
		_, _ = w.Write([]byte(`{"company":"Acme for ` + r.URL.Query().Get("d") + `"}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crm.db")
	path := writeFile(t, dir, "leads.csv", "domain\nacme.io\nfoo.dev\n")
	_, stderr, code := crm(t, dbPath, "table", "import", path, "--as", "leads", "--key", "domain")
	require.Equal(t, 0, code, stderr)
	_, stderr, code = crm(t, dbPath, "provider", "set", "api", "--kind", "http",
		"--url", srv.URL+"/?d={{input}}", "--error-path", "error", "--price", "0.001", "--cache-ttl", "7d")
	require.Equal(t, 0, code, stderr)
	_, stderr, code = crm(t, dbPath, "column", "add", "leads", "company", "--type", "enrich",
		"--waterfall", "api", "--field", "company")
	require.Equal(t, 0, code, stderr)

	_, _, _ = crm(t, dbPath, "run", "leads", "--columns", "company", "--budget", "1")
	out, stderr, code := crm(t, dbPath, "run", "show", "1", "--items", "-f", "json")
	require.Equal(t, 0, code, stderr)
	for _, it := range jsonRows(t, out) {
		assert.Equal(t, "failed", it["status"])
		assert.Contains(t, it["error"], "error in response (error: quota exceeded)")
	}
	assert.Equal(t, 0, cacheRows(t, dbPath), "an error body is never cached")

	broken.Store(false)
	_, stderr, code = crm(t, dbPath, "run", "leads", "--columns", "company", "--budget", "1")
	require.Equal(t, 0, code, stderr)
	out, _, _ = crm(t, dbPath, "table", "export", "leads", "-f", "json")
	rows := jsonRows(t, out)
	require.Len(t, rows, 2)
	assert.Equal(t, "Acme for acme.io", rows[0]["company"])
	assert.Equal(t, int64(4), hits.Load())
	assert.Equal(t, 2, cacheRows(t, dbPath))

	out, stderr, code = crm(t, dbPath, "cache", "clear", "--provider", "api", "-f", "json")
	require.Equal(t, 0, code, stderr)
	assert.EqualValues(t, 2, jsonRows(t, out)[0]["deleted"])
	assert.Equal(t, 0, cacheRows(t, dbPath))

	_, stderr, code = crm(t, dbPath, "cache", "clear", "--older-than", "soon")
	assert.Equal(t, 2, code, stderr)
	_, stderr, code = crm(t, dbPath, "cache", "clear", "--provider", "nope")
	assert.Equal(t, 3, code, stderr)
}

func cacheRows(t *testing.T, dbPath string) int {
	t.Helper()
	d, err := db.Open(dbPath)
	require.NoError(t, err)
	defer d.Close()
	var n int
	require.NoError(t, d.QueryRow("SELECT COUNT(*) FROM enrichment_cache").Scan(&n))
	return n
}
