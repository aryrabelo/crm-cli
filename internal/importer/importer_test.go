package importer_test

import (
	"context"
	"testing"

	"github.com/jdanielnd/crm-cli/internal/csvio"
	"github.com/jdanielnd/crm-cli/internal/db"
	"github.com/jdanielnd/crm-cli/internal/db/repo"
	"github.com/jdanielnd/crm-cli/internal/importer"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveColumns_AutoMap(t *testing.T) {
	cols, err := importer.ResolveColumns("person", []string{"Full Name", "E-mail", "Telefone", "Company", "Cidade", "Mobile"}, nil)
	require.NoError(t, err)
	assert.Equal(t, []model.ImportColumn{
		{Header: "Full Name", Field: "name"},
		{Header: "E-mail", Field: "email"},
		{Header: "Telefone", Field: "phone"},
		{Header: "Company", Field: "company"},
		{Header: "Cidade", Field: "Cidade", Custom: true},
		// second phone-like column: phone already taken, so it stays custom
		{Header: "Mobile", Field: "Mobile", Custom: true},
	}, cols)
}

func TestResolveColumns_OrgAutoMap(t *testing.T) {
	cols, err := importer.ResolveColumns("organization", []string{"Company", "Website", "Setor", "Funcionários"}, nil)
	require.NoError(t, err)
	assert.Equal(t, "name", cols[0].Field)
	assert.Equal(t, "domain", cols[1].Field)
	assert.Equal(t, "industry", cols[2].Field)
	assert.True(t, cols[3].Custom)
}

func TestResolveColumns_MapOverrides(t *testing.T) {
	cols, err := importer.ResolveColumns("person",
		[]string{"Contato", "email", "Empresa"},
		[]string{"Contato=name", "email=", "Empresa=employer"})
	require.NoError(t, err)
	assert.Equal(t, model.ImportColumn{Header: "Contato", Field: "name"}, cols[0])
	assert.Equal(t, model.ImportColumn{Header: "email", Field: "email", Custom: true}, cols[1])
	assert.Equal(t, model.ImportColumn{Header: "Empresa", Field: "employer", Custom: true}, cols[2])
}

func TestResolveColumns_Errors(t *testing.T) {
	cases := []struct {
		name   string
		entity string
		header []string
		maps   []string
		want   string
	}{
		{"duplicate header", "person", []string{"name", "x", "x"}, nil, `duplicate header "x" (columns 2 and 3)`},
		{"empty header", "person", []string{"name", ""}, nil, "column 2 has an empty header"},
		{"no name", "person", []string{"email", "x"}, nil, "no column maps to the person's name"},
		{"no org name", "organization", []string{"domain"}, nil, "no column maps to the organization's name"},
		{"unknown map column", "person", []string{"name"}, []string{"nope=email"}, `no column named "nope"`},
		{"bad map syntax", "person", []string{"name"}, []string{"name"}, "expected csvcol=field"},
		{"two cols one field", "person", []string{"a", "b"}, []string{"a=name", "b=name"}, "both map to name"},
		{"name with first", "person", []string{"name", "first"}, []string{"first=first_name"}, "cannot be combined"},
		{"custom collision", "person", []string{"name", "a", "b"}, []string{"a=b"}, `both map to custom field "b"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := importer.ResolveColumns(tc.entity, tc.header, tc.maps)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.Equal(t, 2, model.ExitCode(err))
		})
	}
}

func TestSplitJoinName_RoundTrip(t *testing.T) {
	for _, s := range []string{"", "Ana", "Jane Smith", "Maria  Clara", "Bia ", " lead", "José da Silva Júnior"} {
		first, last := importer.SplitName(s)
		assert.Equal(t, s, importer.JoinName(first, last), "name %q", s)
	}
	first, last := importer.SplitName("Jane Mary Smith")
	assert.Equal(t, "Jane", first)
	assert.Equal(t, "Mary Smith", *last)
}

func TestRun_UpsertByKeyAndCell(t *testing.T) {
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	ctx := context.Background()

	tbl := &csvio.Table{
		Header:  []string{"name", "email", "Cidade"},
		Rows:    [][]string{{"Ana Souza", "ana@x.com", "Recife"}, {"Bob", "bob@x.com", ""}},
		Dialect: csvio.DefaultDialect,
	}
	opts := importer.Options{EntityType: "person", Source: "test.csv", Key: "email"}
	res, err := importer.Run(ctx, d, opts, tbl)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Created)

	tbl.Rows = [][]string{{"Ana Souza", "ana@x.com", ""}, {"Bob Lee", "bob@x.com", "Natal"}, {"Cy", "cy@x.com", "Belém"}}
	res, err = importer.Run(ctx, d, opts, tbl)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Created)
	assert.Equal(t, 2, res.Updated)

	rows, err := repo.NewImportRepo(d).BatchRows(ctx, res.Batch)
	require.NoError(t, err)
	require.Len(t, rows, 3)
	got := make([][]string, len(rows))
	for i, r := range rows {
		for _, c := range res.Batch.Columns {
			got[i] = append(got[i], importer.Cell("person", c, r))
		}
	}
	assert.Equal(t, tbl.Rows, got)

	all, _, err := repo.NewImportRepo(d).ListEntityRows(ctx, "person")
	require.NoError(t, err)
	assert.Len(t, all, 3)
}

func TestRun_KeyValidation(t *testing.T) {
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })

	tbl := &csvio.Table{Header: []string{"name", "email"}, Rows: [][]string{{"A", "a@x"}, {"B", "a@x"}}}
	_, err = importer.Run(context.Background(), d, importer.Options{EntityType: "person", Key: "email"}, tbl)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `rows 1 and 2 have the same key "a@x"`)

	tbl.Rows = [][]string{{"A", ""}}
	_, err = importer.Run(context.Background(), d, importer.Options{EntityType: "person", Key: "email"}, tbl)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `row 1: key column "email" is empty`)

	_, err = importer.Run(context.Background(), d, importer.Options{EntityType: "person", Key: "nope"}, tbl)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `--key "nope" matches no column`)
}

func TestExportColumns_UniqueHeaders(t *testing.T) {
	cols := importer.ExportColumns("person", []string{"id", "custom.id", "email"})
	seen := map[string]bool{}
	for _, c := range cols {
		require.False(t, seen[c.Header], "duplicate header %q", c.Header)
		seen[c.Header] = true
	}
}
