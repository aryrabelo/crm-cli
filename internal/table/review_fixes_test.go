package table_test

import (
	"context"
	"testing"

	"github.com/jdanielnd/crm-cli/internal/column"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/table"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseFilter_ColumnNamesWithOperatorCharacters(t *testing.T) {
	cols := []string{"x=y", "Score>=70", "Score"}
	f, err := table.ParseFilter("x=y=3", cols)
	require.NoError(t, err)
	assert.Equal(t, "x=y", f.Column)
	assert.Equal(t, "=", f.Op)
	assert.Equal(t, "3", f.Value)

	f, err = table.ParseFilter("Score>=70>=1", cols)
	require.NoError(t, err)
	assert.Equal(t, "Score>=70", f.Column, "the longest known column wins")
	assert.Equal(t, ">=", f.Op)

	f, err = table.ParseFilter("Score>=5", cols)
	require.NoError(t, err)
	assert.Equal(t, "Score", f.Column)

	_, err = table.ParseFilter("nope=1", cols)
	require.ErrorIs(t, err, model.ErrValidation)
	assert.Contains(t, err.Error(), `no column named "nope"`)
}

func TestFilter_NaNAndInfAreText(t *testing.T) {
	ge, err := table.ParseFilter("c>=5", []string{"c"})
	require.NoError(t, err)
	le, err := table.ParseFilter("c<=5", []string{"c"})
	require.NoError(t, err)
	for _, text := range []string{"NaN", "Inf", "Infinity"} {
		cells := map[string]any{"c": text}
		assert.False(t, ge.Match(cells) && le.Match(cells), text+" must not satisfy both >= and <=")
	}
	eq, err := table.ParseFilter("c=Inf", []string{"c"})
	require.NoError(t, err)
	assert.False(t, eq.Match(map[string]any{"c": "+Inf"}), "Inf strings are not numerically equal")
}

func TestRemoveColumn_RejectsDottedReferenceThatWouldFallBack(t *testing.T) {
	ctx := context.Background()
	_, s := openStore(t)
	_, err := s.Import(ctx, table.ImportOptions{Name: "t"}, parse(t, "company,company.domain\nAcme,acme.com\n"))
	require.NoError(t, err)
	ws, err := s.FindWorkspace(ctx, "t")
	require.NoError(t, err)
	require.NoError(t, s.AddColumn(ctx, &column.Column{WorkspaceID: ws.ID, Name: "f", Type: column.Formula,
		Config: column.Config{Expr: "{{company.domain}}"}}))

	err = s.RemoveColumn(ctx, ws, "company.domain")
	require.ErrorIs(t, err, model.ErrConflict, "the formula would silently fall back to column company")
	assert.NoError(t, s.RemoveColumn(ctx, ws, "f"))
	assert.NoError(t, s.RemoveColumn(ctx, ws, "company.domain"))
}

func TestAddColumn_ReAddedNameDoesNotExposeStaleValues(t *testing.T) {
	ctx := context.Background()
	_, s := openStore(t)
	_, err := s.Import(ctx, table.ImportOptions{Name: "t"}, parse(t, "name,city\nAna,Rio\n"))
	require.NoError(t, err)
	ws, err := s.FindWorkspace(ctx, "t")
	require.NoError(t, err)
	require.NoError(t, s.RemoveColumn(ctx, ws, "city"))

	require.NoError(t, s.AddColumn(ctx, &column.Column{WorkspaceID: ws.ID, Name: "city", Type: column.Input}))
	rows := mustRows(t, s, ws.ID)
	assert.NotContains(t, rows[0].Data, "city")
	assert.Equal(t, "Ana", rows[0].Data["name"])
}

func TestImport_RejectsHeadersWithSurroundingWhitespace(t *testing.T) {
	ctx := context.Background()
	_, s := openStore(t)
	_, err := s.Import(ctx, table.ImportOptions{Name: "t"}, parse(t, "first, last\nA,B\n"))
	require.ErrorIs(t, err, model.ErrValidation)
	assert.Contains(t, err.Error(), "whitespace")
}
