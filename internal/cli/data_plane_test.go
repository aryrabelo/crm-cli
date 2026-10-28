package cli_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// crmEnv runs the binary with extra environment variables.
func crmEnv(t *testing.T, env []string, dbPath string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(binaryPath, append([]string{"--db", dbPath}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		exitErr, ok := err.(*exec.ExitError)
		require.True(t, ok, "unexpected error: %v", err)
		code = exitErr.ExitCode()
	}
	return stdout.String(), stderr.String(), code
}

func jsonRows(t *testing.T, s string) []map[string]any {
	t.Helper()
	var out []map[string]any
	require.NoError(t, json.Unmarshal([]byte(s), &out), s)
	return out
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	return p
}

func TestTable_ImportShowExportFilter(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crm.db")
	src := "login,score,note\nzed,40,\"a, b\"\namy,80,\nbob,70,ok\n"
	path := writeFile(t, dir, "leads.csv", src)

	out, stderr, code := crm(t, dbPath, "table", "import", path, "--as", "leads", "--key", "login", "-f", "json")
	require.Equal(t, 0, code, stderr)
	assert.EqualValues(t, 3, jsonRows(t, out)[0]["created"])

	out, _, code = crm(t, dbPath, "table", "export", "leads")
	require.Equal(t, 0, code)
	assert.Equal(t, src, out, "rows keep the original order; csv is the default")

	out, _, code = crm(t, dbPath, "table", "export", "leads", "--only", "score>=70", "-f", "tsv")
	require.Equal(t, 0, code)
	assert.Equal(t, "login\tscore\tnote\namy\t80\t\nbob\t70\tok\n", out)

	out, _, code = crm(t, dbPath, "table", "export", "leads", "--only", "note!=", "--only", "login~^b")
	require.Equal(t, 0, code)
	assert.Equal(t, "login,score,note\nbob,70,ok\n", out)

	_, stderr, code = crm(t, dbPath, "column", "add", "leads", "label", "--type", "formula", "--expr", "{{login}}={{score}}")
	require.Equal(t, 0, code, stderr)
	out, _, code = crm(t, dbPath, "table", "export", "leads", "-f", "json", "--only", "label=amy=80")
	require.Equal(t, 0, code)
	assert.Equal(t, "[\n  {\"login\": \"amy\", \"score\": \"80\", \"note\": \"\", \"label\": \"amy=80\"}\n]\n", out)

	// Re-import with the key updates rows instead of duplicating them.
	path2 := writeFile(t, dir, "more.csv", "login,score\namy,95\ncy,10\n")
	out, stderr, code = crm(t, dbPath, "table", "import", path2, "--as", "leads", "-f", "json")
	require.Equal(t, 0, code, stderr)
	assert.EqualValues(t, 1, jsonRows(t, out)[0]["updated"])

	out, _, code = crm(t, dbPath, "table", "show", "leads", "-f", "json")
	require.Equal(t, 0, code)
	rows := jsonRows(t, out)
	require.Len(t, rows, 4)
	assert.Equal(t, "95", rows[1]["score"])
	assert.Equal(t, "cy", rows[3]["login"])

	out, _, code = crm(t, dbPath, "table", "list", "-f", "json")
	require.Equal(t, 0, code)
	list := jsonRows(t, out)
	require.Len(t, list, 1)
	assert.EqualValues(t, 4, list[0]["rows"])
	assert.Equal(t, "login", list[0]["key_column"])

	_, stderr, code = crm(t, dbPath, "table", "export", "nope")
	assert.Equal(t, 3, code, stderr)
	_, _, code = crm(t, dbPath, "table", "export", "leads", "--only", "ghost=1")
	assert.Equal(t, 2, code)
}

func TestColumn_AddListRm(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crm.db")
	path := writeFile(t, dir, "gh.csv", "login\noctocat\n")
	_, stderr, code := crm(t, dbPath, "table", "import", path, "--as", "gh", "--key", "login")
	require.Equal(t, 0, code, stderr)

	_, stderr, code = crm(t, dbPath, "column", "add", "gh", "x", "--type", "enrich", "--waterfall", "nope")
	assert.Equal(t, 3, code, stderr)
	_, stderr, code = crm(t, dbPath, "column", "add", "gh", "x", "--type", "ai")
	assert.Equal(t, 2, code, stderr)

	out, stderr, code := crm(t, dbPath, "column", "add", "gh", "profile", "--type", "enrich", "--waterfall", "github,exa", "-f", "json")
	require.Equal(t, 0, code, stderr)
	col := jsonRows(t, out)[0]
	assert.Equal(t, "login", col["config"].(map[string]any)["from"], "--from defaults to the table key")
	assert.Equal(t, "$0.005", col["cost_est"], "worst case: github (free) + exa")

	out, _, code = crm(t, dbPath, "column", "list", "gh", "-f", "json")
	require.Equal(t, 0, code)
	cols := jsonRows(t, out)
	require.Len(t, cols, 2)
	assert.Equal(t, "1/1", cols[0]["filled"])
	assert.Equal(t, "0/1", cols[1]["filled"])

	_, _, code = crm(t, dbPath, "column", "rm", "gh", "login")
	assert.Equal(t, 4, code, "key column cannot be removed")
	_, stderr, code = crm(t, dbPath, "column", "rm", "gh", "profile")
	require.Equal(t, 0, code, stderr)
	out, _, _ = crm(t, dbPath, "column", "list", "gh", "-f", "json")
	assert.Len(t, jsonRows(t, out), 1)
}

func TestRun_EndToEnd(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		login := strings.TrimPrefix(r.URL.Path, "/users/")
		if login == "ghost" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"login":"` + login + `","company":"` + strings.ToUpper(login) + ` Inc","public_repos":7}`))
	}))
	defer srv.Close()
	env := []string{"CRM_PROVIDER_GITHUB_URL=" + srv.URL, "GITHUB_TOKEN="}

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crm.db")
	path := writeFile(t, dir, "gh.csv", "login\nalice\nghost\nbob\n")
	_, stderr, code := crmEnv(t, env, dbPath, "table", "import", path, "--as", "gh", "--key", "login")
	require.Equal(t, 0, code, stderr)
	_, stderr, code = crmEnv(t, env, dbPath, "column", "add", "gh", "company", "--type", "enrich",
		"--waterfall", "github", "--field", "company")
	require.Equal(t, 0, code, stderr)
	before, _, _ := crmEnv(t, env, dbPath, "table", "export", "gh")

	out, stderr, code := crmEnv(t, env, dbPath, "run", "gh", "--columns", "company", "--dry-run", "-f", "json")
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stderr, "nothing written")
	est := jsonRows(t, out)
	require.Len(t, est, 2)
	assert.Equal(t, "TOTAL", est[1]["column"])
	assert.EqualValues(t, 3, est[1]["calls"])
	after, _, _ := crmEnv(t, env, dbPath, "table", "export", "gh")
	assert.Equal(t, before, after)
	out, _, _ = crmEnv(t, env, dbPath, "run", "list", "-f", "json")
	assert.Empty(t, jsonRows(t, out))
	assert.Zero(t, hits.Load())

	out, stderr, code = crmEnv(t, env, dbPath, "run", "gh", "--columns", "company", "-f", "json")
	require.Equal(t, 0, code, stderr)
	sum := jsonRows(t, out)[0]
	assert.Equal(t, "completed", sum["status"])
	assert.EqualValues(t, 2, sum["ok"])
	assert.EqualValues(t, 1, sum["empty"])
	assert.EqualValues(t, 3, hits.Load())

	out, stderr, code = crmEnv(t, env, dbPath, "run", "gh", "--columns", "company", "-f", "json")
	require.Equal(t, 0, code, stderr)
	sum = jsonRows(t, out)[0]
	assert.EqualValues(t, 3, sum["cache_hits"], "the known-empty login is a cache hit too")
	assert.EqualValues(t, 0, sum["spent_usd"])
	assert.EqualValues(t, 0, sum["calls"])
	assert.EqualValues(t, 3, hits.Load(), "second run is served from the cache")

	out, _, code = crmEnv(t, env, dbPath, "table", "export", "gh")
	require.Equal(t, 0, code)
	assert.Equal(t, "login,company\nalice,ALICE Inc\nghost,\nbob,BOB Inc\n", out)

	out, _, code = crmEnv(t, env, dbPath, "run", "list", "-f", "json")
	require.Equal(t, 0, code)
	assert.Len(t, jsonRows(t, out), 2)
	out, _, code = crmEnv(t, env, dbPath, "run", "show", "1", "--items", "-f", "json")
	require.Equal(t, 0, code)
	items := jsonRows(t, out)
	require.Len(t, items, 3)
	assert.Equal(t, "github", items[0]["provider"])
	assert.Equal(t, `"ALICE Inc"`, items[0]["value"])
	_, _, code = crmEnv(t, env, dbPath, "run", "show", "99")
	assert.Equal(t, 3, code)

	// Cached answers serve a new field for free; a paid provider with an
	// uncached row needs --budget, and its absence is a usage error (exit 2).
	path = writeFile(t, dir, "more.csv", "login\ncarol\n")
	_, stderr, code = crmEnv(t, env, dbPath, "table", "import", path, "--as", "gh")
	require.Equal(t, 0, code, stderr)
	_, stderr, code = crmEnv(t, env, dbPath, "provider", "set", "github", "--price", "0.01")
	require.Equal(t, 0, code, stderr)
	_, stderr, code = crmEnv(t, env, dbPath, "column", "add", "gh", "repos", "--type", "enrich", "--waterfall", "github", "--field", "public_repos")
	require.Equal(t, 0, code, stderr)
	_, stderr, code = crmEnv(t, env, dbPath, "run", "gh", "--columns", "repos")
	assert.Equal(t, 2, code)
	assert.Contains(t, stderr, "--budget")
}

func TestProvider_SetAndList(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "crm.db")
	out, _, code := crm(t, dbPath, "provider", "list", "-f", "json")
	require.Equal(t, 0, code)
	names := []string{}
	for _, p := range jsonRows(t, out) {
		names = append(names, p["name"].(string))
	}
	assert.Equal(t, []string{"exa", "exa-contents", "github", "jina", "llm"}, names)

	_, _, code = crm(t, dbPath, "provider", "set", "mine", "--url", "https://x/{{input}}")
	assert.Equal(t, 2, code, "--kind required to create")
	out, stderr, code := crm(t, dbPath, "provider", "set", "mine", "--kind", "http", "--url", "https://x/{{input}}",
		"--path", "data", "--price", "0.002", "--auth-env", "MINE_KEY", "-f", "json")
	require.Equal(t, 0, code, stderr)
	p := jsonRows(t, out)[0]
	assert.EqualValues(t, 0.002, p["price_usd"])
	assert.Equal(t, "MINE_KEY", p["auth_ref"])
	assert.Equal(t, "data", p["config"].(map[string]any)["path"])

	_, _, code = crm(t, dbPath, "provider", "set", "mine", "--enabled=false")
	require.Equal(t, 0, code)
	out, _, _ = crm(t, dbPath, "provider", "list", "-f", "json")
	for _, p := range jsonRows(t, out) {
		if p["name"] == "mine" {
			assert.Equal(t, false, p["enabled"])
			assert.Equal(t, "https://x/{{input}}", p["config"].(map[string]any)["url"], "unchanged flags are kept")
		}
	}
}

func TestColumnAdd_ReportsFilledOfRows(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crm.db")
	_, stderr, code := crm(t, dbPath, "table", "import", writeFile(t, dir, "l.csv", "name\na\nb\n"), "--as", "l")
	require.Equal(t, 0, code, stderr)
	out, stderr, code := crm(t, dbPath, "column", "add", "l", "label", "--type", "formula", "--expr", "{{name}}!", "-f", "json")
	require.Equal(t, 0, code, stderr)
	assert.Equal(t, "2/2", jsonRows(t, out)[0]["filled"])
}

func TestTable_ListKeylessExportFormatCaseShowJSONOrder(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crm.db")
	_, stderr, code := crm(t, dbPath, "table", "import", writeFile(t, dir, "l.csv", "zed,amy\n1,2\n"), "--as", "l")
	require.Equal(t, 0, code, stderr)

	out, _, code := crm(t, dbPath, "table", "list", "-f", "table")
	require.Equal(t, 0, code)
	assert.NotContains(t, out, "<nil>")

	exported, stderr, code := crm(t, dbPath, "table", "export", "l", "-f", "JSON")
	require.Equal(t, 0, code, stderr)
	assert.Less(t, strings.Index(exported, `"zed"`), strings.Index(exported, `"amy"`))

	shown, stderr, code := crm(t, dbPath, "table", "show", "l", "-f", "json")
	require.Equal(t, 0, code, stderr)
	assert.Less(t, strings.Index(shown, `"zed"`), strings.Index(shown, `"amy"`), "show keeps column order like export")
}
