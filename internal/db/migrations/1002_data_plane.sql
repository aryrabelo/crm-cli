-- up

-- Data plane (fork, slice S1): workspace tables of rows, computed columns,
-- batch runs with a provider waterfall, an enrichment cache and budgets.
-- Table names follow the upstream plural convention; PLANO §2 names them
-- workspace, row, column, run, run_item, enrichment_cache, provider.
-- Money is stored as integer micro-USD (1 USD = 1000000) so budget math is exact.

-- A workspace is one "Clay table": a named set of rows plus declared columns.
-- plan holds the markdown plan used by later slices (`crm run --plan`).
CREATE TABLE IF NOT EXISTS workspaces (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    uuid TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL UNIQUE,
    key_column TEXT,
    plan TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Rows keep their import order in position. data is a JSON object holding the
-- input cells (strings) and the values written by runs (any JSON).
CREATE TABLE IF NOT EXISTS workspace_rows (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    position INTEGER NOT NULL,
    key TEXT,
    data TEXT NOT NULL DEFAULT '{}',
    state TEXT NOT NULL DEFAULT 'new',
    source TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (workspace_id, position)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_workspace_rows_key
    ON workspace_rows(workspace_id, key) WHERE key IS NOT NULL;

-- Column definitions (the declared schema of a workspace). config is a JSON
-- object whose shape depends on type; cost_est_micros is the worst-case cost
-- of computing the column for one row.
CREATE TABLE IF NOT EXISTS workspace_columns (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    type TEXT NOT NULL CHECK(type IN ('input', 'formula', 'enrich', 'ai', 'signal', 'score')),
    config TEXT NOT NULL DEFAULT '{}',
    cost_est_micros INTEGER NOT NULL DEFAULT 0,
    position INTEGER NOT NULL,
    archived INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_workspace_columns_name
    ON workspace_columns(workspace_id, name) WHERE archived = 0;

-- Provider adapters. auth_ref names the environment variable holding the
-- credential (secrets are never stored); rate is max requests per second
-- (0 = unlimited); price_micros is the cost of one successful call.
CREATE TABLE IF NOT EXISTS providers (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,
    kind TEXT NOT NULL,
    auth_ref TEXT,
    rate REAL NOT NULL DEFAULT 0,
    price_micros INTEGER NOT NULL DEFAULT 0,
    config TEXT NOT NULL DEFAULT '{}',
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

INSERT OR IGNORE INTO providers (name, kind, auth_ref, price_micros, config) VALUES
    ('exa', 'exa', 'EXA_API_KEY', 5000, '{"endpoint":"search","num_results":5}'),
    ('exa-contents', 'exa', 'EXA_API_KEY', 1000, '{"endpoint":"contents"}'),
    ('jina', 'jina', 'JINA_API_KEY', 0, '{}'),
    ('github', 'github', 'GITHUB_TOKEN', 0, '{"resource":"users"}');

-- A batch run over (rows × columns). columns is a JSON array of column names.
CREATE TABLE IF NOT EXISTS runs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    uuid TEXT NOT NULL UNIQUE,
    workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    status TEXT NOT NULL DEFAULT 'running' CHECK(status IN ('running', 'completed', 'budget_exhausted', 'interrupted')),
    columns TEXT NOT NULL,
    budget_micros INTEGER NOT NULL DEFAULT 0,
    spent_micros INTEGER NOT NULL DEFAULT 0,
    stats TEXT NOT NULL DEFAULT '{}',
    started_at TEXT NOT NULL DEFAULT (datetime('now')),
    finished_at TEXT
);

-- One row × one column of a run: the checkpoint unit for --resume.
CREATE TABLE IF NOT EXISTS run_items (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    row_id INTEGER NOT NULL REFERENCES workspace_rows(id) ON DELETE CASCADE,
    column_id INTEGER NOT NULL REFERENCES workspace_columns(id) ON DELETE CASCADE,
    status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending', 'ok', 'empty', 'failed', 'skipped')),
    attempts INTEGER NOT NULL DEFAULT 0,
    calls INTEGER NOT NULL DEFAULT 0,
    value TEXT,
    provider TEXT,
    cache_hit INTEGER NOT NULL DEFAULT 0,
    error TEXT,
    ms INTEGER NOT NULL DEFAULT 0,
    cost_micros INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (run_id, row_id, column_id)
);

CREATE INDEX IF NOT EXISTS idx_run_items_status ON run_items(run_id, status);

-- Provider responses by (provider, input hash): re-running is free.
CREATE TABLE IF NOT EXISTS enrichment_cache (
    provider TEXT NOT NULL,
    input_hash TEXT NOT NULL,
    output TEXT NOT NULL,
    cost_micros INTEGER NOT NULL DEFAULT 0,
    fetched_at TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (provider, input_hash)
);

-- down

DROP TABLE IF EXISTS enrichment_cache;
DROP INDEX IF EXISTS idx_run_items_status;
DROP TABLE IF EXISTS run_items;
DROP TABLE IF EXISTS runs;
DROP TABLE IF EXISTS providers;
DROP INDEX IF EXISTS idx_workspace_columns_name;
DROP TABLE IF EXISTS workspace_columns;
DROP INDEX IF EXISTS idx_workspace_rows_key;
DROP TABLE IF EXISTS workspace_rows;
DROP TABLE IF EXISTS workspaces;
