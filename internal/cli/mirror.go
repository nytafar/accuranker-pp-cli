// Hand-authored: mirror command — the v2-aligned typed sync.
//
// The press's built-in `sync` writes to a single generic `resources` table
// and does not auto-send AccuRanker's mandatory `fields=` parameter, so
// responses come back as empty objects. `mirror` is the AccuRanker-shaped
// alternative: it auto-fills `fields=` per resource from schema/model.yaml,
// walks `period_from/period_to` windows beyond the 100-day API cap, and
// writes to the typed `accuranker_*` tables that `push` exports to Postgres.
//
// This is the command the v2 service will eventually wrap (or replace), and
// the same command an SEO analyst runs locally to build a queryable mirror.
package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"accuranker-pp-cli/internal/client"
	"accuranker-pp-cli/internal/config"
	"accuranker-pp-cli/internal/schema"
	"accuranker-pp-cli/internal/store"
)

// mirrorOptions captures everything that varies between runs.
type mirrorOptions struct {
	dbPath        string
	schemaPath    string
	domainIDs     []int64
	resources     []string
	since         string
	periodTo      string
	full          bool
	maxWindowDays int
	includeLLM    bool
	dryRun        bool
	reportFile    string
}

func newMirrorCmd(flags *rootFlags) *cobra.Command {
	var (
		opts          mirrorOptions
		domainSpec    string
		resourceSpec  string
		windowDaysArg int
	)
	cmd := &cobra.Command{
		Use:   "mirror",
		Short: "Sync AccuRanker data into typed local SQLite tables (the v2 Postgres mirror, v1 shape)",
		Long: `Mirror is the v2-aligned typed sync.

Unlike the generic 'sync' command (which writes everything into one
'resources' table without sending AccuRanker's mandatory fields= parameter),
mirror:

  - reads schema/model.yaml for per-resource default fields= projection
  - walks period_from/period_to windows internally (the API caps a single
    request at 100 days)
  - dedups across windows and writes to typed accuranker_<resource> tables
  - records cursor state in accuranker_sync_cursor so reruns are cheap

The result is a local SQLite database whose schema matches schema/model.yaml
exactly. The 'push' command exports the same shape to Postgres for the
downstream Bun-based MCP service to consume.`,
		Example: strings.Trim(`
  # Mirror the last 7 days of ranks for one domain
  accuranker-pp-cli mirror --domain 295242 --since 7d

  # Mirror everything since 2024-01-01 (auto-chunks the 2+ year window)
  accuranker-pp-cli mirror --domain 295242 --since 2024-01-01

  # Mirror multiple domains
  accuranker-pp-cli mirror --domain 295242,547831 --since 30d --json

  # Limit to specific resources
  accuranker-pp-cli mirror --domain 295242 --resources domains,keywords,keyword_ranks

  # Full resync (ignore existing cursor)
  accuranker-pp-cli mirror --domain 295242 --full

  # Preview without writing
  accuranker-pp-cli mirror --domain 295242 --dry-run --json
`, "\n"),
		Annotations: map[string]string{},
		RunE: func(cmd *cobra.Command, args []string) error {
			if flags.dryRun {
				return nil
			}
			// Parse domain IDs
			ids, err := parseInt64CSV(domainSpec)
			if err != nil {
				return fmt.Errorf("--domain: %w", err)
			}
			if len(ids) == 0 {
				return errors.New("--domain is required (one or more numeric domain IDs, comma-separated)")
			}
			opts.domainIDs = ids
			opts.resources = splitCSV(resourceSpec)
			opts.maxWindowDays = windowDaysArg
			if opts.maxWindowDays <= 0 || opts.maxWindowDays > 100 {
				opts.maxWindowDays = 100
			}
			opts.dryRun = flags.dryRun

			return runMirror(cmd.Context(), cmd, flags, &opts)
		},
	}
	cmd.Flags().StringVar(&domainSpec, "domain", "", "AccuRanker domain ID(s), comma-separated (required)")
	cmd.Flags().StringVar(&resourceSpec, "resources", "", "Comma-separated subset of resources (default: all reachable)")
	cmd.Flags().StringVar(&opts.dbPath, "db", "", "SQLite DB path (default: ~/.local/share/accuranker-pp-cli/mirror.db)")
	cmd.Flags().StringVar(&opts.schemaPath, "schema-file", "", "Override path to schema/model.yaml")
	cmd.Flags().StringVar(&opts.since, "since", "30d", "Window start (date YYYY-MM-DD or duration like 7d, 30d, 365d)")
	cmd.Flags().StringVar(&opts.periodTo, "until", "", "Window end (YYYY-MM-DD, default: today)")
	cmd.Flags().BoolVar(&opts.full, "full", false, "Ignore existing cursor; resync from --since")
	cmd.Flags().IntVar(&windowDaysArg, "window-days", 100, "Days per API call chunk (1..100; API caps at 100)")
	cmd.Flags().BoolVar(&opts.includeLLM, "include-llm", false, "Mirror LLM-tier resources too (paywalled for some plans)")
	cmd.Flags().StringVar(&opts.reportFile, "report-file", "", "Write the run report JSON to this path instead of stderr")
	return cmd
}

