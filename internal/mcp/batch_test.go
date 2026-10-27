package mcp_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jdanielnd/crm-cli/internal/bench"
	"github.com/jdanielnd/crm-cli/internal/db"
	"github.com/jdanielnd/crm-cli/internal/httpguard"
	crmmcp "github.com/jdanielnd/crm-cli/internal/mcp"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	gomcp "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testToken = "s3cret"

// newBatchServer is a crm MCP server on a fresh database whose providers are
// the bench fakes (firmo enrichment, an OpenAI-compatible llm).
func newBatchServer(t *testing.T) *crmmcp.Server {
	t.Helper()
	fakes := bench.NewFakes()
	t.Cleanup(fakes.Close)
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	require.NoError(t, fakes.Provision(context.Background(), d))
	s := crmmcp.New(d, "test", crmmcp.WithRegistry(fakes.Registry()))
	t.Cleanup(s.Close)
	return s
}

// connect returns an initialized client of s over transport kind:
// inprocess, stdio (JSON-RPC over pipes) or http (streamable, bearer token).
func connect(t *testing.T, s *crmmcp.Server, kind string) *client.Client {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var c *client.Client
	var err error
	switch kind {
	case "stdio":
		srvIn, cliOut := io.Pipe()
		cliIn, srvOut := io.Pipe()
		go func() { _ = s.ServeStdio(ctx, srvIn, srvOut) }()
		t.Cleanup(func() { _ = cliOut.Close(); _ = srvOut.Close() })
		c = client.NewClient(transport.NewIO(cliIn, cliOut, io.NopCloser(strings.NewReader(""))))
	case "http":
		hs := guardedServer(t, s)
		c, err = client.NewStreamableHttpClient(hs.URL+crmmcp.HTTPPath,
			transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + testToken}))
	default:
		c, err = client.NewInProcessClient(s.MCP())
	}
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, c.Start(ctx))
	init := gomcp.InitializeRequest{}
	init.Params.ProtocolVersion = gomcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = gomcp.Implementation{Name: "test", Version: "1"}
	_, err = c.Initialize(ctx, init)
	require.NoError(t, err)
	return c
}

// call invokes tool name and decodes its JSON result; isError reports a
// tool-level error (whose text is returned as a string).
func call(t *testing.T, c *client.Client, name string, args map[string]any) (out map[string]any, isError bool, text string) {
	t.Helper()
	req := gomcp.CallToolRequest{}
	req.Params.Name, req.Params.Arguments = name, args
	res, err := c.CallTool(context.Background(), req)
	require.NoError(t, err)
	require.NotEmpty(t, res.Content)
	tc, ok := res.Content[0].(gomcp.TextContent)
	require.True(t, ok)
	if res.IsError {
		return nil, true, tc.Text
	}
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &out), tc.Text)
	return out, false, tc.Text
}

func ok(t *testing.T, c *client.Client, name string, args map[string]any) map[string]any {
	t.Helper()
	out, isErr, text := call(t, c, name, args)
	require.False(t, isErr, "%s: %s", name, text)
	return out
}

func results(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(out["results"])
	require.NoError(t, err)
	var rs []map[string]any
	require.NoError(t, json.Unmarshal(raw, &rs))
	return rs
}

const leadsCSV = "company,domain,description\n" +
	"Acme,acme.io,B2B payroll software\n" +
	"Bluth,bluth.shop,consumer banana stand\n" +
	"Rekall,rekall.media,media group\n" +
	"Zorg,zorg.fr,B2B industrial software\n"

var transports = []string{"inprocess", "stdio", "http"}

