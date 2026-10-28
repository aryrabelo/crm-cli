package db

import (
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// legacyForkFiles are the fork migrations as the positional runner applied
// them (user_version = file count) before they moved to 1001+.
var legacyForkFiles = map[string]string{
	"002_imports.sql":    "1001_imports.sql",
	"003_data_plane.sql": "1002_data_plane.sql",
	"004_ai_columns.sql": "1003_ai_columns.sql",
	"001_initial.sql":    "001_initial.sql",
}

// legacyForkDB builds a database exactly as the old positional runner left
// it: 001 + the three fork files applied, user_version = 4.
func legacyForkDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy.db"))
	require.NoError(t, err)
	d.SetMaxOpenConns(1)
	t.Cleanup(func() { d.Close() })
	for _, old := range []string{"001_initial.sql", "002_imports.sql", "003_data_plane.sql", "004_ai_columns.sql"} {
		content, err := fs.ReadFile(migrationsFS, "migrations/"+legacyForkFiles[old])
		require.NoError(t, err)
		_, err = d.Exec(extractUpSection(string(content)))
		require.NoError(t, err, old)
	}
	_, err = d.Exec("PRAGMA user_version = 4")
	require.NoError(t, err)
	return d
}

// withUpstream is the embedded migrations plus a migration upstream could
// ship next (002_*), as after merging upstream into the fork.
func withUpstream(t *testing.T) fs.FS {
	t.Helper()
	m := fstest.MapFS{}
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	require.NoError(t, err)
	for _, e := range entries {
		b, err := fs.ReadFile(migrationsFS, "migrations/"+e.Name())
		require.NoError(t, err)
		m["migrations/"+e.Name()] = &fstest.MapFile{Data: b}
	}
	m["migrations/002_deal_stages.sql"] = &fstest.MapFile{Data: []byte(
		"-- up\nCREATE TABLE deal_stages (id INTEGER PRIMARY KEY, name TEXT NOT NULL);\n-- down\nDROP TABLE deal_stages;\n")}
	return m
}

func versions(t *testing.T, d *sql.DB) []int {
	t.Helper()
	rows, err := d.Query("SELECT version FROM schema_migrations ORDER BY version")
	require.NoError(t, err)
	defer rows.Close()
	var out []int
	for rows.Next() {
		var v int
		require.NoError(t, rows.Scan(&v))
		out = append(out, v)
	}
	require.NoError(t, rows.Err())
	return out
}

// Design review P1-4: an upstream 002_* migration merged into the fork must
// not break an existing fork database (it used to re-run 004 → "duplicate
// column" and skip upstream's 002 for good).
func TestMigrations_UpstreamMigrationAfterForkDB(t *testing.T) {
	d := legacyForkDB(t)
	require.NoError(t, runMigrations(d, withUpstream(t), "migrations"))

	assert.Equal(t, []int{1, 2, 1001, 1002, 1003}, versions(t, d)[:5])
	var n int
	require.NoError(t, d.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name = 'deal_stages'").Scan(&n))
	assert.Equal(t, 1, n, "upstream 002 applied")
	require.NoError(t, d.QueryRow("SELECT COUNT(*) FROM pragma_table_info('run_items') WHERE name = 'tokens_in'").Scan(&n))
	assert.Equal(t, 1, n)

	// Idempotent: a second open applies nothing.
	require.NoError(t, runMigrations(d, withUpstream(t), "migrations"))
}

func TestMigrations_FreshDBWithUpstreamMigration(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "fresh.db"))
	require.NoError(t, err)
	d.SetMaxOpenConns(1)
	t.Cleanup(func() { d.Close() })
	require.NoError(t, runMigrations(d, withUpstream(t), "migrations"))
	got := versions(t, d)
	assert.Equal(t, []int{1, 2, 1001, 1002, 1003}, got[:5])
	var uv int
	require.NoError(t, d.QueryRow("PRAGMA user_version").Scan(&uv))
	assert.Equal(t, got[len(got)-1], uv)
}

// A database migrated only by upstream (user_version counts upstream files)
// keeps its upstream migrations and gets the fork's.
func TestMigrations_UpstreamOnlyDB(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "upstream.db"))
	require.NoError(t, err)
	d.SetMaxOpenConns(1)
	t.Cleanup(func() { d.Close() })
	content, err := fs.ReadFile(migrationsFS, "migrations/001_initial.sql")
	require.NoError(t, err)
	_, err = d.Exec(extractUpSection(string(content)))
	require.NoError(t, err)
	_, err = d.Exec("CREATE TABLE deal_stages (id INTEGER PRIMARY KEY, name TEXT NOT NULL)")
	require.NoError(t, err)
	_, err = d.Exec("PRAGMA user_version = 2")
	require.NoError(t, err)

	require.NoError(t, runMigrations(d, withUpstream(t), "migrations"))
	assert.Equal(t, []int{1, 2, 1001, 1002, 1003}, versions(t, d)[:5])
}

func TestLoadMigrations_RejectsBadNames(t *testing.T) {
	for name, file := range map[string]string{"no number": "initial.sql", "zero": "000_x.sql"} {
		t.Run(name, func(t *testing.T) {
			_, err := loadMigrations(fstest.MapFS{"m/" + file: {Data: []byte("-- up\nSELECT 1;\n")}}, "m")
			require.Error(t, err)
		})
	}
	_, err := loadMigrations(fstest.MapFS{
		"m/002_a.sql": {Data: []byte("-- up\nSELECT 1;\n")},
		"m/002_b.sql": {Data: []byte("-- up\nSELECT 1;\n")},
	}, "m")
	require.ErrorContains(t, err, "share version 2")
}
