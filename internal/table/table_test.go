package table_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jdanielnd/crm-cli/internal/column"
	"github.com/jdanielnd/crm-cli/internal/csvio"
	"github.com/jdanielnd/crm-cli/internal/db"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/table"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openStore(t *testing.T) (*sql.DB, *table.Store) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "crm.db"))
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	return d, table.NewStore(d)
}

func parse(t *testing.T, s string) *csvio.Table {
	t.Helper()
	tb, err := csvio.Parse([]byte(s))
	require.NoError(t, err)
	return tb
}

func TestImport_KeepsOrderAndUpsertsByKey(t *testing.T) {
	ctx := context.Background()
	_, s := openStore(t)

	res, err := s.Import(ctx, table.ImportOptions{Name: "leads", Key: "login"}, parse(t, "login,score\nzed,10\namy,80\nbob,70\n"))
	require.NoError(t, err)
	assert.Equal(t, 3, res.Created)
	assert.Equal(t, []string{"login", "score"}, res.NewColumns)

	ws, err := s.FindWorkspace(ctx, "leads")
	require.NoError(t, err)
	require.NoError(t, s.AddColumn(ctx, &column.Column{WorkspaceID: ws.ID, Name: "profile", Type: column.Enrich,
		Config: column.Config{Waterfall: []string{"p"}, From: "login"}}))
	require.NoError(t, s.SetCells(ctx, mustRows(t, s, ws.ID)[1].ID, map[string]any{"profile": map[string]any{"company": "Acme"}}))

	// The re-import also carries a "profile" header: it names the enrich column, whose cells must stay.
	res, err = s.Import(ctx, table.ImportOptions{Name: "leads"}, parse(t, "login,score,city,profile\namy,90,Rio,overwritten\ncy,50,SP,seed\n"))
	require.NoError(t, err)
	assert.Equal(t, 1, res.Created)
	assert.Equal(t, 1, res.Updated)
	assert.Equal(t, []string{"city"}, res.NewColumns)

	rows := mustRows(t, s, ws.ID)
	require.Len(t, rows, 4)
	var logins []any
	for _, r := range rows {
		logins = append(logins, r.Data["login"])
	}
	assert.Equal(t, []any{"zed", "amy", "bob", "cy"}, logins)
	assert.Equal(t, "90", rows[1].Data["score"])
	assert.Equal(t, map[string]any{"company": "Acme"}, rows[1].Data["profile"], "computed cells survive re-import")
}

func mustRows(t *testing.T, s *table.Store, id int64) []*table.Row {
	t.Helper()
	rows, err := s.Rows(context.Background(), id)
	require.NoError(t, err)
	return rows
}

// Header errors fail the whole import; row errors skip only their row.
func TestImport_Validation(t *testing.T) {
	ctx := context.Background()
	_, s := openStore(t)
	header := map[string]struct {
		key, csv, want string
	}{
		"missing key":  {"nope", "id,a\n1,x\n", "not in the CSV header"},
		"dup header":   {"", "a,a\n1,2\n", "same header"},
		"empty header": {"", "a,\n1,2\n", "empty header"},
	}
	for name, tc := range header {
		t.Run(name, func(t *testing.T) {
			_, err := s.Import(ctx, table.ImportOptions{Name: "t-" + strings.ReplaceAll(name, " ", "-"), Key: tc.key}, parse(t, tc.csv))
			require.ErrorContains(t, err, tc.want)
			assert.ErrorIs(t, err, model.ErrValidation)
		})
	}
	rows := map[string]struct {
		csv, want string
	}{
		"empty key":     {"id,a\n1,x\n,y\n", "row 2: key column \"id\" is empty"},
		"duplicate key": {"id,a\n1,x\n1,y\n", "rows 1 and 2 have the same key"},
	}
	for name, tc := range rows {
		t.Run(name, func(t *testing.T) {
			res, err := s.Import(ctx, table.ImportOptions{Name: "r-" + strings.ReplaceAll(name, " ", "-"), Key: "id"}, parse(t, tc.csv))
			require.NoError(t, err)
			assert.Equal(t, 1, res.Created, "the valid row is written")
			assert.Equal(t, 1, res.Failed)
			require.Len(t, res.Results, 2)
			assert.NoError(t, res.Results[0].Err)
			require.ErrorContains(t, res.Results[1].Err, tc.want)
			assert.ErrorIs(t, res.Results[1].Err, model.ErrValidation)
			assert.Len(t, mustRows(t, s, res.Workspace.ID), 1)
		})
	}
	_, err := s.Import(ctx, table.ImportOptions{Name: "bad name"}, parse(t, "a\n1\n"))
	assert.ErrorIs(t, err, model.ErrValidation)
}

