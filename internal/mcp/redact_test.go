package mcp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jdanielnd/crm-cli/internal/db"
	crmmcp "github.com/jdanielnd/crm-cli/internal/mcp"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// codebase-details M4: crm_run_status items:true returned raw item errors,
// so an http provider with ?key= in its URL leaked the key over MCP.
func TestRunStatusItems_NeverLeakCredentials(t *testing.T) {
	const secret = "SECRET-KEY-123"
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	require.NoError(t, provider.NewStore(d).Save(context.Background(), &provider.Config{
		Name: "down", Kind: "http", Enabled: true,
		Settings: provider.Settings{URL: "http://127.0.0.1:1/find?key=" + secret + "&q={{input}}"},
	}))
	registry := provider.NewRegistry(&http.Client{Timeout: 5 * time.Second}, func(string) string { return "" })
	s := crmmcp.New(d, "test", crmmcp.WithRegistry(registry))
	t.Cleanup(s.Close)
	c := connect(t, s, "inprocess")

	ok(t, c, "crm_table_import", map[string]any{"table": "leads", "key": "domain", "csv": "domain\nacme.io\n"})
	ok(t, c, "crm_column_add", map[string]any{"table": "leads", "columns": []map[string]any{
		{"name": "email", "type": "enrich", "waterfall": []string{"down"}, "from": "domain"}}})
	started := ok(t, c, "crm_run_start", map[string]any{"table": "leads", "columns": []string{"email"}, "retry": 0})
	runID := started["run"].(map[string]any)["id"]

	_, _, text := call(t, c, "crm_run_status", map[string]any{"run_ids": []any{runID}, "items": true})
	assert.NotContains(t, text, secret)
	var status struct {
		Results []struct {
			Items []struct {
				Status string `json:"status"`
				Error  string `json:"error"`
			} `json:"items"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &status))
	require.Len(t, status.Results, 1)
	require.Len(t, status.Results[0].Items, 1)
	it := status.Results[0].Items[0]
	assert.Equal(t, "failed", it.Status)
	assert.Contains(t, it.Error, `down: Get "http://127.0.0.1:1/find?<redacted>"`)
}
