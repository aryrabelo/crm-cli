package cli_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPush_DedupeByEmailAndFuzzyOrgs(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crm.db")
	src := writeFile(t, dir, "leads.csv", "name,email,company,stage\n"+
		"Ana Lima,ana@acme.io,Acme,hot\n"+
		"Ana Lima,ANA@acme.io ,Acme,cold\n"+
		"Bruno Costa,bruno@globex.com,Globex,\n"+
		"No Email,,Initech,\n")
	_, stderr, code := crm(t, dbPath, "table", "import", src, "--as", "leads")
	require.Equal(t, 0, code, stderr)

	// Dry run: the plan, nothing written.
	out, stderr, code := crm(t, dbPath, "push", "leads", "--to", "person", "--dry-run", "-f", "json")
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stderr, "dry run: 4 rows of leads to person (dedupe email): 2 created, 0 updated, 1 merged, 1 failed")
	rows := jsonRows(t, out)
	require.Len(t, rows, 4)
	assert.Equal(t, "create", rows[0]["action"])
	assert.Equal(t, "merge", rows[1]["action"])
	assert.Equal(t, "ana@acme.io", rows[1]["key"])
	assert.Equal(t, "error", rows[3]["action"])
	out, _, _ = crm(t, dbPath, "person", "list", "-f", "json")
	assert.Empty(t, jsonRows(t, out))
	quiet, stderr, code := crm(t, dbPath, "push", "leads", "--to", "person", "--dry-run", "--quiet")
	require.Equal(t, 0, code, stderr)
	assert.Empty(t, strings.TrimSpace(quiet), "dry-run --quiet prints no placeholder 0 ids")

	out, stderr, code = crm(t, dbPath, "push", "leads", "--to", "person", "-f", "json")
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stderr, "pushed 4 rows of leads to person (dedupe email): 2 created, 0 updated, 1 merged, 1 failed")
	rows = jsonRows(t, out)
	assert.Equal(t, rows[0]["id"], rows[1]["id"], "the duplicate merged into the first row's person")
	out, _, _ = crm(t, dbPath, "person", "list", "-f", "json")
	assert.Len(t, jsonRows(t, out), 2)

	// Pushing again updates instead of creating.
	_, stderr, code = crm(t, dbPath, "push", "leads", "--to", "person")
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stderr, "0 created, 3 updated, 0 merged, 1 failed")

	// Organizations by fuzzy name, from the company column.
	orgs := writeFile(t, dir, "orgs.csv", "company\nAcme Analytics\n\"ACME Analytics, Inc.\"\nAcmé Analytics Ltda\nGlobex\n")
	_, stderr, code = crm(t, dbPath, "table", "import", orgs, "--as", "orgs")
	require.Equal(t, 0, code, stderr)
	_, stderr, code = crm(t, dbPath, "push", "orgs", "--to", "org", "--dedupe", "fuzz", "--only", "company!=Globex")
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stderr, "pushed 3 rows of orgs to org (dedupe fuzz): 1 created, 0 updated, 2 merged, 0 failed")
}

// Cubic #4381117094: an explicit --threshold 0 is honoured, not replaced by the default.
func TestPush_ThresholdZeroIsNotTheDefault(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crm.db")
	orgs := writeFile(t, dir, "orgs.csv", "company\nAcme Analytics\nGlobex\n")
	_, stderr, code := crm(t, dbPath, "table", "import", orgs, "--as", "orgs")
	require.Equal(t, 0, code, stderr)
	_, stderr, code = crm(t, dbPath, "push", "orgs", "--to", "org", "--dedupe", "fuzz", "--dry-run")
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stderr, "2 created, 0 updated, 0 merged")
	_, stderr, code = crm(t, dbPath, "push", "orgs", "--to", "org", "--dedupe", "fuzz", "--threshold", "0", "--dry-run")
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stderr, "1 created, 0 updated, 1 merged")
}

func TestPush_UsageErrors(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crm.db")
	src := writeFile(t, dir, "t.csv", "name,city\nAna,Rio\n")
	_, stderr, code := crm(t, dbPath, "table", "import", src, "--as", "t")
	require.Equal(t, 0, code, stderr)
	cases := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"missing --to", []string{"push", "t"}, 1, `required flag(s) "to" not set`},
		{"bad target", []string{"push", "t", "--to", "deal"}, 2, "--to must be person or org"},
		{"email for orgs", []string{"push", "t", "--to", "org", "--dedupe", "email"}, 2, "needs --to person"},
		{"no email column", []string{"push", "t", "--to", "person"}, 2, "no column of table t maps to email"},
		{"bad threshold", []string{"push", "t", "--to", "person", "--dedupe", "fuzz", "--threshold", "2"}, 2, "between 0 and 1"},
		{"nan threshold", []string{"push", "t", "--to", "person", "--dedupe", "fuzz", "--threshold", "NaN"}, 2, "between 0 and 1"},
		{"missing table", []string{"push", "nope", "--to", "person"}, 3, "not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, code := crm(t, dbPath, tc.args...)
			assert.Equal(t, tc.code, code, stderr)
			assert.Contains(t, stderr, tc.want)
		})
	}
}
