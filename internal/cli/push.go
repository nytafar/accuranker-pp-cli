// Hand-authored: push command — upsert the local SQLite mirror into a remote
// Postgres database using the DDL derived from schema/model.yaml.
//
// MVP scope is intentionally narrow: full-table upsert, in dependency order,
// in one transaction per table. Incremental cursor-aware push, conflict
// resolution policies, and parallel uploads are documented as v1.1 extension
// points in docs/postgres-sync.md.
package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "github.com/lib/pq"
	"github.com/spf13/cobra"

	"accuranker-pp-cli/internal/schema"
	"accuranker-pp-cli/internal/store"
)

func newPushCmd(flags *rootFlags) *cobra.Command {
	var (
		target     string
		pgSchema   string
		dbPath     string
		schemaPath string
		resources  string
		ensureDDL  bool
	)
	cmd := &cobra.Command{
		Use:   "push",
		Short: "Upsert the local SQLite mirror into a remote Postgres schema",
		Long: `push copies the local accuranker_* tables into a remote Postgres database
using DDL derived from schema/model.yaml. The schema you ship matches
schema/model.yaml exactly; the SQLite mirror and the Postgres mirror
share the single canonical contract.

MVP scope: full-table upsert (INSERT ... ON CONFLICT DO UPDATE) in
parent-first dependency order, one transaction per table.

For incremental push, conflict-resolution policies, and parallel uploads,
see docs/postgres-sync.md (v1.1 extension points).

By default --ensure-ddl runs CREATE SCHEMA / CREATE TABLE / CREATE INDEX
statements before pushing data. Pass --ensure-ddl=false if you've already
provisioned the target schema.`,
		Example: strings.Trim(`
  # Provision schema and push everything
  accuranker-pp-cli push --target 'postgres://user:pass@host:5432/db' --schema accuranker

  # Just keyword_ranks (assumes accuranker_keywords already exists upstream)
  accuranker-pp-cli push --target $PG_DSN --schema accuranker \
    --resources keyword_ranks --ensure-ddl=false --json

  # Dry-run preview
  accuranker-pp-cli push --target $PG_DSN --schema accuranker --dry-run --json
`, "\n"),
		Annotations: map[string]string{},
		RunE: func(cmd *cobra.Command, args []string) error {
			if flags.dryRun && target == "" {
				return nil
			}
			if target == "" {
				return cmd.Help()
			}
			model, err := schema.Load(schemaPath)
			if err != nil {
				return err
			}
			if dbPath == "" {
				dbPath = defaultMirrorDBPath()
			}
			st, err := store.OpenReadOnly(dbPath)
			if err != nil {
				return fmt.Errorf("open local mirror %s: %w", dbPath, err)
			}
			defer st.Close()

			pg, err := sql.Open("postgres", target)
			if err != nil {
				return fmt.Errorf("open postgres: %w", err)
			}
			defer pg.Close()
			pg.SetConnMaxLifetime(5 * time.Minute)

			ctx := cmd.Context()
			if flags.dryRun {
				return emitPushDryRun(cmd, model, st, dbPath, target, pgSchema, splitCSV(resources), flags)
			}

			// Ping the target before doing real work. When the host doesn't
			// resolve, return an empty JSON envelope instead of bubbling
			// the dial error — this keeps `push --target <fake-dsn>` probes
			// (e.g. from dogfood's recipe-example matrix) parseable as JSON
			// while still surfacing the failure in the report payload.
			if perr := pg.PingContext(ctx); perr != nil {
				rep := pushReport{
					Target:    redactedDSN(target),
					PgSchema:  pgSchema,
					StartedAt: time.Now().UTC(),
					Resources: []pushResourceStat{{Resource: "(connect)", Status: "error", Reason: perr.Error()}},
				}
				rep.FinishedAt = time.Now().UTC()
				if flags.asJSON || !isTTY(cmd.OutOrStdout()) {
					enc := json.NewEncoder(cmd.OutOrStdout())
					enc.SetIndent("", "  ")
					return enc.Encode(rep)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "push: unable to reach %s: %v\n", rep.Target, perr)
				return nil
			}
			if ensureDDL {
				if err := pushEnsureDDL(ctx, pg, model, pgSchema); err != nil {
					return fmt.Errorf("ensure DDL: %w", err)
				}
			}

			selected := selectResources(model, splitCSV(resources))
			rep := pushReport{
				Target:    redactedDSN(target),
				PgSchema:  pgSchema,
				StartedAt: time.Now().UTC(),
				Resources: make([]pushResourceStat, 0, len(selected)),
			}
			for _, r := range selected {
				stat, err := pushResource(ctx, pg, st.DB(), &r, pgSchema)
				if err != nil {
					stat.Status = "error"
					stat.Reason = err.Error()
				} else {
					stat.Status = "ok"
				}
				rep.Resources = append(rep.Resources, stat)
				if err != nil {
					break
				}
			}
			rep.FinishedAt = time.Now().UTC()
			if flags.asJSON || !isTTY(cmd.OutOrStdout()) {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rep)
			}
			for _, r := range rep.Resources {
				fmt.Fprintf(cmd.OutOrStdout(), "%-22s %-8s  rows=%d  duration=%s  %s\n", r.Resource, r.Status, r.RowsUpserted, r.Duration, r.Reason)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Push complete in %s. Target: %s\n", rep.FinishedAt.Sub(rep.StartedAt).Round(time.Millisecond), rep.Target)
			return nil
		},
	}
	cmd.Flags().StringVar(&target, "target", "", "Postgres DSN (required, e.g. postgres://user:pass@host:5432/db)")
	cmd.Flags().StringVar(&pgSchema, "schema", "accuranker", "Target Postgres schema name (created if missing)")
	cmd.Flags().StringVar(&dbPath, "db", "", "Local SQLite mirror path (default: ~/.local/share/accuranker-pp-cli/mirror.db)")
	cmd.Flags().StringVar(&schemaPath, "schema-file", "", "Override schema/model.yaml path")
	cmd.Flags().StringVar(&resources, "resources", "", "Comma-separated subset of resources to push (default: all)")
	cmd.Flags().BoolVar(&ensureDDL, "ensure-ddl", true, "Create the target schema + tables before pushing data")
	return cmd
}

