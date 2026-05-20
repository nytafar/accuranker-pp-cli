# Postgres Sync — v1 contract and v1.1 extension points

## What `push` does today (MVP)

`accuranker-pp-cli push` copies every `accuranker_*` table from the local
SQLite mirror into a remote Postgres schema using DDL derived from
`schema/model.yaml`. The same `model.yaml` drives both:

- the SQLite migrations applied by `mirror` at startup (`internal/store/accuranker_schema.go`)
- the Postgres DDL emitted by `schema --format postgres-ddl` and run by `push --ensure-ddl`

There is one canonical model; SQLite and Postgres mirror it 1:1.

### MVP scope

- Full-table read from SQLite, full-table upsert to Postgres
- Upsert is `INSERT ... ON CONFLICT (<pk>) DO UPDATE SET <non-pk cols>`
- One transaction per table; parent-first dependency order (per `schema/model.yaml`)
- DDL provisioning is opt-out via `--ensure-ddl=false` (default is on)
- Target schema is `--schema accuranker` by default (schema is `CREATE SCHEMA IF NOT EXISTS`-ed first)
- DSN secret is redacted in the JSON report

### MVP non-scope (deliberate)

`push` does NOT do any of the following in v1. They are documented v1.1 work:

- **Cursor-aware incremental push**
- **Conflict-resolution policies**
- **Parallel uploads / streaming COPY**
- **Schema-evolution detection** (drops/renames)
- **Two-phase commit across resources**
- **Soft deletes**
- **RLS or row-level access control**

The MVP exists so the downstream Bun-based MCP service can read the schema
contract and ingest a known-good snapshot. v1.1 adds the cheap-incremental
loop on top.

## Extension points for v1.1

Each extension below names the exact file and function the change would
land in, so the next agent (or human) starts from a real location.

### 1. Incremental cursor-aware push

**Goal:** push only rows changed since the last successful push of that
resource, instead of full-table.

**Where it lives today:** `internal/cli/push.go:pushResource`.

**Cursor state:** add a row to the existing `accuranker_sync_cursor` table
with `resource = "<name>"` and `scope = "push:<schema>:<pg_host>"`. Reuse
the `last_synced_at` column; add `last_pushed_at` if precision matters.

**SELECT change:**

```sql
SELECT ... FROM accuranker_<name>
WHERE synced_at > ?   -- last_pushed_at
```

For tables without `synced_at` (the time-series ones don't need it because
the mirror writes them in their natural date order), filter on
`search_date`, `date`, or `month` instead.

**Upsert change:** no change. The `ON CONFLICT DO UPDATE` already
handles arriving rows. The win is reducing the SELECT and the network
hop, not the write-side semantics.

### 2. Conflict-resolution policies

**Goal:** let the caller pick from `replace` (current default), `merge`,
`first-write-wins`, or `error-on-conflict`.

**Where it lives today:** the `INSERT ... ON CONFLICT ... DO UPDATE SET <non-pk>`
template in `pushResource`.

**Implementation sketch:**

- Add a `--on-conflict` flag with the 4 values above.
- `replace` → current behavior.
- `merge` → `DO UPDATE SET <col> = COALESCE(EXCLUDED.<col>, <table>.<col>)`
  so NULLs from the new row don't overwrite existing non-NULL values.
- `first-write-wins` → `ON CONFLICT DO NOTHING`.
- `error-on-conflict` → omit the `ON CONFLICT` clause entirely; failures
  bubble up.

### 3. Parallel uploads / streaming COPY

**Goal:** reduce wall time for the warehouse loader. For tables with
millions of rows (a year of `keyword_ranks` for ~10k keywords = ~3.6M
rows), per-row `INSERT` is the bottleneck.

**Where it lives today:** `tx.PrepareContext` + per-row `prep.ExecContext`
in `pushResource`.

**Implementation sketch:** swap the prepared insert for `pq.CopyIn` (the
`lib/pq` driver already supports `COPY FROM STDIN`). Per-table parallelism
is fine because each table is independent within a level of the parent-FK
hierarchy.

```go
stmt, _ := tx.Prepare(pq.CopyInSchema(schemaName, "accuranker_"+r.Name, colNames...))
for rows.Next() { ... stmt.Exec(argv...) }
stmt.Exec()
```

