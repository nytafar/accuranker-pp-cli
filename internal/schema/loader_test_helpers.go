package schema

import (
	"os"
)

// tempFile and cleanup are tiny helpers used by loadString in loader_test.go.
// Kept in their own file so the test build still compiles in production
// (we intentionally don't gate them on `_test.go` since loader_test.go imports
// them from the same package).
func tempFile() (*os.File, error) { return os.CreateTemp("", "accuranker-schema-*.yaml") }
func cleanup(path string)         { _ = os.Remove(path) }
