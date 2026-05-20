# AccuRanker CLI Brief

## API Identity

- **Vendor:** AccuRanker (Danish SEO rank tracking SaaS)
- **Domain:** SEO rank tracking, SERP analytics, AI Overview / LLM visibility
- **Base URL:** `https://app.accuranker.com/api/v4/`
- **Auth (Read API):** `Authorization: Token <token>` (preferred) or `Authorization: Bearer <oauth_access_token>`
- **Auth (Write API):** Same `Authorization: Token <token>` (security scheme name `ApiToken`, header)
- **OAuth2 endpoints:** `https://app.accuranker.com/oauth/token/` (grant types: `authorization_code`, `refresh_token`)
- **Rate limits:** **100 requests/minute** AND max **4 simultaneous connections per IP**
- **Period window cap:** Max **100 days** per `period_from`/`period_to` query window
- **Users:** SEO agencies, in-house SEO teams, performance marketers tracking 100s–10k+ keywords across multiple domains
- **Data profile:** Time-series rank data refreshed daily; long historical retention; rich per-keyword SERP metadata; relatively low write volume (configure once, read often)

## Reachability Risk
- **None.** Plain HTTPS REST. Token auth works first try. No bot protection. No Cloudflare challenge. Standard JSON responses with `application/json` Content-Type.

## Authentication Tested
- Token verified live (`/api/v4/accounts/?fields=id,name` returned 1 account).
- Plan tier matters: caller's account has Read API + Keywords/Domains/Tags/Landing Pages access. **LLM/Brand/Prompt endpoints and `/overview/` endpoints return paywall messages** (`{"error":"Your plan does not include LLM API access..."}` or `{"message":"The active plan for this account does not allow access to this endpoint."}`). The CLI must handle these gracefully — not as auth failures.

## Top Workflows (power-user)

1. **Daily/hourly sync of rank changes across all keywords for one or more domains** — most common workflow; need cursor-aware incremental fetch.
2. **Pull full historical rank data for a date window** — backfill or analytical dump up to 100 days at a time.
3. **Filtered keyword export** — "all mobile keywords ranking 1-10 with search_volume > 1000" piped to CSV/JSON.
4. **Domain/landing-page/tag aggregated history** — share-of-voice trends, average rank over time, by dimension.
5. **Competitor rank tracking** — per-keyword competitor ranks, time-series.
6. **AI Overview / LLM rank tracking** — Brand/Prompt endpoints (paywalled in tested plan, but spec is complete).
7. **Write side: programmatic keyword/domain/group/brand provisioning** — used when onboarding new clients or syncing keyword universes.

## Table Stakes (what every CLI in this space must do)

- Authenticated requests with token
- List resources (accounts, domains, keywords, etc.)
- Fetch single resource by ID
- Field selection (this API REQUIRES `fields=` or responses are empty objects)
- Historical date window (`period_from` / `period_to`)
- Filter / segment scoping (`filter_id` or dynamic POST filter body)
- Pagination (none documented — appears to return full list, with `fields=` as the projection lever)
- CRUD on write resources (keywords, domains, groups, prompts, brands)
- Job polling for async keyword create (`/api/v4/status/keyword_job/{job_id}`)

**None of those table stakes currently exist in a real CLI.** See Ecosystem Intelligence below.

## Ecosystem Intelligence

- **No official AccuRanker CLI**
- **No official SDK** for any language (verified npm + PyPI + GitHub)
- **No community wrappers of any kind** beyond integration-platform components:
  - `@pipedream/accuranker` (Pipedream component, not a usable library)
  - n8n integration (workflow platform, not standalone)
  - Zapier integration
  - Relevance AI integration
- **No MCP server exists** for AccuRanker (verified via GitHub search)
- **No Claude Code plugin / skill exists** for AccuRanker

**Implication:** This CLI has no competitive parity bar to clear — the absorb manifest's "match every feature in existing tools" reduces to "match what AccuRanker's own UI exposes." The differentiator is being the **only** programmatic SEO data extractor for this vendor, built specifically for AI-agent consumption.

## Data Layer

This section is intentionally exhaustive because v2 (the upcoming Postgres mirror service) needs the complete shape.

### Resource hierarchy