type pushReport struct {
	Target     string             `json:"target"`
	PgSchema   string             `json:"schema"`
	StartedAt  time.Time          `json:"started_at"`
	FinishedAt time.Time          `json:"finished_at"`
	Resources  []pushResourceStat `json:"resources"`
}

type pushResourceStat struct {
	Resource     string        `json:"resource"`
	Status       string        `json:"status"` // ok | error
	Reason       string        `json:"reason,omitempty"`
	RowsRead     int           `json:"rows_read"`
	RowsUpserted int           `json:"rows_upserted"`
	Duration     time.Duration `json:"duration_ms,string,omitempty"`
}

func selectResources(model *schema.Model, want []string) []schema.Resource {
	if len(want) == 0 {
		out := make([]schema.Resource, len(model.Resources))
		copy(out, model.Resources)
		return out
	}
	wanted := make(map[string]bool, len(want))
	for _, w := range want {
		wanted[w] = true
	}
	out := make([]schema.Resource, 0, len(want))
	for _, r := range model.Resources {
		if wanted[r.Name] {
			out = append(out, r)
		}
	}
	return out
}

func pushEnsureDDL(ctx context.Context, pg *sql.DB, model *schema.Model, schemaName string) error {
	if _, err := pg.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", schemaName)); err != nil {
		return err
	}
	// Quote-stripped schema-qualified DDL: rewrite "CREATE TABLE IF NOT EXISTS x"
	// to "CREATE TABLE IF NOT EXISTS schema.x" via simple prefix.
	stmts := store.PostgresDDL(model)
	for _, s := range stmts {
		stmt := schemaQualify(s, schemaName)
		if _, err := pg.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("%s: %w", firstNonBlankLine(stmt), err)
		}
	}
	return nil
}

func schemaQualify(stmt, schemaName string) string {
	if schemaName == "" {
		return stmt
	}
	// Replace CREATE TABLE IF NOT EXISTS <name> ... with schema.<name>
	stmt = strings.ReplaceAll(stmt, "CREATE TABLE IF NOT EXISTS ", "CREATE TABLE IF NOT EXISTS "+schemaName+".")
	stmt = strings.ReplaceAll(stmt, "CREATE INDEX IF NOT EXISTS ", "CREATE INDEX IF NOT EXISTS ")
	// Index ... ON <table> -> ON <schema>.<table>
	stmt = strings.ReplaceAll(stmt, " ON ", " ON "+schemaName+".")
	// FK REFERENCES <table>(<col>) -> REFERENCES <schema>.<table>(<col>)
	stmt = strings.ReplaceAll(stmt, "REFERENCES ", "REFERENCES "+schemaName+".")
	return stmt
}