func TestBatchTools_ListedAlongsideEntityTools(t *testing.T) {
	c := connect(t, newBatchServer(t), "inprocess")
	res, err := c.ListTools(context.Background(), gomcp.ListToolsRequest{})
	require.NoError(t, err)
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	for _, n := range []string{"crm_person_create", "crm_person_search", "crm_stats", "crm_table_import", "crm_rows_upsert_many",
		"crm_table_query", "crm_column_add", "crm_run_start", "crm_run_status", "crm_qualify", "crm_export", "crm_push_dedupe"} {
		assert.Contains(t, names, n)
	}
	assert.Len(t, names, 27, "18 entity tools + 9 batch tools")
}

func TestTableImportUpsertQuery(t *testing.T) {
	for _, kind := range transports {
		t.Run(kind, func(t *testing.T) {
			c := connect(t, newBatchServer(t), kind)

			out := ok(t, c, "crm_table_import", map[string]any{"table": "leads", "key": "company", "csv": leadsCSV})
			assert.EqualValues(t, 4, out["created"])
			assert.Equal(t, []any{"company", "domain", "description"}, out["new_columns"])

			// Row objects: one row misses the key, one repeats it; the rest is written.
			out = ok(t, c, "crm_table_import", map[string]any{"table": "leads", "rows": []any{
				map[string]any{"company": "Acme", "employees": 40},
				map[string]any{"domain": "nokey.io"},
				map[string]any{"company": "Hooli", "domain": "hooli.com"},
				map[string]any{"company": "Hooli", "domain": "dup.com"},
			}})
			assert.EqualValues(t, 1, out["created"])
			assert.EqualValues(t, 1, out["updated"])
			assert.EqualValues(t, 2, out["failed"])
			rs := results(t, out)
			require.Len(t, rs, 4)
			assert.Equal(t, "updated", rs[0]["action"])
			assert.Equal(t, "error", rs[1]["action"])
			assert.Contains(t, rs[1]["error"], `key column "company" is missing`)
			assert.Equal(t, "created", rs[2]["action"])
			assert.Contains(t, rs[3]["error"], "same key")

			_, isErr, text := call(t, c, "crm_table_import", map[string]any{"table": "leads", "csv": leadsCSV, "rows": []any{}})
			assert.True(t, isErr)
			assert.Contains(t, text, "exactly one of csv or rows")

			// Upsert many: per-row errors; a missing table is a call error.
			out = ok(t, c, "crm_rows_upsert_many", map[string]any{"table": "leads", "rows": []any{
				map[string]any{"company": "Bluth", "employees": "12"},
				map[string]any{"company": ""},
			}})
			assert.EqualValues(t, 1, out["updated"])
			assert.EqualValues(t, 1, out["failed"])
			assert.Contains(t, results(t, out)[1]["error"], "is empty")
			_, isErr, text = call(t, c, "crm_rows_upsert_many", map[string]any{"table": "nope", "rows": []any{map[string]any{"a": "1"}}})
			assert.True(t, isErr)
			assert.Contains(t, text, "not found")

			out = ok(t, c, "crm_table_query", map[string]any{"table": "leads", "filters": []any{"description~B2B"},
				"columns": []any{"company", "employees"}, "limit": 1, "offset": 1})
			assert.EqualValues(t, 2, out["total"])
			assert.Equal(t, []any{"company", "employees"}, out["columns"])
			assert.Equal(t, []any{map[string]any{"_row": float64(4), "company": "Zorg", "employees": nil}}, out["rows"],
				"a cell the row never set is null")
			_, isErr, text = call(t, c, "crm_table_query", map[string]any{"table": "leads", "filters": []any{"nope=1"}})
			assert.True(t, isErr)
			assert.Contains(t, text, "validation error")
		})
	}
}

