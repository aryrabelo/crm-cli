package ui_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jdanielnd/crm-cli/internal/bench"
	"github.com/jdanielnd/crm-cli/internal/db"
	"github.com/jdanielnd/crm-cli/internal/httpguard"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/jdanielnd/crm-cli/internal/run"
	"github.com/jdanielnd/crm-cli/internal/service"
	"github.com/jdanielnd/crm-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testToken = "s3cret-token"

const leadsCSV = "company,domain,description\n" +
	"Acme,acme.io,B2B payroll software\n" +
	"Bluth,bluth.shop,consumer banana stand\n" +
	"Rekall,rekall.media,media group\n" +
	"Zorg,zorg.fr,B2B industrial software\n"

type env struct {
	t     *testing.T
	db    *sql.DB
	fakes *bench.Fakes
	srv   *ui.Server
	hs    *httptest.Server
	port  string
}

// newEnv is a crm ui server on a fresh database whose providers are the
// bench fakes (firmo enrichment at $0.002/call: .fr and .org domains have no
// data; an OpenAI-compatible llm), behind the loopback guard.
func newEnv(t *testing.T) *env {
	t.Helper()
	fakes := bench.NewFakes()
	t.Cleanup(fakes.Close)
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	require.NoError(t, fakes.Provision(context.Background(), d))
	s := ui.New(d, fakes.Registry())
	hs := httptest.NewUnstartedServer(nil)
	h, err := s.Handler(httpguard.Loopback(testToken, hs.Listener.Addr()))
	require.NoError(t, err)
	hs.Config.Handler = h
	hs.Start()
	t.Cleanup(hs.Close)
	t.Cleanup(s.Close) // runs first: stop background runs before the db closes
	_, port, _ := net.SplitHostPort(hs.Listener.Addr().String())
	return &env{t: t, db: d, fakes: fakes, srv: s, hs: hs, port: port}
}

// req sends a request; headers may override the defaults (bearer token,
// application/json for a non-nil body). An empty header value removes it.
func (e *env) req(method, path string, body []byte, headers map[string]string) (int, []byte, http.Header) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	r, err := http.NewRequest(method, e.hs.URL+path, rd)
	require.NoError(e.t, err)
	r.Header.Set("Authorization", "Bearer "+testToken)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		if k == "Host" {
			r.Host = v
			continue
		}
		if v == "" {
			r.Header.Del(k)
		} else {
			r.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(r)
	require.NoError(e.t, err)
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	require.NoError(e.t, err)
	return resp.StatusCode, out, resp.Header
}

// json sends v as JSON and decodes a JSON answer, requiring status want.
func (e *env) json(method, path string, v any, want int) map[string]any {
	e.t.Helper()
	var body []byte
	if v != nil {
		var err error
		body, err = json.Marshal(v)
		require.NoError(e.t, err)
	}
	code, out, _ := e.req(method, path, body, nil)
	require.Equal(e.t, want, code, string(out))
	var m map[string]any
	require.NoError(e.t, json.Unmarshal(out, &m), string(out))
	return m
}

func (e *env) importLeads(name, key string) map[string]any {
	e.t.Helper()
	path := "/api/tables/import?as=" + name
	if key != "" {
		path += "&key=" + key
	}
	code, out, _ := e.req(http.MethodPost, path, []byte(leadsCSV), map[string]string{"Content-Type": "text/csv"})
	require.Equal(e.t, http.StatusOK, code, string(out))
	var m map[string]any
	require.NoError(e.t, json.Unmarshal(out, &m))
	return m
}

func (e *env) addIndustry() {
	e.t.Helper()
	e.json(http.MethodPost, "/api/tables/leads/columns",
		map[string]any{"name": "industry", "type": "enrich", "waterfall": []string{bench.FirmoName}, "field": "industry"},
		http.StatusCreated)
}

