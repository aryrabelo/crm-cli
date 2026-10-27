package cli_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jdanielnd/crm-cli/internal/csvio"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeQualifier is an OpenAI-compatible chat completions server that
// qualifies rows by their description: "B2B" → yes, "consumer" → no, else
// maybe. A row for "Broken Co" always gets a reply that is not JSON.
type fakeQualifier struct {
	srv   *httptest.Server
	calls atomic.Int64
	mu    sync.Mutex
	byRow map[string]int
}

func newQualifier(t *testing.T) *fakeQualifier {
	t.Helper()
	q := &fakeQualifier{byRow: map[string]int{}}
	q.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q.calls.Add(1)
		assert.Equal(t, "/v1/chat/completions", r.URL.Path)
		assert.Equal(t, "Bearer sk-test", r.Header.Get("Authorization"))
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&req)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Equal(t, "fake-model", req.Model)
		user := req.Messages[1].Content
		company, desc := "", ""
		for _, line := range strings.Split(user, "\n") {
			if c, ok := strings.CutPrefix(line, "- company: "); ok {
				company = c
			}
			if d, ok := strings.CutPrefix(line, "- description: "); ok {
				desc = d
			}
		}
		q.mu.Lock()
		q.byRow[company]++
		q.mu.Unlock()
		var content string
		switch {
		case company == "Broken Co":
			content = "I think this one is a yes."
		case strings.Contains(desc, "B2B"):
			content = `{"value": "yes", "reason": "The description says ` + company + ` sells B2B software."}`
		case strings.Contains(desc, "consumer"):
			content = `{"value": "no", "reason": "The description says ` + company + ` is a consumer app."}`
		default:
			content = `{"value": "maybe", "reason": "The description of ` + company + ` does not say who it sells to."}`
		}
		size := 0
		for _, m := range req.Messages {
			size += len(m.Content)
		}
		enc, _ := json.Marshal(content)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%s}}],"usage":{"prompt_tokens":%d,"completion_tokens":30}}`,
			enc, size/4)
	}))
	t.Cleanup(q.srv.Close)
	return q
}

// PLANO S2 aceite: 20 rows → a decision and a reason per row, tokens and cost
// printed per run; a re-run is all cache hits ($0); an answer that stays
// invalid JSON is repaired once, then the row fails cleanly; --dry-run writes
// nothing.
func TestQualify_Acceptance(t *testing.T) {
	q := newQualifier(t)
	env := []string{
		"CRM_LLM_BASE_URL=" + q.srv.URL + "/v1", "CRM_LLM_API_KEY=sk-test", "CRM_LLM_MODEL=fake-model",
		"CRM_LLM_PRICE_IN=1", "CRM_LLM_PRICE_OUT=2",
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crm.db")
	var csv strings.Builder
	csv.WriteString("company,website,description\n")
	for i := range 19 {
		desc := []string{"B2B payroll software for SMBs", "consumer dating app", "a company"}[i%3]
		fmt.Fprintf(&csv, "Co%02d,co%02d.com,%s\n", i, i, desc)
	}
	csv.WriteString("Broken Co,broken.com,B2B tooling\n")
	src := writeFile(t, dir, "leads.csv", csv.String())
	criteria := writeFile(t, dir, "icp.md", "# ICP\n\nB2B software companies selling to SMBs. Consumer apps are out.\n")

	_, stderr, code := crmEnv(t, env, dbPath, "table", "import", src, "--as", "leads", "--key", "company")
	require.Equal(t, 0, code, stderr)
	before, _, _ := crmEnv(t, env, dbPath, "table", "export", "leads")

	// Dry run: estimates 20 calls, writes nothing, calls nobody.
	out, stderr, code := crmEnv(t, env, dbPath, "qualify", "leads", "--criteria", criteria, "--explain", "--dry-run", "-f", "json")
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stderr, "qualify would create ai column qualified (enum:yes|no|maybe) with reasons in qualified_reason")
	assert.Contains(t, stderr, "nothing written")
	est := jsonRows(t, out)
	require.Len(t, est, 2)
	assert.EqualValues(t, 20, est[1]["calls"])
	assert.Positive(t, est[1]["worst_usd"])
	after, _, _ := crmEnv(t, env, dbPath, "table", "export", "leads")
	assert.Equal(t, before, after)
	out, _, _ = crmEnv(t, env, dbPath, "column", "list", "leads", "-f", "json")
	assert.Len(t, jsonRows(t, out), 3, "no column created")
	out, _, _ = crmEnv(t, env, dbPath, "run", "list", "-f", "json")
	assert.Empty(t, jsonRows(t, out))
	assert.Zero(t, q.calls.Load())

	// Paid run without --budget is a usage error.
	_, stderr, code = crmEnv(t, env, dbPath, "qualify", "leads", "--criteria", criteria, "--explain")
	assert.Equal(t, 2, code)
	assert.Contains(t, stderr, "--budget")

	// Real run.
	out, stderr, code = crmEnv(t, env, dbPath, "qualify", "leads", "--criteria", criteria, "--explain",
		"--budget", "1", "--concurrency", "8", "-f", "json")
	require.Equal(t, 6, code, "Broken Co failed: exit 6\n%s", stderr)
	assert.Contains(t, stderr, "crm: error: run 1 finished with 1 failed item(s)")
	sum := jsonRows(t, out)[0]
	assert.EqualValues(t, 7, sum["yes"])
	assert.EqualValues(t, 6, sum["no"])
	assert.EqualValues(t, 6, sum["maybe"])
	assert.EqualValues(t, 1, sum["failed"], "Broken Co")
	assert.Positive(t, sum["tokens_in"])
	assert.EqualValues(t, 20*30+30, sum["tokens_out"], "19 rows + 2 calls for the broken one")
	assert.Positive(t, sum["spent_usd"])
	assert.LessOrEqual(t, sum["spent_usd"], 1.0)
	assert.Regexp(t, `qualify leads\.qualified: yes 7 · no 6 · maybe 6 · failed 1 · skipped 0 — tokens in \d+ / out 630 · spent \$0\.\d+ of \$1\.00 budget`, stderr)
	assert.Regexp(t, `run 1 completed .* tokens in \d+ / out 630 · spent`, stderr)
	assert.EqualValues(t, 21, q.calls.Load())
	assert.Equal(t, 2, q.byRow["Broken Co"], "one repair request, no retry of a non-retryable failure")

	out, _, code = crmEnv(t, env, dbPath, "run", "show", "1", "-f", "json")
	require.Equal(t, 0, code)
	show := jsonRows(t, out)[0]
	assert.Equal(t, sum["tokens_in"], show["tokens_in"])
	assert.EqualValues(t, 630, show["tokens_out"])
	out, _, code = crmEnv(t, env, dbPath, "run", "show", "1", "--items", "-f", "json")
	require.Equal(t, 0, code)
	for _, it := range jsonRows(t, out) {
		if it["status"] == "failed" {
			assert.Contains(t, it["error"], "invalid answer after one repair")
			assert.EqualValues(t, 60, it["tokens_out"])
		}
	}

	out, _, code = crmEnv(t, env, dbPath, "table", "export", "leads")
	require.Equal(t, 0, code)
	recs, err := csvio.Parse([]byte(out))
	require.NoError(t, err)
	require.Equal(t, []string{"company", "website", "description", "qualified", "qualified_reason"}, recs.Header)
	require.Len(t, recs.Rows, 20)
	assert.Equal(t, []string{"Co00", "co00.com", "B2B payroll software for SMBs", "yes", "The description says Co00 sells B2B software."}, recs.Rows[0])
	assert.Equal(t, "no", recs.Rows[1][3])
	assert.Equal(t, "maybe", recs.Rows[2][3])
	assert.Equal(t, []string{"Broken Co", "broken.com", "B2B tooling", "", ""}, recs.Rows[19])
	out, _, _ = crmEnv(t, env, dbPath, "table", "export", "leads", "--only", "qualified=yes", "-f", "tsv")
	assert.Equal(t, 8, strings.Count(out, "\n"), "header + 7 qualified rows")

	// Re-run: every answered row is a cache hit ($0); only the broken row calls again.
	out, stderr, code = crmEnv(t, env, dbPath, "qualify", "leads", "--criteria", criteria, "--explain", "--budget", "1",
		"--only", "company!=Broken Co", "-f", "json")
	require.Equal(t, 0, code, stderr)
	sum = jsonRows(t, out)[0]
	assert.EqualValues(t, 0, sum["spent_usd"])
	assert.EqualValues(t, 0, sum["tokens_in"])
	assert.EqualValues(t, 19, sum["cache_hits"])
	assert.EqualValues(t, 21, q.calls.Load(), "no call on re-run")
	out, _, _ = crmEnv(t, env, dbPath, "column", "list", "leads", "-f", "json")
	assert.Len(t, jsonRows(t, out), 5, "the decision column is redefined, not duplicated")
}

// P1-6: the default qualify prompt leaves raw enrich answers out (a 60 KB
// profile per row here), keeps enrich --field values, says each field is
// cut, and --fields / --field-limit choose and cap the fields explicitly.
func TestQualify_PromptFieldsSkipBlobs(t *testing.T) {
	blob := strings.Repeat("lorem ipsum ", 5000)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		login := strings.TrimPrefix(r.URL.Path, "/users/")
		_, _ = fmt.Fprintf(w, `{"login":%q,"company":"%s Inc","bio":%q}`, login, login, blob)
	}))
	defer gh.Close()
	var mu sync.Mutex
	var prompts []string
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		mu.Lock()
		prompts = append(prompts, req.Messages[len(req.Messages)-1].Content)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"value\": \"yes\"}"}}],"usage":{"prompt_tokens":100,"completion_tokens":5}}`)
	}))
	defer llm.Close()
	env := []string{
		"CRM_PROVIDER_GITHUB_URL=" + gh.URL, "GITHUB_TOKEN=",
		"CRM_LLM_BASE_URL=" + llm.URL + "/v1", "CRM_LLM_API_KEY=sk-test", "CRM_LLM_MODEL=m",
		"CRM_LLM_PRICE_IN=1", "CRM_LLM_PRICE_OUT=2",
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crm.db")
	src := writeFile(t, dir, "leads.csv", "login,description\nacme,B2B payroll\nzorg,industrial software\n")
	criteria := writeFile(t, dir, "icp.md", "B2B software.\n")
	_, stderr, code := crmEnv(t, env, dbPath, "table", "import", src, "--as", "leads", "--key", "login")
	require.Equal(t, 0, code, stderr)
	_, stderr, code = crmEnv(t, env, dbPath, "column", "add", "leads", "profile", "--type", "enrich", "--waterfall", "github")
	require.Equal(t, 0, code, stderr)
	_, stderr, code = crmEnv(t, env, dbPath, "column", "add", "leads", "org", "--type", "enrich", "--waterfall", "github", "--field", "company")
	require.Equal(t, 0, code, stderr)
	_, stderr, code = crmEnv(t, env, dbPath, "run", "leads", "--columns", "profile,org")
	require.Equal(t, 0, code, stderr)

	worst := func(args ...string) (float64, string) {
		t.Helper()
		out, stderr, code := crmEnv(t, env, dbPath, append([]string{"qualify", "leads", "--criteria", criteria, "--dry-run", "-f", "json"}, args...)...)
		require.Equal(t, 0, code, stderr)
		est := jsonRows(t, out)
		return est[len(est)-1]["worst_usd"].(float64), stderr
	}
	def, stderr := worst()
	assert.Contains(t, stderr, "prompt fields: login, description, org (each cut to 1000 characters)")
	withBlob, stderr := worst("--fields", "login,description,profile,org", "--field-limit", "100000")
	assert.Contains(t, stderr, "prompt fields: login, description, profile, org (each cut to 100000 characters)")
	assert.Less(t, def*20, withBlob, "leaving the raw profile out cuts the worst case by far more than 20x")
	capped, _ := worst("--fields", "login,profile", "--field-limit", "200")
	assert.Less(t, capped, withBlob/20, "the field limit caps the blob")
	_, stderr, code = crmEnv(t, env, dbPath, "qualify", "leads", "--criteria", criteria, "--dry-run", "--fields", "qualified")
	assert.Equal(t, 2, code, stderr)
	_, stderr, code = crmEnv(t, env, dbPath, "qualify", "leads", "--criteria", criteria, "--dry-run", "--field-limit", "0")
	assert.Equal(t, 2, code, stderr)

	// The real prompt: no raw blob, the enrich field and the cap notice.
	_, stderr, code = crmEnv(t, env, dbPath, "qualify", "leads", "--criteria", criteria, "--budget", "1")
	require.Equal(t, 0, code, stderr)
	_, stderr, code = crmEnv(t, env, dbPath, "qualify", "leads", "--criteria", criteria, "--budget", "1",
		"--name", "fit", "--fields", "login,profile.bio", "--field-limit", "50")
	require.Equal(t, 0, code, stderr)
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, prompts, 4)
	for _, p := range prompts[:2] {
		assert.Contains(t, p, "Each field is cut to 1000 characters")
		assert.Contains(t, p, "- org: ")
		assert.Contains(t, p, " Inc\n")
		assert.NotContains(t, p, "lorem")
		assert.Less(t, len(p), 2000)
	}
	for _, p := range prompts[2:] {
		assert.Contains(t, p, "Each field is cut to 50 characters")
		assert.Contains(t, p, "- profile.bio: "+blob[:50]+"…\n")
		assert.NotContains(t, p, "- description:")
	}
}

