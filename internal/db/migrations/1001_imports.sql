-- up

-- Import batches: one row per `crm import` run. Keeps what is needed to
-- reproduce the original file with `crm rows --import <id>`: header order,
-- the column → field mapping, and the CSV dialect details.
-- Custom field values themselves live in the existing `custom_fields` table.
CREATE TABLE IF NOT EXISTS import_batches (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    uuid TEXT NOT NULL UNIQUE,
    entity_type TEXT NOT NULL CHECK(entity_type IN ('person', 'organization')),
    source TEXT NOT NULL,
    columns TEXT NOT NULL,
    key_column TEXT,
    has_bom INTEGER NOT NULL DEFAULT 0,
    line_ending TEXT NOT NULL DEFAULT 'lf' CHECK(line_ending IN ('lf', 'crlf')),
    trailing_newline INTEGER NOT NULL DEFAULT 1,
    row_count INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Import rows: original position of each data row and the entity it landed on.
-- entity_id is polymorphic (entity_type comes from the batch), like taggings.
CREATE TABLE IF NOT EXISTS import_rows (
    batch_id INTEGER NOT NULL REFERENCES import_batches(id) ON DELETE CASCADE,
    position INTEGER NOT NULL,
    entity_id INTEGER NOT NULL,
    PRIMARY KEY (batch_id, position)
);

CREATE INDEX IF NOT EXISTS idx_import_rows_entity ON import_rows(entity_id);
CREATE INDEX IF NOT EXISTS idx_custom_fields_lookup ON custom_fields(entity_type, field_name, field_value);

-- down

DROP INDEX IF EXISTS idx_custom_fields_lookup;
DROP INDEX IF EXISTS idx_import_rows_entity;
DROP TABLE IF EXISTS import_rows;
DROP TABLE IF EXISTS import_batches;
