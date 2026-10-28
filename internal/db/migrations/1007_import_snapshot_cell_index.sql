-- up

-- Data plane (fork): an import row keeps the cells of the CSV record it came
-- from (JSON array, one string per batch column), so crm rows --import <id>
-- reproduces the file even after a later import or edit changed the records,
-- or archived them. NULL for batches imported before this migration, which
-- are still read from the current records.
ALTER TABLE import_rows ADD COLUMN cells TEXT;

-- CellStates (the UI's per-cell run status, polled while a run is active)
-- takes the newest item of every row × column: this index serves that
-- lookup without scanning the items of every historical run.
CREATE INDEX IF NOT EXISTS idx_run_items_cell ON run_items(row_id, column_id, id);

-- down

DROP INDEX IF EXISTS idx_run_items_cell;
ALTER TABLE import_rows DROP COLUMN cells;