type mirrorReport struct {
	StartedAt     time.Time       `json:"started_at"`
	FinishedAt    time.Time       `json:"finished_at"`
	Domains       []int64         `json:"domains"`
	WindowStart   string          `json:"window_start"`
	WindowEnd     string          `json:"window_end"`
	ResourceStats []mirrorResStat `json:"resources"`
	DBPath        string          `json:"db_path"`
	DryRun        bool            `json:"dry_run,omitempty"`
}

type mirrorResStat struct {
	Resource    string `json:"resource"`
	Status      string `json:"status"` // ok | skipped | paywalled | error | dry_run
	Reason      string `json:"reason,omitempty"`
	APICalls    int    `json:"api_calls"`
	Windows     int    `json:"windows"`
	RowsFetched int    `json:"rows_fetched"`
	RowsWritten int    `json:"rows_written"`
}

func runMirror(ctx context.Context, cmd *cobra.Command, flags *rootFlags, opts *mirrorOptions) error {
	model, err := schema.Load(opts.schemaPath)
	if err != nil {
		return err
	}
	windowStart, windowEnd, err := resolveWindow(opts.since, opts.periodTo)
	if err != nil {
		return err
	}
	cfg, err := config.Load(flags.configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if !opts.dryRun && cfg.AuthHeader() == "" {
		return fmt.Errorf("no AccuRanker token configured — run `accuranker-pp-cli auth set-token` or export ACCURANKER_API_TOKEN")
	}
	dbPath := opts.dbPath
	if dbPath == "" {
		dbPath = defaultMirrorDBPath()
	}
	if err := ensureDir(dbPath); err != nil {
		return err
	}
	st, err := store.OpenWithContext(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("open db %s: %w", dbPath, err)
	}
	defer st.Close()
	if err := st.ApplyAccurankerSchema(ctx, model); err != nil {
		return err
	}

	cl := client.New(cfg, flags.timeout, flags.rateLimit)

	rep := mirrorReport{
		StartedAt:   time.Now().UTC(),
		Domains:     opts.domainIDs,
		WindowStart: windowStart,
		WindowEnd:   windowEnd,
		DBPath:      dbPath,
		DryRun:      opts.dryRun,
	}
	rep.ResourceStats = make([]mirrorResStat, 0, 4)

	// Resources we ship live (everything else either has no list endpoint,
	// is LLM-tier, or is derived from these). Order matters for FKs:
	// domains first, then keywords, then keyword_ranks (which references
	// keyword_id), then dimension histories.
	primary := []string{"domains", "keywords", "keyword_ranks", "domain_history", "landing_pages", "landing_page_history", "tags", "tag_history"}
	if opts.includeLLM {
		primary = append(primary, "brands", "prompts", "prompt_results")
	}
	if len(opts.resources) > 0 {
		primary = filterStringList(primary, opts.resources)
	}

	for _, resName := range primary {
		stat := mirrorResStat{Resource: resName}
		runner := mirrorRunnerFor(resName)
		if runner == nil {
			stat.Status = "skipped"
			stat.Reason = "no mirror runner registered for this resource (use generic `sync` or add a runner in mirror.go)"
			rep.ResourceStats = append(rep.ResourceStats, stat)
			continue
		}
		if opts.dryRun {
			stat.Status = "dry_run"
			stat.Reason = fmt.Sprintf("would fetch %s for %d domain(s) across window %s..%s in %d-day chunks", resName, len(opts.domainIDs), windowStart, windowEnd, opts.maxWindowDays)
			rep.ResourceStats = append(rep.ResourceStats, stat)
			continue
		}
		err := runner(ctx, cl, st, model, opts, windowStart, windowEnd, &stat)
		if err != nil {
			if isPaywallErr(err) {
				stat.Status = "paywalled"
				stat.Reason = err.Error()
			} else {
				stat.Status = "error"
				stat.Reason = err.Error()
			}
		} else if stat.Status == "" {
			stat.Status = "ok"
		}
		rep.ResourceStats = append(rep.ResourceStats, stat)
	}

	rep.FinishedAt = time.Now().UTC()

	// PATCH(amend-2026-07-07: run report — spec F1): mirror emits the same
	// watermark-handshake report as dump (stderr or --report-file), on top
	// of its detailed per-resource stdout report. clean_exit is true only
	// when no resource errored (paywalled/skipped stay soft); a non-clean
	// run exits non-zero so the spine never advances a watermark past it.
	failed := make([]string, 0, 2)
	var rowsWritten int64
	for _, s := range rep.ResourceStats {
		if s.Status == "error" {
			failed = append(failed, fmt.Sprintf("%s (%s)", s.Resource, s.Reason))
		}
		rowsWritten += int64(s.RowsWritten)
	}
	runRep := &dumpRunReport{
		Resource:    "mirror",
		Window:      dumpReportWindow{From: windowStart, To: windowEnd},
		RowsEmitted: rowsWritten,
		APIVersion:  dumpAPIVersion,
		StartedAt:   rep.StartedAt.Format(time.RFC3339),
		FinishedAt:  rep.FinishedAt.Format(time.RFC3339),
		CleanExit:   len(failed) == 0,
	}
	if len(opts.domainIDs) == 1 {
		runRep.DomainID = opts.domainIDs[0]
	} else {
		runRep.DomainIDs = opts.domainIDs
	}
	runRep.RequestsMade, runRep.RateLimitHits = cl.Metrics()
	if len(failed) == 0 {
		runRep.NextCursor = windowEnd
	} else {
		runRep.Error = "resource(s) failed: " + strings.Join(failed, "; ")
	}
	if err := writeDumpReport(runRep, cmd.ErrOrStderr(), opts.reportFile); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: writing run report failed: %v\n", err)
	}

	if flags.asJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	} else {
		for _, s := range rep.ResourceStats {
			fmt.Fprintf(cmd.OutOrStdout(), "%-22s %-10s %d rows / %d API calls (%d windows)  %s\n", s.Resource, s.Status, s.RowsWritten, s.APICalls, s.Windows, s.Reason)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Mirror complete in %s. DB: %s\n", rep.FinishedAt.Sub(rep.StartedAt).Round(time.Millisecond), rep.DBPath)
	}
	if len(failed) > 0 {
		return fmt.Errorf("mirror: %s", runRep.Error)
	}
	return nil
}