```
Account (1)
├── Organization (n) — read-only billing context (3 fields)
├── Brand (n) — LLM/AccuLLM tracking (paywalled in tested plan)
│   └── Prompt (n) — AI search prompts (8 fields, 9-field results array)
└── Group ("Client", 4 fields) — visible inside Domain payload
    ├── Domain (n) — 15 fields + 24-field DomainHistory time-series
    │   ├── Competitor (n) — 12 fields + 24-field CompetitorHistory time-series
    │   ├── Tag (per-domain) + 23-field TagHistory time-series
    │   ├── LandingPage (per-domain) + 24-field LandingPageHistory time-series
    │   └── Keyword (n) — 24 fields per keyword
    │       ├── KeywordRank (32 fields) — daily snapshot
    │       │   ├── search_intent (informational/commercial/transactional/navigational)
    │       │   ├── page_serp_features (featured snippet, AI overview, local pack, etc.)
    │       │   ├── extra_ranks (multiple URLs from same domain)
    │       │   ├── above_the_fold, browser_position_x1/y1/x2/y2 (geometry)
    │       │   ├── ai_share_of_voice, ai_traffic_value, ai_overview_text/urls
    │       │   └── share_of_voice, share_of_voice_percentage, estimated_visits, ctr
    │       ├── KeywordInitialRank (32 fields, same shape) — baseline at add-time
    │       ├── KeywordCompetitorRank (11 fields) — competitor rank for same keyword
    │       ├── SearchVolume (6 fields) + SearchVolumeHistory (2 fields)
    │       ├── AiSearchVolume (4 fields) + AiSearchVolumeHistory (3 fields)
    │       ├── PreferredLandingPage (3 fields)
    │       ├── FullSerp (4 fields with `elements` JSON)
    │       └── PeopleAlso / RelatedSearches / RelatedQuestions arrays
└── DomainLLM (9 fields, AccuLLM mode) + DomainLLMHistory (9 fields) + CompetitorLLM (6 fields)
```

### Read endpoints (14)

| Method | Path | Operation | Notes |
|--------|------|-----------|-------|
| GET    | `/accounts/` | List accounts | Auth context |
| POST   | `/accounts/` | List accounts with filters | |
| GET    | `/accounts/{id}/` | Get one account | |
| GET    | `/brands/` | List brands (LLM) | Paywalled in low tier |
| POST   | `/brands/` | List brands w/ filters | |
| GET    | `/brands/{id}/` | Get brand | |
| GET    | `/brands/{brands_pk}/prompts/` | List prompts | |
| POST   | `/brands/{brands_pk}/prompts/` | List prompts w/ filters | |
| GET    | `/brands/{brands_pk}/prompts/{id}/` | Get prompt | |
| GET    | `/domains/` | List domains | **Most common entry point** |
| POST   | `/domains/` | List domains w/ filters | |
| GET    | `/domains/{id}/` | Get domain (+ history) | `period_from/to` for history |
| GET    | `/domains/{domain_pk}/keywords/` | List keywords | **Highest-cardinality endpoint** |
| POST   | `/domains/{domain_pk}/keywords/` | List keywords w/ filters | |
| GET    | `/domains/{domain_pk}/keywords/{id}/` | Get keyword | |
| GET    | `/domains/{domain_pk}/keywords/{keyword_pk}/ranks/{search_date}/` | Get raw SERP HTML for one rank-date | Returns HTML of SERP snapshot |
| GET    | `/domains/{domain_pk}/landing_pages/` | List landing pages | |
| POST   | `/domains/{domain_pk}/landing_pages/` | List landing pages w/ filters | |
| GET    | `/domains/{domain_pk}/landing_pages/{id}/` | Get landing page | |
| GET    | `/domains/{domain_pk}/tags/` | List tags | |
| POST   | `/domains/{domain_pk}/tags/` | List tags w/ filters | |

### Write endpoints (11)

