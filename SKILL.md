---
name: pp-accuranker
description: "The first real CLI for AccuRanker — built to feed your warehouse and your AI agents, not to ship dashboards. Trigger phrases: `use accuranker`, `run accuranker`, `sync accuranker into postgres`, `dump accuranker keyword history`, `list domains in accuranker`, `check rank for keyword`, `find decaying keywords`, `detect keyword cannibalization`, `ai overview keywords gained`, `filter accuranker by`, `accuranker rank history`."
author: "Lasse Jellum"
license: "Apache-2.0"
argument-hint: "<command> [args] | install cli|mcp"
allowed-tools: "Read Bash"
metadata:
  openclaw:
    requires:
      bins:
        - accuranker-pp-cli
---

# AccuRanker — Printing Press CLI

## Prerequisites: Install the CLI

This skill drives the `accuranker-pp-cli` binary. **You must verify the CLI is installed before invoking any command from this skill.** If it is missing, install it first:

1. Install via the Printing Press installer:
   ```bash
   npx -y @mvanhorn/printing-press install accuranker --cli-only
   ```
2. Verify: `accuranker-pp-cli --version`
3. Ensure `$GOPATH/bin` (or `$HOME/go/bin`) is on `$PATH`.

If the `npx` install fails before this CLI has a public-library category, install Node or use the category-specific Go fallback after publish.

If `--version` reports "command not found" after install, the install step did not put the binary on `$PATH`. Do not proceed with skill commands until verification succeeds.

AccuRanker has rich rank-tracking data — 100+ filter dimensions, daily rank history per keyword, AI Overview tracking, share of voice, search intent — but no SDK, no MCP, no CLI. This CLI exposes every Read and Write endpoint as a typed command, auto-chunks the 100-day historical cap, surfaces the full filter DSL on the command line, and ships a `sync` command that proves the v2 Postgres-mirror pattern. Built for AI-agent consumption: NDJSON streams, --select dot-path projection, typed exit codes, and a discoverable filter catalog.

## When to Use This CLI

Reach for this CLI when an agent or pipeline needs AccuRanker data programmatically. It is the right tool for warehouse ETL (use `sync` or `dump`), for ad-hoc filtering with the full 100+-dimension DSL (use `keywords list --filter`), for safe bulk keyword imports (`keywords-diff` then `--apply`), and for any agent task where token-efficient structured output matters more than dashboards. Skip it for one-tab visual rank review — the AccuRanker UI is the right surface for that.

## Unique Capabilities

These capabilities aren't available in any other tool for this API.

### Sync facilitator
- **`mirror`** — Sync a domain's data into typed SQLite tables that match schema/model.yaml exactly — keywords, keyword_ranks, domain_history, etc. AccuRanker's mandatory fields= parameter is auto-filled per resource. Resumable across runs.

  _Reach for this whenever you intend to push to Postgres or query the local mirror with typed SQL. `sync` (press builtin) is the generic alternative for one-off fetches._

  ```bash
  accuranker-pp-cli mirror --domain 295242 --since 2024-01-01 --json
  ```
- **`dump`** — Stream historical rank or domain-history data over any window. The CLI walks 100-day slices internally and dedups, emitting one record per line.

  _Reach for this when an agent or pipeline asks for arbitrary historical windows. Auto-chunks the 100-day cap so callers don't have to._

  ```bash
  accuranker-pp-cli dump keyword-ranks --domain 295242 --from 2024-01-01 --to 2026-05-20 --json
  ```
- **`schema`** — Print the canonical data shape — every resource, field, filter dimension, comparator legality, and sync hint — as JSON or Postgres DDL.

  _Reach for this before standing up a warehouse. The DDL stays in lockstep with the CLI, so the loader and the schema can't drift._

  ```bash
  accuranker-pp-cli schema --resource keyword_ranks --format postgres-ddl
  ```
- **`push`** — Upsert every table from the local SQLite mirror into a remote Postgres schema using DDL derived from schema/model.yaml. The bridge to a downstream analytics layer.

  _Reach for this when handing data off to the downstream Bun MCP service or any Postgres consumer. The schema you ship matches schema/model.yaml exactly; nothing is hidden behind Go structs._

  ```bash
  accuranker-pp-cli push --target postgres://user:pass@host:5432/db --schema accuranker --dry-run --json
  ```

