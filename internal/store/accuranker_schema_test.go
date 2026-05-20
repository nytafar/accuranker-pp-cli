package store

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"accuranker-pp-cli/internal/schema"
)

func testModelPath(t *testing.T) string {
	t.Helper()
	_, this, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(this), "..", "..", "schema", "model.yaml")
}

func openTestStore(t *testing.T) (*Store, *schema.Model) {
	t.Helper()
	dir := t.TempDir()
	db := filepath.Join(dir, "test.db")
	st, err := Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := schema.Load(testModelPath(t))
	if err != nil {
		t.Fatalf("schema.Load: %v", err)
	}
	if err := st.ApplyAccurankerSchema(context.Background(), model); err != nil {
		t.Fatalf("ApplyAccurankerSchema: %v", err)
	}
	return st, model
}

func TestApplyAccurankerSchema_CreatesAllTables(t *testing.T) {
	st, model := openTestStore(t)
	got, err := st.AccurankerTableNames(context.Background())
	if err != nil {
		t.Fatalf("AccurankerTableNames: %v", err)
	}
	if len(got) != len(model.Resources) {
		t.Errorf("got %d accuranker_* tables, want %d", len(got), len(model.Resources))
	}
	expected := make(map[string]bool, len(model.Resources))
	for _, r := range model.Resources {
		expected["accuranker_"+r.Name] = true
	}
	for _, name := range got {
		if !expected[name] {
			t.Errorf("unexpected table %q", name)
		}
		delete(expected, name)
	}
	for name := range expected {
		t.Errorf("missing table %q", name)
	}
}

func TestApplyAccurankerSchema_IsIdempotent(t *testing.T) {
	st, model := openTestStore(t)
	ctx := context.Background()
	if err := st.ApplyAccurankerSchema(ctx, model); err != nil {
		t.Errorf("second ApplyAccurankerSchema: %v", err)
	}
}

func TestSchemaDriftCheck(t *testing.T) {
	st, model := openTestStore(t)
	ctx := context.Background()
	for _, r := range model.Resources {
		live, err := st.TableInfo(ctx, "accuranker_"+r.Name)
		if err != nil {
			t.Errorf("%s: %v", r.Name, err)
			continue
		}
		if len(live) != len(r.Columns) {
			t.Errorf("table %s column count: live=%d, model=%d", r.Name, len(live), len(r.Columns))
		}
		liveCols := make(map[string]bool, len(live))
		for _, c := range live {
			liveCols[c.Name] = true
		}
		for _, c := range r.Columns {
			if !liveCols[c.Name] {
				t.Errorf("table %s: model column %q missing from live schema", r.Name, c.Name)
			}
		}
	}
}

func TestPostgresDDL_Emits(t *testing.T) {
	model, err := schema.Load(testModelPath(t))
	if err != nil {
		t.Fatalf("schema.Load: %v", err)
	}
	stmts := PostgresDDL(model)
	if len(stmts) < len(model.Resources) {
		t.Errorf("PostgresDDL emitted %d stmts, want >= %d", len(stmts), len(model.Resources))
	}
	// spot-check a few invariants
	joined := strings.Join(stmts, "\n")
	if !strings.Contains(joined, "JSONB") {
		t.Error("expected JSONB type to appear in Postgres DDL")
	}
	if !strings.Contains(joined, "TIMESTAMPTZ") {
		t.Error("expected TIMESTAMPTZ type to appear in Postgres DDL")
	}
	if !strings.Contains(joined, "BIGINT") {
		t.Error("expected BIGINT type to appear in Postgres DDL")
	}
	if !strings.Contains(joined, "FOREIGN KEY") {
		t.Error("expected FOREIGN KEY constraints in Postgres DDL")
	}
}
