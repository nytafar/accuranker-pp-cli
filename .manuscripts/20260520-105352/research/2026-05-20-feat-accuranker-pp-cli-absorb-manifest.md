# AccuRanker Absorb Manifest

## Absorbed (every Read + Write endpoint exposed as a typed CLI command)

| # | Feature | Best Source | Our Implementation | Added Value |
|---|---------|-------------|--------------------|-------------|
| 1 | List accounts | AccuRanker UI > Profile | `accuranker accounts list` | --json, --select, --fields, structured exit codes |
| 2 | Get account by id | AccuRanker UI | `accuranker accounts get <id>` | same |
| 3 | List domains | AccuRanker UI > Dashboard | `accuranker domains list` | --fields, --period, --filter, --saved-filter |
| 4 | Get domain w/ history | AccuRanker UI > Domain | `accuranker domains get <id>` | period auto-chunks beyond 100-day cap |
| 5 | Filter domains (POST DSL) | AccuRanker UI > Filters | `accuranker domains list --filter "share_of_voice gt 1000"` | full dynamic-filter DSL on CLI |
| 6 | List keywords for domain | AccuRanker UI > Keywords | `accuranker keywords list --domain <id>` | --fields, --period, --filter |
| 7 | Get keyword by id | AccuRanker UI > Keyword detail | `accuranker keywords get <id> --domain <id>` | same |
| 8 | Filter keywords (POST DSL) | AccuRanker UI > Filters | `accuranker keywords list --domain <id> --filter "rank between 1,10"` | full DSL |
| 9 | Raw SERP HTML for rank-date | AccuRanker UI > Keyword > SERP | `accuranker serps get --domain <id> --keyword <id> --date <YYYY-MM-DD>` | scriptable HTML capture |
| 10 | List landing pages | AccuRanker UI > Landing Pages | `accuranker landing-pages list --domain <id>` | --period, --filter |
| 11 | Get landing page | AccuRanker UI | `accuranker landing-pages get <id> --domain <id>` | same |
| 12 | Filter landing pages | AccuRanker UI > Filters | `accuranker landing-pages list --domain <id> --filter "..."` | DSL |
| 13 | List tags | AccuRanker UI > Tags | `accuranker tags list --domain <id>` | --period, --filter |
| 14 | List brands (LLM tier) | AccuRanker UI > LLM | `accuranker brands list` | typed paywall exit code (45) |
| 15 | Get brand | AccuRanker UI > LLM | `accuranker brands get <id>` | same |
| 16 | List prompts (LLM tier) | AccuRanker UI > LLM | `accuranker prompts list --brand <id>` | --period, --filter |
| 17 | Get prompt | AccuRanker UI > LLM | `accuranker prompts get <id> --brand <id>` | same |
| 18 | Overview group_domains | AccuRanker UI > Overview | `accuranker overview group-domains` | typed paywall exit code |
| 19 | Overview keywords for domain | AccuRanker UI > Overview | `accuranker overview keywords --domain <id>` | typed paywall exit code |
| 20 | Create group | AccuRanker UI > Settings | `accuranker groups create --name <name>` | --dry-run, --json |
| 21 | Update group | AccuRanker UI | `accuranker groups update <id> --name <name>` | --dry-run, idempotent |
| 22 | Delete group | AccuRanker UI | `accuranker groups delete <id>` | --dry-run, --yes |
| 23 | Create domain | AccuRanker UI | `accuranker domains create --domain ex.com --group <id>` | --dry-run, --json |
| 24 | Update domain | AccuRanker UI | `accuranker domains update <id> --display-name <name>` | --dry-run |
| 25 | Delete domain | AccuRanker UI | `accuranker domains delete <id>` | --dry-run, --yes |
| 26 | Bulk create keywords (async) | AccuRanker UI | `accuranker keywords create --domain <id> --stdin` | NDJSON in, returns job_id, --wait |
| 27 | Bulk update keywords | AccuRanker UI | `accuranker keywords update --stdin` | NDJSON in |
| 28 | Bulk delete keywords | AccuRanker UI | `accuranker keywords delete --ids 1,2,3` | --dry-run |
| 29 | Poll keyword job | AccuRanker UI > Jobs | `accuranker jobs keyword <job_id>` | --wait, --timeout |
| 30 | Create prompt (LLM) | AccuRanker UI > LLM | `accuranker prompts create --brand <id> --prompt "..."` | --dry-run |
| 31 | Update prompt | AccuRanker UI | `accuranker prompts update --stdin` | bulk |
| 32 | Delete prompt | AccuRanker UI | `accuranker prompts delete --ids 1,2` | --dry-run |
| 33 | Create brand | AccuRanker UI > LLM | `accuranker brands create --name <name>` | --dry-run |
| 34 | Update brand | AccuRanker UI | `accuranker brands update <id> --name <name>` | --dry-run |
| 35 | Delete brand | AccuRanker UI | `accuranker brands delete <id>` | --dry-run |
| 36 | Saved API filter passthrough | AccuRanker UI > Integrations > API | `accuranker keywords list --domain <id> --saved-filter 42` | filter_id passthrough |
| 37 | Auth & token mgmt | AccuRanker UI > Profile | `accuranker auth set-token`, `auth status` | local config |
| 38 | Health check | n/a in AccuRanker | `accuranker doctor` | token + plan tier + reachability + RL budget |

