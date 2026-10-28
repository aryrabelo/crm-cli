-- up

-- Data plane (fork, slice S2): ai columns. Run items record the LLM tokens
-- their calls used (cache hits use none), and an OpenAI-compatible llm
-- provider is seeded; its model and token prices come from
-- `crm provider set llm` or CRM_LLM_MODEL / CRM_LLM_PRICE_IN / CRM_LLM_PRICE_OUT.
ALTER TABLE run_items ADD COLUMN tokens_in INTEGER NOT NULL DEFAULT 0;
ALTER TABLE run_items ADD COLUMN tokens_out INTEGER NOT NULL DEFAULT 0;

INSERT OR IGNORE INTO providers (name, kind, auth_ref, price_micros, config) VALUES
    ('llm', 'llm', 'CRM_LLM_API_KEY', 0, '{}');

-- down

DELETE FROM providers WHERE name = 'llm' AND kind = 'llm';
ALTER TABLE run_items DROP COLUMN tokens_out;
ALTER TABLE run_items DROP COLUMN tokens_in;