// waitRun polls GET /api/runs/{id} until the run is no longer active.
func (e *env) waitRun(id int64) map[string]any {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r := e.json(http.MethodGet, "/api/runs/"+strconv.FormatInt(id, 10), nil, http.StatusOK)["run"].(map[string]any)
		if r["active"] == false && r["status"] != string(run.StatusRunning) {
			return r
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatalf("run %d still active", id)
	return nil
}

func runIDOf(t *testing.T, started map[string]any) int64 {
	t.Helper()
	return int64(started["run"].(map[string]any)["id"].(float64))
}

func TestUI_Auth(t *testing.T) {
	e := newEnv(t)
	code, _, _ := e.req(http.MethodGet, "/api/tables", nil, map[string]string{"Authorization": ""})
	assert.Equal(t, http.StatusUnauthorized, code, "no token")
	code, _, _ = e.req(http.MethodGet, "/api/tables", nil, map[string]string{"Authorization": "Bearer wrong"})
	assert.Equal(t, http.StatusUnauthorized, code, "wrong token")
	code, _, _ = e.req(http.MethodGet, "/api/tables", nil, nil)
	assert.Equal(t, http.StatusOK, code)

	code, _, _ = e.req(http.MethodGet, "/api/tables", nil, map[string]string{"Host": "attacker:" + e.port})
	assert.Equal(t, http.StatusForbidden, code, "rebound Host")
	code, _, _ = e.req(http.MethodPost, "/api/tables/leads/runs", []byte(`{}`), map[string]string{"Origin": "https://evil.example"})
	assert.Equal(t, http.StatusForbidden, code, "foreign Origin")
	code, _, _ = e.req(http.MethodGet, "/api/tables", nil, map[string]string{"Origin": "http://localhost:" + e.port})
	assert.Equal(t, http.StatusOK, code, "own Origin")

	// The page itself carries no data: it loads without the token, but still
	// only under an allowlisted Host.
	code, page, hdr := e.req(http.MethodGet, "/", nil, map[string]string{"Authorization": ""})
	assert.Equal(t, http.StatusOK, code)
	assert.Contains(t, string(page), "<title>crm ui</title>")
	assert.Contains(t, hdr.Get("Content-Security-Policy"), "script-src 'self'")
	assert.Equal(t, "nosniff", hdr.Get("X-Content-Type-Options"))
	code, _, _ = e.req(http.MethodGet, "/vendor/tabulator.min.js", nil, map[string]string{"Authorization": ""})
	assert.Equal(t, http.StatusOK, code)
	code, _, _ = e.req(http.MethodGet, "/", nil, map[string]string{"Authorization": "", "Host": "attacker:" + e.port})
	assert.Equal(t, http.StatusForbidden, code)
	code, _, _ = e.req(http.MethodPost, "/", []byte(`{}`), map[string]string{"Authorization": ""})
	assert.Equal(t, http.StatusMethodNotAllowed, code)
}

// State-changing endpoints refuse what a cross-site form or a simple
// request could send, even with the right token.
func TestUI_CSRF(t *testing.T) {
	e := newEnv(t)
	e.importLeads("leads", "domain")
	spec := []byte(`{"name":"x","type":"input"}`)
	for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x", ""} {
		code, out, _ := e.req(http.MethodPost, "/api/tables/leads/columns", spec, map[string]string{"Content-Type": ct})
		assert.Equal(t, http.StatusUnsupportedMediaType, code, "%q: %s", ct, out)
	}
	code, _, _ := e.req(http.MethodPost, "/api/tables/leads/columns", spec, map[string]string{"Sec-Fetch-Site": "cross-site"})
	assert.Equal(t, http.StatusForbidden, code)
	code, _, _ = e.req(http.MethodPost, "/api/tables/leads/columns", spec, map[string]string{"Sec-Fetch-Site": "same-site"})
	assert.Equal(t, http.StatusForbidden, code, "another origin of the same site")
	code, _, _ = e.req(http.MethodPost, "/api/tables/import?as=more", []byte(leadsCSV), nil)
	assert.Equal(t, http.StatusUnsupportedMediaType, code, "import takes text/csv only")
	code, _, _ = e.req(http.MethodPost, "/api/runs/1/cancel", []byte(`{}`), map[string]string{"Content-Type": "text/plain"})
	assert.Equal(t, http.StatusUnsupportedMediaType, code)

	code, out, _ := e.req(http.MethodPost, "/api/tables/leads/columns", spec, map[string]string{"Sec-Fetch-Site": "same-origin"})
	assert.Equal(t, http.StatusCreated, code, string(out))
}

