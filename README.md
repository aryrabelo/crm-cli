# crm — your contacts, in your terminal.

![crm banner](.github/banner-crm-cli.png)

[![CI](https://github.com/jdanielnd/crm-cli/actions/workflows/ci.yml/badge.svg)](https://github.com/jdanielnd/crm-cli/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/jdanielnd/crm-cli)](https://goreportcard.com/report/github.com/jdanielnd/crm-cli)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

A local-first personal CRM that lives in your terminal. Manage contacts, organizations, interactions, deals, and tasks — all from the command line. Ships with a built-in [MCP server](https://modelcontextprotocol.io/) so AI agents like Claude can read and update your CRM directly.

Single static binary. SQLite database. No cloud. No accounts. Your data stays on your machine.

## Features

- **People & Organizations** — contacts with full profiles, linked to companies
- **Interaction Log** — track calls, emails, meetings, notes, and messages
- **Deals & Pipeline** — sales opportunities with stages, values, and a pipeline view
- **Tasks** — follow-ups with due dates, priorities, and completion tracking
- **Tags** — labels on any entity for flexible organization
- **Relationships** — person-to-person links (colleague, mentor, referred-by, …)
- **Full-Text Search** — find anything across all entities instantly
- **Context Briefing** — one command to get everything about a person before a meeting
- **AI-Ready** — built-in MCP server for Claude and other AI agents
- **Enrichment Pipeline** — CSV tables, computed columns and batch runs with provider waterfall, cache, budget and resume
- **AI Columns & Qualify** — per-row LLM answers (typed: text, score, enum, bool) and `crm qualify --explain` decisions with reasons, through any OpenAI-compatible endpoint, with tokens and cost per run
- **Multiple Output Formats** — table, JSON, CSV, TSV — pipe into anything
- **Zero Dependencies** — single binary, pure-Go SQLite, runs anywhere

## Install

```bash
# Homebrew (macOS/Linux)
brew install jdanielnd/tap/crm

# Go
go install github.com/jdanielnd/crm-cli/cmd/crm@latest

# Or download a binary from GitHub Releases
# https://github.com/jdanielnd/crm-cli/releases
```

## Quick Start

```bash
# Add a company
crm org add "Acme Corp" --domain acme.com

# Add a contact and link them to the org
crm person add "Jane Smith" --email jane@example.com --org 1

# Log a call
crm log call 1 --subject "Intro call"

# Get a full briefing before your next meeting
crm context 1

# See your dashboard
crm status
```

## Commands

### People

```bash
crm person add "Jane Smith" --email jane@example.com --phone 555-1234 --org 1
crm person list                          # all contacts
crm person list --tag vip --limit 10     # filtered
crm person show 1                        # full details
crm person edit 1 --title "CTO" --company "Acme Corp"
crm person delete 1                      # soft-delete (recoverable)
```

### Organizations

```bash
crm org add "Acme Corp" --domain acme.com --industry SaaS
crm org list
crm org show 1 --with-people             # show org + its members
crm org edit 1 --domain acme.io
crm org delete 1
```

### Interactions

```bash
crm log call 1 --subject "Discussed roadmap" --content "Agreed on Q2 timeline"
crm log email 1 --subject "Follow-up" --direction outbound
crm log meeting 1 2 --subject "Product demo" --at "2026-03-05 14:00"
crm log note 1 --subject "Quick check-in" --content "Seemed interested"
crm log list --person 1 --limit 5        # recent interactions with person
```

### Deals

```bash
crm deal add "Website Redesign" --value 15000 --person 1 --stage proposal
crm deal list --open                     # exclude won/lost
crm deal list --stage proposal
crm deal show 1
crm deal edit 1 --stage won --closed-at 2026-03-15
crm deal delete 1
crm deal pipeline                        # summary by stage
```

### Tasks

```bash
crm task add "Follow up on proposal" --person 1 --due 2026-03-14 --priority high
crm task list                            # open tasks
crm task list --overdue                  # past due
crm task list --all                      # include completed
crm task show 1
crm task edit 1 --priority medium
crm task done 1                          # mark completed
crm task delete 1
```

### Tags

```bash
crm tag apply person 1 "vip"
crm tag apply deal 1 "q2"
crm tag show person 1                    # tags on a person
crm tag remove person 1 "vip"
crm tag list                             # all tags
crm tag delete "old-tag"                 # remove tag entirely
```

### Relationships

```bash
crm person relate 1 2 --type colleague --notes "Met at ReactConf"
crm person relationships 1               # list all relationships
crm person unrelate 1                    # remove by relationship ID
```

Relationship types: `colleague`, `friend`, `manager`, `mentor`, `referred-by`

### Search

```bash
crm search "jane"                        # searches people, orgs, interactions, deals
crm search "roadmap" --type interaction  # filter by entity type
```

### Context Briefing

```bash
crm context 1                            # full briefing by person ID
crm context "Jane Smith"                 # or by name
```

Returns person profile, organization, recent interactions, open deals, pending tasks, relationships, and tags — everything you need before a meeting.

### Dashboard

```bash
crm status
```

Shows contact/org counts, open deals with total value, task counts, overdue items, pipeline breakdown, and recent activity.

### Import & Rows (CSV with custom fields)

```bash
crm import leads.csv --as person --key email          # upsert by email, no duplicates on re-import
crm import orgs.csv --as org --map "Razão Social=name,Site=domain" --dry-run
cat leads.csv | crm import - --as person
crm rows person -f csv                                 # id + core fields + every custom field
crm rows person --import 1 -f csv > roundtrip.csv      # original header and row order of batch 1
crm rows org --only industry=SaaS -f json
```

Headers like `name`, `email`, `phone`, `company`, `domain` map to core fields automatically (`--map csvcol=field` overrides; `--map csvcol=` keeps a column custom). Every other column becomes a custom field, shown by `person show` / `org show` (`custom_fields` in JSON). Each import is recorded as a batch, so `crm rows --import <id> -f csv` reproduces the file byte for byte — header text, row order, UTF-8 BOM, CRLF/LF and trailing newline included. Normalized on the way back: unnecessary quotes (only cells with a comma, quote or line break are quoted) and blank lines in multi-column files. Duplicate or empty headers, ragged rows and broken quoting fail with exit code `2`.

### Tables, Columns & Runs (enrichment pipeline)

A **table** is a named set of rows imported from CSV; **columns** are declared on it; a **run** fills enrich columns in batch through a provider **waterfall**, with a cache, a hard budget and resume.

```bash
crm table import leads.csv --as leads --key login        # rows keep file order; --key upserts on re-import
crm column add leads profile --type enrich --waterfall github --from login
crm column add leads company --type enrich --waterfall github,exa --from login --field company
crm column add leads label --type formula --expr "{{login}} ({{company}})"
crm run leads --columns profile,company --dry-run         # estimated calls, cost, cache hits; writes nothing
crm run leads --columns profile,company --concurrency 8 --budget 2.00
crm run --resume 3 --budget 5                             # continue run 3 (pending/failed/skipped items only)
crm run list
crm run show 3 --items
crm table show leads --only "company!="                   # filters: = != > >= < <= ~ (regex) !~
crm table export leads -f csv --only "score>=70"          # csv (default), tsv or json
crm column list leads                                     # type, definition, max cost per row, filled rows
crm provider list
crm provider set mine --kind http --url "https://api.example.com/v1/find?q={{input}}" --path data.email \
  --auth-env EXAMPLE_KEY --price 0.002
```

- **Column types:** `input` (imported cells), `formula` (template evaluated on read; `{{col}}`, `{{col.json.path}}`), `enrich` (filled by `crm run`; `--from` is the input column, default the table key; `--field` keeps one value of the answer), `ai` (filled by `crm run` with an LLM answer per row — see below). `crm run --only expr` restricts a run to matching rows.
- **Waterfall:** providers are tried in order; the first non-empty answer wins, errors and empty answers fall through, and the run item records which provider answered.
- **Cache:** every answer is cached by (provider, input hash), and the cache is consulted for the whole waterfall before anything is spent — re-running costs `$0`.
- **Budget:** a call is made only after its price is reserved, so a run never spends more than `--budget`; items that do not fit are marked `skipped`. `--budget` is required whenever a run may spend money (`--dry-run` shows how much).
- **Retry:** 429, 408, 5xx and network timeouts are retried `--retry` times (default 2) with exponential backoff from `--backoff` (default 500ms), honoring `Retry-After`. Failed calls cost nothing.
- **Checkpoint/resume:** each row × column item is saved as it finishes; Ctrl-C leaves the run `interrupted`, and `crm run --resume <id>` processes only items that are not done.
- **One executor per run:** a run is leased by the process executing it (owner + heartbeat, renewed every 10 s and on every checkpoint). A second `crm run --resume`, or `crm_run_start` on a run another process is executing, is refused with exit `4` naming the holder, so the budget and the stats are never spent or counted twice. If the holder died, its run can be resumed once its heartbeat has been silent for 30 s.
- **Providers:** `exa` (search, `EXA_API_KEY`, $0.005/call), `exa-contents` ($0.001/page), `jina` (Reader, optional `JINA_API_KEY`, free), `github` (users/orgs API, optional `GITHUB_TOKEN`, free), plus any JSON API as a generic `http` provider. Prices and rates are provider config (`crm provider set`); credentials are read from the environment, never stored. `CRM_PROVIDER_<NAME>_URL` overrides a provider's base URL.

### AI Columns & Qualify (per-row LLM decisions)

An **ai column** asks an LLM one question per row: the prompt is a template whose `{{column}}` placeholders are filled with the row's cells, and the answer is typed. **`crm qualify`** builds that column for you from an ICP criteria file: a `yes`/`no`/`maybe` decision per row and, with `--explain`, a 1–2 sentence reason citing the row's fields. Both run through `crm run`, so cache, budget, retry, resume and `--dry-run` work the same way.

```bash
crm provider set llm --model gpt-4o-mini --price-in 0.15 --price-out 0.60   # USD per 1M tokens
export CRM_LLM_API_KEY=sk-...                                               # never stored
crm column add leads fit --type ai --prompt files/fit.md --output score:0-100 --explain
crm run leads --columns fit --budget 0.50
crm table export leads --only "fit>=70"

crm qualify leads --criteria files/icp.md --explain --dry-run   # estimated calls and worst-case cost; writes nothing
crm qualify leads --criteria files/icp.md --explain --budget 0.50
# stderr, 5 rows on a subscription gateway (price 0):
# qualify leads.qualified: yes 2 · no 2 · maybe 1 · failed 0 · skipped 0 — tokens in 1991 / out 444 · spent $0.00 of $0.50 budget
crm table export leads -f csv --only "qualified=yes"            # company,…,qualified,qualified_reason
crm run show 4                                                  # tokens_in / tokens_out per run
```

- **LLM provider:** the built-in `llm` provider speaks OpenAI-compatible chat completions (`POST {base}/chat/completions`). Base URL from `--base-url`, else `CRM_LLM_BASE_URL`, else `https://api.openai.com/v1`; key from `CRM_LLM_API_KEY`; model from `--model` or `CRM_LLM_MODEL`; prices from `--price-in`/`--price-out` or `CRM_LLM_PRICE_IN`/`CRM_LLM_PRICE_OUT` (required — `0` for a local model or a subscription gateway); `--max-tokens` per call (default 512). More models/gateways: `crm provider set <name> --kind llm …`, then `--waterfall a,b` on the column (the next model is tried when one fails).
- **Prompt:** `--prompt` is a file (or inline text). Every `{{placeholder}}` must name a column — unknown ones fail at `column add`. A row whose placeholders are all empty is not sent.
- **Output:** `text`, `score:MIN-MAX` (stored as a number), `enum:a|b|c` (stored as the canonical value), `bool`. The model must answer a JSON object; an invalid answer gets one repair request, then the row fails (resumable).
- **Cost:** each call is charged the tokens the provider reports × the price. The budget reserves the worst case first — prompt bytes (a token is at least one byte) plus max tokens, twice to cover the repair — so a run never spends more than `--budget`. Cached answers cost `$0` and no tokens.
- **qualify:** `--name` (default `qualified`), `--explain` (adds `<name>_reason`), `--only` (row filter), `--budget`, `--dry-run`, `--waterfall`, `--concurrency`. The prompt is the criteria plus every other column; re-running with the same criteria and rows is all cache hits.

### Push (table rows → people / organizations, deduplicated)

```bash
crm push leads --to person --dry-run                   # plan: create / update / merge per row; writes nothing
crm push leads --to person --only "qualified=yes"      # dedupe by email (default for people)
crm push leads --to org                                # dedupe by domain (default for orgs; URL and email work too)
crm push companies --to org --dedupe fuzz --threshold 0.9   # similar names: "ACME, Inc." = "Acmé Ltda" = "acme"
```

Columns map to fields as in `crm import` (aliases such as `name`, `email`, `company`, `website`; other columns become custom fields; `--map col=field` overrides). Each row is matched against existing entities and against the rows pushed before it, so duplicates inside the table merge too. A match keeps its values: the row only fills empty fields and custom fields it does not have yet. Rows without a name or dedupe key are reported per row and skipped. Exit code `2` when no column maps to the dedupe field.

## Output Formats

Every command supports `--format` / `-f`:

```bash
crm person list                          # table (default in TTY)
crm person list -f json                  # JSON (default when piped)
crm person list -f csv                   # CSV
crm person list -f tsv                   # TSV
```

Use `-q` / `--quiet` for minimal output (just IDs), useful for scripting:

```bash
crm person list --tag vip -q | xargs -I{} crm tag apply person {} "priority"
```

## Piping & Composition

`crm` follows Unix conventions — data to stdout, messages to stderr, structured exit codes — so it plays well with other tools:

```bash
# Bulk tag everyone at an org
crm person list -f json | jq '.[] | select(.org_id == 1) | .id' | xargs -I{} crm tag apply person {} "acme-team"

# Interactive selection with fzf
crm person list -f tsv | fzf | cut -f1 | xargs crm person show

# Export contacts to CSV
crm person list -f csv > contacts.csv

# Overdue task notifications
crm task list --overdue -f json | jq -r '.[] | "OVERDUE: \(.title)"'
```

Exit codes: `0` success, `1` error, `2` usage, `3` not found, `4` conflict, `10` database error.

## AI Integration (MCP Server)

`crm` includes a built-in [MCP](https://modelcontextprotocol.io/) server so AI agents can query and update your CRM over a structured protocol: over stdio (what desktop clients spawn) or streamable HTTP.

### Claude Code

```bash
claude mcp add crm -- crm mcp serve
```

Then copy [`AGENT.md`](AGENT.md) into your project as `CLAUDE.md` so Claude knows how to use the CRM effectively.

### Claude Desktop (macOS)

1. Open the config file at `~/Library/Application Support/Claude/claude_desktop_config.json`

2. Add the CRM server (if the file already has other servers, add the `"crm"` entry inside the existing `mcpServers` object):

```json
{
  "mcpServers": {
    "crm": {
      "command": "/opt/homebrew/bin/crm",
      "args": ["mcp", "serve"]
    }
  }
}
```

> **Note:** Use the full path to the binary. Homebrew on Apple Silicon installs to `/opt/homebrew/bin/crm`, on Intel Macs to `/usr/local/bin/crm`. Run `which crm` to find yours.

3. Restart Claude Desktop

4. Create a new **Project**, click the project settings, and paste the contents of [`AGENT.md`](AGENT.md) into the project instructions. This teaches Claude when to log interactions, how to maintain contact summaries, tagging conventions, and more.

### Streamable HTTP

```bash
crm mcp serve --http 127.0.0.1:8765                         # http://127.0.0.1:8765/mcp; prints a generated token on stderr
CRM_MCP_TOKEN=s3cret crm mcp serve --http 127.0.0.1:8765    # every request needs "Authorization: Bearer s3cret"
claude mcp add --transport http crm http://127.0.0.1:8765/mcp --header "Authorization: Bearer s3cret"
```

The HTTP transport always requires a bearer token:

- `--token` (or `CRM_MCP_TOKEN`) sets it. Without one, a loopback address (`127.0.0.1`, `::1`, `localhost`) gets a random token generated at startup and printed on stderr, the way Jupyter does; any other address refuses to start. A request without the right token gets `401`.
- On a loopback address, only a `Host` of `127.0.0.1`, `localhost` or `[::1]` with the bound port is accepted, so a web page that rebinds its own name to 127.0.0.1 (DNS rebinding) gets `403`. On every address a request with a foreign `Origin` header gets `403` (no `Origin`, `Origin: null` and same-origin requests pass).
- Bodies are capped at 16 MiB (`413`); slow clients are cut off by read (60 s) and idle (120 s) timeouts.
- The transport is plain HTTP: put a TLS reverse proxy in front before listening beyond this machine.

Ctrl-C shuts the server down gracefully; runs started with `wait: false` are left `interrupted` and resumable.

### Available Tools

**Batch tools** (data plane). They accept arrays where an agent would otherwise loop, and report errors per item instead of failing the call — one call does what the one-entity tools need N calls for. They call the same service as the CLI commands, so both behave identically.

| Tool | Description | CLI twin |
|------|-------------|----------|
| `crm_table_import` | Create/append a table from `csv` text or a `rows` array of objects; upsert by `key` | `crm table import` |
| `crm_rows_upsert_many` | Upsert many rows into an existing table (each row sets only the cells it names) | — |
| `crm_table_query` | `filters` (AND), `columns`, `limit` (default 100) / `offset`; returns `total` and `_row` per row | `crm table show` |
| `crm_column_add` | Declare one or more columns (`input`, `formula`, `enrich`, `ai`) | `crm column add` |
| `crm_run_start` | Fill enrich/ai columns in one budget-capped run; `dry_run` estimates; `wait: false` returns the run id at once | `crm run` |
| `crm_run_status` | Status, counts, calls, tokens, spend of several runs; `items: true` adds every item | `crm run show` |
| `crm_qualify` | yes/no/maybe (+ reason) per row from markdown criteria | `crm qualify` |
| `crm_export` | Filtered rows as `csv`, `tsv` or `json` text | `crm table export` |
| `crm_push_dedupe` | Table rows → people/orgs, dedupe by email, domain or fuzzy name; `dry_run` | `crm push` |

Budgets are a number or a string of dollars (`0.5`, `"0.50"`). Call-level errors (unknown table, invalid filter, missing budget) come back as tool errors with the message; database and network details stay out of results. Item-level errors (`crm_run_status` with `items`, `crm run show --items`, `run_items.error`) are redacted before they are stored and again when they are returned: URLs lose their query string, credentials and fragment, the request's key/token values and credential-looking values (`Bearer …`, `api_key=…`, `"token": …`) are masked, and each error is capped at 300 bytes. Message sequences (`crm_sequence_render`) belong to slice S4 and are not part of this server yet.

**One-entity tools** (upstream, unchanged):

| Tool | Description |
|------|-------------|
| `crm_person_search` | Search people by name, email, tag, org |
| `crm_person_get` | Get full details for a person |
| `crm_person_create` | Create a new person |
| `crm_person_update` | Update person fields |
| `crm_person_delete` | Delete (archive) a person |
| `crm_person_relate` | Create relationship between people |
| `crm_org_search` | Search organizations |
| `crm_org_get` | Get org details with members |
| `crm_interaction_log` | Log a call, email, meeting, or note |
| `crm_interaction_list` | List interactions for a person |
| `crm_search` | Cross-entity full-text search |
| `crm_context` | Full person briefing |
| `crm_deal_create` | Create a deal |
| `crm_deal_update` | Update deal stage/fields |
| `crm_task_create` | Create a task |
| `crm_task_list` | List open tasks |
| `crm_tag_apply` | Apply a tag |
| `crm_stats` | CRM summary stats |

### Bench: what a task costs an agent

The number of tool calls is what an agent pays for (each call is a model round trip). `crm bench run` measures it on 10 reproducible tasks in [`bench/`](bench/) — JSON task files plus CSV/markdown fixtures — each executed by an MCP client against a crm server on a fresh in-memory database, with local fake providers (an enrichment API at $0.002/call and an OpenAI-compatible LLM at $0.15 / $0.60 per 1M tokens), so a run is deterministic and free.

```bash
crm bench run                                  # per task: tool calls, provider calls, tokens, cost, wall time, KB, check
crm bench run --baseline per-entity            # same tasks with the one-entity tools only
crm bench run --transport http -f json         # through streamable HTTP + bearer auth instead of in-process
```

Result of `crm bench run --baseline per-entity` (call counts, tokens and cost are deterministic; wall time is a few ms per task against the local fakes):

| Task | Batch calls | Per-entity calls | Gap | Provider calls | Tokens | Cost | Batch check | Per-entity check |
|------|------------:|-----------------:|----:|---------------:|-------:|-----:|:-----------:|:----------------:|
| import-135-csv | 1 | 135 | 135× | 0 | 0 | $0.00 | pass | pass |
| update-20-titles | 1 | 40 | 40× | 0 | 0 | $0.00 | pass | pass |
| enrich-industry (135 rows, 27 domains) | 2 | n/a | | 27 | 0 | $0.054 | pass | n/a |
| enrich-budget-cap ($0.01) | 2 | n/a | | 5 | 0 | $0.01 | pass | n/a |
| qualify-20 | 1 | n/a | | 20 | 5,175 | $0.001057 | pass | n/a |
| ai-score-and-filter | 3 | n/a | | 20 | 1,950 | $0.000576 | pass | n/a |
| find-dedupe-people (40 rows → 30) | 1 | 40 | 40× | 0 | 0 | $0.00 | pass | **FAIL** (35 people) |
| fuzzy-dedupe-orgs (12 rows → 5) | 1 | n/a | | 0 | 0 | $0.00 | pass | n/a |
| export-filtered | 1 | 1 | 1× | 0 | 0 | $0.00 | pass | pass |
| pipeline: import → qualify → push → export | 4 | n/a | | 20 | 4,500 | $0.000957 | pass | n/a |
| **Total** | **17** (4 on shared tasks) | **216** | **54×** | 92 | 11,625 | $0.06659 | 10/10 | 3/4 |

n/a: the one-entity tools have no tables, columns, providers or organization create. The per-entity dedupe relies on `crm_person_create`'s exact-match duplicate check, so case variants of an email slip through; a filtered read (`export-filtered`) is one call either way, but the batch answer is 0.4 KB of CSV instead of 2 KB of full person records.

A task file has a `setup` (a table for batch mode, entities for per-entity mode, written directly and not counted), a `batch` plan and either a `per_entity` plan or `per_entity_na` with the reason. A plan is `steps` (tool calls; `foreach` repeats calls per CSV row; arguments are templates: `{{file:path}}`, `{{rows:file.csv}}`, `{{row.col}}`, `{{last.0.id}}`) and `checks` (`sql` or a `result` path, compared with `equals`, `gte`, `lte` or `len`). Per-entity plans may only call the upstream tools. `crm bench run` exits `1` when a batch task fails its check; a baseline that gets a task wrong is reported, not fatal.

### Example AI Workflow

After a meeting, tell Claude:

> "Just had a great meeting with Jane. We agreed on the timeline, she'll sign next week."

Claude can:
1. Log the interaction with `crm_interaction_log`
2. Update the deal stage with `crm_deal_update`
3. Create a follow-up task with `crm_task_create`
4. Update Jane's summary with `crm_person_update`

The `summary` field on contacts acts as a living dossier that AI agents maintain — reading it before meetings for context, updating it after interactions with new facts and preferences.

## Configuration

The database lives at `~/.crm/crm.db` by default. Override with:

```bash
# Flag (highest priority)
crm --db /path/to/crm.db person list

# Environment variable
export CRM_DB=/path/to/crm.db
```

The database and directory are auto-created on first use.

### iCloud Sync (macOS)

Point your database to iCloud Drive for automatic backup across machines:

```bash
export CRM_DB="$HOME/Library/Mobile Documents/com~apple~CloudDocs/crm/crm.db"
```

SQLite WAL mode handles concurrent reads well, but avoid writing from two machines simultaneously.

## Building from Source

```bash
git clone https://github.com/jdanielnd/crm-cli.git
cd crm-cli
go build -o crm ./cmd/crm
go test ./...
```

Requires Go 1.23+. No CGO — builds anywhere Go runs.

## Tech Stack

| Component | Choice |
|-----------|--------|
| Language | Go |
| Database | SQLite via [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) (pure Go, no CGO) |
| CLI | [Cobra](https://github.com/spf13/cobra) |
| MCP | [mcp-go](https://github.com/mark3labs/mcp-go) |
| Search | SQLite FTS5 |

## License

MIT