Note: `COPY` does not support `ON CONFLICT`. The two patterns combine via
a staging table per resource: `COPY` into `accuranker_<name>_stage`, then
`INSERT INTO accuranker_<name> SELECT * FROM accuranker_<name>_stage ON CONFLICT ...`.
Adds complexity; only do it once row counts justify it.

### 4. Schema-evolution detection

**Goal:** catch schema/model.yaml drift between mirror and push runs, and
fail loudly instead of silently truncating a column.

**Where it lives today:** the `TestSchemaDriftCheck` test in
`internal/store/accuranker_schema_test.go` verifies the live SQLite schema
matches `model.yaml`. We need the same check against the live Postgres
schema before push.

**Implementation sketch:**

```go
// In pushEnsureDDL or a new pushPreflight step:
for _, r := range model.Resources {
  liveCols, err := pgTableInfo(ctx, pg, schemaName, "accuranker_"+r.Name)
  // compare liveCols against r.Columns; fail if model has a column the
  // table lacks (additive); warn if table has a column the model lacks
  // (legacy / abandoned).
}
```

`pg_catalog.pg_attribute` is the source of truth here.

### 5. Soft deletes

The `accuranker_*` upsert path never deletes rows that disappear from
AccuRanker. A v1.1 option `--soft-delete-missing=keywords,landing_pages`
would mark rows from the local mirror that don't exist in the destination
table as deleted, by populating a `deleted_at` column. Requires:

- adding `deleted_at TIMESTAMPTZ NULL` to each resource that opts in
  (declare in `model.yaml` so it lands in both stores)
- a `WHERE NOT EXISTS (SELECT 1 FROM source_table ...)` step after the
  upsert phase

### 6. RLS / per-tenant scoping

If the downstream MCP service is multi-tenant, push needs to stamp a
`tenant_id` column. The cleanest way is to add `tenant_id BIGINT NOT NULL`
to the relevant resources in `model.yaml` and have the caller pass
`--tenant-id <n>` to `push`. The local SQLite mirror would also start
carrying tenant_id from the day it's added; this is a model-level change,
not a push-time hack.

## Operational notes

### Idempotency

A second run of `push` against the same target is safe:

- `--ensure-ddl` uses `IF NOT EXISTS` everywhere.
- The upsert reapplies the same `INSERT ... ON CONFLICT DO UPDATE`, so
  the final state matches the source mirror.

### Failure modes

- **Network partition mid-push**: the per-table transaction rolls back.
  Already-committed tables are not affected. Restart from where it failed
  (the `--resources` flag lets you pick up specific tables).
- **Schema drift**: if `model.yaml` has a column the live target table
  lacks, the `INSERT` fails with a Postgres-side error citing the
  missing column. Fix: re-run with `--ensure-ddl` to add the column, OR
  ALTER TABLE manually.
- **Type mismatch**: `adaptForPostgres` in `push.go` handles JSON columns
  (passes through the JSON text) and booleans (converts SQLite INTEGER
  0/1 to Postgres BOOLEAN). New types added to `model.yaml` may need a
  case there.

### DSN format

`--target` accepts any libpq DSN:

```
postgres://user:pass@host:5432/dbname?sslmode=require
host=db.example.com port=5432 user=loader password=... dbname=warehouse
```

The password is redacted in the JSON report.

### Permissions

The push role needs at minimum:

- `CREATE` on the database (for `CREATE SCHEMA IF NOT EXISTS`)
- `USAGE, CREATE` on the target schema
- `INSERT, UPDATE` on each `accuranker_*` table

If `--ensure-ddl=false`, the role only needs `INSERT, UPDATE` on the
pre-provisioned tables.

## Contract for the downstream Bun-based MCP service

The downstream service reads `schema/model.yaml` directly and:

1. Asserts schema version matches what it built against (`version: 1` today).
2. Asserts the set of resource names is a superset of what it queries.
3. Generates its own typed Bun models from `model.yaml` (so both sides stay in
   lockstep).
4. Reads from the `accuranker_*` tables (table prefix is part of the
   contract; do not strip it).
5. Treats `synced_at` (where present) as the only sync-freshness signal.
   The push timestamp is not stored per row in v1; that's a v1.1 add via
   `accuranker_sync_cursor`.

Breaking the contract requires bumping `version:` in `model.yaml`. The
push command refuses to load a model whose version it doesn't know.