func TestUI_ImportExportRoundTrip(t *testing.T) {
	e := newEnv(t)
	res := e.importLeads("leads", "domain")
	assert.Equal(t, "leads", res["table"])
	assert.EqualValues(t, 4, res["rows"])

	code, out, hdr := e.req(http.MethodGet, "/api/tables/leads/export", nil, nil)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, leadsCSV, string(out), "export of an imported table is the file")
	assert.Equal(t, `attachment; filename="leads.csv"`, hdr.Get("Content-Disposition"))

	// Same bytes as crm table export (service.Export), filtered or not.
	svc := service.New(e.db, e.fakes.Registry())
	_, view, err := svc.View(context.Background(), "leads", []string{"description~B2B"})
	require.NoError(t, err)
	var want bytes.Buffer
	require.NoError(t, service.Export(&want, view, "csv"))
	code, out, _ = e.req(http.MethodGet, "/api/tables/leads/export?only=description~B2B", nil, nil)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, want.String(), string(out))
	assert.Equal(t, 3, strings.Count(string(out), "\n"))

	// Re-importing with the key updates instead of duplicating.
	res = e.importLeads("leads", "")
	assert.EqualValues(t, 0, res["created"])
	assert.EqualValues(t, 4, res["updated"])

	code, out, _ = e.req(http.MethodPost, "/api/tables/import?as=bad", []byte("a,b\n1,2,3\n"), map[string]string{"Content-Type": "text/csv"})
	assert.Equal(t, http.StatusBadRequest, code, string(out))
	code, _, _ = e.req(http.MethodPost, "/api/tables/import?as=../x", []byte(leadsCSV), map[string]string{"Content-Type": "text/csv"})
	assert.Equal(t, http.StatusBadRequest, code)
}

func TestUI_ViewPayload(t *testing.T) {
	e := newEnv(t)
	e.importLeads("leads", "domain")
	e.addIndustry()
	e.json(http.MethodPost, "/api/tables/leads/columns", map[string]any{"name": "label", "type": "formula", "expr": "{{company}} ({{domain}})"}, http.StatusCreated)

	tables := e.json(http.MethodGet, "/api/tables", nil, http.StatusOK)["tables"].([]any)
	require.Len(t, tables, 1)
	assert.Equal(t, map[string]any{"name": "leads", "key_column": "domain", "rows": 4.0, "columns": 5.0}, tables[0])

	v := e.json(http.MethodGet, "/api/tables/leads", nil, http.StatusOK)
	assert.Equal(t, "leads", v["table"])
	assert.Equal(t, "domain", v["key_column"])
	assert.Nil(t, v["live_run"])
	assert.Nil(t, v["latest_run"])
	cols := v["columns"].([]any)
	require.Len(t, cols, 5)
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.(map[string]any)["name"].(string)
	}
	assert.Equal(t, []string{"company", "domain", "description", "industry", "label"}, names)
	ind := cols[3].(map[string]any)
	assert.Equal(t, "enrich", ind["type"])
	assert.Equal(t, true, ind["runnable"])
	assert.Equal(t, "firmo ← domain .industry", ind["definition"])
	assert.Equal(t, "$0.002", ind["cost_est"])
	assert.Equal(t, false, cols[4].(map[string]any)["runnable"])

	rows := v["rows"].([]any)
	require.Len(t, rows, 4)
	first := rows[0].(map[string]any)
	assert.Equal(t, 1.0, first["row"])
	assert.NotZero(t, first["id"])
	assert.Equal(t, []any{"Acme", "acme.io", "B2B payroll software", nil, "Acme (acme.io)"}, first["values"])
	assert.Equal(t, []any{nil, nil, nil, nil, nil}, first["states"], "no run yet")

	only := e.json(http.MethodGet, "/api/tables/leads?only=description~B2B&only=domain~io$", nil, http.StatusOK)
	assert.Len(t, only["rows"], 1)
	code, _, _ := e.req(http.MethodGet, "/api/tables/leads?only=nope=1", nil, nil)
	assert.Equal(t, http.StatusBadRequest, code)
	code, _, _ = e.req(http.MethodGet, "/api/tables/missing", nil, nil)
	assert.Equal(t, http.StatusNotFound, code)
}

