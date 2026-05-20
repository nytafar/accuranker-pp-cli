package schema

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testdataModelPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// loader_test.go is at <repo>/internal/schema/loader_test.go
	// model.yaml is at <repo>/schema/model.yaml
	repo := filepath.Join(filepath.Dir(thisFile), "..", "..")
	p := filepath.Join(repo, "schema", "model.yaml")
	return p
}

func TestLoad_FixtureLoadsAllResources(t *testing.T) {
	m, err := Load(testdataModelPath(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := len(m.Resources), 21; got != want {
		t.Errorf("len(Resources) = %d, want %d", got, want)
	}
	if got, want := len(m.FilterDimensions), 96; got != want {
		t.Errorf("len(FilterDimensions) = %d, want %d", got, want)
	}
	if m.SchemaName != "accuranker" {
		t.Errorf("SchemaName = %q, want %q", m.SchemaName, "accuranker")
	}
	if m.Version != 1 {
		t.Errorf("Version = %d, want %d", m.Version, 1)
	}
}

func TestResource_LookupAndDefaults(t *testing.T) {
	m, err := Load(testdataModelPath(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := m.Resource("domains")
	if d == nil {
		t.Fatal("Resource(\"domains\") returned nil")
	}
	if !strings.Contains(d.DefaultFields, "id,domain") {
		t.Errorf("domains.DefaultFields = %q, want to contain \"id,domain\"", d.DefaultFields)
	}
	if d.CursorField != "last_scraped" {
		t.Errorf("domains.CursorField = %q, want \"last_scraped\"", d.CursorField)
	}
	if got := m.DefaultFields("nonexistent"); got != "" {
		t.Errorf("DefaultFields(unknown) = %q, want empty", got)
	}
}

func TestComparatorsFor_NumericDefault(t *testing.T) {
	m, err := Load(testdataModelPath(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := m.ComparatorsFor("rank")
	want := NumericComparators
	if len(got) != len(want) {
		t.Fatalf("ComparatorsFor(rank) len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ComparatorsFor(rank)[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestComparatorsFor_AcceptedOverride(t *testing.T) {
	m, err := Load(testdataModelPath(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := m.ComparatorsFor("country_locale_id")
	if len(got) != 1 || got[0] != "eq" {
		t.Errorf("ComparatorsFor(country_locale_id) = %v, want [eq]", got)
	}
}

func TestFilterByName_LLMTagging(t *testing.T) {
	m, err := Load(testdataModelPath(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if fd := m.FilterByName("llm_rank"); fd == nil || !fd.LLM {
		t.Errorf("FilterByName(llm_rank) LLM = %v (full=%+v), want true", fd != nil && fd.LLM, fd)
	}
	if fd := m.FilterByName("rank"); fd == nil || fd.LLM {
		t.Errorf("FilterByName(rank) LLM = %v, want false", fd != nil && fd.LLM)
	}
}

func TestFilterNames_Sorted(t *testing.T) {
	m, err := Load(testdataModelPath(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	names := m.FilterNames()
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Errorf("FilterNames() not sorted: %q > %q at i=%d", names[i-1], names[i], i)
		}
	}
}

func TestResourcePrimaryKeyColumns(t *testing.T) {
	m, err := Load(testdataModelPath(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// keyword_ranks uses primary_key_constraint, not primary_key
	kr := m.Resource("keyword_ranks")
	if kr == nil {
		t.Fatal("keyword_ranks missing")
	}
	pk := kr.PrimaryKeyColumns()
	if len(pk) < 2 {
		t.Errorf("keyword_ranks PK = %v, want composite key", pk)
	}
}

func TestValidate_RejectsDuplicateResource(t *testing.T) {
	bad := `
version: 1
schema_name: test
resources:
  - name: a
    primary_key: [id]
    columns:
      - { name: id, type: bigint, not_null: true, primary_key: true }
  - name: a
    primary_key: [id]
    columns:
      - { name: id, type: bigint, not_null: true, primary_key: true }
`
	if _, err := loadString(bad); err == nil {
		t.Error("expected duplicate-resource error")
	}
}

func TestValidate_RejectsUnknownFK(t *testing.T) {
	bad := `
version: 1
schema_name: test
resources:
  - name: a
    primary_key: [id]
    columns:
      - { name: id, type: bigint, not_null: true, primary_key: true }
      - { name: parent_id, type: bigint, foreign_key: "nonexistent.id" }
`
	if _, err := loadString(bad); err == nil {
		t.Error("expected unknown-FK error")
	}
}

// loadString is an inline helper used only in tests to feed YAML strings
// through the same Load path (validation + index build).
func loadString(yamlStr string) (*Model, error) {
	tmp := writeTemp(yamlStr)
	defer cleanup(tmp)
	return Load(tmp)
}

func writeTemp(content string) string {
	f, err := tempFile()
	if err != nil {
		panic(err)
	}
	if _, err := f.WriteString(content); err != nil {
		panic(err)
	}
	if err := f.Close(); err != nil {
		panic(err)
	}
	return f.Name()
}
