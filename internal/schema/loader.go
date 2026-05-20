// Package schema loads and validates the canonical AccuRanker data model
// from schema/model.yaml. This package is the single source of truth that
// drives:
//
//   - The SQLite migrations applied at CLI startup (internal/store/accuranker_schema.go)
//   - The Postgres DDL emitted by `accuranker-pp-cli schema --format postgres-ddl`
//   - The default `fields=` query-parameter value used by `mirror` and `dump`
//   - The filter-dimension catalog surfaced by `filters list` / `filters describe`
//
// The downstream Bun-based MCP service reads schema/model.yaml directly; this
// package is its Go counterpart.
package schema

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// Model is the parsed schema/model.yaml document.
type Model struct {
	Version          int               `yaml:"version"`
	SchemaName       string            `yaml:"schema_name"`
	Resources        []Resource        `yaml:"resources"`
	FilterDimensions []FilterDimension `yaml:"filter_dimensions"`

	byResource map[string]*Resource
	byFilter   map[string]*FilterDimension
}

// Resource describes one typed table that mirrors an AccuRanker API resource.
type Resource struct {
	Name                 string   `yaml:"name"`
	Description          string   `yaml:"description"`
	SourceEndpoint       string   `yaml:"source_endpoint"`
	CursorField          string   `yaml:"cursor_field"`
	PrimaryKey           []string `yaml:"primary_key"`
	PrimaryKeyConstraint []string `yaml:"primary_key_constraint"`
	DefaultFields        string   `yaml:"default_fields"`
	Columns              []Column `yaml:"columns"`
	Indexes              []Index  `yaml:"indexes"`
}

// Column is one typed column in a Resource's table.
type Column struct {
	Name       string `yaml:"name"`
	Type       string `yaml:"type"`
	NotNull    bool   `yaml:"not_null"`
	PrimaryKey bool   `yaml:"primary_key"`
	ForeignKey string `yaml:"foreign_key"`
	Indexed    bool   `yaml:"indexed"`
	Default    any    `yaml:"default"`
	DefaultFn  string `yaml:"default_fn"`
}

// Index is a secondary index on a Resource.
type Index struct {
	Name    string   `yaml:"name"`
	Columns []string `yaml:"columns"`
}

// FilterDimension is one entry in AccuRanker's 100+ dynamic-filter catalog.
type FilterDimension struct {
	Name                string   `yaml:"name"`
	Class               string   `yaml:"class"`
	AcceptedComparators []string `yaml:"accepted_comparators"`
	ValueSet            []string `yaml:"value_set"`
	LLM                 bool     `yaml:"llm"`
}

// Comparator classes (used by ComparatorsFor when AcceptedComparators is empty).
var (
	NumericComparators = []string{"eq", "ne", "gt", "gte", "lt", "lte", "between", "is_null"}
	StringComparators  = []string{"eq", "ne", "contains", "not_contains", "starts_with", "ends_with", "regex", "not_regex"}
	ArrayComparators   = []string{"any", "all", "none", "empty"}
	BooleanComparators = []string{"eq"}
	DateComparators    = []string{"eq", "ne", "gt", "gte", "lt", "lte", "between", "is_null"}
	FolderComparators  = []string{"folder_or_subfolder", "exact_folder"}
)

// Load reads model.yaml. Path resolution order:
//  1. explicit path argument (if non-empty)
//  2. $ACCURANKER_SCHEMA_PATH env var
//  3. <executable directory>/schema/model.yaml
//  4. ./schema/model.yaml (current working dir)
func Load(path string) (*Model, error) {
	resolved, err := resolvePath(path)
	if err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, fmt.Errorf("schema: reading %s: %w", resolved, err)
	}
	var m Model
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("schema: parsing %s: %w", resolved, err)
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("schema: validating %s: %w", resolved, err)
	}
	m.buildIndexes()
	return &m, nil
}

// LoadDefault is Load("") — uses path resolution above.
func LoadDefault() (*Model, error) {
	return Load("")
}

func resolvePath(explicit string) (string, error) {
	candidates := make([]string, 0, 4)
	if explicit != "" {
		candidates = append(candidates, explicit)
	}
	if envPath := os.Getenv("ACCURANKER_SCHEMA_PATH"); envPath != "" {
		candidates = append(candidates, envPath)
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "schema", "model.yaml"))
	}
	candidates = append(candidates, filepath.Join("schema", "model.yaml"))
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("model.yaml not found; tried: %v (set $ACCURANKER_SCHEMA_PATH or place schema/model.yaml alongside the binary)", candidates)
}