func TestUI_Estimate(t *testing.T) {
	e := newEnv(t)
	e.importLeads("leads", "domain")
	spec := map[string]any{"name": "industry", "type": "enrich", "waterfall": []string{bench.FirmoName}, "field": "industry"}

	// Estimated from the Add column dialog before the column exists...
	preview := e.json(http.MethodPost, "/api/tables/leads/estimate", map[string]any{"preview": spec}, http.StatusOK)
	// ...and once saved, as crm run --dry-run estimates it.
	e.addIndustry()
	saved := e.json(http.MethodPost, "/api/tables/leads/estimate", map[string]any{"columns": []string{"industry"}}, http.StatusOK)
	assert.Equal(t, saved["total"], preview["total"])

	total := saved["total"].(map[string]any)
	assert.EqualValues(t, 4, total["items"])
	assert.EqualValues(t, 4, total["calls"])
	assert.EqualValues(t, 0, total["cache_hits"])
	assert.Equal(t, "$0.008", total["worst"])
	assert.Equal(t, true, saved["needs_budget"])
	assert.Equal(t, false, saved["unbounded"])

	est, err := run.NewEngine(e.db, e.fakes.Registry()).Estimate(context.Background(), run.Options{Table: "leads", Columns: []string{"industry"}})
	require.NoError(t, err)
	assert.Equal(t, service.EstimateData(&est.Total)["worst"], total["worst"])
	assert.EqualValues(t, est.Total.Calls, total["calls"])

	filtered := e.json(http.MethodPost, "/api/tables/leads/estimate",
		map[string]any{"columns": []string{"industry"}, "only": []string{"description~B2B"}}, http.StatusOK)
	assert.EqualValues(t, 2, filtered["total"].(map[string]any)["items"])

	// An ai prompt reading a column computed earlier in the same run has no
	// known size: the estimate says unbounded.
	e.json(http.MethodPost, "/api/tables/leads/columns",
		map[string]any{"name": "fit", "type": "ai", "prompt": "Score {{company}} in {{industry}}", "output": "score:0-100"}, http.StatusCreated)
	unb := e.json(http.MethodPost, "/api/tables/leads/estimate", map[string]any{"columns": []string{"industry", "fit"}}, http.StatusOK)
	assert.Equal(t, true, unb["unbounded"])
	assert.Equal(t, true, unb["needs_budget"])
	assert.EqualValues(t, 4, unb["total"].(map[string]any)["unbounded_items"])
	assert.Contains(t, unb["total"].(map[string]any)["worst"], "unbounded")

	code, out, _ := e.req(http.MethodPost, "/api/tables/leads/estimate", []byte(`{"columns":["company"]}`), nil)
	assert.Equal(t, http.StatusBadRequest, code, string(out))
	code, _, _ = e.req(http.MethodPost, "/api/tables/leads/estimate", []byte(`{"columns":["industry"],"bogus":1}`), nil)
	assert.Equal(t, http.StatusBadRequest, code, "unknown fields are refused")
}