| Method | Path | Operation | Notes |
|--------|------|-----------|-------|
| GET    | `/overview/group_domains` | List groups + domains | Paywalled in low tier |
| GET    | `/overview/keywords_for_domain/{domain_id}` | List keywords for domain (overview shape) | Paywalled in low tier |
| POST   | `/group/` | Create group | |
| PUT    | `/group/{group_id}` | Update group | |
| DELETE | `/group/{group_id}` | Delete group | |
| POST   | `/domain/` | Create domain | |
| PUT    | `/domain/{domain_id}` | Update domain | |
| DELETE | `/domain/{domain_id}` | Delete domain | |
| POST   | `/keyword/` | Bulk create keywords | Returns `job_id` for async |
| PUT    | `/keyword/` | Bulk update keywords | |
| DELETE | `/keyword/` | Bulk delete keywords | |
| GET    | `/status/keyword_job/{job_id}` | Poll keyword job status | Async progress |
| POST   | `/prompt/` | Create prompt | LLM-tier |
| PUT    | `/prompt/` | Update prompt | |
| DELETE | `/prompt/` | Delete prompt | |
| POST   | `/brand/` | Create brand | LLM-tier |
| PUT    | `/brand/{brand_id}` | Update brand | |
| DELETE | `/brand/{brand_id}` | Delete brand | |

### Query parameter system (the critical bit)

| Param | Where | Behaviour |
|-------|-------|-----------|
| `fields` | query, applies to almost every Read endpoint | **MANDATORY for non-empty responses.** GraphQL-like dot notation. Without it, you get `{}` per row. Drives projection — both list and detail. |
| `period_from` | query, historical endpoints | `YYYY-MM-DD` inclusive lower bound |
| `period_to` | query, historical endpoints | `YYYY-MM-DD` inclusive upper bound. **MAX 100 days between from/to.** |
| `filter_id` | query, Read list endpoints | Apply a saved API filter (segment marked "API filter" in app) |
| Dynamic filters | POST body | Nested `{filters: [...], operator: "and"|"or"}` with 13 comparators (`eq`, `ne`, `gt`, `gte`, `lt`, `lte`, `between`, `contains`, `not_contains`, `starts_with`, `ends_with`, `regex`, `not_regex`, `any`, `all`, `none`, `empty`, `is_null`, `folder_or_subfolder`, `exact_folder`) |

### Filter dimensions (100+ named filterable fields)

Verified from spec — these are the **filter names** the dynamic POST body accepts. Each is a queryable dimension that v2 Postgres mirror should index:

- Identity: `keyword`, `keywords` (array), `multiple_keywords`, `domain`, `subdomain`, `group`, `tags`, `folders`, `starred`, `date_added`
- Rank metrics: `rank`, `rank_change`, `rank_compare`, `base_rank`, `base_rank_change`, `max_rank`, `local_pack_rank`, `local_pack_rank_change`, `page_serp_features_rank`
- Volume / value: `search_volume`, `ai_search_volume`, `avg_cost_per_click`, `competition`, `traffic_value`, `traffic_value_change`, `max_traffic_value`, `organic_traffic`, `organic_traffic_change`, `ai_traffic_value`, `ai_traffic_value_change`, `max_ai_traffic_value`
- Share of voice: `share_of_voice`, `share_of_voice_change`, `max_sov`, `max_ai_sov`
- CTR: `dynamic_ctr`, `dynamic_ctr_change`, `dynamic_ctr_max`
- SERP geometry: `pixels_from_top`, `pixels_from_top_change`, `above_the_fold`
- Intent: `search_intent` (array), `element_type`
- SERP features: `page_serp_features` (array), `page_serp_features_owned`, `has_ai_overview`, `has_extra_ranks`
- Page / URL: `highest_ranking_page`, `highest_ranking_page_match`, `landing_page_title`, `landingpages` (array), `path`, `url_changed`, `is_first_occurrence_of_domain`, `title_contains_keyword`
- Search config: `search_engine`, `search_type`, `country_locale_id`, `location`, `alphabet`
- Competitor: `top_domain` (array)
- Keyword options: `keyword_options` (array: `ignore_featured_snippet`, `ignore_local_results`, `ignore_in_share_of_voice`, `enable_autocorrect`, `ignored_domain`)
- Forecasting: `forecasts`, `has_forecast_target`, `has_forecasts`
- LLM-mode (paywalled): `llm_rank`, `llm_rank_change`, `llm_visibility`, `llm_visibility_change`, `llm_sentiment`, `llm_brand` (array), `llm_brand_mentions`, `llm_brand_count`, `llm_source` (array), `llm_source_count`, `llm_top_brand`, `llm_top_source`, `llm_top_proximity_score`, `llm_web_search_rate`, `llm_country_locale_id`, `llm_search_engine`, `llm_winners`, `llm_losers`, `llm_starred`, `llm_tags`, `llm_folders`, `llm_multiple_prompts`, `llm_date_added`, `prompt`, `prompts`

