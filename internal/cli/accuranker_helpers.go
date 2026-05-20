// Hand-authored: small helpers shared by the AccuRanker-specific commands.
package cli

import (
	"io"
	"os"

	"accuranker-pp-cli/internal/store"
)

// openLocalDB opens (read-write) the local SQLite mirror. Falls back to the
// default path under ~/.local/share/accuranker-pp-cli/mirror.db when path is
// empty.
func openLocalDB(path string) (*store.Store, error) {
	if path == "" {
		path = defaultMirrorDBPath()
	}
	return store.Open(path)
}

// isTTY returns true when the writer is a terminal — used to switch between
// JSON and pretty-printed output when --json is not set explicitly.
func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

// firstNonBlankLine returns the first non-empty line of s. Used to keep
// error messages short when a multi-line SQL statement fails.
func firstNonBlankLine(s string) string {
	for _, line := range splitLines(s) {
		if trimmed := trimWS(line); trimmed != "" {
			return trimmed
		}
	}
	return s
}

func splitLines(s string) []string {
	out := make([]string, 0, 4)
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start <= len(s) {
		out = append(out, s[start:])
	}
	return out
}

func trimWS(s string) string {
	a, b := 0, len(s)
	for a < b && (s[a] == ' ' || s[a] == '\t' || s[a] == '\r') {
		a++
	}
	for b > a && (s[b-1] == ' ' || s[b-1] == '\t' || s[b-1] == '\r') {
		b--
	}
	return s[a:b]
}

// truncateStr is a length-capped string formatter shared by AccuRanker-specific
// commands. Avoids re-declaring the generated `truncate` helper.
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
