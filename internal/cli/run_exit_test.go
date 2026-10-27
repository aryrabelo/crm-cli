package cli_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A run that does not finish every item exits non-zero: 6 with failed
// items, 5 when the budget stopped it; resuming it to completion exits 0.
func TestRun_ExitStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		login := strings.TrimPrefix(r.URL.Path, "/users/")
		if login == "bad" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"bad login"}`))
			return
		}
		_, _ = fmt.Fprintf(w, `{"login":%q,"company":"%s Inc"}`, login, login)
	}))
	defer srv.Close()
	env := []string{"CRM_PROVIDER_GITHUB_URL=" + srv.URL, "GITHUB_TOKEN="}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crm.db")

	_, stderr, code := crmEnv(t, env, dbPath, "table", "import", writeFile(t, dir, "a.csv", "login\nalice\nbad\n"), "--as", "a", "--key", "login")
	require.Equal(t, 0, code, stderr)
	_, stderr, code = crmEnv(t, env, dbPath, "column", "add", "a", "company", "--type", "enrich", "--waterfall", "github", "--field", "company")
	require.Equal(t, 0, code, stderr)
	out, stderr, code := crmEnv(t, env, dbPath, "run", "a", "--columns", "company", "-f", "json")
	assert.Equal(t, 6, code, "completed with a failed item")
	assert.Contains(t, stderr, "crm: error: run 1 finished with 1 failed item(s)")
	sum := jsonRows(t, out)[0]
	assert.Equal(t, "completed", sum["status"], "the summary is still printed")
	assert.EqualValues(t, 1, sum["failed"])

	_, stderr, code = crmEnv(t, env, dbPath, "table", "import", writeFile(t, dir, "b.csv", "login\ncarol\ndave\nerin\n"), "--as", "b", "--key", "login")
	require.Equal(t, 0, code, stderr)
	_, stderr, code = crmEnv(t, env, dbPath, "column", "add", "b", "company", "--type", "enrich", "--waterfall", "github", "--field", "company")
	require.Equal(t, 0, code, stderr)
	_, stderr, code = crmEnv(t, env, dbPath, "provider", "set", "github", "--price", "0.01")
	require.Equal(t, 0, code, stderr)
	out, stderr, code = crmEnv(t, env, dbPath, "run", "b", "--columns", "company", "--budget", "0.01", "-f", "json")
	assert.Equal(t, 5, code, "budget_exhausted")
	assert.Contains(t, stderr, "crm: error: run 2 stopped at its budget: 2 item(s) skipped")
	assert.Equal(t, "budget_exhausted", jsonRows(t, out)[0]["status"])

	out, stderr, code = crmEnv(t, env, dbPath, "run", "--resume", "2", "--budget", "1", "-f", "json")
	assert.Equal(t, 0, code, stderr)
	assert.Equal(t, "completed", jsonRows(t, out)[0]["status"])
}

// SIGINT and SIGTERM stop a run (status interrupted, resumable) and the
// process exits 128 + the signal number.
func TestRun_SignalExitStatus(t *testing.T) {
	for _, tc := range []struct {
		sig  syscall.Signal
		code int
	}{{syscall.SIGINT, 130}, {syscall.SIGTERM, 143}} {
		t.Run(tc.sig.String(), func(t *testing.T) {
			arrived, release := make(chan struct{}, 8), make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				arrived <- struct{}{}
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			t.Cleanup(srv.Close)
			t.Cleanup(func() { close(release) })
			env := []string{"CRM_PROVIDER_GITHUB_URL=" + srv.URL, "GITHUB_TOKEN="}
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "crm.db")
			_, stderr, code := crmEnv(t, env, dbPath, "table", "import", writeFile(t, dir, "gh.csv", "login\nalice\nbob\n"), "--as", "gh", "--key", "login")
			require.Equal(t, 0, code, stderr)
			_, stderr, code = crmEnv(t, env, dbPath, "column", "add", "gh", "company", "--type", "enrich", "--waterfall", "github", "--field", "company")
			require.Equal(t, 0, code, stderr)

			cmd := exec.Command(binaryPath, "--db", dbPath, "run", "gh", "--columns", "company", "--concurrency", "1", "-f", "json")
			cmd.Env = append(os.Environ(), env...)
			var errBuf strings.Builder
			cmd.Stderr = &errBuf
			require.NoError(t, cmd.Start())
			select {
			case <-arrived:
			case <-time.After(10 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatal("the run never called the provider")
			}
			require.NoError(t, cmd.Process.Signal(tc.sig))
			err := cmd.Wait()
			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr, errBuf.String())
			assert.Equal(t, tc.code, exitErr.ExitCode(), errBuf.String())
			assert.Contains(t, errBuf.String(), "crm: error: run 1 interrupted")

			out, _, code := crmEnv(t, env, dbPath, "run", "show", "1", "-f", "json")
			require.Equal(t, 0, code)
			assert.Equal(t, "interrupted", jsonRows(t, out)[0]["status"])
		})
	}
}
