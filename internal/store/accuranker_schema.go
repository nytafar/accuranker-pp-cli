// Hand-authored extension to the generated store.
//
// This file applies the SQLite migrations described by schema/model.yaml on
// top of whatever schema the generated store already created. The generic
// `resources` table emitted by the press stays in place; the typed tables
// produced here are what `accuranker-pp-cli mirror` writes into and what
// `accuranker-pp-cli push` exports to Postgres.
//
// model.yaml is the single source of truth: both the SQLite migrations and
// the Postgres DDL emitted by `accuranker-pp-cli schema --format postgres-ddl`
// derive from this file. A test in this package verifies the live SQLite
// schema matches schema/model.yaml.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"accuranker-pp-cli/internal/schema"
)

// ApplyAccurankerSchema is the public entry point called from CLI commands
// (mirror, push, dump, etc.) just after Open. Calling it more than once is a
// no-op because every CREATE statement uses IF NOT EXISTS.
//
// model is the loaded schema/model.yaml. Pass schema.LoadDefault() to use
// the default path resolution.
func (s *Store) ApplyAccurankerSchema(ctx context.Context, model *schema.Model) error {
	if model == nil {
		return fmt.Errorf("apply accuranker schema: model is nil")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	statements := SQLiteDDL(model)
	for _, stmt := range statements {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("apply accuranker schema: %s: %w", firstLine(stmt), err)
		}
	}
	return nil
}

// SQLiteDDL emits the full set of CREATE TABLE and CREATE INDEX statements
// for the model. Pure function so the same output drives both the runtime
// migrations and the `schema --format sqlite-ddl` command.
func SQLiteDDL(model *schema.Model) []string {
	stmts := make([]string, 0, len(model.Resources)*2)
	for i := range model.Resources {
		r := &model.Resources[i]
		stmts = append(stmts, sqliteCreateTable(r))
		for _, idx := range r.Indexes {
			stmts = append(stmts, sqliteCreateIndex(r.Name, idx))
		}
	}
	return stmts
}

// PostgresDDL emits the Postgres equivalent. Schema-qualified table names
// are not added here; the caller (push command) sets the schema via
// `SET search_path` or `CREATE SCHEMA … ; CREATE TABLE …`.
func PostgresDDL(model *schema.Model) []string {
	stmts := make([]string, 0, len(model.Resources)*2)
	for i := range model.Resources {
		r := &model.Resources[i]
		stmts = append(stmts, postgresCreateTable(r))
		for _, idx := range r.Indexes {
			stmts = append(stmts, postgresCreateIndex(r.Name, idx))
		}
	}
	return stmts
}

// PostgresComments emits COMMENT ON TABLE / COMMENT ON COLUMN statements from
// model.yaml descriptions. Table comments come from the resource description
// (always present); column comments are emitted only for columns that carry a
// `description:` in model.yaml. Grain and watermark, when set, are appended to
// the table comment so the catalog survives even in a comments-only pipeline.
//
// PATCH(amend-2026-07-07: catalog seeding — spec F5) — consumed by
// `schema --format postgres-ddl --comments`.
func PostgresComments(model *schema.Model) []string {
	stmts := make([]string, 0, len(model.Resources))
	for i := range model.Resources {
		r := &model.Resources[i]
		comment := strings.TrimSpace(r.Description)
		if r.Grain != "" {
			comment += " Grain: " + r.Grain + "."
		}
		if r.Watermark != "" {
			comment += " Watermark column: " + r.Watermark + "."
		}
		if comment != "" {
			stmts = append(stmts, fmt.Sprintf("COMMENT ON TABLE %s IS '%s'", r.Name, escapeSQLString(comment)))
		}
		for _, c := range r.Columns {
			if c.Description == "" {
				continue
			}
			stmts = append(stmts, fmt.Sprintf("COMMENT ON COLUMN %s.%s IS '%s'", r.Name, c.Name, escapeSQLString(c.Description)))
		}
	}
	return stmts
}