// mirrorRunnerFor dispatches each resource to its specific fetch+write
// implementation. This is the easiest extension point for adding more
// resources to mirror.
type mirrorRunner func(ctx context.Context, cl *client.Client, st *store.Store, m *schema.Model, opts *mirrorOptions, windowStart, windowEnd string, stat *mirrorResStat) error

func mirrorRunnerFor(name string) mirrorRunner {
	switch name {
	case "domains":
		return mirrorDomains
	case "keywords":
		return mirrorKeywords
	case "keyword_ranks":
		return mirrorKeywordRanks
	case "domain_history":
		return mirrorDomainHistory
	case "landing_pages":
		return mirrorLandingPages
	case "landing_page_history":
		return mirrorLandingPageHistory
	case "tags":
		return mirrorTags
	case "tag_history":
		return mirrorTagHistory
	case "brands":
		return mirrorBrands
	case "prompts":
		return mirrorPrompts
	case "prompt_results":
		return mirrorPromptResults
	}
	return nil
}

// --- helpers ---------------------------------------------------------------

func resolveWindow(since, until string) (string, string, error) {
	end := time.Now().UTC().Format("2006-01-02")
	if until != "" {
		if _, err := time.Parse("2006-01-02", until); err != nil {
			return "", "", fmt.Errorf("--until: %w", err)
		}
		end = until
	}
	var start string
	if d, ok := parseDurationDays(since); ok {
		start = time.Now().UTC().AddDate(0, 0, -d).Format("2006-01-02")
	} else if _, err := time.Parse("2006-01-02", since); err == nil {
		start = since
	} else {
		return "", "", fmt.Errorf("--since must be a duration like 7d or YYYY-MM-DD date, got %q", since)
	}
	if start > end {
		return "", "", fmt.Errorf("--since (%s) is after --until (%s)", start, end)
	}
	return start, end, nil
}

