package cli_test

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var updateFixtures = flag.Bool("update", false, "regenerate testdata CSV fixtures")

// csvCell quotes a cell RFC 4180-style only when needed (minimal quoting, as
// written by spreadsheets), independently of the code under test.
func csvCell(s string) string {
	if strings.ContainsAny(s, ",\"\r\n") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

func csvLine(cells []string) string {
	out := make([]string, len(cells))
	for i, c := range cells {
		out[i] = csvCell(c)
	}
	return strings.Join(out, ",")
}

var trickyCells = []string{
	"São Paulo, SP",
	`Diz "oi" sempre`,
	"linha 1\nlinha 2",
	"",
	"Açaí & Pão de Queijo",
	" espaço na borda ",
	"emoji 🚀, ok",
	"=SUM(A1)",
	"",
	"x\r\ny",
}

// people135x4 builds a 135-row × 4-column people CSV (LF, trailing newline).
func people135x4() string {
	lines := []string{csvLine([]string{"name", "email", "company", "Cidade"})}
	names := []string{"José da Silva", "Ana", "Maria  Clara", "Bia ", "Zoë O'Brien", "Smith, Jane", `Joe "JJ" Doe`}
	for i := range 135 {
		lines = append(lines, csvLine([]string{
			fmt.Sprintf("%s %03d", names[i%len(names)], i),
			fmt.Sprintf("lead%03d@exemplo.com.br", i),
			trickyCells[(i+3)%len(trickyCells)],
			trickyCells[i%len(trickyCells)],
		}))
	}
	return strings.Join(lines, "\n") + "\n"
}

// people6x30 builds a 6-row × 30-column people CSV with a UTF-8 BOM, CRLF
// line endings, no trailing newline and quoted headers.
func people6x30() string {
	header := []string{"Nome", "E-mail"}
	for i := 3; i <= 30; i++ {
		switch i {
		case 7:
			header = append(header, "Observação")
		case 12:
			header = append(header, "Last, First")
		case 20:
			header = append(header, `Apelido "nick"`)
		default:
			header = append(header, fmt.Sprintf("Campo %02d", i))
		}
	}
	lines := []string{csvLine(header)}
	for r := range 6 {
		row := []string{fmt.Sprintf("Pessoa Número %d", r+1), fmt.Sprintf("p%d@exemplo.com", r+1)}
		for c := 3; c <= 30; c++ {
			row = append(row, trickyCells[(r*7+c)%len(trickyCells)])
		}
		lines = append(lines, csvLine(row))
	}
	return "\xEF\xBB\xBF" + strings.Join(lines, "\r\n")
}

func fixture(t *testing.T, name, content string) string {
	t.Helper()
	root, err := findProjectRoot()
	require.NoError(t, err)
	path := filepath.Join(root, "internal", "cli", "testdata", name)
	if *updateFixtures {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
	data, err := os.ReadFile(path)
	require.NoError(t, err, "fixture missing; run go test ./internal/cli -run TestImport -update")
	require.Equal(t, content, string(data), "fixture %s is stale; run with -update", name)
	return path
}

func importBatchID(t *testing.T, stdout string) int64 {
	t.Helper()
	var out []map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &out), stdout)
	require.Len(t, out, 1)
	return int64(out[0]["batch_id"].(float64))
}

func TestImport_RoundTripByteIdentical(t *testing.T) {
	cases := map[string]string{
		"people_135x4.csv":         people135x4(),
		"people_6x30_bom_crlf.csv": people6x30(),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := fixture(t, name, content)
			dbPath := filepath.Join(t.TempDir(), "crm.db")

			stdout, stderr, code := crm(t, dbPath, "import", path, "--as", "person", "-f", "json")
			require.Equal(t, 0, code, stderr)
			id := importBatchID(t, stdout)

			out, stderr, code := crm(t, dbPath, "rows", "person", "--import", fmt.Sprint(id), "-f", "csv")
			require.Equal(t, 0, code, stderr)
			assert.Equal(t, content, out)
		})
	}
}