func TestFilter(t *testing.T) {
	cols := []string{"fit", "email", "name", "profile"}
	cells := map[string]any{"fit": "72", "email": "ana@acme.com", "name": "", "profile": map[string]any{"company": "Acme"}}
	cases := []struct {
		expr string
		want bool
	}{
		{"fit>=70", true},
		{"fit>=72.0", true},
		{"fit>72", false},
		{"fit<100", true},
		{"fit<=9", false},
		{"fit=72", true},
		{"fit=72.0", true},
		{"email=ana@acme.com", true},
		{"email~@acme\\.com$", true},
		{"email!~acme", false},
		{"name!=", false},
		{"email!=", true},
		{"name=", true},
		{"name>=0", false},
		{"profile~\"company\":\"Acme\"", true},
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			f, err := table.ParseFilter(tc.expr, cols)
			require.NoError(t, err)
			assert.Equal(t, tc.want, f.Match(cells))
		})
	}
	_, err := table.ParseFilter("nope=1", cols)
	assert.ErrorContains(t, err, "no column named")
	_, err = table.ParseFilter("fit", cols)
	assert.ErrorContains(t, err, "expected <column><op><value>")
	_, err = table.ParseFilter("email~(", cols)
	assert.ErrorContains(t, err, "invalid regex")
}

func TestColumns_AddFormulaAndRemove(t *testing.T) {
	ctx := context.Background()
	_, s := openStore(t)
	_, err := s.Import(ctx, table.ImportOptions{Name: "p"}, parse(t, "first,last\nAna,Lima\n"))
	require.NoError(t, err)
	ws, err := s.FindWorkspace(ctx, "p")
	require.NoError(t, err)

	full := &column.Column{WorkspaceID: ws.ID, Name: "full", Type: column.Formula, Config: column.Config{Expr: "{{first}} {{ last }}"}}
	require.NoError(t, s.AddColumn(ctx, full))
	err = s.AddColumn(ctx, &column.Column{WorkspaceID: ws.ID, Name: "x", Type: column.Formula, Config: column.Config{Expr: "{{nope}}"}})
	assert.ErrorContains(t, err, "references no column")
	err = s.AddColumn(ctx, &column.Column{WorkspaceID: ws.ID, Name: "full", Type: column.Input})
	assert.ErrorIs(t, err, model.ErrConflict)

	cols, err := s.Columns(ctx, ws.ID)
	require.NoError(t, err)
	view := table.NewView(cols, mustRows(t, s, ws.ID), nil)
	assert.Equal(t, [][]string{{"Ana", "Lima", "Ana Lima"}}, view.Records())

	assert.ErrorIs(t, s.RemoveColumn(ctx, ws, "last"), model.ErrConflict, "used by formula")
	require.NoError(t, s.RemoveColumn(ctx, ws, "full"))
	require.NoError(t, s.RemoveColumn(ctx, ws, "last"))
	cols, err = s.Columns(ctx, ws.ID)
	require.NoError(t, err)
	require.Len(t, cols, 1)
	require.NoError(t, s.AddColumn(ctx, &column.Column{WorkspaceID: ws.ID, Name: "last", Type: column.Input}), "name is free again")
}
