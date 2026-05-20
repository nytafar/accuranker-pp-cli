# Novel Features Brainstorm — AccuRanker

## Customer model

### Persona 1 — Mira, Senior SEO Analyst at a mid-size agency (~25 clients, ~8k tracked keywords)

**Today (without this CLI):** Mira keeps the AccuRanker UI open in two tabs and a Looker Studio tab in a third. Every Monday she pulls a "what moved" report per client by exporting CSVs from AccuRanker's UI one domain at a time, dropping them into a shared Drive folder, and rebuilding a pivot table in Sheets. For the agency's biggest client she has a Python script that hits `/keywords/` with a hard-coded `fields=` string, writes JSON to disk, and a second script that loads it into a Postgres warehouse — but the script breaks every time the date window exceeds 100 days and she has to hand-chunk. She cannot answer "which keywords gained an AI Overview in the last 14 days across all clients" without writing throwaway code each time.

**Weekly ritual:** Monday morning rank-delta review across all client domains; Wednesday SERP-feature audit (featured snippets gained/lost, AI Overview appearances); Friday share-of-voice trend export for the agency-wide client report.

**Frustration:** The 100-day window cap. Every backfill or historical question becomes a loop she has to write by hand. Adjacent frustration: the empty-object response when she forgets `fields=`, which has eaten hours.

### Persona 2 — Devansh, Data Engineer building the agency's analytics warehouse

**Today (without this CLI):** Devansh is the one tasked with building the v2 Postgres mirror. Right now he's running a nightly Airflow DAG that calls AccuRanker through `requests`, juggling retry-after handling and 4-concurrent-connection limits manually. He has a notebook full of `curl` examples taped to his monitor because he can't remember which filter dimensions accept `between` vs `regex`. He has no canonical schema document — he derives the Postgres DDL by inspecting responses and praying nothing new shows up. His cursor table tracks `(domain_id, resource, last_period_to)` but he has no good way to test a single window in isolation before pushing to prod.

**Weekly ritual:** Run the nightly sync, triage the inevitable 429s and partial-window failures, hand-patch the cursor table when a domain backfill drops a chunk. On Fridays, manually reconcile rank counts between AccuRanker UI and the warehouse to catch drift.

**Frustration:** No machine-readable schema. He's writing the same defensive parsing code for every new resource. Worse, he doesn't trust his own sync because there's no canonical dump command he can re-run to compare.

### Persona 3 — Yusra, In-house SEO Manager running an AI agent over rank data

**Today (without this CLI):** Yusra runs Claude inside her terminal and wants to ask "for our top 200 commercial-intent keywords, which ones lost rank but gained search volume in the last month?" — but there is no MCP, no CLI, nothing the agent can call. She copies-pastes CSV exports from the UI into the chat and the context window blows up immediately. When she does write the filter herself, she has to keep the AccuRanker filter-dimension docs open in another tab to remember whether it's `search_volume` or `volume`, and which comparators are legal for which field.

**Weekly ritual:** Briefing prep for her CMO — pulling 4-5 ad-hoc cross-cutting questions per week ("show me decay candidates", "rank-1 keywords with falling CTR", "AI-Overview-eligible queries we don't own").

**Frustration:** The filter DSL has 100+ dimensions and she remembers maybe 12 of them. The agent can't discover the rest. Token blowup from full JSON responses kills her sessions before she gets to the answer.

### Persona 4 — Tomás, Freelance SEO Consultant onboarding new clients

**Today (without this CLI):** Tomás runs the same setup playbook every time he takes a new client: create a group in AccuRanker, create the domain, bulk-import 300-1500 keywords from a spreadsheet, attach the right tags, and verify the keyword job finished without errors. Today he clicks through the UI for the group + domain, then uses the "Add keywords" modal which silently truncates large pastes. He has no way to dry-run an import to see what the API will accept vs reject. He's not a data engineer; he just wants the import to be reproducible per client.

**Weekly ritual:** New-client onboarding (1-2 per month) and bulk keyword-universe refreshes across existing clients (weekly).

**Frustration:** No dry-run, no diff between "keywords I want" and "keywords AccuRanker has", no clean way to script the onboarding so the next client takes 10 minutes instead of two hours.