func TestImport_RowCountsAndCustomFields(t *testing.T) {
	path := fixture(t, "people_6x30_bom_crlf.csv", people6x30())
	dbPath := filepath.Join(t.TempDir(), "crm.db")
	_, stderr, code := crm(t, dbPath, "import", path, "--as", "person", "-f", "json")
	require.Equal(t, 0, code, stderr)

	stdout, _, code := crm(t, dbPath, "person", "list", "-f", "json")
	require.Equal(t, 0, code)
	var people []map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &people))
	assert.Len(t, people, 6)

	stdout, _, code = crm(t, dbPath, "person", "show", "1", "-f", "json")
	require.Equal(t, 0, code)
	var shown []map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &shown))
	assert.Equal(t, "Pessoa", shown[0]["first_name"])
	assert.Equal(t, "Número 1", shown[0]["last_name"])
	assert.Equal(t, "p1@exemplo.com", shown[0]["email"])
	custom := shown[0]["custom_fields"].(map[string]any)
	// 28 custom columns minus the empty cells of row 1
	nonEmpty := 0
	for c := 3; c <= 30; c++ {
		if trickyCells[c%len(trickyCells)] != "" {
			nonEmpty++
		}
	}
	assert.Len(t, custom, nonEmpty)
	assert.Equal(t, trickyCells[7%len(trickyCells)], custom["Observação"])

	stdout, _, code = crm(t, dbPath, "person", "show", "1", "-f", "table")
	require.Equal(t, 0, code)
	assert.Contains(t, stdout, "CUSTOM FIELD")
	assert.Contains(t, stdout, "Observação")
	assert.Contains(t, stdout, "=SUM(A1)")
}

func TestImport_ReimportWithKeyDoesNotDuplicate(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crm.db")
	first := "name,email,Plano\nAna Lima,ana@x.com,free\nBob,bob@x.com,pro\n"
	second := "name,email,Plano\nAna Lima,ana@x.com,pro\nBob,bob@x.com,\nCy,cy@x.com,free\n"
	f1 := filepath.Join(dir, "a.csv")
	f2 := filepath.Join(dir, "b.csv")
	require.NoError(t, os.WriteFile(f1, []byte(first), 0o644))
	require.NoError(t, os.WriteFile(f2, []byte(second), 0o644))

	_, stderr, code := crm(t, dbPath, "import", f1, "--as", "person", "--key", "email", "-f", "json")
	require.Equal(t, 0, code, stderr)
	_, stderr, code = crm(t, dbPath, "import", f1, "--as", "person", "--key", "email", "-f", "json")
	require.Equal(t, 0, code, stderr)
	stdout, stderr, code := crm(t, dbPath, "import", f2, "--as", "person", "--key", "email", "-f", "json")
	require.Equal(t, 0, code, stderr)
	var summary []map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &summary))
	assert.EqualValues(t, 1, summary[0]["created"])
	assert.EqualValues(t, 2, summary[0]["updated"])

	out, _, code := crm(t, dbPath, "rows", "person", "-f", "csv")
	require.Equal(t, 0, code)
	assert.Equal(t, "id,first_name,last_name,email,phone,title,company,location,notes,summary,Plano\n"+
		"1,Ana,Lima,ana@x.com,,,,,,,pro\n"+
		"2,Bob,,bob@x.com,,,,,,,\n"+
		"3,Cy,,cy@x.com,,,,,,,free\n", out)

	out, _, code = crm(t, dbPath, "rows", "person", "--import", "3", "-f", "csv")
	require.Equal(t, 0, code)
	assert.Equal(t, second, out)

	out, _, code = crm(t, dbPath, "rows", "person", "--only", "Plano=free", "-q")
	require.Equal(t, 0, code)
	assert.Equal(t, "3\n", out)
}

func TestImport_DryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crm.db")
	f := filepath.Join(dir, "a.csv")
	require.NoError(t, os.WriteFile(f, []byte("name,email,Plano\nAna,ana@x.com,free\nBob,bob@x.com,pro\n"), 0o644))

	_, _, code := crm(t, dbPath, "import", f, "--as", "person", "--key", "email", "-f", "json")
	require.Equal(t, 0, code)
	require.NoError(t, os.WriteFile(f, []byte("name,email,Plano\nAna,ana@x.com,CHANGED\nNew,new@x.com,free\n"), 0o644))

	before, _, _ := crm(t, dbPath, "rows", "person", "-f", "csv")
	stdout, stderr, code := crm(t, dbPath, "import", f, "--as", "person", "--key", "email", "--dry-run", "-f", "json")
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stderr, "dry run: 2 rows → 1 create, 1 update; nothing written")
	var actions []map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &actions))
	require.Len(t, actions, 2)
	assert.Equal(t, "update", actions[0]["action"])
	assert.EqualValues(t, 1, actions[0]["id"])
	assert.Equal(t, "create", actions[1]["action"])

	after, _, _ := crm(t, dbPath, "rows", "person", "-f", "csv")
	assert.Equal(t, before, after)
	_, stderr, code = crm(t, dbPath, "rows", "person", "--import", "2", "-f", "csv")
	assert.Equal(t, 3, code, "dry run must not create a batch")
	assert.Contains(t, stderr, "not found")
}

