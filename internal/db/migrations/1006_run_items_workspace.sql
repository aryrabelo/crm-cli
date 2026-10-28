-- up

-- Data plane (fork): a run item's row and column must belong to the
-- workspace of its run. The engine only creates consistent items; this
-- guards direct SQL and future bugs, where a mismatched item would crash
-- the worker that processes it.
CREATE TRIGGER IF NOT EXISTS run_items_same_workspace
BEFORE INSERT ON run_items
WHEN NOT EXISTS (
    SELECT 1 FROM runs r
    JOIN workspace_rows wr ON wr.workspace_id = r.workspace_id AND wr.id = NEW.row_id
    JOIN workspace_columns wc ON wc.workspace_id = r.workspace_id AND wc.id = NEW.column_id
    WHERE r.id = NEW.run_id
)
BEGIN
    SELECT RAISE(ABORT, 'run item: row and column must belong to the workspace of the run');
END;

-- down

DROP TRIGGER IF EXISTS run_items_same_workspace;