### Comparator semantics

Each filter declares which comparators it accepts. Numerics get `eq/ne/gt/gte/lt/lte/between/is_null`. Strings get `eq/ne/contains/not_contains/starts_with/ends_with/regex/not_regex` (RE2 syntax — no PCRE lookarounds/backreferences). Arrays get `any/all/none/empty`. Booleans get only `eq`.

### High-gravity entities (sync priority)

For the v2 Postgres mirror, sync priority order:

1. **Keywords** — highest cardinality (typically 10s-100s per domain, 1000s across an account), changes daily.
2. **KeywordRank** — time-series per keyword, refreshed daily. The "fact table".
3. **KeywordCompetitorRank** — time-series per keyword × competitor.
4. **DomainHistory / LandingPageHistory / TagHistory / CompetitorHistory** — dimension-aggregated daily history. Lower volume but high analytical value.
5. **SearchVolumeHistory** — monthly granularity, low volume.
6. **Domains, Competitors, Tags, Landing Pages** — slow-changing dimension tables.
7. **Accounts, Groups, Brands, Prompts** — slowest-changing, top-of-hierarchy.

### Sync cursor strategy (for v2 service consumers)

- **Daily delta:** Run nightly with `period_from = max(local.last_synced_date - 1d, today - 100d)` and `period_to = today`. The 100-day cap forces backfills into 100-day chunks.
- **Backfill:** Iterate 100-day windows backward from `today` until `period_from` < domain's `created_at`.
- **What to track in cursor table (v2 Postgres):** per (domain_id, resource_type) tuple, store `last_synced_at`, `last_period_to`, `last_full_refresh_at`, plus a per-entity `last_modified_at` mirror.
- **No `Last-Modified` / ETag header observed** — relies entirely on date-window pagination + client-side dedup by `(keyword_id, created_at)` for rank entries.

### Rate-limit handling

- 100 req/min = ~1 request every 600ms steady state.
- Max 4 concurrent connections — limits parallel-fetch fan-out.
- The CLI must implement adaptive rate limiting + retry-after on 429.

## Codebase Intelligence

- DeepWiki: no relevant repos exist (verified GitHub search returned 0).
- MCP source code: no AccuRanker MCP exists.
- Best available reference: the embedded ReDoc spec at `https://app.accuranker.com/api/read-docs` and `/api/write-docs` (HTML pages with inlined OpenAPI definitions in `<script id="json-schema">`). Both extracted in this run.
- Read spec format: Swagger 2.0 (host `app.accuranker.com`, basePath `/api/v4`)
- Write spec format: OpenAPI 3.x (no `servers` block; needs enrichment)

## User Vision

Captured from the briefing:

> "Thoroughly document what data we have access to and how we can scope it. Our goal for the next iteration of this CLI is maintaining a comprehensive postgres database that mirrors all the data through iterative sync (bypassing rate limits by only syncing new data, and being able to query historically locally). So we need complete schema for our v2 spec. v1 also needs to anticipate this future direction in its design, most likely by being the tool to facilitate this sync, not the layer initiating it and managing the database, that will be another service.
>
> Token efficiency in output and well-planned tools that are ergonomic for the AI agent is also important.
>
> We want all data and dimensions to be accessible."

**Implications for v1 design:**

1. **CLI is the sync facilitator, not the sync owner.** The CLI exposes thin, composable, deterministic data extraction. Another service holds Postgres and decides when to call us.
2. **Sync-facilitator features must include:**
   - `fetch` commands per resource that emit clean JSON streams with stable field ordering
   - `--period-from / --period-to` flags with automatic 100-day chunking when the requested range exceeds the cap (the CLI walks windows internally and concatenates)
   - `--filter` flags that expose the full dynamic-filter DSL
   - `--fields` flag (with `--fields '*'` resolving to "all known fields per resource") so the caller can choose projection
   - `--cursor-after` flag for incremental sync that maps to a date or timestamp
   - Exit codes that distinguish rate-limit / plan-paywall / not-found / transport / auth
   - **NDJSON output mode** for bulk extraction (one record per line, streamable, append-friendly for the database loader)
   - **Resumable pagination semantics** — even though the API itself does not paginate, the CLI fakes pagination by chunking the date window
