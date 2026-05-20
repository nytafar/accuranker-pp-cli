// Hand-authored: schema command.
//
// Emits the canonical AccuRanker data model from schema/model.yaml in one of
// three forms: JSON (machine-readable), Postgres DDL (for the v2 mirror
// service), or SQLite DDL (matches what `mirror` writes locally).
package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"accuranker-pp-cli/internal/schema"
	"accuranker-pp-cli/internal/store"
)

func newSchemaCmd(flags *rootFlags) *cobra.Command {
	var (
		resourceName string
		format       string
		schemaPath   string
	)
	cmd := &cobra.Command{
		Use:   "schema",
		Short: "Print the canonical data model (drives the v2 Postgres mirror)",
		Long: `Emit the canonical AccuRanker data model as JSON or DDL.

The model lives in schema/model.yaml in this repo and is the single source
of truth for both the local SQLite store and the remote Postgres schema
the v2 mirror service maintains.

Use --format=postgres-ddl to print the exact CREATE TABLE / CREATE INDEX
statements the 'push' command will run against your Postgres instance.`,
		Example: strings.Trim(`
  # Print the full model as JSON (default)
  accuranker-pp-cli schema

  # Show one resource only
  accuranker-pp-cli schema --resource keyword_ranks

  # Emit Postgres DDL ready for `+"`psql -f`"+`
  accuranker-pp-cli schema --format postgres-ddl > accuranker.sql

  # Emit SQLite DDL (what the 'mirror' command creates locally)
  accuranker-pp-cli schema --format sqlite-ddl

  # Pipe filter dimensions into another tool
  accuranker-pp-cli schema --format json --resource '' | jq '.filter_dimensions[] | .name'
`, "\n"),
		Annotations: map[string]string{
			"mcp:read-only": "true",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			model, err := schema.Load(schemaPath)
			if err != nil {
				return err
			}

			switch format {
			case "", "json":
				return emitSchemaJSON(cmd, model, resourceName, flags)
			case "postgres-ddl", "postgres":
				return emitSchemaDDL(cmd, store.PostgresDDL(model), resourceName, model)
			case "sqlite-ddl", "sqlite":
				return emitSchemaDDL(cmd, store.SQLiteDDL(model), resourceName, model)
			default:
				return fmt.Errorf("unknown --format %q (want json, postgres-ddl, or sqlite-ddl)", format)
			}
		},
	}
	cmd.Flags().StringVar(&resourceName, "resource", "", "Filter to one resource by name (e.g. keyword_ranks)")
	cmd.Flags().StringVar(&format, "format", "json", "Output format: json | postgres-ddl | sqlite-ddl")
	cmd.Flags().StringVar(&schemaPath, "schema-file", "", "Override path to schema/model.yaml (defaults to $ACCURANKER_SCHEMA_PATH or schema/model.yaml next to the binary)")
	return cmd
}

func emitSchemaJSON(cmd *cobra.Command, model *schema.Model, resourceName string, flags *rootFlags) error {
	if resourceName != "" {
		r := model.Resource(resourceName)
		if r == nil {
			return fmt.Errorf("unknown resource %q (try `schema | jq '.resources[].name'`)", resourceName)
		}
		return writeJSON(cmd, r, flags)
	}
	// Wrap in a header that downstream consumers can version-check.
	out := map[string]any{
		"version":           model.Version,
		"schema_name":       model.SchemaName,
		"resources":         model.Resources,
		"filter_dimensions": model.FilterDimensions,
		"resource_count":    len(model.Resources),
		"filter_count":      len(model.FilterDimensions),
	}
	return writeJSON(cmd, out, flags)
}

func emitSchemaDDL(cmd *cobra.Command, stmts []string, resourceName string, model *schema.Model) error {
	if resourceName != "" {
		r := model.Resource(resourceName)
		if r == nil {
			return fmt.Errorf("unknown resource %q", resourceName)
		}
		// Filter DDL to statements that name this resource.
		filtered := make([]string, 0, 2)
		for _, s := range stmts {
			if strings.Contains(s, r.Name) {
				filtered = append(filtered, s)
			}
		}
		stmts = filtered
	}
	for _, s := range stmts {
		fmt.Fprintln(cmd.OutOrStdout(), s+";")
		fmt.Fprintln(cmd.OutOrStdout())
	}
	return nil
}

// writeJSON marshals v as JSON with the indent the user's --json/--compact
// flags expect. Stays compatible with the press's helper conventions.
func writeJSON(cmd *cobra.Command, v any, _ *rootFlags) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
