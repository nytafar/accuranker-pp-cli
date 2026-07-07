// Tests for the COMMENT ON emitter (spec F5, catalog seeding).
package store

import (
	"strings"
	"testing"

	"accuranker-pp-cli/internal/schema"
)

func TestPostgresComments(t *testing.T) {
	model := &schema.Model{
		SchemaName: "accuranker",
		Resources: []schema.Resource{
			{
				Name:        "keyword_ranks",
				Description: "Daily rank snapshot per keyword.",
				Grain:       "one row per keyword per search_date",
				Watermark:   "search_date",
				Columns: []schema.Column{
					{Name: "rank", Type: "integer", Description: "Organic position; it's null when not ranking"},
					{Name: "undocumented", Type: "text"},
				},
			},
			{
				Name:        "bare",
				Description: "",
				Columns:     []schema.Column{{Name: "x", Type: "text"}},
			},
		},
	}
	stmts := PostgresComments(model)
	joined := strings.Join(stmts, "\n")

	if !strings.Contains(joined, "COMMENT ON TABLE keyword_ranks IS 'Daily rank snapshot per keyword. Grain: one row per keyword per search_date. Watermark column: search_date.'") {
		t.Errorf("table comment missing grain/watermark:\n%s", joined)
	}
	// Single quotes must be SQL-escaped.
	if !strings.Contains(joined, "it''s null") {
		t.Errorf("column comment not SQL-escaped:\n%s", joined)
	}
	if strings.Contains(joined, "undocumented") {
		t.Errorf("columns without description must not emit comments:\n%s", joined)
	}
	if strings.Contains(joined, "COMMENT ON TABLE bare") {
		t.Errorf("resources without any description must not emit a table comment:\n%s", joined)
	}
}
