# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `crm import <file.csv|-> --as person|org [--map csvcol=field] [--key field] [--dry-run]`: CSV import where known columns map to core fields and every other column becomes a custom field; `--key` upserts instead of duplicating
- `crm rows <person|org> [--import <batch-id>] [--only column=value]`: row export with core + custom fields; with `--import`, byte-identical CSV round trip of the original file
- Custom fields in `person show` / `org show` (table, CSV/TSV columns, `custom_fields` in JSON)
- Migration `1001_imports.sql`: `import_batches` and `import_rows` (provenance for round-trip export)
- `NOTICE` with upstream MIT attribution
- `crm table import <file.csv|-> --as <name> [--key col]`, `crm table list|show|export <name> [-f csv|tsv|json] [--only expr]`: workspace tables of rows kept in import order; filters `col>=70`, `col=value`, `col~regex`, `col!=` (repeatable, AND)
- `crm column add <table> <name> --type input|formula|enrich [--waterfall p1,p2] [--from col] [--field path] [--expr template]`, `crm column list|rm`
- `crm run <table> --columns a,b [--concurrency N] [--budget USD] [--dry-run] [--retry N] [--backoff D] [--resume RUN_ID]`, `crm run list|show <id> [--items]`: batch enrichment with provider waterfall (first non-empty answer wins), cache by (provider, input hash) consulted before spending, hard budget (reserve before calling), exponential backoff on 429/5xx/timeouts, per-item checkpoint and resume
- Provider adapters: Exa (search/contents), Jina Reader, GitHub (users/orgs) and a generic `http` adapter (URL/body template + JSON path); `crm provider list|set` for prices, rates, credentials (env var names) and endpoints; `CRM_PROVIDER_<NAME>_URL` base URL override
- Migration `1002_data_plane.sql`: `workspaces`, `workspace_rows`, `workspace_columns`, `runs`, `run_items`, `enrichment_cache`, `providers` (seeded with exa, exa-contents, jina, github)
- `ai` column type: `crm column add <table> <name> --type ai --prompt <file.md|text> --output text|score:MIN-MAX|enum:a|b|c|bool [--explain] [--waterfall llm,...]`; prompt `{{column}}` placeholders are checked at `column add` (unknown ones are a usage error); answers are stored typed (numbers, booleans, canonical enum values) so `--only "fit>=70"` works; `--explain` stores a 1–2 sentence reason in `<name>_reason`
- `crm qualify <table> --criteria <file.md> [--explain] [--name qualified] [--budget USD] [--dry-run] [--only expr] [--waterfall p1,p2] [--concurrency N]`: yes/no/maybe decision column (plus reason column with `--explain`) computed through `crm run`; prints decision counts, tokens in/out and cost vs budget
- `llm` provider kind: OpenAI-compatible chat completions with JSON answers (one repair request on an invalid answer, then the item fails); configured by `crm provider set <name> --model --price-in --price-out --max-tokens --base-url` or `CRM_LLM_BASE_URL`, `CRM_LLM_API_KEY`, `CRM_LLM_MODEL`, `CRM_LLM_PRICE_IN`, `CRM_LLM_PRICE_OUT`; cost = reported tokens × price per 1M tokens; the budget reserves the worst case (prompt bytes + max tokens, repair included)
- `crm run --only expr` restricts a new run to matching rows; `crm run`, `run show` and `run show --items` report `tokens_in` / `tokens_out`
- Migration `1003_ai_columns.sql`: `run_items.tokens_in` / `tokens_out`, seeded `llm` provider (auth `CRM_LLM_API_KEY`)
- Batch MCP tools next to the upstream one-entity tools: `crm_table_import` (CSV text or a rows array), `crm_rows_upsert_many`, `crm_table_query` (filters, columns, limit/offset, total), `crm_column_add` (several columns), `crm_run_start` (`dry_run`, budget, `wait: false` to return the run id at once), `crm_run_status` (several runs, optional items), `crm_qualify`, `crm_export` (csv/tsv/json text), `crm_push_dedupe`; arrays in, JSON out, errors reported per item
- `crm mcp serve --http HOST:PORT [--token T]`: streamable HTTP transport at `/mcp` behind a mandatory bearer token (`--token`, `CRM_MCP_TOKEN`, or generated at startup and printed on stderr for loopback addresses; non-loopback addresses need an explicit token); graceful shutdown on Ctrl-C
- `crm push <table> --to person|org [--dedupe email|domain|fuzz] [--threshold 0.88] [--only expr] [--map col=field] [--dry-run]`: table rows become people or organizations, deduplicated against existing entities and earlier rows by normalized email, domain (from a domain, URL or email) or fuzzy name (accents, punctuation and Inc/LLC/Ltda… ignored); a match only fills empty fields
- `crm bench run [--tasks bench] [--baseline per-entity] [--transport inprocess|http] [-f table|json]`: runs the 10 task files in `bench/` through an MCP client against fake providers and prints tool calls, provider calls, tokens, cost, wall time, payload size and check per task, plus totals; `--baseline per-entity` compares the call count with the one-entity tools (54× fewer calls on the 4 tasks both can do)
- `crm cache clear [--provider P] [--older-than D]`: delete cached provider answers; `crm provider set --cache-ttl D` (default `30d`, `0` never expires) and, for `http` providers, `--error-path` / `--ok-path`
- `crm run` / `crm qualify --retry-uncertain` and MCP `retry_uncertain`: opt in to retrying paid POST calls that timed out after the request was sent; runs report `uncertain_spent` / items `uncertain` (migration `1005_uncertain_cost.sql`)

### Changed