func TestColumnAddRunStatusExport(t *testing.T) {
	for _, kind := range transports {
		t.Run(kind, func(t *testing.T) {
			c := connect(t, newBatchServer(t), kind)
			ok(t, c, "crm_table_import", map[string]any{"table": "leads", "key": "company", "csv": leadsCSV})

			out := ok(t, c, "crm_column_add", map[string]any{"table": "leads", "columns": []any{
				map[string]any{"name": "industry", "type": "enrich", "waterfall": []any{"firmo"}, "from": "domain", "field": "industry"},
				map[string]any{"name": "bad", "type": "enrich", "waterfall": []any{"nope"}},
				map[string]any{"name": "label", "type": "formula", "expr": "{{company}} ({{industry}})"},
			}})
			assert.EqualValues(t, 2, out["added"])
			rs := results(t, out)
			assert.Contains(t, rs[1]["error"], "not found")
			assert.Empty(t, rs[2]["error"])

			// Dry run: estimate only.
			out = ok(t, c, "crm_run_start", map[string]any{"table": "leads", "columns": []any{"industry"}, "dry_run": true})
			assert.Equal(t, true, out["dry_run"])
			total := out["total"].(map[string]any)
			assert.EqualValues(t, 4, total["calls"])
			assert.InDelta(t, 0.008, total["worst_usd"], 1e-9)

			// A paid run needs a budget.
			_, isErr, text := call(t, c, "crm_run_start", map[string]any{"table": "leads", "columns": []any{"industry"}})
			assert.True(t, isErr)
			assert.Contains(t, text, "budget")

			// wait=false returns at once; run_status follows the run.
			out = ok(t, c, "crm_run_start", map[string]any{"table": "leads", "columns": []any{"industry"}, "budget": "0.05", "wait": false})
			id := out["run"].(map[string]any)["id"].(float64)
			var st map[string]any
			require.Eventually(t, func() bool {
				out := ok(t, c, "crm_run_status", map[string]any{"run_ids": []any{id}})
				st = results(t, out)[0]
				return st["active"] == false && st["run"].(map[string]any)["status"] != "running"
			}, 10*time.Second, 20*time.Millisecond)
			run := st["run"].(map[string]any)
			assert.Equal(t, "completed", run["status"])
			assert.EqualValues(t, 3, run["ok"])
			assert.EqualValues(t, 1, run["empty"], "zorg.fr is unknown to firmo")
			assert.InDelta(t, 0.008, run["spent_usd"], 1e-9)

			// Per-item errors: an unknown run id next to a known one.
			out = ok(t, c, "crm_run_status", map[string]any{"run_ids": []any{id, 999}, "items": true})
			rs = results(t, out)
			assert.Len(t, rs[0]["items"], 4)
			assert.Contains(t, rs[1]["error"], "not found")

			// A waited run of the same column is all cache hits.
			out = ok(t, c, "crm_run_start", map[string]any{"table": "leads", "columns": []any{"industry"}, "budget": 0.05})
			run = out["run"].(map[string]any)
			assert.EqualValues(t, 4, run["cache_hits"])
			assert.EqualValues(t, 0, run["calls"])

			out = ok(t, c, "crm_export", map[string]any{"table": "leads", "filters": []any{"industry!="},
				"columns": []any{"company", "label"}, "format": "json"})
			assert.EqualValues(t, 3, out["rows"])
			var rows []map[string]any
			require.NoError(t, json.Unmarshal([]byte(out["content"].(string)), &rows))
			assert.Equal(t, "Acme", rows[0]["company"])
			assert.Contains(t, rows[0]["label"], "Acme (")
			out = ok(t, c, "crm_export", map[string]any{"table": "leads", "columns": []any{"company"}})
			assert.Equal(t, "company\nAcme\nBluth\nRekall\nZorg\n", out["content"])
			_, isErr, text = call(t, c, "crm_export", map[string]any{"table": "leads", "format": "xml"})
			assert.True(t, isErr)
			assert.Contains(t, text, "csv, tsv or json")
		})
	}
}