**Status:** All 38 absorbed features will ship as live, no stubs. Items #14–#17 and #18–#19 are LLM/Overview tier-gated and will return a typed exit code (45 = plan_tier_required) with a clear message when the caller's plan lacks access. Tier-gated commands still install — they're not stubbed.

## Transcendence (only possible with our approach)

| # | Feature | Command | Score | Buildability | Why Only We Can Do This |
|---|---------|---------|-------|--------------|------------------------|
| 1 | Cursor-aware incremental sync into local SQLite | `accuranker sync --domain <id>` | 9/10 | hand-code | Requires a cursor table, dedup logic across date windows, and v2-aligned local schema — none of which is in the API. The v2 Postgres mirror's pattern lives here first. |
| 2 | NDJSON dump with auto-chunked date window | `accuranker dump keyword-ranks --domain <id> --from <date> --to <date>` | 9/10 | hand-code | Server hard-caps `period_from/to` at 100 days. The CLI walks windows backward, dedups by `(keyword_id, search_date)`, and streams NDJSON ready for `psql COPY`. No single API call can do this. |
| 3 | Machine-readable schema for v2 mirror | `accuranker schema` (with `--resource <name>`, `--format json\|postgres-ddl`) | 8/10 | hand-code | Internal model captures field shapes + filter dimensions + sync hints (cursor field, primary key, parent FK). The API offers no `/schema` endpoint; this is built from the absorbed model. |
| 4 | Filter-dimension catalog and describe | `accuranker filters list`, `accuranker filters describe <name>` | 8/10 | hand-code | The 100+ filter dimensions are documented as a Markdown table in the upstream docs. We surface them as a discoverable CLI subtree so AI agents and humans can build expressions without re-reading docs. |
| 5 | SERP-feature gain/loss delta | `accuranker serp-features delta --domain <id> --from <date> --to <date> --feature ai_overview` | 7/10 | hand-code | Server filters can ask "has feature now," not "gained vs lost between dates." This requires local SQL over two snapshot dates of `page_serp_features` — only available after sync. |
| 6 | Cannibalization detection | `accuranker cannibalize --domain <id>` | 7/10 | hand-code | Joins `extra_ranks` length and `highest_ranking_page` history per keyword. The API exposes the fields; only a local engine can detect the multi-URL anti-pattern. |
| 7 | Decay opportunity finder | `accuranker decay --domain <id> --window 30d` | 7/10 | hand-code | Joins `keyword_ranks` × `search_volume_history` over a window. No server endpoint correlates rank trend with volume trend; this is a SQL window function over the local mirror. |
| 8 | Keyword spec diff before apply | `accuranker keywords diff --domain <id> --spec keywords.csv` | 7/10 | hand-code | Reads a CSV/NDJSON spec, fetches the current keyword list, partitions into adds/removes/updates. The UI offers no dry-run; this is the only safe way to script bulk keyword operations. |
| 9 | Push local SQLite to remote Postgres | `accuranker push --target postgres://...` | 8/10 | hand-code | Reads `schema/model.yaml` for Postgres DDL, ensures target schema exists, then upserts every table from the local SQLite mirror to Postgres. MVP is full-table upsert; v1.1 path (incremental, cursor-aware, conflict resolution) is documented in `docs/postgres-sync.md`. Bridge to the downstream Bun-based MCP service. |

## Schema artifact (canonical source of truth)

A standalone repo artifact, language-agnostic:

- **File:** `schema/model.yaml`
- **Purpose:** Single source of truth for the data model. Both SQLite migrations (runtime store) AND Postgres DDL (`schema --format postgres-ddl`, used by `push`) are derived from this file. Downstream consumers (Bun-based MCP service) read this file directly.
- **Shape:** Per-resource block listing columns (name, type, nullability, primary-key, foreign-key, indexes), FTS5 hints, sync-cursor field, parent FK relationships.
- **Enforcement:** A test compares `model.yaml` against the actual SQLite schema returned by `PRAGMA table_info(*)` and fails if they drift.

**Hand-code commitment:** 9 of 9 transcendence features are `hand-code` (no `spec-emits`). Each is ~80-200 LoC plus `root.go` wiring. Add ~200 LoC for the filter-DSL parser and ~300 LoC for the YAML model loader + DDL emitter + SQLite/Postgres adapters. Total novel-feature scope: roughly 1,500–2,000 LoC of hand-written Go plus ~250 lines of YAML.

**No stubs.** All 9 transcendence commands ship live. Tier-gated absorbed commands (Brands/Prompts/Overview) ship live but emit typed exit code 45 when the caller's plan blocks them.