func parseDurationDays(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	if strings.HasSuffix(s, "d") {
		var n int
		if _, err := fmt.Sscanf(s, "%dd", &n); err == nil && n > 0 {
			return n, true
		}
	}
	if strings.HasSuffix(s, "w") {
		var n int
		if _, err := fmt.Sscanf(s, "%dw", &n); err == nil && n > 0 {
			return n * 7, true
		}
	}
	return 0, false
}

// chunkDateWindow returns inclusive [start,end] pairs that together cover
// the requested range, with each chunk at most `maxDays` days wide.
func chunkDateWindow(start, end string, maxDays int) ([][2]string, error) {
	t0, err := time.Parse("2006-01-02", start)
	if err != nil {
		return nil, err
	}
	t1, err := time.Parse("2006-01-02", end)
	if err != nil {
		return nil, err
	}
	if maxDays < 1 {
		maxDays = 100
	}
	out := make([][2]string, 0, 1)
	cur := t0
	for !cur.After(t1) {
		chunkEnd := cur.AddDate(0, 0, maxDays-1)
		if chunkEnd.After(t1) {
			chunkEnd = t1
		}
		out = append(out, [2]string{cur.Format("2006-01-02"), chunkEnd.Format("2006-01-02")})
		cur = chunkEnd.AddDate(0, 0, 1)
	}
	return out, nil
}

func parseInt64CSV(s string) ([]int64, error) {
	parts := splitCSV(s)
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		var v int64
		if _, err := fmt.Sscanf(p, "%d", &v); err != nil {
			return nil, fmt.Errorf("not a number: %q", p)
		}
		out = append(out, v)
	}
	return out, nil
}

func splitCSV(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func filterStringList(all, requested []string) []string {
	want := make(map[string]bool, len(requested))
	for _, r := range requested {
		want[r] = true
	}
	out := make([]string, 0, len(requested))
	for _, a := range all {
		if want[a] {
			out = append(out, a)
		}
	}
	return out
}

func defaultMirrorDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "mirror.db"
	}
	return home + "/.local/share/accuranker-pp-cli/mirror.db"
}