func TestColumnAdd_AIValidationAndTypedValues(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		score := "35"
		if strings.Contains(req.Messages[1].Content, "big") {
			score = "88"
		}
		_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"content":"{\"value\": %s}"}}],"usage":{"prompt_tokens":50,"completion_tokens":5}}`, score)
	}))
	defer srv.Close()
	env := []string{"CRM_LLM_BASE_URL=" + srv.URL + "/v1", "CRM_LLM_API_KEY=sk-test"}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crm.db")
	src := writeFile(t, dir, "leads.csv", "company,size\nAcme,big\nTiny,small\n")
	_, stderr, code := crmEnv(t, env, dbPath, "table", "import", src, "--as", "leads", "--key", "company")
	require.Equal(t, 0, code, stderr)

	_, stderr, code = crmEnv(t, env, dbPath, "column", "add", "leads", "fit", "--type", "ai",
		"--prompt", "Score {{company}} by {{headcount}}", "--output", "score:0-100")
	assert.Equal(t, 2, code)
	assert.Contains(t, stderr, "{{headcount}} references no column")
	_, _, code = crmEnv(t, env, dbPath, "column", "add", "leads", "fit", "--type", "ai",
		"--prompt", "Score {{company}}", "--output", "score:1")
	assert.Equal(t, 2, code)
	_, stderr, code = crmEnv(t, env, dbPath, "column", "add", "leads", "fit", "--type", "ai",
		"--prompt", "Score {{company}} ({{size}})", "--output", "score:0-100")
	assert.Equal(t, 2, code, "llm without model/price")
	assert.Contains(t, stderr, "no model")

	_, stderr, code = crmEnv(t, env, dbPath, "provider", "set", "llm", "--model", "m", "--price-in", "0.5", "--price-out", "1.5")
	require.Equal(t, 0, code, stderr)
	prompt := writeFile(t, dir, "fit.md", "Score how well {{company}} (size: {{size}}) fits.\n")
	out, stderr, code := crmEnv(t, env, dbPath, "column", "add", "leads", "fit", "--type", "ai",
		"--prompt", prompt, "--output", "score:0-100", "-f", "json")
	require.Equal(t, 0, code, stderr)
	col := jsonRows(t, out)[0]
	assert.Equal(t, "Score how well {{company}} (size: {{size}}) fits.\n", col["config"].(map[string]any)["prompt"], "prompt read from the file")
	assert.Equal(t, "score:0-100 ← llm", col["definition"])

	out, stderr, code = crmEnv(t, env, dbPath, "run", "leads", "--columns", "fit", "--budget", "0.10", "-f", "json")
	require.Equal(t, 0, code, stderr)
	assert.EqualValues(t, 100, jsonRows(t, out)[0]["tokens_in"])
	assert.EqualValues(t, 10, jsonRows(t, out)[0]["tokens_out"])
	out, _, code = crmEnv(t, env, dbPath, "table", "export", "leads", "-f", "json", "--only", "fit>=70")
	require.Equal(t, 0, code)
	assert.Equal(t, "[\n  {\"company\": \"Acme\", \"size\": \"big\", \"fit\": 88}\n]\n", out, "scores are stored as numbers")

	_, _, code = crmEnv(t, env, dbPath, "column", "rm", "leads", "size")
	assert.Equal(t, 4, code, "used by the ai prompt")
	_, _, code = crmEnv(t, env, dbPath, "column", "add", "leads", "x", "--type", "enrich", "--waterfall", "llm")
	assert.Equal(t, 2, code, "llm providers serve ai columns only")
	assert.EqualValues(t, 2, calls.Load())
}
