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
	assert.GreaterOrEqual(t, highest, 1003)
}