func escapeSQLString(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

func sqliteCreateTable(r *schema.Resource) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE IF NOT EXISTS accuranker_%s (\n", r.Name)
	for i, c := range r.Columns {
		fmt.Fprintf(&b, "  %s %s", c.Name, sqliteType(c.Type))
		if c.NotNull {
			b.WriteString(" NOT NULL")
		}
		if c.PrimaryKey {
			b.WriteString(" PRIMARY KEY")
		}
		if def, ok := defaultClause(c, false); ok {
			fmt.Fprintf(&b, " DEFAULT %s", def)
		}
		if i < len(r.Columns)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	if len(r.PrimaryKeyConstraint) > 0 {
		fmt.Fprintf(&b, "  , PRIMARY KEY (%s)\n", strings.Join(r.PrimaryKeyConstraint, ", "))
	}
	b.WriteString(")")
	return b.String()
}

func postgresCreateTable(r *schema.Resource) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE IF NOT EXISTS %s (\n", r.Name)
	for i, c := range r.Columns {
		fmt.Fprintf(&b, "  %s %s", c.Name, postgresType(c.Type))
		if c.NotNull {
			b.WriteString(" NOT NULL")
		}
		if c.PrimaryKey {
			b.WriteString(" PRIMARY KEY")
		}
		if def, ok := defaultClause(c, true); ok {
			fmt.Fprintf(&b, " DEFAULT %s", def)
		}
		if i < len(r.Columns)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	if len(r.PrimaryKeyConstraint) > 0 {
		fmt.Fprintf(&b, "  , PRIMARY KEY (%s)\n", strings.Join(r.PrimaryKeyConstraint, ", "))
	}
	// Add explicit FK constraints (Postgres enforces them; SQLite needs PRAGMA on).
	for _, c := range r.Columns {
		if c.ForeignKey == "" {
			continue
		}
		parts := strings.SplitN(c.ForeignKey, ".", 2)
		if len(parts) == 2 {
			fmt.Fprintf(&b, "  , FOREIGN KEY (%s) REFERENCES %s(%s)\n", c.Name, parts[0], parts[1])
		}
	}
	b.WriteString(")")
	return b.String()
}

func sqliteCreateIndex(table string, idx schema.Index) string {
	return fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s_acc ON accuranker_%s (%s)", idx.Name, table, strings.Join(idx.Columns, ", "))
}

func postgresCreateIndex(table string, idx schema.Index) string {
	return fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (%s)", idx.Name, table, strings.Join(idx.Columns, ", "))
}

// sqliteType maps our canonical type names to SQLite affinities.
func sqliteType(t string) string {
	switch t {
	case "integer", "bigint":
		return "INTEGER"
	case "numeric":
		return "REAL"
	case "text":
		return "TEXT"
	case "timestamp", "date":
		return "TEXT"
	case "boolean":
		return "INTEGER"
	case "json":
		return "TEXT"
	default:
		return "TEXT"
	}
}

// postgresType maps our canonical type names to Postgres types.
func postgresType(t string) string {
	switch t {
	case "integer":
		return "INTEGER"
	case "bigint":
		return "BIGINT"
	case "numeric":
		return "NUMERIC"
	case "text":
		return "TEXT"
	case "timestamp":
		return "TIMESTAMPTZ"
	case "date":
		return "DATE"
	case "boolean":
		return "BOOLEAN"
	case "json":
		return "JSONB"
	default:
		return "TEXT"
	}
}

// defaultClause returns the literal DEFAULT clause for a column (with the
// dialect-specific quoting needed for the destination database). The second
// return is false when the column has no default.
func defaultClause(c schema.Column, postgres bool) (string, bool) {
	if c.DefaultFn != "" {
		switch c.DefaultFn {
		case "now":
			if postgres {
				return "NOW()", true
			}
			return "CURRENT_TIMESTAMP", true
		}
		return c.DefaultFn, true
	}
	if c.Default == nil {
		return "", false
	}
	switch v := c.Default.(type) {
	case bool:
		if postgres {
			if v {
				return "TRUE", true
			}
			return "FALSE", true
		}
		if v {
			return "1", true
		}
		return "0", true
	case int, int64, float64:
		return fmt.Sprintf("%v", v), true
	case string:
		return fmt.Sprintf("'%s'", strings.ReplaceAll(v, "'", "''")), true
	}
	return fmt.Sprintf("%v", c.Default), true
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	return s
}

// SortColumns returns the columns sorted alphabetically (used by tests and by
// `schema --format json` to produce stable output).
func SortColumns(cols []schema.Column) []schema.Column {
	out := make([]schema.Column, len(cols))
	copy(out, cols)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// AccurankerTableNames returns the typed-table names in the live database
// (table-prefix "accuranker_"). Useful for the push and dump commands.
func (s *Store) AccurankerTableNames(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'accuranker_%' ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list accuranker tables: %w", err)
	}
	defer rows.Close()
	names := make([]string, 0)
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("scan table name: %w", err)
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// TableInfo returns the live column list for a table via PRAGMA table_info.
// Used by tests to verify model.yaml has not drifted from the live schema.
type ColumnInfo struct {
	Name     string
	Type     string
	NotNull  bool
	Default  sql.NullString
	IsPKPart bool
}

func (s *Store) TableInfo(ctx context.Context, table string) ([]ColumnInfo, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, fmt.Errorf("PRAGMA table_info(%s): %w", table, err)
	}
	defer rows.Close()
	out := make([]ColumnInfo, 0)
	for rows.Next() {
		var (
			cid     int
			info    ColumnInfo
			notnull int
			pk      int
		)
		if err := rows.Scan(&cid, &info.Name, &info.Type, &notnull, &info.Default, &pk); err != nil {
			return nil, fmt.Errorf("scan table_info row: %w", err)
		}
		info.NotNull = notnull == 1
		info.IsPKPart = pk > 0
		out = append(out, info)
	}
	return out, rows.Err()
}
