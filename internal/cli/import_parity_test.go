package cli_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	gomcp "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mcpImport calls crm_table_import with csv on `crm mcp serve` (stdio) of the
// real binary against dbPath.
func mcpImport(t *testing.T, dbPath, name, key, csv string) (out map[string]any, isError bool, text string) {
	t.Helper()
	c, err := client.NewStdioMCPClient(binaryPath, nil, "--db", dbPath, "mcp", "serve")
	require.NoError(t, err)
	defer c.Close()
	ctx := context.Background()
	init := gomcp.InitializeRequest{}
	init.Params.ProtocolVersion = gomcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = gomcp.Implementation{Name: "test", Version: "1"}
	_, err = c.Initialize(ctx, init)
	require.NoError(t, err)
	req := gomcp.CallToolRequest{}
	req.Params.Name = "crm_table_import"
	req.Params.Arguments = map[string]any{"table": name, "key": key, "csv": csv}
	res, err := c.CallTool(ctx, req)
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

// crm table import and MCP crm_table_import (csv) share one rule: a header
// error writes nothing; a row error skips that row and writes the others.
func TestTableImport_CLIAndMCPAgree(t *testing.T) {
	dir := t.TempDir()

	t.Run("row errors", func(t *testing.T) {
		csv := "company,domain\nAcme,acme.io\n,nokey.io\nHooli,hooli.com\nAcme,acme2.io\n"
		path := writeFile(t, dir, "rows.csv", csv)
		cliDB, mcpDB := filepath.Join(dir, "rows-cli.db"), filepath.Join(dir, "rows-mcp.db")

		out, stderr, code := crm(t, cliDB, "table", "import", path, "--as", "leads", "--key", "company", "-f", "json")
		assert.Equal(t, 6, code, "rows skipped")
		assert.Contains(t, stderr, "skipped: row 2: key column \"company\" is empty")
		assert.Contains(t, stderr, "skipped: rows 1 and 4 have the same key \"Acme\" in column \"company\"")
		assert.Contains(t, stderr, "crm: error: imported 2 of 4 rows into leads; 2 row(s) skipped")
		cliSum := jsonRows(t, out)[0]

		mcpOut, isErr, text := mcpImport(t, mcpDB, "leads", "company", csv)
		require.False(t, isErr, text)
		for _, k := range []string{"rows", "created", "updated", "failed"} {
			assert.Equal(t, cliSum[k], mcpOut[k], k)
		}
		assert.EqualValues(t, 2, mcpOut["failed"])
		var errs []string
		for _, r := range mcpOut["results"].([]any) {
			if e, ok := r.(map[string]any)["error"].(string); ok && e != "" {
				errs = append(errs, e)
			}
		}
		require.Len(t, errs, 2)
		assert.Contains(t, errs[0], `row 2: key column "company" is empty`)
		assert.Contains(t, errs[1], `rows 1 and 4 have the same key "Acme" in column "company"`)

		cliRows, _, code := crm(t, cliDB, "table", "export", "leads")
		require.Equal(t, 0, code)
		mcpRows, _, code := crm(t, mcpDB, "table", "export", "leads")
		require.Equal(t, 0, code)
		assert.Equal(t, "company,domain\nAcme,acme.io\nHooli,hooli.com\n", cliRows)
		assert.Equal(t, cliRows, mcpRows, "the same rows are written")
	})

	t.Run("header errors", func(t *testing.T) {
		for name, csv := range map[string]string{
			"repeated header":   "company,company\nAcme,Acme2\n",
			"key not in header": "name,domain\nAcme,acme.io\n",
		} {
			t.Run(name, func(t *testing.T) {
				path := writeFile(t, dir, "header.csv", csv)
				cliDB := filepath.Join(t.TempDir(), "cli.db")
				mcpDB := filepath.Join(t.TempDir(), "mcp.db")

				_, stderr, code := crm(t, cliDB, "table", "import", path, "--as", "leads", "--key", "company")
				assert.Equal(t, 2, code, stderr)
				_, isErr, text := mcpImport(t, mcpDB, "leads", "company", csv)
				assert.True(t, isErr, text)
				assert.Contains(t, text, strings.TrimPrefix(strings.TrimSpace(stderr), "crm: error: "), "the same message")

				for _, d := range []string{cliDB, mcpDB} {
					out, _, code := crm(t, d, "table", "list", "-f", "json")
					require.Equal(t, 0, code)
					assert.Empty(t, jsonRows(t, out), "nothing written")
				}
			})
		}
	})
}
