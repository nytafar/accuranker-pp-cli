# AccuRanker CLI

**The first real CLI for AccuRanker — built to feed your warehouse and your AI agents, not to ship dashboards.**

AccuRanker has rich rank-tracking data — 100+ filter dimensions, daily rank history per keyword, AI Overview tracking, share of voice, search intent — but no SDK, no MCP, no CLI. This CLI exposes every Read and Write endpoint as a typed command, auto-chunks the 100-day historical cap, surfaces the full filter DSL on the command line, and ships a `sync` command that proves the v2 Postgres-mirror pattern. Built for AI-agent consumption: NDJSON streams, --select dot-path projection, typed exit codes, and a discoverable filter catalog.

Printed by [@nytafar](https://github.com/nytafar) (Lasse Jellum).

## Install

The recommended path installs both the `accuranker-pp-cli` binary and the `pp-accuranker` agent skill (Claude Code, Codex, Cursor, Gemini CLI, GitHub Copilot, and other agents supported by the upstream [`skills`](https://github.com/vercel-labs/skills) CLI) in one shot:

```bash
npx -y @mvanhorn/printing-press install accuranker
```

For CLI only (no skill):

```bash
npx -y @mvanhorn/printing-press install accuranker --cli-only
```

For skill only — installs the skill into the same agents as the default command above, but skips the CLI binary (use this to update or reinstall just the skill):

```bash
npx -y @mvanhorn/printing-press install accuranker --skill-only
```

To constrain the skill install to one or more specific agents (repeatable — agent names match the [`skills`](https://github.com/vercel-labs/skills) CLI):

```bash
npx -y @mvanhorn/printing-press install accuranker --agent claude-code
npx -y @mvanhorn/printing-press install accuranker --agent claude-code --agent codex
```

### Without Node

The generated install path is category-agnostic until this CLI is published. If `npx` is not available before publish, install Node or use the category-specific Go fallback from the public-library entry after publish.

### Pre-built binary

Download a pre-built binary for your platform from the [latest release](https://github.com/mvanhorn/printing-press-library/releases/tag/accuranker-current). On macOS, clear the Gatekeeper quarantine: `xattr -d com.apple.quarantine <binary>`. On Unix, mark it executable: `chmod +x <binary>`.

<!-- pp-hermes-install-anchor -->
## Install for Hermes

From the Hermes CLI:

```bash
hermes skills install mvanhorn/printing-press-library/cli-skills/pp-accuranker --force
```

Inside a Hermes chat session:

```bash
/skills install mvanhorn/printing-press-library/cli-skills/pp-accuranker --force
```

## Install for OpenClaw

Tell your OpenClaw agent (copy this):

```
Install the pp-accuranker skill from https://github.com/mvanhorn/printing-press-library/tree/main/cli-skills/pp-accuranker. The skill defines how its required CLI can be installed.
```

## Use with Claude Desktop

This CLI ships an [MCPB](https://github.com/modelcontextprotocol/mcpb) bundle — Claude Desktop's standard format for one-click MCP extension installs (no JSON config required).

To install:

1. Download the `.mcpb` for your platform from the [latest release](https://github.com/mvanhorn/printing-press-library/releases/tag/accuranker-current).
2. Double-click the `.mcpb` file. Claude Desktop opens and walks you through the install.
3. Fill in `ACCURANKER_API_TOKEN` when Claude Desktop prompts you.

Requires Claude Desktop 1.0.0 or later. Pre-built bundles ship for macOS Apple Silicon (`darwin-arm64`) and Windows (`amd64`, `arm64`); for other platforms, use the manual config below.

<details>
<summary>Manual JSON config (advanced)</summary>

If you can't use the MCPB bundle (older Claude Desktop, unsupported platform), install the MCP binary and configure it manually.


Install the MCP binary from this CLI's published public-library entry or pre-built release.

Add to your Claude Desktop config (`~/Library/Application Support/Claude/claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "accuranker": {
      "command": "accuranker-pp-mcp",
      "env": {
        "ACCURANKER_API_TOKEN": "<your-key>"
      }
    }
  }
}
```

</details>

## Authentication

Generate a token at https://app.accuranker.com/app/profile, then `accuranker-pp-cli auth set-token` (stores locally) or `export ACCURANKER_API_TOKEN=...`. The token sends as `Authorization: Token <token>`. OAuth2 is also supported for multi-account integrations.

## Quick Start

```bash
# Save your AccuRanker token; stored at ~/.config/accuranker-pp-cli/config.yaml with 0600 perms
accuranker-pp-cli auth set-token YOUR_TOKEN_HERE


# Confirms token works, plan tier, rate-limit budget, reachability
accuranker-pp-cli doctor


# List every domain you have access to
accuranker-pp-cli domains list --json


# Fetch the last week of ranks for one domain — note the mandatory --fields parameter
accuranker-pp-cli domains keywords list 295242 --fields 'id,keyword,ranks.rank,ranks.created_at' --period-from 2026-05-13 --period-to 2026-05-20 --json


# Build the typed v2 SQLite mirror — picks up where it left off on reruns
accuranker-pp-cli mirror --domain 295242 --since 2024-01-01 --json


# Auto-chunked NDJSON dump of 2+ years of rank history, ready to pipe into Postgres
accuranker-pp-cli dump keyword-ranks --domain 295242 --from 2024-01-01 --to 2026-05-20 --json

```

## Unique Features

These capabilities aren't available in any other tool for this API.

### Sync facilitator
- **`mirror`** — Sync a domain's data into typed SQLite tables that match schema/model.yaml exactly — keywords, keyword_ranks, domain_history, etc. AccuRanker's mandatory fields= parameter is auto-filled per resource. Resumable across runs.

  _Reach for this whenever you intend to push to Postgres or query the local mirror with typed SQL. `sync` (press builtin) is the generic alternative for one-off fetches._

  ```bash
  accuranker-pp-cli mirror --domain 295242 --since 2024-01-01 --json
  ```
- **`dump`** — Stream NDJSON for **every** warehouse resource in `schema/model.yaml` (all except the internal `sync_cursor`). Windowed families (require `--from`; walk 100-day slices internally and dedup): `keyword-ranks`, `competitor-ranks`, `domain-history`, `full-serp`, `search-volume-history`, `ai-search-volume-history`, `competitor-history`, `landing-page-history`, `tag-history`. Snapshot families (current dimension state): `keywords`, `competitors`, `landing-pages`, `tags`, `people-also-ask`. **Global** families (dump the whole set, **no `--domain`**): `domains` (carries `created_at` + group linkage), `accounts`, `groups`. The **LLM/AccuLLM tier** (`brands`, `prompts`, `prompt-results`) is wired but off by default: opt in with `--include-llm` in a sweep, and it degrades to **exit 45** (`skipped_unentitled` inside a sweep) when your plan lacks the tier — `tag-history` rides the same graceful gate. `dump all` sweeps every non-LLM resource through one code path. Every run writes exactly one machine-readable run report (stderr or `--report-file`) with `rows_emitted`, `requests_made`, `rate_limit_hits`, `clean_exit`, and `next_cursor` — the watermark handshake a sync spine advances from. `--envelope` wraps each record as `{source, endpoint, params_hash, api_version, fetched_at, payload}` for lossless raw-JSONB landing. `dump keyword-ranks` emits the `is_initial=true` baseline row (KeywordInitialRank) so every keyword's series has a defined left edge; `--probe-earliest` binary-searches the earliest date with data per domain and reports it as `earliest_available` (the backfill floor).

  _Reach for this when an agent or pipeline asks for arbitrary historical windows, or as the extract adapter under a warehouse sync spine — every resource is landable via `dump <resource> --envelope --report-file` with zero adapter code. Partial failures exit non-zero with `next_cursor` = the last chunk boundary that completed for every domain._

  ```bash
  accuranker-pp-cli dump keyword-ranks --domain 295242 --from 2024-01-01 --to 2026-05-20 --report-file run.json
  accuranker-pp-cli dump keywords --domain 295242 > keywords-$(date +%F).ndjson   # daily panel snapshot
  accuranker-pp-cli dump domains --envelope                                        # global: every tracked domain
  accuranker-pp-cli dump all --domain 295242 --from 2024-01-01 --envelope          # full non-LLM sweep
  accuranker-pp-cli dump keyword-ranks --domain 295242 --probe-earliest            # discover the retention floor
  ```
- **`schema`** — Print the canonical data shape — every resource, field, filter dimension, comparator legality, and sync hint — as JSON or Postgres DDL. `--format postgres-ddl --comments` appends `COMMENT ON TABLE/COLUMN` seeded from model.yaml descriptions; `--catalog` emits catalog.json (per table: grain, upsert key, watermark column, timezone notes) for the warehouse curator.

  _Reach for this before standing up a warehouse. The DDL, comments, and catalog stay in lockstep with the CLI, so the loader and the schema can't drift._

  ```bash
  accuranker-pp-cli schema --resource keyword_ranks --format postgres-ddl
  accuranker-pp-cli schema --format postgres-ddl --comments > accuranker.sql
  accuranker-pp-cli schema --catalog > catalog.json
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
- **`keywords-diff`** — Compare a local CSV/NDJSON keyword spec against AccuRanker's current keyword list. Emits adds/removes/updates without writing. With `--against <prev-snapshot.ndjson|mirror>` it becomes a panel-membership differ: compare the live keyword list against a previous `dump keywords` snapshot and emit NDJSON `added`/`removed` events — the soft-delete signal the API itself never provides (the warehouse stamps `tracked_to = observed_at`).

  _Reach for this every time you're about to bulk-import (use --apply to commit), or daily under a sync spine to synthesize keyword panel events._

  ```bash
  accuranker-pp-cli keywords-diff --domain 295242 --spec ./keywords.csv --json
  accuranker-pp-cli keywords-diff --domain 295242 --against keywords-2026-07-06.ndjson
  ```

## Usage

Run `accuranker-pp-cli --help` for the full command reference and flag list.

## Commands

### accounts

List/get accounts associated with the caller's API token

- **`accuranker-pp-cli accounts create`** - POST with filters in body is equivalent to GET with filters.
This enables complex filter queries without URL length limitations.

Expected body format:
{
    "filters": [
        {"name": "rank", "comparator": "lt", "value": 10},
        {"name": "keyword", "comparator": "contains", "value": "test"}
    ],
    "operator": "and"
}
- **`accuranker-pp-cli accounts get`** - Get
- **`accuranker-pp-cli accounts list`** - List

### brand

AccuLLM brands. Requires LLM API plan tier.

- **`accuranker-pp-cli brand create`** - This endpoint creates a new brand for your group. A brand can only be created on groups or subaccounts authorized by the token. Curl example:

    curl -X POST \
        -H "Authorization: Token YOUR_API_TOKEN" \
        -d "{\"domain\":\"DOMAIN_URL\",\"display_name\":\"BRAND_NAME\",\"brand_list\":[\"BRAND\"],\"default_countrylocale\":\"en-GB\",\"group_id\":YOUR_GROUP_ID}" \
        https://app.accuranker.com/api/v4/brand/
- **`accuranker-pp-cli brand delete`** - This endpoint deletes a brand and all underlying prompts - hence use this endpoint with caution. Curl example:

    curl -X DELETE \
    -H "Authorization: Token YOUR_API_TOKEN" \
    https://app.accuranker.com/api/v4/brand/YOUR_BRAND_ID
- **`accuranker-pp-cli brand update`** - This endpoint updates a brand in your group. All fields are optional. If you leave a field out, the value will not be updated. Curl example:

    curl -X PUT \
    -H "Authorization: Token YOUR_API_TOKEN" \
    -d "{\"domain\":\"BRAND_URL\", \"display_name\": \"Brand display name\"}" \
    https://app.accuranker.com/api/v4/brand/YOUR_BRAND_ID

### brands

AccuLLM brands. Requires LLM API plan tier.

- **`accuranker-pp-cli brands get`** - Get
- **`accuranker-pp-cli brands list`** - List
- **`accuranker-pp-cli brands list-with-filters`** - POST alternative to GET with filters in request body. Useful for complex filter queries.

### domain

List, get, create, update, delete domains. Domain payloads include 24-field DomainHistory time-series when period_from/to are set.

- **`accuranker-pp-cli domain create`** - This endpoint creates a new domain for your group. A domain can only be created on groups or subaccounts authorized by the token.
    Set primary=true for a SearchSetting object you want to be primary for the domain. If the primary value is not set for any SearchSetting object,
    the first will be assigned as primary. Curl example:

    curl -X POST \
        -H "Authorization: Token YOUR_API_TOKEN" \
        -d "{\"domain\":\"DOMAIN_URL\",\"default_searchsettings_names\":[{\"countrylocale\":\"en-GB\",\"search_engine_names\":[{\"search_engine\":\"Google\",\"search_type_names\":[\"Desktop\"]}],\"locations\":[\"Buckingham Palace, London\"]}],\"group_id\":YOUR_GROUP_ID}" \
        https://app.accuranker.com/api/v4/domain/
- **`accuranker-pp-cli domain delete`** - This endpoint deletes a domain and all underlying keywordss - hence use this endpoint with caution. Curl example:

    curl -X DELETE \
    -H "Authorization: Token YOUR_API_TOKEN" \
    https://app.accuranker.com/api/v4/domain/YOUR_DOMAIN_ID
- **`accuranker-pp-cli domain update`** - This endpoint updates a domain in your group. All fields are optional. If you leave a field out, the value will not be updated.
    The exception is sub-fields to default_searchsettings_names: the entire default_searchsettings_names objects will overwrite whatever exists on the domain.
    Set primary=true for a SearchSetting object you want to be primary for the domain. If the primary value is not set for any SearchSetting object,
    the first will be assigned as primary. Curl example:

    curl -X PUT \
    -H "Authorization: Token YOUR_API_TOKEN" \
    -d "{\"domain\":\"DOMAIN_URL\", \"display_name\": \"Domain display name\"}" \
    https://app.accuranker.com/api/v4/domain/YOUR_DOMAIN_ID

### domains

List, get, create, update, delete domains. Domain payloads include 24-field DomainHistory time-series when period_from/to are set.

- **`accuranker-pp-cli domains get`** - Get
- **`accuranker-pp-cli domains list`** - List
- **`accuranker-pp-cli domains list-with-filters`** - POST alternative to GET with filters in request body. Useful for complex filter queries.

### group

Write API: groups ("Clients") that contain domains.

- **`accuranker-pp-cli group create`** - This endpoint creates a new group on your account. The group can only be created on accounts or subaccounts authorized by the token. Curl example:

    curl -X POST \
    -H "Authorization: Token YOUR_API_TOKEN" \
    -d ""{\"name\":\"Group name\",\"account_id\":YOUR_ACCOUNT_ID}"" \
    https://app.accuranker.com/api/v4/group/
- **`accuranker-pp-cli group delete`** - This endpoint deletes a group and all underlying domains and keywords - hence use this endpoint with caution. Curl example:

    curl -X DELETE \
    -H "Authorization: Token YOUR_API_TOKEN" \
    https://app.accuranker.com/api/v4/group/YOUR_GROUP_ID
- **`accuranker-pp-cli group update`** - This endpoint updates a group for your account. Only groups on accounts or subaccounts authorized by the token can be updated.
    All fields are optional. If you leave a field out, the value will not be updated. Curl example:

    curl -X PUT \
    -H "Authorization: Token YOUR_API_TOKEN" \
    -d ""{\"name\":\"Group name\",\"account_id\":YOUR_ACCOUNT_ID}"" \
    https://app.accuranker.com/api/v4/group/YOUR_GROUP_ID

### keyword

Read keyword ranks (highest-cardinality data). Supports period windows and dynamic filters. Write side supports bulk create/update/delete (async job).

- **`accuranker-pp-cli keyword create`** - With this endpoint, you can create one or more keywords on a domain. Keywords can only be created on domains authorized by the token on accounts that allow keyword exceedings.
    Go to https://app.accuranker.com/app/account to check if it is enable.
    If no search settings are provided, the default search settings from the domain will be used.
    This endpoint starts a job to import your keyword(s) - If you try to add existing keywords, these will be ignored. The response header 'Location' points to a specific endpoint for monitoring the progress status of your keyword job. Curl example:

    curl -X POST \
    -H "Authorization: Token YOUR_API_TOKEN" \
    -d "{\"domain_id\": YOUR_DOMAIN_ID, \"keywords\":[\"keyword 1\", \"keyword 2\"]}" \
    https://app.accuranker.com/api/v4/keyword/
- **`accuranker-pp-cli keyword delete`** - This endpoint deletes is used to delete a keyword - hence you should use this with caution. Curl example:

    curl -X DELETE \
    -H "Authorization: Token YOUR_API_TOKEN" \
    -d "{\"keyword_ids\":[YOUR_KEYWORD_IDS]}" \
    https://app.accuranker.com/api/v4/keyword/
- **`accuranker-pp-cli keyword update`** - This endpoint updates settings for the given keywords. Only keywords on domains authorized by the token can be updated. Curl example:

    curl -X PUT \
    -H "Authorization: Token YOUR_API_TOKEN" \
    -d "{\"keyword_ids\":[YOUR_KEYWORD_IDS]}" \
    https://app.accuranker.com/api/v4/keyword/

### overview

High-level account overview. Requires additional plan tier.

- **`accuranker-pp-cli overview get-group-and-domains`** - This endpoint returns a list of all accounts, groups and domains for your account. The list is based on the token provided. If you are looking for account_id, group_id or domain_id use this endpoint. Curl example:

    curl -X GET \
    -H "Authorization: Token YOUR_API_TOKEN" \
    https://app.accuranker.com/api/v4/overview/group_domains
- **`accuranker-pp-cli overview get-keyword-for-domain`** - This endpoint returns a list of all your keywords on the provided domain. Curl example:

    curl -X GET \
    -H "Authorization: Token YOUR_API_TOKEN" \
    https://app.accuranker.com/api/v4/overview/keywords_for_domain/YOUR_DOMAIN_ID\?keyword_contains=test

### prompt

AccuLLM prompts. Requires LLM API plan tier.

- **`accuranker-pp-cli prompt create`** - With this endpoint, you can create one or more prompts on a brand. Prompts can only be created on brands authorized by the token.
    If no countrylocale is provided, the default countrylocale from the brand will be used.
    This endpoint starts a job to import your prompt(s) - If you try to add existing prompts, these will be ignored. The response header 'Location' points to a specific endpoint for monitoring the progress status of your prompt job. Curl example:

    curl -X POST \
    -H "Authorization: Token YOUR_API_TOKEN" \
    -d "{\"domain_id\": YOUR_DOMAIN_ID, \"prompts\":[\"prompt 1\", \"prompt 2\"]}" \
    https://app.accuranker.com/api/v4/prompt/
- **`accuranker-pp-cli prompt delete`** - This endpoint deletes is used to delete a prompt - hence you should use this with caution. Curl example:

    curl -X DELETE \
    -H "Authorization: Token YOUR_API_TOKEN" \
    -d "{\"prompt_ids\":[YOUR_PROMPT_IDS]}" \
    https://app.accuranker.com/api/v4/prompt/
- **`accuranker-pp-cli prompt update`** - This endpoint updates settings for the given prompts. Only prompts on domains authorized by the token can be updated. Curl example:

    curl -X PUT \
    -H "Authorization: Token YOUR_API_TOKEN" \
    -d "{\"prompt_ids\":[YOUR_PROMPT_IDS]}" \
    https://app.accuranker.com/api/v4/prompt/

### status

Manage status

- **`accuranker-pp-cli status <job_id>`** - Return status of an keyword job, given a job ID. To start a job call POST or PUT /keyword.
    Any response code other than 200 means the job is not finished or failed.
    Response code 200 means the job is finished and the keywords should be available on your domain. Curl example:

    curl -X GET \
    -H "Authorization: Token YOUR_API_TOKEN" \
    https://app.accuranker.com/api/v4/status/keyword_job/YOUR_JOB_ID


## Output Formats

```bash
# Human-readable table (default in terminal, JSON when piped)
accuranker-pp-cli accounts list

# JSON for scripting and agents
accuranker-pp-cli accounts list --json

# Filter to specific fields
accuranker-pp-cli accounts list --json --select id,name,status

# Dry run — show the request without sending
accuranker-pp-cli accounts list --dry-run

# Agent mode — JSON + compact + no prompts in one flag
accuranker-pp-cli accounts list --agent
```

## Agent Usage

This CLI is designed for AI agent consumption:

- **Non-interactive** - never prompts, every input is a flag
- **Pipeable** - `--json` output to stdout, errors to stderr
- **Filterable** - `--select id,name` returns only fields you need
- **Previewable** - `--dry-run` shows the request without sending
- **Explicit retries** - add `--idempotent` to create retries and `--ignore-missing` to delete retries when a no-op success is acceptable
- **Confirmable** - `--yes` for explicit confirmation of destructive actions
- **Piped input** - write commands can accept structured input when their help lists `--stdin`
- **Offline-friendly** - sync/search commands can use the local SQLite store when available
- **Agent-safe by default** - no colors or formatting unless `--human-friendly` is set

Exit codes: `0` success, `2` usage error, `3` not found, `4` auth error, `5` API error, `7` rate limited, `10` config error.

## Health Check

```bash
accuranker-pp-cli doctor
```

Verifies configuration, credentials, and connectivity to the API.

## Configuration

Config file: `~/.config/accuranker-pp-cli/config.toml`

Static request headers can be configured under `headers`; per-command header overrides take precedence.

Environment variables:

| Name | Kind | Required | Description |
| --- | --- | --- | --- |
| `ACCURANKER_API_TOKEN` | per_call | Yes | Set to your API credential. |

## Troubleshooting
**Authentication errors (exit code 4)**
- Run `accuranker-pp-cli doctor` to check credentials
- Verify the environment variable is set: `echo $ACCURANKER_API_TOKEN`
**Not found errors (exit code 3)**
- Check the resource ID is correct
- Run the `list` command to see available items

### API-specific

- **Response items are empty objects `{}`** — AccuRanker requires the `fields=` query parameter on Read endpoints. The CLI defaults to a sensible per-resource field set when --fields is omitted; if you used --fields explicitly, check the dot-notation.
- **HTTP 200 but body is `{"error":"Your plan does not include LLM API access..."}`** — Brands and Prompts endpoints require the AccuLLM plan tier. The CLI exits with code 45 (plan_tier_required). Upgrade your AccuRanker plan or skip the LLM-tier commands.
- **HTTP 429 / rate-limited** — AccuRanker enforces 100 req/min and 4 concurrent connections. The client backs off automatically. For long backfills use `sync` or `dump` which already pace themselves; reduce concurrency with --concurrency 2.
- **Date window error: 'maximum of 100 days'** — Use `dump` or `sync` instead of `domains get --period-*` — they auto-chunk windows wider than 100 days.
- **Token works in curl but not the CLI** — Run `accuranker-pp-cli doctor --json` to see which token source it picked. Order: --token flag > ACCURANKER_API_TOKEN > config file. Re-set with `accuranker-pp-cli auth set-token`.

## Known Gaps

- **`workflow archive --json` returns mixed output.** The press-generated `workflow archive` command emits per-resource sync NDJSON events to stdout (from `internal/cli/sync.go`) and also a final JSON envelope on stdout, so the combined output is not parseable as a single JSON document. This is a press-side coordination issue between `sync.go` and `channel_workflow.go` (both regenerated each run); a fix has to land in the printing-press codegen, not in this CLI. **Workaround:** use the hand-authored `mirror` command instead — `accuranker-pp-cli mirror --domain <id> --json` produces a clean JSON envelope and is the recommended path for v2 Postgres mirroring. The plain-text form (`workflow archive` without `--json`) works as expected.

---

Generated by [CLI Printing Press](https://github.com/mvanhorn/cli-printing-press)