### Agent-native discovery
- **`filters list`** — List all 100+ filter dimensions AccuRanker accepts in dynamic filter expressions, with type class and accepted comparators.

  _Reach for this before composing a complex --filter argument. An agent can pipe `filters list --json` to discover dimensions on demand._

  ```bash
  accuranker-pp-cli filters describe search_intent
  ```

### Local SQL transcendence
- **`serp-features delta`** — Partition a domain's keywords into gained / lost / held for any SERP feature (AI Overview, featured snippet, local pack, etc.) over a date window.

  _Reach for this when tracking AI Overview rollout or featured-snippet wars. Reports answer 'who changed' in one call._

  ```bash
  accuranker-pp-cli serp-features delta --domain 295242 --feature ai_overview --from 2026-05-01 --to 2026-05-20 --json
  ```
- **`cannibalize`** — Flag keywords where multiple URLs from the same domain rank simultaneously, or where the highest-ranking page flips across the window.

  _Reach for this when planning consolidation or canonicalization work. The output is a worklist, not a dashboard._

  ```bash
  accuranker-pp-cli cannibalize --domain 295242 --from 2026-04-01 --json
  ```
- **`decay`** — Find keywords whose rank is getting worse while their search volume is climbing — the classic 'losing ground on rising demand' set.

  _Reach for this for briefing prep or content-refresh prioritization. The output is the SEO equivalent of a P&L variance report._

  ```bash
  accuranker-pp-cli decay --domain 295242 --window 30 --min-volume 500 --json
  ```

### Write-side safety
- **`keywords-diff`** — Compare a local CSV/NDJSON keyword spec against AccuRanker's current keyword list. Emits adds/removes/updates without writing.

  _Reach for this every time you're about to bulk-import. Use --apply to commit the partition that diff produced._

  ```bash
  accuranker-pp-cli keywords-diff --domain 295242 --spec ./keywords.csv --json
  ```

## Command Reference

**accounts** — List/get accounts associated with the caller's API token

- `accuranker-pp-cli accounts create` — POST with filters in body is equivalent to GET with filters. This enables complex filter queries without URL length...
- `accuranker-pp-cli accounts get` — Get
- `accuranker-pp-cli accounts list` — List

**brand** — AccuLLM brands. Requires LLM API plan tier.

- `accuranker-pp-cli brand create` — This endpoint creates a new brand for your group. A brand can only be created on groups or subaccounts authorized by...
- `accuranker-pp-cli brand delete` — This endpoint deletes a brand and all underlying prompts - hence use this endpoint with caution. Curl example: curl...
- `accuranker-pp-cli brand update` — This endpoint updates a brand in your group. All fields are optional. If you leave a field out, the value will not...

**brands** — AccuLLM brands. Requires LLM API plan tier.

- `accuranker-pp-cli brands get` — Get
- `accuranker-pp-cli brands list` — List
- `accuranker-pp-cli brands list-with-filters` — POST alternative to GET with filters in request body. Useful for complex filter queries.

**domain** — List, get, create, update, delete domains. Domain payloads include 24-field DomainHistory time-series when period_from/to are set.

- `accuranker-pp-cli domain create` — This endpoint creates a new domain for your group. A domain can only be created on groups or subaccounts authorized...
- `accuranker-pp-cli domain delete` — This endpoint deletes a domain and all underlying keywordss - hence use this endpoint with caution. Curl example:...
- `accuranker-pp-cli domain update` — This endpoint updates a domain in your group. All fields are optional. If you leave a field out, the value will not...

**domains** — List, get, create, update, delete domains. Domain payloads include 24-field DomainHistory time-series when period_from/to are set.

- `accuranker-pp-cli domains get` — Get
- `accuranker-pp-cli domains list` — List
- `accuranker-pp-cli domains list-with-filters` — POST alternative to GET with filters in request body. Useful for complex filter queries.

**group** — Write API: groups ("Clients") that contain domains.

- `accuranker-pp-cli group create` — This endpoint creates a new group on your account. The group can only be created on accounts or subaccounts...
- `accuranker-pp-cli group delete` — This endpoint deletes a group and all underlying domains and keywords - hence use this endpoint with caution. Curl...
- `accuranker-pp-cli group update` — This endpoint updates a group for your account. Only groups on accounts or subaccounts authorized by the token can...

