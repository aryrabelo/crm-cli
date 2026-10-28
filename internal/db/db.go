package db

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Open opens a SQLite database, sets pragmas, and runs migrations.
func Open(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// Set a single connection — SQLite doesn't benefit from a pool
	db.SetMaxOpenConns(1)

	if err := setPragmas(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("set pragmas: %w", err)
	}

	if err := runMigrations(db, migrationsFS, "migrations"); err != nil {
		db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	return db, nil
}

func setPragmas(db *sql.DB) error {
	pragmas := []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA foreign_keys = ON",
		"PRAGMA cache_size = -64000",
		"PRAGMA temp_store = MEMORY",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
	}
	return nil
}

// A migration is one migrations/NNN_name.sql file. Its version is the
// numeric prefix of the file name, never its position in the listing, so a
// migration added upstream cannot shift the versions of the fork's.
// Upstream owns versions 1–999; the fork numbers its migrations from 1001.
type migration struct {
	version int
	name    string
	up      string
}

// forkVersionStart is the first migration version owned by the fork.
const forkVersionStart = 1001

// loadMigrations reads every *.sql file of dir in fsys, ordered by version.
func loadMigrations(fsys fs.FS, dir string) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	var out []migration
	seen := map[int]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		prefix, _, _ := strings.Cut(name, "_")
		version, err := strconv.Atoi(prefix)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("migration %s: file name must start with a positive version number (NNN_name.sql)", name)
		}
		if other, dup := seen[version]; dup {
			return nil, fmt.Errorf("migrations %s and %s share version %d", other, name, version)
		}
		seen[version] = name
		content, err := fs.ReadFile(fsys, path.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", name, err)
		}
		up := extractUpSection(string(content))
		if strings.TrimSpace(up) == "" {
			return nil, fmt.Errorf("migration %s: no -- up section found", name)
		}
		out = append(out, migration{version: version, name: name, up: up})
	}
	slices.SortFunc(out, func(a, b migration) int { return a.version - b.version })
	return out, nil
}

// legacyProbes detect the fork migrations a database applied before
// migrations were tracked by version, when the fork shipped them as
// 002_imports, 003_data_plane and 004_ai_columns and user_version counted
// files. Each probe returns a row when the migration's schema is present.
var legacyProbes = map[int]string{
	1001: "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'import_batches'",
	1002: "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'workspaces'",
	1003: "SELECT 1 FROM pragma_table_info('run_items') WHERE name = 'tokens_in'",
}

// runMigrations applies every migration of fsys/dir whose version is not
// recorded in schema_migrations, in version order, one transaction each.
// user_version only grows (to the highest version applied so far); it is
// informational and no longer decides what runs.
func runMigrations(db *sql.DB, fsys fs.FS, dir string) error {
	migrations, err := loadMigrations(fsys, dir)
	if err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	applied, err := appliedVersions(db)
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		if applied, err = adoptLegacy(db, migrations); err != nil {
			return err
		}
	}
	for _, m := range migrations {
		if applied[m.version] {
			continue
		}
		if err := apply(db, m); err != nil {
			return err
		}
		applied[m.version] = true
	}
	return nil
}

func appliedVersions(db *sql.DB) (map[int]bool, error) {
	rows, err := db.Query("SELECT version FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()
	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schema_migrations: %w", err)
	}
	return applied, nil
}

// adoptLegacy records the migrations of a database migrated by the old
// positional runner (user_version > 0, no schema_migrations rows). The
// fork's migrations are detected by their schema. A fork database counted
// 001 plus fork files in user_version; a database without fork schema was
// migrated by upstream, whose user_version counts its own files 001..N.
func adoptLegacy(db *sql.DB, migrations []migration) (map[int]bool, error) {
	applied := map[int]bool{}
	var userVersion int
	if err := db.QueryRow("PRAGMA user_version").Scan(&userVersion); err != nil {
		return nil, fmt.Errorf("get user_version: %w", err)
	}
	if userVersion == 0 {
		return applied, nil
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin legacy adoption: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	fork := false
	for _, m := range migrations {
		probe, ok := legacyProbes[m.version]
		if !ok {
			continue
		}
		var one int
		switch err := tx.QueryRow(probe).Scan(&one); {
		case err == nil:
			applied[m.version], fork = true, true
		case !errors.Is(err, sql.ErrNoRows):
			return nil, fmt.Errorf("probe legacy migration %s: %w", m.name, err)
		}
	}
	for _, m := range migrations {
		if m.version < forkVersionStart && (m.version == 1 || !fork && m.version <= userVersion) {
			applied[m.version] = true
		}
	}
	for _, m := range migrations {
		if !applied[m.version] {
			continue
		}
		if _, err := tx.Exec("INSERT INTO schema_migrations (version, name) VALUES (?, ?)", m.version, m.name); err != nil {
			return nil, fmt.Errorf("record legacy migration %s: %w", m.name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit legacy adoption: %w", err)
	}
	return applied, nil
}

func apply(db *sql.DB, m migration) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", m.name, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(m.up); err != nil {
		return fmt.Errorf("execute migration %s: %w", m.name, err)
	}
	if _, err := tx.Exec("INSERT INTO schema_migrations (version, name) VALUES (?, ?)", m.version, m.name); err != nil {
		return fmt.Errorf("record migration %s: %w", m.name, err)
	}
	// PRAGMA user_version does not support ? placeholders; the version is
	// parsed from an embedded file name, never from user input. It only ever
	// grows, so an upstream migration applied late does not lower it.
	var current int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("get user_version: %w", err)
	}
	if m.version > current {
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
			return fmt.Errorf("set user_version to %d: %w", m.version, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %s: %w", m.name, err)
	}
	return nil
}

func extractUpSection(content string) string {
	lines := strings.Split(content, "\n")
	var result []string
	inUp := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "-- up" {
			inUp = true
			continue
		}
		if trimmed == "-- down" {
			break
		}
		if inUp {
			result = append(result, line)
		}
	}

	return strings.Join(result, "\n")
}