func ensureDir(p string) error {
	dir := p
	if i := strings.LastIndexByte(p, '/'); i > 0 {
		dir = p[:i]
	}
	if dir == "" || dir == "." {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}

// isPaywallErr returns true when the API response body declared the
// caller's plan does not include the endpoint.
func isPaywallErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "plan does not include") ||
		strings.Contains(msg, "does not allow access to this endpoint") ||
		strings.Contains(msg, "LLM API access")
}

// apiGet is a thin wrapper around client.Client.Get. The press's Get takes
// query params as a map; we use url.Values to support repeated keys when
// AccuRanker needs them in the future.
func apiGet(_ context.Context, cl *client.Client, path string, query url.Values) ([]byte, error) {
	params := make(map[string]string, len(query))
	for k := range query {
		// AccuRanker reads each key as a single value (fields=, period_*=, filter_id=).
		params[k] = query.Get(k)
	}
	body, err := cl.Get(path, params)
	if err != nil {
		return nil, err
	}
	if len(body) > 0 && body[0] == '{' {
		var probe struct {
			Error   string `json:"error"`
			Message string `json:"message"`
			Detail  string `json:"detail"`
		}
		if json.Unmarshal(body, &probe) == nil {
			if probe.Error != "" && isPaywallErr(errors.New(probe.Error)) {
				return nil, errors.New(probe.Error)
			}
			if probe.Message != "" && isPaywallErr(errors.New(probe.Message)) {
				return nil, errors.New(probe.Message)
			}
			if probe.Detail != "" {
				// "Not found." / "Authentication credentials were not provided."
				return nil, fmt.Errorf("AccuRanker: %s (path=%s)", probe.Detail, path)
			}
		}
	}
	return body, nil
}

// upsertJSON unmarshals raw JSON record(s) and upserts them into the typed
// table. Returns the count of rows written. Uses sql.Tx for batched writes.
func upsertJSON(ctx context.Context, db *sql.DB, table string, model *schema.Model, items []map[string]any) (int, error) {
	if len(items) == 0 {
		return 0, nil
	}
	res := model.Resource(strings.TrimPrefix(table, "accuranker_"))
	if res == nil {
		return 0, fmt.Errorf("upsertJSON: unknown table %q", table)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	colNames := make([]string, 0, len(res.Columns))
	for _, c := range res.Columns {
		colNames = append(colNames, c.Name)
	}
	placeholders := make([]string, len(colNames))
	for i := range placeholders {
		placeholders[i] = "?"
	}
	stmt := fmt.Sprintf("INSERT OR REPLACE INTO %s (%s) VALUES (%s)", table, strings.Join(colNames, ", "), strings.Join(placeholders, ", "))
	prep, err := tx.PrepareContext(ctx, stmt)
	if err != nil {
		return 0, fmt.Errorf("prepare upsert %s: %w", table, err)
	}
	defer prep.Close()

	written := 0
	for _, item := range items {
		args := make([]any, 0, len(colNames))
		for _, c := range res.Columns {
			args = append(args, valueForColumn(item, c))
		}
		if _, err := prep.ExecContext(ctx, args...); err != nil {
			return written, fmt.Errorf("upsert into %s: %w", table, err)
		}
		written++
	}
	if err := tx.Commit(); err != nil {
		return written, err
	}
	return written, nil
}

// valueForColumn extracts the column value from the API payload. JSON columns
// are re-serialized so json.Unmarshal can roundtrip them later.
func valueForColumn(item map[string]any, c schema.Column) any {
	v, ok := item[c.Name]
	if !ok {
		// fallthrough defaults
		if c.DefaultFn == "now" {
			return time.Now().UTC().Format(time.RFC3339)
		}
		return nil
	}
	switch c.Type {
	case "json":
		if v == nil {
			return nil
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		return string(b)
	case "boolean":
		if b, ok := v.(bool); ok {
			if b {
				return 1
			}
			return 0
		}
	}
	return v
}