**keyword** — Read keyword ranks (highest-cardinality data). Supports period windows and dynamic filters. Write side supports bulk create/update/delete (async job).

- `accuranker-pp-cli keyword create` — With this endpoint, you can create one or more keywords on a domain. Keywords can only be created on domains...
- `accuranker-pp-cli keyword delete` — This endpoint deletes is used to delete a keyword - hence you should use this with caution. Curl example: curl -X...
- `accuranker-pp-cli keyword update` — This endpoint updates settings for the given keywords. Only keywords on domains authorized by the token can be...

**overview** — High-level account overview. Requires additional plan tier.

- `accuranker-pp-cli overview get-group-and-domains` — This endpoint returns a list of all accounts, groups and domains for your account. The list is based on the token...
- `accuranker-pp-cli overview get-keyword-for-domain` — This endpoint returns a list of all your keywords on the provided domain. Curl example: curl -X GET -H...

**prompt** — AccuLLM prompts. Requires LLM API plan tier.

- `accuranker-pp-cli prompt create` — With this endpoint, you can create one or more prompts on a brand. Prompts can only be created on brands authorized...
- `accuranker-pp-cli prompt delete` — This endpoint deletes is used to delete a prompt - hence you should use this with caution. Curl example: curl -X...
- `accuranker-pp-cli prompt update` — This endpoint updates settings for the given prompts. Only prompts on domains authorized by the token can be...

**status** — Manage status

- `accuranker-pp-cli status <job_id>` — Return status of an keyword job, given a job ID. To start a job call POST or PUT /keyword. Any response code other...


### Finding the right command

When you know what you want to do but not which command does it, ask the CLI directly:

```bash
accuranker-pp-cli which "<capability in your own words>"
```

`which` resolves a natural-language capability query to the best matching command from this CLI's curated feature index. Exit code `0` means at least one match; exit code `2` means no confident match — fall back to `--help` or use a narrower query.

## Recipes


### Build a local typed mirror of one client's data

```bash
accuranker-pp-cli mirror --domain 295242 --since 30d --json
```

Builds typed SQLite tables (accuranker_keywords, accuranker_keyword_ranks, etc.) from schema/model.yaml. Reruns are incremental via accuranker_sync_cursor.

### Find SERP-feature gains in the last 14 days

```bash
accuranker-pp-cli serp-features delta --domain 295242 --feature ai_overview --from 2026-05-06 --to 2026-05-20 --partition gained --json --select keyword,keyword_id
```

`--partition gained` narrows the response to a single bucket; `--json` + `--select` cap the token footprint to the keyword identity an agent needs.

### Dump 2+ years of rank history into Postgres

```bash
accuranker-pp-cli dump keyword-ranks --domain 295242 --from 2024-01-01 --to 2026-05-20 --json | psql -c '\COPY accuranker_ranks FROM STDIN CSV'
```

The CLI walks 100-day windows internally and dedups by (keyword_id, search_date). Pipe to `psql -c '\COPY accuranker_ranks FROM STDIN CSV'`.

### Discover a filter dimension before composing a query

```bash
accuranker-pp-cli filters describe search_intent --json
```

Agents pipe `filters describe` to learn the comparator class before composing any --filter expression; no doc lookups required.

### Push the local mirror to Postgres

```bash
accuranker-pp-cli push --target 'postgres://loader:***@warehouse:5432/seo' --schema accuranker --json
```

Provisions the schema with `CREATE SCHEMA IF NOT EXISTS`, runs the Postgres DDL derived from schema/model.yaml, then upserts every accuranker_* table.

### Dry-run a bulk keyword import

```bash
accuranker-pp-cli keywords-diff --domain 295242 --spec ./client-keywords.csv --json && accuranker-pp-cli keywords-diff --domain 295242 --spec ./client-keywords.csv --apply
```

Diff shows the adds/removes/updates partition. --apply commits exactly that partition via the absorbed bulk endpoints; no UI surprises.

## Auth Setup

Generate a token at https://app.accuranker.com/app/profile, then `accuranker-pp-cli auth set-token YOUR_TOKEN_HERE` (stores locally) or `export ACCURANKER_API_TOKEN=...`. The token sends as `Authorization: Token <token>`. OAuth2 is also supported for multi-account integrations.

Run `accuranker-pp-cli doctor` to verify setup.

## Agent Mode

Add `--agent` to any command. Expands to: `--json --compact --no-input --no-color --yes`.