func TestImport_Errors(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crm.db")
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
		return p
	}

	cases := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"duplicate header", []string{"import", write("dup.csv", "name,x,x\na,1,2\n"), "--as", "person"}, 2, `duplicate header "x" (columns 2 and 3)`},
		{"ragged row", []string{"import", write("rag.csv", "name,x\na\n"), "--as", "person"}, 2, "row 1 has 1 fields, header has 2"},
		{"unterminated quote", []string{"import", write("q.csv", "name,x\n\"a,1\n"), "--as", "person"}, 2, "unterminated quoted field"},
		{"missing --as", []string{"import", write("ok.csv", "name\na\n")}, 2, "entity is required"},
		{"missing file", []string{"import", filepath.Join(dir, "nope.csv"), "--as", "person"}, 3, "file not found"},
		{"duplicate key", []string{"import", write("k.csv", "name,email\na,e@x\nb,e@x\n"), "--as", "person", "--key", "email"}, 2, "same key"},
		{"missing batch", []string{"rows", "org", "--import", "1"}, 3, "not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, code := crm(t, dbPath, tc.args...)
			assert.Equal(t, tc.code, code, stderr)
			assert.Contains(t, stderr, tc.want)
		})
	}

	// nothing above may have written a batch
	_, _, code := crm(t, dbPath, "rows", "person", "--import", "1", "-f", "csv")
	assert.Equal(t, 3, code)
}

func TestImport_Stdin(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "crm.db")
	in := "Company,Website,Funcionários\n\"Acme, Inc.\",acme.com,120\nÓtica Sol,,\n"
	cmd := exec.Command(binaryPath, "--db", dbPath, "import", "-", "--as", "org", "-f", "json")
	cmd.Stdin = strings.NewReader(in)
	out, err := cmd.Output()
	require.NoError(t, err)
	id := importBatchID(t, string(out))

	got, _, code := crm(t, dbPath, "rows", "org", "--import", fmt.Sprint(id), "-f", "csv")
	require.Equal(t, 0, code)
	assert.Equal(t, in, got)

	stdout, _, code := crm(t, dbPath, "org", "show", "1", "-f", "json")
	require.Equal(t, 0, code)
	var shown []map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &shown))
	assert.Equal(t, "Acme, Inc.", shown[0]["name"])
	assert.Equal(t, "acme.com", shown[0]["domain"])
	assert.Equal(t, map[string]any{"Funcionários": "120"}, shown[0]["custom_fields"])

	_, stderr, code := crm(t, dbPath, "rows", "person", "--import", fmt.Sprint(id))
	assert.Equal(t, 2, code)
	assert.Contains(t, stderr, "holds organization records, not person")
}

// Cubic #4380317756: --import is an immutable export of the batch as it was
// imported, even after a keyed re-import changes the records or one is archived.
func TestRowsImport_IsASnapshot(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crm.db")
	f := filepath.Join(dir, "a.csv")
	first := "name,email,Plano\nAna,ana@x.com,free\nBob,bob@x.com,pro\n"
	require.NoError(t, os.WriteFile(f, []byte(first), 0o644))
	_, stderr, code := crm(t, dbPath, "import", f, "--as", "person", "--key", "email")
	require.Equal(t, 0, code, stderr)

	require.NoError(t, os.WriteFile(f, []byte("name,email,Plano\nAna Maria,ana@x.com,pro\n"), 0o644))
	_, stderr, code = crm(t, dbPath, "import", f, "--as", "person", "--key", "email")
	require.Equal(t, 0, code, stderr)
	_, stderr, code = crm(t, dbPath, "person", "delete", "2")
	require.Equal(t, 0, code, stderr)

	out, stderr, code := crm(t, dbPath, "rows", "person", "--import", "1", "-f", "csv")
	require.Equal(t, 0, code, stderr)
	assert.Equal(t, first, out)
}