func TestQualifyAndPushDedupe(t *testing.T) {
	for _, kind := range transports {
		t.Run(kind, func(t *testing.T) {
			c := connect(t, newBatchServer(t), kind)
			ok(t, c, "crm_table_import", map[string]any{"table": "leads", "key": "company", "csv": leadsCSV})

			out := ok(t, c, "crm_qualify", map[string]any{"table": "leads", "criteria": "B2B only", "dry_run": true})
			assert.Equal(t, true, out["dry_run"])
			assert.EqualValues(t, 4, out["total"].(map[string]any)["calls"])
			assert.Equal(t, []any{"company", "domain", "description"}, out["fields"])
			assert.EqualValues(t, 1000, out["field_limit"])
			narrow := ok(t, c, "crm_qualify", map[string]any{"table": "leads", "criteria": "B2B only", "dry_run": true,
				"fields": []any{"description"}, "field_limit": 10})
			assert.Equal(t, []any{"description"}, narrow["fields"])
			assert.EqualValues(t, 10, narrow["field_limit"])
			assert.Less(t, narrow["total"].(map[string]any)["worst_usd"], out["total"].(map[string]any)["worst_usd"],
				"fewer, capped fields cost less")

			_, isErr, text := call(t, c, "crm_qualify", map[string]any{"table": "leads", "criteria": " "})
			assert.True(t, isErr)
			assert.Contains(t, text, "criteria are empty")

			out = ok(t, c, "crm_qualify", map[string]any{"table": "leads", "criteria": "B2B only", "explain": true, "budget": 0.01})
			assert.Equal(t, "completed", out["status"])
			assert.EqualValues(t, 2, out["yes"])
			assert.EqualValues(t, 1, out["no"])
			assert.EqualValues(t, 1, out["maybe"])
			assert.Positive(t, out["tokens_in"])

			// Push the qualified rows to organizations, deduplicated by domain.
			args := map[string]any{"table": "leads", "to": "org", "filters": []any{"qualified=yes"}, "dry_run": true}
			out = ok(t, c, "crm_push_dedupe", args)
			assert.Equal(t, true, out["dry_run"])
			assert.EqualValues(t, 2, out["created"])
			stats := ok(t, c, "crm_stats", nil)
			assert.EqualValues(t, 0, stats["organizations"], "dry run writes nothing")

			delete(args, "dry_run")
			out = ok(t, c, "crm_push_dedupe", args)
			assert.EqualValues(t, 2, out["created"])
			out = ok(t, c, "crm_push_dedupe", args)
			assert.EqualValues(t, 2, out["updated"], "a second push matches by domain")
			stats = ok(t, c, "crm_stats", nil)
			assert.EqualValues(t, 2, stats["organizations"])

			// Per-row errors: rows without a dedupe key are reported, not fatal.
			ok(t, c, "crm_table_import", map[string]any{"table": "people", "rows": []any{
				map[string]any{"name": "Ana Lima", "email": "ana@acme.io"},
				map[string]any{"name": "No Email"},
				map[string]any{"name": "Ana L.", "email": " ANA@acme.io"},
			}})
			out = ok(t, c, "crm_push_dedupe", map[string]any{"table": "people", "to": "person"})
			assert.EqualValues(t, 1, out["created"])
			assert.EqualValues(t, 1, out["merged"])
			assert.EqualValues(t, 1, out["failed"])
			assert.NotEmpty(t, results(t, out)[1]["error"])

			_, isErr, text = call(t, c, "crm_push_dedupe", map[string]any{"table": "people", "to": "deal"})
			assert.True(t, isErr)
			assert.Contains(t, text, "person or org")
		})
	}
}

// guardedServer serves s over HTTP on a loopback port with the guard of a
// loopback bind (Host/Origin allowlists, testToken).
func guardedServer(t *testing.T, s *crmmcp.Server) *httptest.Server {
	t.Helper()
	hs := httptest.NewUnstartedServer(nil)
	h, err := s.Handler(httpguard.Loopback(testToken, hs.Listener.Addr()))
	require.NoError(t, err)
	hs.Config.Handler = h
	hs.Start()
	t.Cleanup(hs.Close)
	return hs
}

const initializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`

// post sends an initialize request to hs with the given headers ("Host"
// overrides the request host) and returns the status code.
func post(t *testing.T, hs *httptest.Server, headers map[string]string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, hs.URL+crmmcp.HTTPPath, strings.NewReader(initializeBody))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func TestHTTPBearerAuth(t *testing.T) {
	s := newBatchServer(t)
	hs := guardedServer(t, s)
	for name, auth := range map[string]string{"missing": "", "wrong": "Bearer nope", "not bearer": testToken} {
		t.Run(name, func(t *testing.T) {
			h := map[string]string{}
			if auth != "" {
				h["Authorization"] = auth
			}
			assert.Equal(t, http.StatusUnauthorized, post(t, hs, h))
		})
	}

	// An mcp-go client without the token cannot initialize.
	c, err := client.NewStreamableHttpClient(hs.URL + crmmcp.HTTPPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, c.Start(context.Background()))
	init := gomcp.InitializeRequest{}
	init.Params.ProtocolVersion = gomcp.LATEST_PROTOCOL_VERSION
	_, err = c.Initialize(context.Background(), init)
	require.Error(t, err)

	// There is no open mode: a handler without a token cannot be built.
	_, err = s.Handler(httpguard.Config{})
	require.Error(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.Error(t, s.ServeHTTP(ctx, "127.0.0.1:0", "", nil))
}

// Design review P0-1: a web page that rebinds its name to 127.0.0.1 (or
// posts cross-origin) must not reach the tools, even with a valid token.
func TestHTTPGuard_DNSRebindingAndOrigin(t *testing.T) {
	s := newBatchServer(t)
	hs := guardedServer(t, s)
	_, port, err := net.SplitHostPort(hs.Listener.Addr().String())
	require.NoError(t, err)
	bearer := "Bearer " + testToken
	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"rebound host", map[string]string{"Host": "evil.example:" + port, "Authorization": bearer}, http.StatusForbidden},
		{"rebound host, same-origin Origin", map[string]string{"Host": "evil.example:" + port, "Origin": "http://evil.example:" + port, "Authorization": bearer}, http.StatusForbidden},
		{"foreign origin", map[string]string{"Origin": "http://evil.example", "Authorization": bearer}, http.StatusForbidden},
		{"foreign origin, no token", map[string]string{"Origin": "http://evil.example"}, http.StatusForbidden},
		{"loopback origin", map[string]string{"Origin": "http://localhost:" + port, "Authorization": bearer}, http.StatusOK},
		{"localhost host", map[string]string{"Host": "localhost:" + port, "Authorization": bearer}, http.StatusOK},
		{"null origin", map[string]string{"Origin": "null", "Authorization": bearer}, http.StatusOK},
		{"no origin", map[string]string{"Authorization": bearer}, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, post(t, hs, tc.headers))
		})
	}
}

func TestHTTPGuard_BodyLimit(t *testing.T) {
	s := newBatchServer(t)
	hs := guardedServer(t, s)
	big := strings.Repeat(" ", httpguard.DefaultMaxBodyBytes+1)
	req, err := http.NewRequest(http.MethodPost, hs.URL+crmmcp.HTTPPath, strings.NewReader(big))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
}

func TestServeHTTP_ShutsDownOnCancel(t *testing.T) {
	s := newBatchServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	addrc := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- s.ServeHTTP(ctx, "127.0.0.1:0", testToken, func(a net.Addr) { addrc <- a.String() })
	}()
	addr := <-addrc
	c, err := client.NewStreamableHttpClient("http://"+addr+crmmcp.HTTPPath,
		transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + testToken}))
	require.NoError(t, err)
	require.NoError(t, c.Start(ctx))
	init := gomcp.InitializeRequest{}
	init.Params.ProtocolVersion = gomcp.LATEST_PROTOCOL_VERSION
	_, err = c.Initialize(ctx, init)
	require.NoError(t, err)
	_ = c.Close()
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("ServeHTTP did not stop")
	}
}