## Candidates (pre-cut)

[20 candidates generated — see Survivors and kills for outcomes]

## Survivors and kills

### Survivors

| # | Feature | Command | Score | Buildability | How It Works | Evidence |
|---|---------|---------|-------|--------------|--------------|----------|
| 1 | Cursor-aware incremental sync into local SQLite | `accuranker sync --domain <id>` | 9/10 | hand-code | Walks `/domains/{id}/keywords/` and `/keywords/{id}/` with cursor stored in local SQLite `sync_cursor` table; resumes from `last_period_to`; writes to `keywords` + `keyword_ranks` tables mirroring v2 schema | Brief User Vision §1-2, Devansh persona, Build Priority 2 |
| 2 | NDJSON dump with auto-chunked date window | `accuranker dump keyword-ranks --domain <id> --from <date> --to <date>` | 9/10 | hand-code | Splits requested window into 100-day slices, calls per slice, dedups by `(keyword_id, search_date)`, emits NDJSON | Brief §Period window cap (100 days), User Vision §2, Mira + Devansh |
| 3 | Machine-readable data schema for v2 mirror | `accuranker schema [--resource <name>] [--format postgres-ddl]` | 8/10 | hand-code | Emits a static JSON/DDL document describing every resource's field set, types, filter dimensions, comparator legality, and sync hints | Brief §Data Layer, User Vision §complete schema for v2, Devansh |
| 4 | Filter-dimension catalog and describe | `accuranker filters list`, `accuranker filters describe <name>` | 8/10 | hand-code | Static reference table built from spec's 100+ filter dimensions; `describe` prints accepted comparators per type class | Brief §Filter dimensions, User Vision §all dimensions accessible, Yusra |
| 5 | SERP-feature gain/loss delta | `accuranker serp-features delta --domain <id> --from <date> --to <date> --feature ai_overview` | 7/10 | hand-code | Local SQL diff of `page_serp_features` JSON arrays between two snapshot dates per keyword; emits `gained`/`lost`/`held` partition | Brief Top Workflow #6, filter dim `has_ai_overview`, Mira |
| 6 | Cannibalization detection | `accuranker cannibalize --domain <id>` | 7/10 | hand-code | Local SQL over synced ranks: flag keywords where `extra_ranks` length >1 or `highest_ranking_page` differs across window | Brief §KeywordRank `extra_ranks`, classic SEO anti-pattern, Mira |
| 7 | Decay opportunity finder | `accuranker decay --domain <id> --window 30d` | 7/10 | hand-code | Local SQL join of `keyword_ranks` × `search_volume_history`: select keywords where rank trend up (worse) AND volume trend up | Brief §SearchVolumeHistory+KeywordRank, Yusra |
| 8 | Keyword spec diff before apply | `accuranker keywords diff --domain <id> --spec keywords.csv` | 7/10 | hand-code | Reads local CSV/NDJSON, fetches current keywords, emits adds/removes/updates partition; `--apply` runs absorbed bulk | Brief Top Workflow #7, Tomás, no UI dry-run |

### Killed candidates

| # | Feature | Reason cut |
|---|---------|-----------|
| 5 | `movers` local rank delta | Subsumed by absorbed `keywords list --filter "rank_change > N"` (server-side) |
| 7 | `share-of-voice trend` | Subsumed by `dump` (#2) over `domain history` — no separate command needed |
| 10 | `onboard` orchestration | Scope creep (YAML parser); descopes into absorbed bulk `keywords create --stdin` + survivor #8 `keywords diff` |
| 12 | `rate` budget | Verifiability weak — no documented rate-limit headers; fold into absorbed `doctor` |
| 14 | `replay` from local SQLite | Narrow audience (v2-dev only); cut to hit 8-survivor target |
| 15 | `watch` interval poller | Scope creep (persistent process); one-shot is cron + absorbed list |
| 16 | `backfill` | Subsumed by `sync` + `dump` |
| 17 | `token-budget` preview | Verifiability fail — estimating response bytes without calling defeats it |
| 18 | `cost` estimator | Too thin (arithmetic); informational only |