- **Pipeable** — JSON on stdout, errors on stderr
- **Filterable** — `--select` keeps a subset of fields. Dotted paths descend into nested structures; arrays traverse element-wise. Critical for keeping context small on verbose APIs:

  ```bash
  accuranker-pp-cli accounts list --agent --select id,name,status
  ```
- **Previewable** — `--dry-run` shows the request without sending
- **Offline-friendly** — sync/search commands can use the local SQLite store when available
- **Non-interactive** — never prompts, every input is a flag
- **Explicit retries** — use `--idempotent` only when an already-existing create should count as success, and `--ignore-missing` only when a missing delete target should count as success

### Response envelope

Commands that read from the local store or the API wrap output in a provenance envelope:

```json
{
  "meta": {"source": "live" | "local", "synced_at": "...", "reason": "..."},
  "results": <data>
}
```

Parse `.results` for data and `.meta.source` to know whether it's live or local. A human-readable `N results (live)` summary is printed to stderr only when stdout is a terminal AND no machine-format flag (`--json`, `--csv`, `--compact`, `--quiet`, `--plain`, `--select`) is set — piped/agent consumers and explicit-format runs get pure JSON on stdout.

## Agent Feedback

When you (or the agent) notice something off about this CLI, record it:

```
accuranker-pp-cli feedback "the --since flag is inclusive but docs say exclusive"
accuranker-pp-cli feedback --stdin < notes.txt
accuranker-pp-cli feedback list --json --limit 10
```

Entries are stored locally at `~/.accuranker-pp-cli/feedback.jsonl`. They are never POSTed unless `ACCURANKER_FEEDBACK_ENDPOINT` is set AND either `--send` is passed or `ACCURANKER_FEEDBACK_AUTO_SEND=true`. Default behavior is local-only.

Write what *surprised* you, not a bug report. Short, specific, one line: that is the part that compounds.

## Output Delivery

Every command accepts `--deliver <sink>`. The output goes to the named sink in addition to (or instead of) stdout, so agents can route command results without hand-piping. Three sinks are supported:

| Sink | Effect |
|------|--------|
| `stdout` | Default; write to stdout only |
| `file:<path>` | Atomically write output to `<path>` (tmp + rename) |
| `webhook:<url>` | POST the output body to the URL (`application/json` or `application/x-ndjson` when `--compact`) |

Unknown schemes are refused with a structured error naming the supported set. Webhook failures return non-zero and log the URL + HTTP status on stderr.

## Named Profiles

A profile is a saved set of flag values, reused across invocations. Use it when a scheduled agent calls the same command every run with the same configuration - HeyGen's "Beacon" pattern.

```
accuranker-pp-cli profile save briefing --json
accuranker-pp-cli --profile briefing accounts list
accuranker-pp-cli profile list --json
accuranker-pp-cli profile show briefing
accuranker-pp-cli profile delete briefing --yes
```

Explicit flags always win over profile values; profile values win over defaults. `agent-context` lists all available profiles under `available_profiles` so introspecting agents discover them at runtime.

## Exit Codes

| Code | Meaning |
|------|---------|
| 0 | Success |
| 2 | Usage error (wrong arguments) |
| 3 | Resource not found |
| 4 | Authentication required |
| 5 | API error (upstream issue) |
| 7 | Rate limited (wait and retry) |
| 10 | Config error |

## Argument Parsing

Parse `$ARGUMENTS`:

1. **Empty, `help`, or `--help`** → show `accuranker-pp-cli --help` output
2. **Starts with `install`** → ends with `mcp` → MCP installation; otherwise → see Prerequisites above
3. **Anything else** → Direct Use (execute as CLI command with `--agent`)

## MCP Server Installation

Install the MCP binary from this CLI's published public-library entry or pre-built release, then register it:

```bash
claude mcp add accuranker-pp-mcp -- accuranker-pp-mcp
```

Verify: `claude mcp list`

## Direct Use

1. Check if installed: `which accuranker-pp-cli`
   If not found, offer to install (see Prerequisites at the top of this skill).
2. Match the user query to the best command from the Unique Capabilities and Command Reference above.
3. Execute with the `--agent` flag:
   ```bash
   accuranker-pp-cli <command> [subcommand] [args] --agent
   ```
4. If ambiguous, drill into subcommand help: `accuranker-pp-cli <command> --help`.
