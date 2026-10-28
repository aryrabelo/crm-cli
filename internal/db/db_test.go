package db_test

import (
	"testing"

	"github.com/jdanielnd/crm-cli/internal/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpen_InMemory(t *testing.T) {
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	defer d.Close()

	// Verify tables exist
	tables := []string{"people", "organizations", "interactions", "deals", "tasks", "tags", "taggings", "custom_fields", "relationships", "import_batches", "import_rows"}
	for _, table := range tables {
		var name string
		err := d.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name)
		assert.NoError(t, err, "table %s should exist", table)
		assert.Equal(t, table, name)
	}
}

func TestOpen_FTS5Tables(t *testing.T) {
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	defer d.Close()

	fts := []string{"people_fts", "organizations_fts", "interactions_fts", "deals_fts"}
	for _, name := range fts {
		var found string
		err := d.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&found)
		assert.NoError(t, err, "FTS table %s should exist", name)
	}
}

func TestOpen_Pragmas(t *testing.T) {
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	defer d.Close()

	var journalMode string
	_ = d.QueryRow("PRAGMA journal_mode").Scan(&journalMode)
	// In-memory databases use "memory" journal mode, not WAL
	assert.Contains(t, []string{"wal", "memory"}, journalMode)

	var fk int
	_ = d.QueryRow("PRAGMA foreign_keys").Scan(&fk)
	assert.Equal(t, 1, fk)
}

func TestOpen_MigrationVersion(t *testing.T) {
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	defer d.Close()

	// Versions come from file names; user_version is the highest applied.
	var version, highest int
	_ = d.QueryRow("PRAGMA user_version").Scan(&version)
	require.NoError(t, d.QueryRow("SELECT MAX(version) FROM schema_migrations").Scan(&highest))
	assert.Equal(t, highest, version)
	assert.Equal(t, 1006, highest, "the newest migration (1006_run_items_workspace) is applied")
}

func TestRunItems_RowAndColumnMustBelongToTheRunsWorkspace(t *testing.T) {
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	defer d.Close()

	for _, q := range []string{
		"INSERT INTO workspaces (id, uuid, name) VALUES (1, 'u1', 'a'), (2, 'u2', 'b')",
		"INSERT INTO workspace_rows (id, workspace_id, position, data) VALUES (1, 1, 1, '{}'), (2, 2, 1, '{}')",
		"INSERT INTO workspace_columns (id, workspace_id, name, type, config, position) VALUES (1, 1, 'c', 'input', '{}', 1), (2, 2, 'c', 'input', '{}', 1)",
		"INSERT INTO runs (id, uuid, workspace_id, status, columns) VALUES (1, 'r1', 1, 'running', '[]')",
	} {
		_, err := d.Exec(q)
		require.NoError(t, err, q)
	}
	_, err = d.Exec("INSERT INTO run_items (run_id, row_id, column_id) VALUES (1, 1, 1)")
	require.NoError(t, err, "same workspace")
	_, err = d.Exec("INSERT INTO run_items (run_id, row_id, column_id) VALUES (1, 2, 1)")
	assert.ErrorContains(t, err, "belong to the workspace of the run", "row of another workspace")
	_, err = d.Exec("INSERT INTO run_items (run_id, row_id, column_id) VALUES (1, 1, 2)")
	assert.ErrorContains(t, err, "belong to the workspace of the run", "column of another workspace")
}