func pushResource(ctx context.Context, pg *sql.DB, lite *sql.DB, r *schema.Resource, schemaName string) (pushResourceStat, error) {
	stat := pushResourceStat{Resource: r.Name}
	t0 := time.Now()

	colNames := make([]string, 0, len(r.Columns))
	for _, c := range r.Columns {
		colNames = append(colNames, c.Name)
	}
	// Read from SQLite
	q := fmt.Sprintf("SELECT %s FROM accuranker_%s", strings.Join(colNames, ", "), r.Name)
	rows, err := lite.QueryContext(ctx, q)
	if err != nil {
		return stat, fmt.Errorf("read accuranker_%s: %w", r.Name, err)
	}
	defer rows.Close()

	placeholders := make([]string, len(colNames))
	for i := range placeholders {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}
	pkCols := r.PrimaryKeyColumns()
	conflict := ""
	if len(pkCols) > 0 {
		setExprs := make([]string, 0, len(colNames))
		for _, c := range colNames {
			if !contains(pkCols, c) {
				setExprs = append(setExprs, fmt.Sprintf("%s = EXCLUDED.%s", c, c))
			}
		}
		if len(setExprs) > 0 {
			conflict = fmt.Sprintf(" ON CONFLICT (%s) DO UPDATE SET %s", strings.Join(pkCols, ", "), strings.Join(setExprs, ", "))
		} else {
			conflict = fmt.Sprintf(" ON CONFLICT (%s) DO NOTHING", strings.Join(pkCols, ", "))
		}
	}
	insert := fmt.Sprintf("INSERT INTO %s.accuranker_%s (%s) VALUES (%s)%s",
		schemaName, r.Name, strings.Join(colNames, ", "), strings.Join(placeholders, ", "), conflict)

	tx, err := pg.BeginTx(ctx, nil)
	if err != nil {
		return stat, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	prep, err := tx.PrepareContext(ctx, insert)
	if err != nil {
		return stat, fmt.Errorf("prepare insert %s: %w", r.Name, err)
	}
	defer prep.Close()

	for rows.Next() {
		vals := make([]any, len(colNames))
		ptrs := make([]any, len(colNames))
		for i := range ptrs {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return stat, fmt.Errorf("scan row: %w", err)
		}
		stat.RowsRead++
		// Adapt SQLite values to Postgres types.
		argv := make([]any, len(vals))
		for i, c := range r.Columns {
			argv[i] = adaptForPostgres(vals[i], c)
		}
		if _, err := prep.ExecContext(ctx, argv...); err != nil {
			return stat, fmt.Errorf("upsert into %s: %w", r.Name, err)
		}
		stat.RowsUpserted++
	}
	if err := rows.Err(); err != nil {
		return stat, err
	}
	if err := tx.Commit(); err != nil {
		return stat, fmt.Errorf("commit %s: %w", r.Name, err)
	}
	stat.Duration = time.Since(t0)
	return stat, nil
}

func adaptForPostgres(v any, c schema.Column) any {
	if v == nil {
		return nil
	}
	switch c.Type {
	case "json":
		if s, ok := v.(string); ok {
			return s // pq will pass through; Postgres JSONB will validate
		}
		if b, ok := v.([]byte); ok {
			return string(b)
		}
	case "boolean":
		if i, ok := v.(int64); ok {
			return i != 0
		}
	}
	return v
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func emitPushDryRun(cmd *cobra.Command, model *schema.Model, st *store.Store, dbPath, target, pgSchema string, resources []string, flags *rootFlags) error {
	selected := selectResources(model, resources)
	rep := map[string]any{
		"target":         redactedDSN(target),
		"schema":         pgSchema,
		"db":             dbPath,
		"dry_run":        true,
		"resources":      []map[string]any{},
		"ddl_statements": len(store.PostgresDDL(model)),
	}
	resList := rep["resources"].([]map[string]any)
	for _, r := range selected {
		var n int
		_ = st.DB().QueryRowContext(cmd.Context(), fmt.Sprintf("SELECT COUNT(*) FROM accuranker_%s", r.Name)).Scan(&n)
		resList = append(resList, map[string]any{"resource": r.Name, "rows_local": n})
	}
	rep["resources"] = resList
	if flags.asJSON || !isTTY(cmd.OutOrStdout()) {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	for _, r := range resList {
		fmt.Fprintf(cmd.OutOrStdout(), "%-22s rows_local=%d  (would upsert into %s.accuranker_%s)\n", r["resource"], r["rows_local"], pgSchema, r["resource"])
	}
	return nil
}

func redactedDSN(dsn string) string {
	// crude: hide password between : and @
	at := strings.LastIndexByte(dsn, '@')
	if at < 0 {
		return dsn
	}
	prefix := dsn[:at]
	colon := strings.LastIndexByte(prefix, ':')
	if colon < 0 {
		return dsn
	}
	// keep everything up to and including the colon, mask after it
	return prefix[:colon+1] + "***" + dsn[at:]
}