func TestUI_RunStartStatusCompletion(t *testing.T) {
	e := newEnv(t)
	e.importLeads("leads", "domain")
	e.addIndustry()

	out := e.json(http.MethodPost, "/api/tables/leads/runs", map[string]any{"columns": []string{"industry"}}, http.StatusBadRequest)
	assert.Contains(t, out["error"], "set --budget")
	e.json(http.MethodPost, "/api/tables/leads/runs", map[string]any{"columns": []string{"industry"}, "budget": "abc"}, http.StatusBadRequest)

	started := e.json(http.MethodPost, "/api/tables/leads/runs", map[string]any{"columns": []string{"industry"}, "budget": "0.05"}, http.StatusAccepted)
	id := runIDOf(t, started)
	r := e.waitRun(id)
	assert.Equal(t, string(run.StatusCompleted), r["status"])
	assert.EqualValues(t, 4, r["items"])
	assert.EqualValues(t, 3, r["ok"])
	assert.EqualValues(t, 1, r["empty"], "zorg.fr has no data")
	assert.Equal(t, "$0.008", r["spent"], "4 firmo calls at $0.002 (a 404 is a paid call without data)")
	assert.Equal(t, "$0.05", r["budget"])
	assert.Equal(t, "", r["error"])

	rn, err := run.NewStore(e.db).Get(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, provider.Cost(8000), rn.Spent)
	assert.InDelta(t, rn.Spent.USD(), r["spent_usd"], 1e-9, "the UI shows runs.spent_micros")

	v := e.json(http.MethodGet, "/api/tables/leads", nil, http.StatusOK)
	latest := v["latest_run"].(map[string]any)
	assert.EqualValues(t, id, latest["id"])
	assert.Equal(t, false, latest["active"])
	assert.Nil(t, v["live_run"])
	var statuses []string
	for _, row := range v["rows"].([]any) {
		rm := row.(map[string]any)
		st := rm["states"].([]any)[3].(map[string]any)
		statuses = append(statuses, st["status"].(string))
		assert.EqualValues(t, id, st["run_id"])
		if st["status"] == "ok" {
			assert.NotEmpty(t, rm["values"].([]any)[3])
			assert.Equal(t, bench.FirmoName, st["provider"])
		}
	}
	assert.Equal(t, []string{"ok", "ok", "ok", "empty"}, statuses)

	// The same column again: all cache hits, nothing to pay, values unchanged.
	again := e.json(http.MethodPost, "/api/tables/leads/estimate", map[string]any{"columns": []string{"industry"}}, http.StatusOK)
	assert.Equal(t, "$0.00", again["total"].(map[string]any)["worst"])
	assert.EqualValues(t, 4, again["total"].(map[string]any)["cache_hits"], "answers and the 404 are cached")
	second := runIDOf(t, e.json(http.MethodPost, "/api/tables/leads/runs", map[string]any{"columns": []string{"industry"}, "budget": "0"}, http.StatusAccepted))
	r = e.waitRun(second)
	assert.Equal(t, "$0.00", r["spent"])
	assert.EqualValues(t, 4, r["cache_hits"])
	v2 := e.json(http.MethodGet, "/api/tables/leads", nil, http.StatusOK)
	for i, row := range v2["rows"].([]any) {
		assert.Equal(t, v["rows"].([]any)[i].(map[string]any)["values"], row.(map[string]any)["values"])
	}

	code, _, _ := e.req(http.MethodGet, "/api/runs/999", nil, nil)
	assert.Equal(t, http.StatusNotFound, code)
	code, _, _ = e.req(http.MethodPost, "/api/runs/"+strconv.FormatInt(id, 10)+"/cancel", []byte(`{}`), nil)
	assert.Equal(t, http.StatusConflict, code, "a finished run cannot be cancelled")
}