3. **Local SQLite store remains** in v1 — useful for ad-hoc queries, dogfood verification, and as the on-disk demo of the schema. It does NOT replace v2's Postgres.
4. **Token efficiency for AI agents:** Default outputs should drop nulls, drop empty arrays, and never include nested base64 / large blob fields without explicit opt-in. `--compact` strips everything but high-gravity fields. `--select` does dot-path projection on top of the raw response. `--no-nulls` filters nulls server-side via `fields=`. The full-SERP HTML endpoint (`/ranks/{search_date}/`) is opt-in only — not part of any default workflow.
5. **All data and dimensions accessible:** No dimension may be hidden behind "minor / not interesting." The dynamic filter system above exposes 100+ filter names; the CLI surfaces all of them with `--list-filters` and accepts any of them through `--filter '<name> <comparator> <value>'`.

## Source Priority

Single-source CLI (AccuRanker only). No multi-source ordering needed.

- Primary: AccuRanker REST API v4 (Read + Write specs combined)
- Spec state: Official, recovered from public ReDoc pages
- Auth: API key (free for users with an AccuRanker plan)

## Product Thesis

- **Name:** `accuranker-pp-cli` (binary), CLI display name "AccuRanker CLI", brand "AccuRanker"
- **Why it should exist:** AccuRanker has no programmatic CLI, no SDK, no MCP, and no Claude skill. Every existing integration is a generic workflow-platform connector (Zapier, n8n, Pipedream, Relevance AI) — useful for one-off triggers, useless for bulk data extraction or for hosting under an AI agent. SEO teams who want to keep their rank data in their own warehouse currently write throwaway Python scripts. This CLI replaces those scripts with a deterministic, token-efficient, agent-shaped binary that can also seed v2's Postgres mirror.
- **Headline:** "The first real CLI for AccuRanker — built to feed your warehouse and your AI agents, not to ship dashboards."
- **Differentiation:** (a) only proper CLI; (b) only agent-shaped output (`--json`, `--select`, `--compact`, NDJSON, typed exit codes); (c) only tool that automatically chunks 100-day windows for arbitrary date ranges; (d) only tool that surfaces the full dynamic-filter DSL on the command line; (e) only tool that ships a sync-cursor abstraction designed for an external Postgres mirror; (f) local SQLite store for offline analytics and dogfood verification.

## Build Priorities

1. **Spec preparation (Phase 2 prep):**
   - Convert Read spec Swagger 2.0 → OpenAPI 3.0
   - Add explicit `servers:` block (`https://app.accuranker.com`) to both specs
   - Preserve `fields`, `period_from`, `period_to`, `filter_id` as per-endpoint query parameters (prevent the generator's global-param filter from hiding them)
   - Add `x-auth-env-vars: [ACCURANKER_API_TOKEN]` to security scheme (canonical env var name)
   - Add `x-mcp` block: `transport: [stdio, http]` (we want remote MCP access for the v2 service), `orchestration: code` (will be many tools), `endpoint_tools: hidden` (suppress redundant raw mirrors)
2. **Priority 0 (data layer):** All resources from the hierarchy above modeled in SQLite for v1 local store. Schema mirrors the field set so v2 Postgres can adopt it 1:1.
3. **Priority 1 (absorbed):** Every Read + Write endpoint as a typed CLI command. Field projection. Filter passthrough. Period flags with auto-chunking. Plan-tier detection (graceful "this endpoint needs a higher AccuRanker plan" with a typed exit code).
4. **Priority 2 (transcendence):**
   - `sync` — incremental cursor-aware sync into local SQLite (proves the v2 pattern)
   - `dump` — NDJSON stream of any resource over any window, auto-chunked, ready to pipe into `psql COPY` or `pg_dump`-style loaders
   - `schema` — print the data shape v2 should mirror (machine-readable, includes fields, filter dimensions, sync hints)
   - `filters list` / `filters describe` — surface every filter dimension and its comparators (so an AI agent can build filter expressions without re-reading docs)
   - `rate` — show current rate-limit budget + reset (read from response headers or a local in-memory bucket)
   - `replay` — re-emit a previous sync's output (useful for v2 ingest dev)
5. **Priority 3 (polish):**
   - Smart `--compact` (drop nulls, drop empty arrays)
   - `--select` over raw response with dot-path semantics that mirror `fields=`
   - `doctor` — verify token, plan tier, rate-limit health, network reachability
   - Strong README with sync-cursor examples and v2-handoff guide
