-- up

-- Data plane (fork): the part of an item's cost that is not confirmed. A call
-- that timed out (or lost its connection) after the request was sent may have
-- been billed, so it is charged at its reserved estimate and that amount is
-- recorded here as well as in cost_micros.
ALTER TABLE run_items ADD COLUMN uncertain_micros INTEGER NOT NULL DEFAULT 0;

-- down

ALTER TABLE run_items DROP COLUMN uncertain_micros;
