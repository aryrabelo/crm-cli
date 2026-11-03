-- up

-- Task claims (fork): one row per task an agent has claimed. A task with no
-- row is free. Upstream's tasks table is untouched, so the Homebrew binary
-- keeps reading and writing a migrated database.
CREATE TABLE IF NOT EXISTS task_claims (
    task_id         INTEGER PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    state           TEXT NOT NULL CHECK (state IN ('free', 'held', 'waiting')),
    holder          TEXT,
    previous_holder TEXT,
    updated_at      INTEGER NOT NULL,
    note            TEXT,
    link            TEXT,
    CHECK ((state = 'held') = (holder IS NOT NULL)),
    CHECK ((state = 'waiting') = (link IS NOT NULL))
);

-- A locator holds at most one task.
CREATE UNIQUE INDEX IF NOT EXISTS idx_task_claims_holder_held
    ON task_claims(holder) WHERE state = 'held';

-- Completing or archiving a task, by any binary, drops its claim.
CREATE TRIGGER IF NOT EXISTS task_claims_clear_on_close
AFTER UPDATE OF completed, archived ON tasks
WHEN NEW.completed = 1 OR NEW.archived = 1
BEGIN
    DELETE FROM task_claims WHERE task_id = NEW.id;
END;

-- down

DROP TRIGGER IF EXISTS task_claims_clear_on_close;
DROP INDEX IF EXISTS idx_task_claims_holder_held;
DROP TABLE IF EXISTS task_claims;