// A run in progress shows its open cells as running, picks up the run in
// /runs, and a cancel leaves it interrupted with its open cells pending.
func TestUI_RunningCellsAndCancel(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"v":"x"}`))
	}))
	t.Cleanup(slow.Close)
	t.Cleanup(func() { close(release) })
	require.NoError(t, provider.NewStore(e.db).Save(context.Background(), &provider.Config{Name: "slow", Kind: "http", Enabled: true,
		Settings: provider.Settings{URL: slow.URL + "/?q={{input}}", Path: "v"}}))
	e.importLeads("leads", "domain")
	e.json(http.MethodPost, "/api/tables/leads/columns", map[string]any{"name": "s", "type": "enrich", "waterfall": []string{"slow"}}, http.StatusCreated)

	est := e.json(http.MethodPost, "/api/tables/leads/estimate", map[string]any{"columns": []string{"s"}}, http.StatusOK)
	assert.Equal(t, false, est["needs_budget"], "a free provider needs no budget")
	id := runIDOf(t, e.json(http.MethodPost, "/api/tables/leads/runs", map[string]any{"columns": []string{"s"}}, http.StatusAccepted))

	deadline := time.Now().Add(5 * time.Second)
	var live map[string]any
	for time.Now().Before(deadline) {
		runs := e.json(http.MethodGet, "/api/tables/leads/runs", nil, http.StatusOK)
		if l, ok := runs["live_run"].(map[string]any); ok {
			live = l
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.NotNil(t, live, "the executing run is live")
	assert.EqualValues(t, id, live["id"])
	assert.Equal(t, true, live["active"])
	assert.Equal(t, true, live["cancellable"])

	v := e.json(http.MethodGet, "/api/tables/leads", nil, http.StatusOK)
	for _, row := range v["rows"].([]any) {
		assert.Equal(t, "running", row.(map[string]any)["states"].([]any)[3].(map[string]any)["status"])
	}

	e.json(http.MethodPost, "/api/runs/"+strconv.FormatInt(id, 10)+"/cancel", map[string]any{}, http.StatusAccepted)
	r := e.waitRun(id)
	assert.Equal(t, string(run.StatusInterrupted), r["status"])
	assert.EqualValues(t, 4, r["pending"])
	v = e.json(http.MethodGet, "/api/tables/leads", nil, http.StatusOK)
	for _, row := range v["rows"].([]any) {
		assert.Equal(t, "pending", row.(map[string]any)["states"].([]any)[3].(map[string]any)["status"])
	}
}

// with_sources: running an ai column from its header also runs, first, the
// enrich columns it reads that still have empty cells; without the flag the
// request is taken as given.
func TestUI_WithSourcesAddsUnfilledSourceColumns(t *testing.T) {
	e := newEnv(t)
	e.importLeads("leads", "company")
	e.addIndustry()
	e.json(http.MethodPost, "/api/tables/leads/columns",
		map[string]any{"name": "fit", "type": "ai", "prompt": "Score {{company}} in {{industry}}", "output": "score:0-100"}, http.StatusCreated)

	columns := func(out map[string]any) []string {
		var names []string
		for _, c := range out["columns"].([]any) {
			names = append(names, c.(map[string]any)["column"].(string))
		}
		return names
	}
	alone := e.json(http.MethodPost, "/api/tables/leads/estimate", map[string]any{"columns": []string{"fit"}}, http.StatusOK)
	assert.Equal(t, []string{"fit"}, columns(alone))
	with := e.json(http.MethodPost, "/api/tables/leads/estimate", map[string]any{"columns": []string{"fit"}, "with_sources": true}, http.StatusOK)
	assert.Equal(t, []string{"industry", "fit"}, columns(with))

	id := runIDOf(t, e.json(http.MethodPost, "/api/tables/leads/runs",
		map[string]any{"columns": []string{"fit"}, "with_sources": true, "budget": "1"}, http.StatusAccepted))
	e.waitRun(id)

	// industry is filled now: nothing more to add.
	done := e.json(http.MethodPost, "/api/tables/leads/estimate", map[string]any{"columns": []string{"fit"}, "with_sources": true}, http.StatusOK)
	assert.Equal(t, []string{"fit"}, columns(done))
}