func (m *Model) validate() error {
	resourceNames := make(map[string]bool, len(m.Resources))
	resourceColumns := make(map[string]map[string]bool, len(m.Resources))

	for i := range m.Resources {
		r := &m.Resources[i]
		if r.Name == "" {
			return fmt.Errorf("resource[%d] has empty name", i)
		}
		if resourceNames[r.Name] {
			return fmt.Errorf("duplicate resource %q", r.Name)
		}
		resourceNames[r.Name] = true

		if len(r.PrimaryKey) == 0 && len(r.PrimaryKeyConstraint) == 0 {
			return fmt.Errorf("resource %q has no primary_key or primary_key_constraint", r.Name)
		}

		cols := make(map[string]bool, len(r.Columns))
		for j := range r.Columns {
			c := &r.Columns[j]
			if c.Name == "" {
				return fmt.Errorf("resource %q column[%d] has empty name", r.Name, j)
			}
			if cols[c.Name] {
				return fmt.Errorf("resource %q has duplicate column %q", r.Name, c.Name)
			}
			cols[c.Name] = true
		}
		resourceColumns[r.Name] = cols
	}

	// Pass 2: validate foreign keys after all resources are indexed.
	for _, r := range m.Resources {
		for _, c := range r.Columns {
			if c.ForeignKey == "" {
				continue
			}
			parts := splitFK(c.ForeignKey)
			if len(parts) != 2 {
				return fmt.Errorf("resource %q column %q: foreign_key %q must be \"table.column\"", r.Name, c.Name, c.ForeignKey)
			}
			if !resourceNames[parts[0]] {
				return fmt.Errorf("resource %q column %q: foreign_key %q references unknown table %q", r.Name, c.Name, c.ForeignKey, parts[0])
			}
			if !resourceColumns[parts[0]][parts[1]] {
				return fmt.Errorf("resource %q column %q: foreign_key %q references unknown column %s.%s", r.Name, c.Name, c.ForeignKey, parts[0], parts[1])
			}
		}
	}

	// Filter dimensions: enforce unique names.
	filterNames := make(map[string]bool, len(m.FilterDimensions))
	for i, f := range m.FilterDimensions {
		if f.Name == "" {
			return fmt.Errorf("filter_dimension[%d] has empty name", i)
		}
		if filterNames[f.Name] {
			return fmt.Errorf("duplicate filter dimension %q", f.Name)
		}
		filterNames[f.Name] = true
		switch f.Class {
		case "numeric", "string", "array", "boolean", "date", "folder":
		default:
			return fmt.Errorf("filter_dimension %q has unknown class %q", f.Name, f.Class)
		}
	}

	return nil
}

func (m *Model) buildIndexes() {
	m.byResource = make(map[string]*Resource, len(m.Resources))
	for i := range m.Resources {
		m.byResource[m.Resources[i].Name] = &m.Resources[i]
	}
	m.byFilter = make(map[string]*FilterDimension, len(m.FilterDimensions))
	for i := range m.FilterDimensions {
		m.byFilter[m.FilterDimensions[i].Name] = &m.FilterDimensions[i]
	}
}

// Resource returns the named resource, or nil if not found.
func (m *Model) Resource(name string) *Resource {
	if m.byResource == nil {
		return nil
	}
	return m.byResource[name]
}

// ResourceNames returns all resource names in declaration order.
func (m *Model) ResourceNames() []string {
	out := make([]string, 0, len(m.Resources))
	for _, r := range m.Resources {
		out = append(out, r.Name)
	}
	return out
}

// DefaultFields returns the default fields= value for the named resource,
// or empty string if the resource is not found / has no defaults configured.
func (m *Model) DefaultFields(resource string) string {
	if r := m.Resource(resource); r != nil {
		return r.DefaultFields
	}
	return ""
}

// ComparatorsFor returns the legal comparators for a filter dimension.
// If the dimension has an AcceptedComparators override that wins; otherwise
// the class default is used.
func (m *Model) ComparatorsFor(name string) []string {
	fd := m.FilterByName(name)
	if fd == nil {
		return nil
	}
	if len(fd.AcceptedComparators) > 0 {
		out := make([]string, len(fd.AcceptedComparators))
		copy(out, fd.AcceptedComparators)
		return out
	}
	switch fd.Class {
	case "numeric":
		return copySlice(NumericComparators)
	case "string":
		return copySlice(StringComparators)
	case "array":
		return copySlice(ArrayComparators)
	case "boolean":
		return copySlice(BooleanComparators)
	case "date":
		return copySlice(DateComparators)
	case "folder":
		return copySlice(FolderComparators)
	}
	return nil
}

// FilterByName returns the named filter dimension, or nil.
func (m *Model) FilterByName(name string) *FilterDimension {
	if m.byFilter == nil {
		return nil
	}
	return m.byFilter[name]
}

// FilterNames returns all filter dimension names in alphabetical order.
func (m *Model) FilterNames() []string {
	out := make([]string, 0, len(m.FilterDimensions))
	for _, f := range m.FilterDimensions {
		out = append(out, f.Name)
	}
	sort.Strings(out)
	return out
}

// PrimaryKeyColumns returns the column names that compose the resource's
// primary key (either PrimaryKey or PrimaryKeyConstraint, in that priority).
func (r *Resource) PrimaryKeyColumns() []string {
	if len(r.PrimaryKey) > 0 {
		return copySlice(r.PrimaryKey)
	}
	if len(r.PrimaryKeyConstraint) > 0 {
		return copySlice(r.PrimaryKeyConstraint)
	}
	return nil
}

func splitFK(s string) []string {
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return []string{s[:i], s[i+1:]}
		}
	}
	return []string{s}
}

func copySlice(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	return out
}