- CLI data-plane commands (`table import|show|export`, `column add|list`, `run`, `run show`, `qualify`) and the MCP batch tools share one `internal/service` layer
- `crm mcp serve` without `--http` keeps the stdio transport; it stops when stdin closes or on Ctrl-C

### Fixed

- Migrations are versioned by the number in their file name and tracked in `schema_migrations`, not by their position in the listing: the fork's migrations moved to `1001_`–`1003_` (upstream keeps 1–999), so a new upstream `002_*.sql` applies cleanly instead of making every existing fork database fail to open. Databases migrated by earlier builds are adopted on first open (fork migrations detected by their schema)
- Security: the MCP HTTP transport on loopback ran without auth and without Origin/Host checks, so a web page using DNS rebinding could start paid runs and export every lead. It now always requires a bearer token, rejects any Host other than `127.0.0.1`/`localhost`/`[::1]` with the bound port on loopback binds, rejects foreign `Origin` headers (`403`), caps bodies at 16 MiB and sets read/idle timeouts. The guard lives in `internal/httpguard` for reuse by other local HTTP servers
- Two executors could run the same run at once (MCP `wait: false` plus `crm run --resume`), each with its own budget — up to twice the spend and double-counted stats — and a crashed run stayed `running` for good. A run is now claimed atomically (owner + heartbeat, migration `1004_run_lease.sql`); the heartbeat is renewed by the executor and by every checkpoint, a concurrent executor is refused with exit `4`, an executor that lost its lease stops without writing, and a run whose holder has been silent for 30 s can be taken over
- `crm run --resume` after `crm column rm` + `crm column add` with the same name crashed with a nil-pointer panic (and `--dry-run` silently estimated 0 items). Resume now refuses such a run with exit `2`, naming the removed column(s) and the command for a new run; open items of removed columns are never loaded
- Security: item-level errors were stored and returned verbatim, so an `http` provider with `?key=…` in its URL leaked the key through `run_items.error`, `crm run show --items` and `crm_run_status` (Go's transport errors embed the full URL; error bodies may echo the key). Provider errors now drop URL query strings, userinfo and fragments, mask the request's credential values and credential-looking values, and are capped at 300 bytes, before they are stored and again on output
- The enrichment cache never expired and could not be cleared, and an `http` API answering HTTP 200 with an error body had it cached as the answer (or as "no data") for good. Cached answers now expire after the provider's TTL (`fetched_at` is read), `--error-path`/`--ok-path` turn such bodies into charged, uncached item failures, and `crm cache clear` deletes entries
- A call that timed out or lost its connection after the request was sent was recorded at `$0` and retried, so possibly-billed calls (up to 3 per item) were missing from the spend. Such a call is now counted at its reserved estimate and recorded as uncertain, and a paid non-idempotent call (POST: every llm call, `http` with a body) in that state is not retried by default. The `--budget` help text no longer promises the provider's bill: it caps the spend recorded at the configured prices

## [0.1.6] - 2026-03-08

### Added

- Project website at [crmcli.sh](https://www.crmcli.sh) with install guide, FAQ, and AI use case examples
- `AGENT.md` with CRM agent instructions for Claude users
- Claude Desktop setup instructions in README

### Changed

- Improved code quality across CLI commands, repositories, formatters, and tests
- Improved README with banner, badges, and clearer structure

### Fixed

- Hardened input validation, error sanitization, and integer overflow checks in MCP server
- Restricted `~/.crm/` directory permissions to user-only (0700)
- CSV headers now use title-case to match table output
- Mobile responsiveness fixes for website (FAQ cards, MCP section, footer)

### Security

- MCP server no longer leaks internal error messages to clients
- Added interaction type and direction validation
- Added entity existence checks before applying tags

## [0.1.5] - 2026-03-07

### Added

- Duplicate person detection on create (by email)
- `crm_person_delete` MCP tool

## [0.1.4] - 2026-03-07

### Fixed

- Code review findings across codebase

## [0.1.3] - 2026-03-07

### Added

- `crm_person_relate` MCP tool for creating relationships between people via AI agents

## [0.1.2] - 2026-03-07

### Changed

- Switched Homebrew distribution from cask to formula
- Rewrote README to document actual features

## [0.1.1] - 2026-03-07

### Added

- Person CRUD commands (`add`, `list`, `show`, `edit`, `delete`)
- Organization CRUD commands
- Interaction logging (`call`, `email`, `meeting`, `note`, `message`)
- Polymorphic tagging system
- Person-to-person relationships (`colleague`, `friend`, `manager`, `mentor`, `referred-by`)
- Deals and pipeline management with stages
- Tasks and follow-ups with priorities and due dates
- Cross-entity full-text search (FTS5)
- Context briefing command for pre-meeting prep
- Dashboard summary (`crm status`)
- MCP server with 17 tools for AI agent integration
- Output formats: table, JSON, CSV, TSV
- Quiet mode (`-q`) for scripting
- GitHub Actions CI/CD
- GoReleaser for cross-platform builds
- Homebrew tap

[0.1.6]: https://github.com/jdanielnd/crm-cli/releases/tag/v0.1.6
[0.1.5]: https://github.com/jdanielnd/crm-cli/releases/tag/v0.1.5
[0.1.4]: https://github.com/jdanielnd/crm-cli/releases/tag/v0.1.4
[0.1.3]: https://github.com/jdanielnd/crm-cli/releases/tag/v0.1.3
[0.1.2]: https://github.com/jdanielnd/crm-cli/releases/tag/v0.1.2
[0.1.1]: https://github.com/jdanielnd/crm-cli/releases/tag/v0.1.1
