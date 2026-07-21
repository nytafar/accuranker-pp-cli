// Hand-authored: dump command.
//
// Streams NDJSON for every warehouse-facing resource in schema/model.yaml.
// Windowed resources walk the 100-day chunks AccuRanker imposes and dedup by
// natural key; snapshot resources emit the current dimension state. One record
// per line; ready for `psql COPY ... FROM STDIN` or any other line-oriented
// loader.
//
// PATCH(amend-2026-07-07: warehouse ingest — spec F1/F2/F3):
//   - every run emits exactly one machine-readable run report (stderr by
//     default, or --report-file) carrying rows_emitted, requests_made,
//     rate_limit_hits, clean_exit and next_cursor. stdout stays pure NDJSON.
//     clean_exit=true + exit 0 only when every chunk completed for every
//     domain; any partial failure exits non-zero with next_cursor = the last
//     chunk boundary that completed for ALL domains. The sync spine advances
//     its watermark from next_cursor, never from the requested window.
//   - the dump surface is uniform across resource families (spec F2).
//   - --envelope wraps each record in the lossless raw-landing envelope
//     {source, endpoint, params_hash, api_version, fetched_at, payload}
//     (spec F3).
//
// PATCH(amend-2026-07-21: dump completeness — spec F9/F10/F11):
//   - F9: every remaining model.yaml warehouse resource (except the internal
//     sync_cursor) is reachable via `dump <resource> --envelope` under the same
//     contract — full-serp, search-volume-history, ai-search-volume-history,
//     competitor-history, landing-page-history, tag-history, people-also-ask,
//     and the GLOBAL resources domains/accounts/groups (no --domain). `dump all`
//     sweeps them all through one code path.
//   - F10: the LLM/AccuLLM tier (brands, prompts, prompt-results) is wired but
//     off by default — excluded from the `dump all` sweep unless --include-llm,
//     and it degrades to exit 45 (skipped_unentitled inside a sweep) when the
//     account lacks the plan tier. tag-history rides the same graceful gate.
//   - F11: `dump keyword-ranks` emits the is_initial=true baseline row
//     (KeywordInitialRank) and flags every row's is_initial honestly;
//     `dump <windowed> --probe-earliest` binary-searches the earliest date with
//     data per domain and reports it as earliest_available in the F1 report.
package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"accuranker-pp-cli/internal/client"
	"accuranker-pp-cli/internal/config"
	"accuranker-pp-cli/internal/schema"
)

const dumpAPIVersion = "v4"

// probeEarliestFloor is the lower bound for the --probe-earliest binary search.
// It is intentionally older than any AccuRanker plan's retention window so the
// search always brackets the real retention floor from below.
const probeEarliestFloor = "2015-01-01"

// dumpMeta classifies one dump resource.
//
//   - windowed: requires --from; internal 100-day chunking + cross-chunk dedup.
//   - global:   dumps the whole set with no --domain (domains, accounts, ...).
//   - gated:    a plan/tier restriction degrades to exit 45 instead of a hard
//     failure (LLM tier + tag-history), and is recorded skipped_unentitled in a
//     multi-resource sweep.
//   - llm:      part of the AccuLLM tier — excluded from a `dump all` sweep
//     unless --include-llm.
type dumpMeta struct {
	windowed bool
	global   bool
	gated    bool
	llm      bool
}

// dumpResources is the single registry every dump surface reads from: the
// command's resource validation, the help text, `dump all`, the dedup keys and
// the report classification. Adding a resource here plus a case in dumpFetch is
// all it takes to make it landable by the sync spine (which shells out to
// `dump <resource> --envelope --agent --report-file`).
var dumpResources = map[string]dumpMeta{
	// --- windowed (require --from) ---
	"keyword-ranks":    {windowed: true},
	"domain-history":   {windowed: true},
	"competitor-ranks": {windowed: true},
	// F9 windowed additions
	"full-serp":                {windowed: true},
	"search-volume-history":    {windowed: true},
	"ai-search-volume-history": {windowed: true},
	"competitor-history":       {windowed: true},
	"landing-page-history":     {windowed: true},
	"tag-history":              {windowed: true, gated: true}, // tier-restricted on some accounts

	// --- snapshot (current state) ---
	"keywords":      {},
	"competitors":   {},
	"landing-pages": {},
	"tags":          {},
	// F9 snapshot additions
	"people-also-ask": {},
	"domains":         {global: true},
	"accounts":        {global: true},
	"groups":          {global: true},

	// --- F10 LLM/AccuLLM tier (off by default, gated) ---
	"brands":         {global: true, gated: true, llm: true},
	"prompts":        {global: true, gated: true, llm: true},
	"prompt-results": {windowed: true, global: true, gated: true, llm: true},
}

// dumpResourceNames renders the supported resources grouped for help/errors.
func dumpResourceNames() string {
	var windowed, snapshot, llm []string
	for name, m := range dumpResources {
		switch {
		case m.llm:
			llm = append(llm, name)
		case m.windowed:
			windowed = append(windowed, name)
		default:
			snapshot = append(snapshot, name)
		}
	}
	sort.Strings(windowed)
	sort.Strings(snapshot)
	sort.Strings(llm)
	return fmt.Sprintf("%s (windowed); %s (snapshot); %s (LLM tier, --include-llm)",
		strings.Join(windowed, ", "), strings.Join(snapshot, ", "), strings.Join(llm, ", "))
}

// dumpSweepResources returns the ordered resource list a `dump all` sweep
// covers. LLM resources are excluded unless includeLLM. Order puts cheap
// dimension snapshots first and windowed fact tables last so a killed sweep
// still lands the dimensions.
func dumpSweepResources(includeLLM bool) []string {
	order := []string{
		// global dimensions
		"accounts", "groups", "domains",
		// per-domain dimensions
		"keywords", "competitors", "landing-pages", "tags", "people-also-ask",
		// windowed facts
		"keyword-ranks", "competitor-ranks", "domain-history", "full-serp",
		"search-volume-history", "ai-search-volume-history", "competitor-history",
		"landing-page-history", "tag-history",
		// LLM tier
		"brands", "prompts", "prompt-results",
	}
	out := make([]string, 0, len(order))
	for _, r := range order {
		if dumpResources[r].llm && !includeLLM {
			continue
		}
		out = append(out, r)
	}
	return out
}

func newDumpCmd(flags *rootFlags) *cobra.Command {
	var (
		domainSpec    string
		from          string
		until         string
		windowDaysArg int
		schemaPath    string
		envelope      bool
		reportFile    string
		includeLLM    bool
		probeEarliest bool
	)
	cmd := &cobra.Command{
		Use:   "dump [resource|all]",
		Short: "Stream NDJSON for any warehouse resource over an arbitrary date window (auto-chunks the 100-day API cap)",
		Long: `Dump streams one NDJSON record per line to stdout for any resource family
in schema/model.yaml. It is the extract adapter the Noracle sync spine shells
out to (dump <resource> --envelope --agent --report-file); every resource added
here becomes landable with zero adapter code.

Windowed resources (require --from; internal 100-day chunking + cross-chunk
dedup): keyword-ranks, competitor-ranks, domain-history, full-serp,
search-volume-history, ai-search-volume-history, competitor-history,
landing-page-history, tag-history.

Snapshot resources (current dimension state, no window): keywords, competitors,
landing-pages, tags, people-also-ask.

Global resources (dump the whole set — NO --domain): domains, accounts, groups.

LLM/AccuLLM tier (brands, prompts, prompt-results) is wired but off by default:
excluded from a 'dump all' sweep unless --include-llm, and dumps of it exit 45
(skipped_unentitled inside a sweep) when your plan lacks the tier. tag-history
rides the same graceful gate.

Use 'dump all' to sweep every non-LLM resource through one code path (add
--include-llm to include the LLM tier).

Every run emits exactly one machine-readable run report as a single JSON object
on stderr (or to --report-file). stdout stays pure NDJSON. The report carries
clean_exit and next_cursor: on a fully-successful run next_cursor is the window
end; on any partial failure the exit code is non-zero and next_cursor is the
last chunk boundary that completed for every requested domain. A sync spine
should advance its watermark from next_cursor only.

--probe-earliest binary-searches the earliest date returning data per domain and
reports it as earliest_available (the backfill floor) without streaming rows.

--envelope wraps each record for lossless raw landing:
  {"source":"accuranker","endpoint":…,"params_hash":…,"api_version":"v4",
   "fetched_at":…,"payload":{…}}`,
		Example: strings.Trim(`
  # Two years of rank history, NDJSON, into Postgres COPY
  accuranker-pp-cli dump keyword-ranks --domain 295242 --from 2024-01-01 --to 2026-05-20 \
    | psql -c '\COPY accuranker_ranks FROM STDIN CSV'

  # Every tracked domain, globally, with created_at (per-domain backfill floor)
  accuranker-pp-cli dump domains --envelope

  # Full non-LLM sweep into raw envelopes, one run report to file
  accuranker-pp-cli dump all --domain 295242 --from 2024-01-01 --envelope --report-file /tmp/run.json

  # Discover the retention floor before backfilling
  accuranker-pp-cli dump keyword-ranks --domain 295242 --probe-earliest
`, "\n"),
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if flags.dryRun {
				return nil
			}
			if len(args) == 0 {
				return cmd.Help()
			}
			resource := args[0]

			model, err := schema.Load(schemaPath)
			if err != nil {
				return err
			}
			cfg, err := config.Load(flags.configPath)
			if err != nil {
				return err
			}
			if cfg.AuthHeader() == "" {
				return fmt.Errorf("no AccuRanker token configured")
			}
			cl := client.New(cfg, flags.timeout, flags.rateLimit)

			// A graceful kill (SIGINT/SIGTERM) mid-chunk must still emit the
			// run report so the spine learns the last completed boundary.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			ids, err := parseInt64CSV(domainSpec)
			if err != nil {
				return fmt.Errorf("--domain: %w", err)
			}
			if windowDaysArg <= 0 || windowDaysArg > 100 {
				windowDaysArg = 100
			}

			base := dumpBaseOptions{
				domainIDs:     ids,
				from:          from,
				until:         until,
				windowDays:    windowDaysArg,
				envelope:      envelope,
				reportFile:    reportFile,
				includeLLM:    includeLLM,
				probeEarliest: probeEarliest,
			}

			if resource == "all" || resource == "everything" {
				return runDumpSweep(ctx, cl, model, cmd.OutOrStdout(), cmd.ErrOrStderr(), base)
			}

			meta, ok := dumpResources[resource]
			if !ok {
				return fmt.Errorf("dump resource %q not supported; try: %s", resource, dumpResourceNames())
			}
			opts, err := buildDumpOptions(resource, meta, base)
			if err != nil {
				return err
			}
			fetch := func(ctx context.Context, scope int64, f, t string) ([]dumpBatch, error) {
				return dumpFetch(ctx, cl, model, resource, scope, f, t)
			}
			return runDump(ctx, fetch, cmd.OutOrStdout(), cmd.ErrOrStderr(), opts, cl.Metrics)
		},
	}
	cmd.Flags().StringVar(&domainSpec, "domain", "", "Domain ID(s), comma-separated (required for domain-scoped resources; ignored for global resources)")
	cmd.Flags().StringVar(&from, "from", "", "Window start (YYYY-MM-DD; required for windowed resources)")
	cmd.Flags().StringVar(&until, "to", "", "Window end (YYYY-MM-DD, default: today)")
	cmd.Flags().IntVar(&windowDaysArg, "window-days", 100, "Days per chunk (max 100)")
	cmd.Flags().StringVar(&schemaPath, "schema-file", "", "Override schema/model.yaml path")
	cmd.Flags().BoolVar(&envelope, "envelope", false, "Wrap each record in the raw-landing envelope {source, endpoint, params_hash, api_version, fetched_at, payload}")
	cmd.Flags().StringVar(&reportFile, "report-file", "", "Write the run report JSON to this path instead of stderr")
	cmd.Flags().BoolVar(&includeLLM, "include-llm", false, "In a 'dump all' sweep, also dump the LLM/AccuLLM tier (brands, prompts, prompt-results); gracefully skipped when unentitled")
	cmd.Flags().BoolVar(&probeEarliest, "probe-earliest", false, "Binary-search the earliest date with data per domain and report earliest_available (no rows streamed)")
	return cmd
}

// dumpBaseOptions is the raw flag set before per-resource classification.
type dumpBaseOptions struct {
	domainIDs     []int64
	from          string
	until         string
	windowDays    int
	envelope      bool
	reportFile    string
	includeLLM    bool
	probeEarliest bool
}

// dumpOptions carries everything runDump needs for ONE resource besides the
// fetcher.
type dumpOptions struct {
	resource      string
	domainIDs     []int64
	windowed      bool
	global        bool
	gated         bool
	ws, we        string
	windowDays    int
	envelope      bool
	reportFile    string
	probeEarliest bool
}

// buildDumpOptions classifies a resource and resolves its window + scope. It
// enforces the --domain (domain-scoped) and --from (windowed) requirements.
func buildDumpOptions(resource string, meta dumpMeta, base dumpBaseOptions) (dumpOptions, error) {
	opts := dumpOptions{
		resource:      resource,
		domainIDs:     base.domainIDs,
		windowed:      meta.windowed,
		global:        meta.global,
		gated:         meta.gated,
		windowDays:    base.windowDays,
		envelope:      base.envelope,
		reportFile:    base.reportFile,
		probeEarliest: base.probeEarliest,
	}
	if !meta.global {
		if len(base.domainIDs) == 0 {
			return dumpOptions{}, fmt.Errorf("--domain is required for %q", resource)
		}
	} else {
		// Global resources dump the whole set; --domain is ignored. A single
		// sentinel scope drives the one fetch.
		opts.domainIDs = []int64{0}
	}
	if meta.windowed && !base.probeEarliest {
		if base.from == "" {
			return dumpOptions{}, fmt.Errorf("--from is required (YYYY-MM-DD) for windowed resource %q", resource)
		}
		ws, we, err := resolveWindow(base.from, base.until)
		if err != nil {
			return dumpOptions{}, err
		}
		opts.ws, opts.we = ws, we
	} else if base.probeEarliest {
		// Probe mode: the search runs from probeEarliestFloor up to --to/today.
		_, we, err := resolveWindow(orDefault(base.from, probeEarliestFloor), base.until)
		if err != nil {
			return dumpOptions{}, err
		}
		opts.we = we
	} else {
		// Snapshot resources: the "window" is the fetch date.
		today := time.Now().UTC().Format("2006-01-02")
		opts.ws, opts.we = today, today
	}
	return opts, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// dumpBatch is one API call's worth of rows plus the request metadata the
// envelope (F3) and report (F1) need. A single (scope, chunk) fetch may return
// several batches when a resource fans out over dependent parents (e.g. prompts
// across brands), so each batch carries the endpoint/params that produced it.
type dumpBatch struct {
	rows     []map[string]any
	endpoint string
	params   url.Values
}

// dumpFetchFn abstracts dumpFetch for tests. It returns one or more batches for
// the (scope, from, to) it is called with.
type dumpFetchFn func(ctx context.Context, scope int64, from, to string) ([]dumpBatch, error)

// dumpReportWindow is the requested window in the run report.
type dumpReportWindow struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// dumpRunReport is the F1 watermark handshake. One per run, stderr or
// --report-file, never stdout.
type dumpRunReport struct {
	Resource          string            `json:"resource"`
	DomainID          int64             `json:"domain_id,omitempty"`
	DomainIDs         []int64           `json:"domain_ids,omitempty"`
	Window            dumpReportWindow  `json:"window"`
	RowsEmitted       int64             `json:"rows_emitted"`
	RequestsMade      int64             `json:"requests_made"`
	RateLimitHits     int64             `json:"rate_limit_hits"`
	APIVersion        string            `json:"api_version"`
	StartedAt         string            `json:"started_at"`
	FinishedAt        string            `json:"finished_at"`
	CleanExit         bool              `json:"clean_exit"`
	NextCursor        string            `json:"next_cursor"`
	Status            string            `json:"status,omitempty"` // "" | skipped_unentitled
	EarliestAvailable map[string]string `json:"earliest_available,omitempty"`
	Error             string            `json:"error,omitempty"`
}

// dumpSweepReport is the single report a `dump all` run emits: one sub-report
// per resource plus a top-level clean_exit that is true only when no
// non-gated resource failed (gated resources degrade to skipped_unentitled).
type dumpSweepReport struct {
	Resource      string           `json:"resource"` // always "all"
	Window        dumpReportWindow `json:"window"`
	StartedAt     string           `json:"started_at"`
	FinishedAt    string           `json:"finished_at"`
	RowsEmitted   int64            `json:"rows_emitted"`
	RequestsMade  int64            `json:"requests_made"`
	RateLimitHits int64            `json:"rate_limit_hits"`
	APIVersion    string           `json:"api_version"`
	CleanExit     bool             `json:"clean_exit"`
	Resources     []*dumpRunReport `json:"resources"`
}

func runDump(ctx context.Context, fetch dumpFetchFn, out, errOut io.Writer, opts dumpOptions, metrics func() (int64, int64)) error {
	rep, runErr := dumpResourceRun(ctx, fetch, out, opts)
	if metrics != nil {
		rep.RequestsMade, rep.RateLimitHits = metrics()
	}
	// Gated resources degrade to exit 45 (not a hard failure) when the account
	// is not entitled to the tier.
	if runErr != nil && opts.gated && isUnentitledErr(runErr) {
		rep.Status = "skipped_unentitled"
		if err := writeDumpReport(rep, errOut, opts.reportFile); err != nil {
			fmt.Fprintf(errOut, "warning: writing run report failed: %v\n", err)
		}
		return unentitledErr(fmt.Errorf("dump %s: plan tier not entitled (exit 45): %w", opts.resource, runErr))
	}
	if err := writeDumpReport(rep, errOut, opts.reportFile); err != nil {
		if runErr == nil {
			return err
		}
		fmt.Fprintf(errOut, "warning: writing run report failed: %v\n", err)
	}
	return runErr
}

// dumpResourceRun streams NDJSON for one resource to out and returns its report
// plus the run error. It does NOT write the report or read metrics — the caller
// (runDump for single-resource, runDumpSweep for a sweep) owns that so a sweep
// can aggregate.
func dumpResourceRun(ctx context.Context, fetch dumpFetchFn, out io.Writer, opts dumpOptions) (*dumpRunReport, error) {
	rep := &dumpRunReport{
		Resource:   opts.resource,
		Window:     dumpReportWindow{From: opts.ws, To: opts.we},
		APIVersion: dumpAPIVersion,
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	if !opts.global {
		if len(opts.domainIDs) == 1 {
			rep.DomainID = opts.domainIDs[0]
		} else {
			rep.DomainIDs = opts.domainIDs
		}
	}

	var runErr error
	if opts.probeEarliest {
		rep.EarliestAvailable, runErr = probeEarliestPerScope(ctx, fetch, opts)
	} else {
		runErr = dumpAllChunks(ctx, fetch, out, opts, rep)
	}

	rep.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	rep.CleanExit = runErr == nil
	if runErr != nil {
		rep.Error = runErr.Error()
	}
	return rep, runErr
}

// runDumpSweep dumps every resource dumpSweepResources returns, streaming all
// their NDJSON to out and emitting ONE combined run report. A gated resource
// that is unentitled is recorded skipped_unentitled and does not fail the
// sweep; any other resource failure marks the sweep non-clean (exit non-zero).
func runDumpSweep(ctx context.Context, cl *client.Client, model *schema.Model, out, errOut io.Writer, base dumpBaseOptions) error {
	resources := dumpSweepResources(base.includeLLM)

	// A sweep spans windowed + domain-scoped resources, so it needs both a
	// window and at least one --domain.
	if len(base.domainIDs) == 0 {
		return fmt.Errorf("dump all requires --domain (the sweep includes domain-scoped resources)")
	}
	if base.from == "" && !base.probeEarliest {
		return fmt.Errorf("dump all requires --from (the sweep includes windowed resources)")
	}
	ws, we, err := resolveWindow(orDefault(base.from, probeEarliestFloor), base.until)
	if err != nil {
		return err
	}

	sweep := &dumpSweepReport{
		Resource:   "all",
		Window:     dumpReportWindow{From: ws, To: we},
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
		APIVersion: dumpAPIVersion,
	}
	hardFail := false
	for _, resource := range resources {
		meta := dumpResources[resource]
		opts, err := buildDumpOptions(resource, meta, base)
		if err != nil {
			// Missing --from etc. cannot happen here (validated above), but be
			// defensive rather than silently dropping a resource.
			sub := &dumpRunReport{Resource: resource, Status: "error", Error: err.Error()}
			sweep.Resources = append(sweep.Resources, sub)
			hardFail = true
			continue
		}
		fetch := func(ctx context.Context, scope int64, f, t string) ([]dumpBatch, error) {
			return dumpFetch(ctx, cl, model, resource, scope, f, t)
		}
		sub, runErr := dumpResourceRun(ctx, fetch, out, opts)
		if runErr != nil {
			if meta.gated && isUnentitledErr(runErr) {
				sub.Status = "skipped_unentitled"
			} else {
				sub.Status = "error"
				hardFail = true
			}
		}
		sweep.RowsEmitted += sub.RowsEmitted
		sweep.Resources = append(sweep.Resources, sub)
		if ctx.Err() != nil {
			// Interrupted mid-sweep: stop starting new resources; the report
			// already carries every completed resource's next_cursor.
			hardFail = true
			break
		}
	}
	sweep.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	sweep.CleanExit = !hardFail
	sweep.RequestsMade, sweep.RateLimitHits = cl.Metrics()

	if err := writeSweepReport(sweep, errOut, base.reportFile); err != nil {
		fmt.Fprintf(errOut, "warning: writing run report failed: %v\n", err)
	}
	if hardFail {
		return apiErr(fmt.Errorf("dump all: one or more resources failed (see run report)"))
	}
	return nil
}

// dumpAllChunks walks chunk-outer/scope-inner so "the last chunk boundary
// completed for ALL scopes" — the report's next_cursor — is well-defined even
// for multi-domain runs. rep.RowsEmitted and rep.NextCursor are updated as the
// walk progresses so a partial failure reports honest progress.
func dumpAllChunks(ctx context.Context, fetch dumpFetchFn, out io.Writer, opts dumpOptions, rep *dumpRunReport) error {
	var chunks [][2]string
	if opts.windowed {
		var err error
		chunks, err = chunkDateWindow(opts.ws, opts.we, opts.windowDays)
		if err != nil {
			return err
		}
	} else {
		chunks = [][2]string{{opts.ws, opts.we}}
	}

	seen := make(map[string]bool, 4096)
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)

	for _, chunk := range chunks {
		for _, scope := range opts.domainIDs {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("dump %s scope=%d %s..%s: interrupted: %w", opts.resource, scope, chunk[0], chunk[1], err)
			}
			batches, err := fetch(ctx, scope, chunk[0], chunk[1])
			if err != nil {
				return fmt.Errorf("dump %s scope=%d %s..%s: %w", opts.resource, scope, chunk[0], chunk[1], err)
			}
			for _, batch := range batches {
				fetchedAt := time.Now().UTC().Format(time.RFC3339)
				for _, row := range batch.rows {
					key := dedupKey(opts.resource, row)
					if seen[key] {
						continue
					}
					seen[key] = true
					var rec any = row
					if opts.envelope {
						rec = envelopeRecord(row, batch.endpoint, batch.params, fetchedAt)
					}
					if err := enc.Encode(rec); err != nil {
						return fmt.Errorf("encode: %w", err)
					}
					rep.RowsEmitted++
				}
			}
		}
		// Chunk completed for every scope — this boundary is safe to advance a
		// watermark to.
		rep.NextCursor = chunk[1]
	}
	return nil
}

// probeEarliestPerScope binary-searches, per scope, the earliest date on which
// the resource returns any data (spec F7). It uses a windowDays-wide probe so a
// single dark day (no crawl) does not break the monotone no-data→data boundary;
// the reported floor is therefore <= the true earliest date, which is exactly
// what a backfill floor wants (conservative, never too late). Returns a map of
// scope-id → date (or "none" when the whole range is empty).
func probeEarliestPerScope(ctx context.Context, fetch dumpFetchFn, opts dumpOptions) (map[string]string, error) {
	floor, err := time.Parse("2006-01-02", probeEarliestFloor)
	if err != nil {
		return nil, err
	}
	hi, err := time.Parse("2006-01-02", opts.we)
	if err != nil {
		return nil, err
	}
	windowDays := opts.windowDays
	if windowDays < 1 {
		windowDays = 100
	}
	out := make(map[string]string, len(opts.domainIDs))
	for _, scope := range opts.domainIDs {
		key := fmt.Sprintf("%d", scope)
		if opts.global {
			key = "global"
		}
		hasData := func(d time.Time) (bool, error) {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			end := d.AddDate(0, 0, windowDays-1)
			if end.After(hi) {
				end = hi
			}
			batches, err := fetch(ctx, scope, d.Format("2006-01-02"), end.Format("2006-01-02"))
			if err != nil {
				return false, err
			}
			for _, b := range batches {
				if len(b.rows) > 0 {
					return true, nil
				}
			}
			return false, nil
		}
		totalDays := int(hi.Sub(floor).Hours() / 24)
		if totalDays < 0 {
			out[key] = "none"
			continue
		}
		// Lower-bound binary search for the smallest day offset with data.
		lo, hiIdx, ans := 0, totalDays, ""
		for lo <= hiIdx {
			mid := (lo + hiIdx) / 2
			ok, err := hasData(floor.AddDate(0, 0, mid))
			if err != nil {
				return out, err
			}
			if ok {
				ans = floor.AddDate(0, 0, mid).Format("2006-01-02")
				hiIdx = mid - 1
			} else {
				lo = mid + 1
			}
		}
		if ans == "" {
			ans = "none"
		}
		out[key] = ans
	}
	return out, nil
}

// envelopeRecord wraps a row in the lossless raw-landing envelope (spec F3).
func envelopeRecord(row map[string]any, endpoint string, params url.Values, fetchedAt string) map[string]any {
	return map[string]any{
		"source":      "accuranker",
		"endpoint":    endpoint,
		"params_hash": dumpParamsHash(endpoint, params),
		"api_version": dumpAPIVersion,
		"fetched_at":  fetchedAt,
		"payload":     row,
	}
}

// dumpParamsHash is a stable content address for the request that produced a
// record: sha256 over the endpoint plus the canonically-encoded (sorted) query
// string.
func dumpParamsHash(endpoint string, params url.Values) string {
	h := sha256.Sum256([]byte(endpoint + "?" + params.Encode()))
	return "sha256:" + hex.EncodeToString(h[:])
}

// writeDumpReport emits the single run report: one JSON line on stderr, or a
// pretty document at --report-file.
func writeDumpReport(rep *dumpRunReport, errOut io.Writer, reportFile string) error {
	if reportFile != "" {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(reportFile, append(b, '\n'), 0o644)
	}
	enc := json.NewEncoder(errOut)
	enc.SetEscapeHTML(false)
	return enc.Encode(rep)
}

func writeSweepReport(rep *dumpSweepReport, errOut io.Writer, reportFile string) error {
	if reportFile != "" {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(reportFile, append(b, '\n'), 0o644)
	}
	enc := json.NewEncoder(errOut)
	enc.SetEscapeHTML(false)
	return enc.Encode(rep)
}

// isUnentitledErr reports whether err is a plan/tier access denial that a gated
// resource should degrade on (exit 45 / skipped_unentitled) rather than treat
// as a hard failure. Covers the AccuLLM paywall bodies plus HTTP 403 and
// 400+access-denial responses (tag-history is tier-restricted on some plans).
func isUnentitledErr(err error) bool {
	if err == nil {
		return false
	}
	if isPaywallErr(err) {
		return true
	}
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		if apiErr.StatusCode == 403 {
			return true
		}
		if apiErr.StatusCode == 400 && looksLikeAccessDenial(apiErr.Body) {
			return true
		}
	}
	return false
}

func dumpFetch(ctx context.Context, cl *client.Client, m *schema.Model, resource string, scope int64, from, to string) ([]dumpBatch, error) {
	switch resource {
	case "keyword-ranks":
		// PATCH(amend-2026-07-21: spec F11): request initial_rank.* alongside
		// ranks.* and emit the KeywordInitialRank baseline row with
		// is_initial=true so every keyword's series has a defined left edge.
		q := url.Values{}
		rankFields := m.DefaultFields("keyword_ranks")
		q.Set("fields", "id,"+rankFields+","+strings.ReplaceAll(rankFields, "ranks.", "initial_rank."))
		q.Set("period_from", from)
		q.Set("period_to", to)
		endpoint := fmt.Sprintf("/api/v4/domains/%d/keywords/", scope)
		body, err := apiGet(ctx, cl, endpoint, q)
		if err != nil {
			return nil, err
		}
		var kw []struct {
			ID          int64            `json:"id"`
			Ranks       []map[string]any `json:"ranks"`
			InitialRank map[string]any   `json:"initial_rank"`
		}
		if err := json.Unmarshal(body, &kw); err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, 256)
		for _, k := range kw {
			for _, r := range k.Ranks {
				r["keyword_id"] = k.ID
				r["domain_id"] = scope
				stampSearchDate(r)
				r["is_initial"] = false
				out = append(out, r)
			}
			if init := k.InitialRank; len(init) > 0 {
				if hasAnyValue(init, "id", "created_at", "rank") {
					init["keyword_id"] = k.ID
					init["domain_id"] = scope
					stampSearchDate(init)
					init["is_initial"] = true
					out = append(out, init)
				}
			}
		}
		return oneBatch(out, endpoint, q), nil

	case "domain-history":
		q := url.Values{}
		q.Set("fields", m.DefaultFields("domain_history"))
		q.Set("period_from", from)
		q.Set("period_to", to)
		endpoint := fmt.Sprintf("/api/v4/domains/%d/", scope)
		body, err := apiGet(ctx, cl, endpoint, q)
		if err != nil {
			return nil, err
		}
		var doc struct {
			History []map[string]any `json:"history"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			return nil, err
		}
		for _, h := range doc.History {
			h["domain_id"] = scope
		}
		return oneBatch(doc.History, endpoint, q), nil

	case "competitor-ranks":
		// PATCH(amend-2026-07-07: uniform dump surface — spec F2): the
		// modelled-but-unwired keyword_competitor_ranks family, windowed like
		// ranks. Rows land in accuranker_keyword_competitor_ranks.
		q := url.Values{}
		q.Set("fields", "id,"+m.DefaultFields("keyword_competitor_ranks"))
		q.Set("period_from", from)
		q.Set("period_to", to)
		endpoint := fmt.Sprintf("/api/v4/domains/%d/keywords/", scope)
		body, err := apiGet(ctx, cl, endpoint, q)
		if err != nil {
			return nil, err
		}
		var kw []struct {
			ID              int64            `json:"id"`
			CompetitorRanks []map[string]any `json:"competitor_ranks"`
		}
		if err := json.Unmarshal(body, &kw); err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, 256)
		for _, k := range kw {
			for _, r := range k.CompetitorRanks {
				r["keyword_id"] = k.ID
				r["domain_id"] = scope
				if comp, ok := r["competitor"].(map[string]any); ok {
					if cid, ok := extractInt64(comp, "id"); ok {
						r["competitor_id"] = cid
					}
				}
				stampSearchDate(r)
				out = append(out, r)
			}
		}
		return oneBatch(out, endpoint, q), nil

	case "full-serp":
		// PATCH(amend-2026-07-21: spec F9): keyword_full_serp, windowed like
		// ranks, projected from the keywords list endpoint's full_serp array.
		q := url.Values{}
		q.Set("fields", "id,"+prefixFields("full_serp.", "created_at,search_intent,top_domain,elements"))
		q.Set("period_from", from)
		q.Set("period_to", to)
		endpoint := fmt.Sprintf("/api/v4/domains/%d/keywords/", scope)
		body, err := apiGet(ctx, cl, endpoint, q)
		if err != nil {
			return nil, err
		}
		var kw []struct {
			ID       int64            `json:"id"`
			FullSerp []map[string]any `json:"full_serp"`
		}
		if err := json.Unmarshal(body, &kw); err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, 256)
		for _, k := range kw {
			for _, s := range k.FullSerp {
				s["keyword_id"] = k.ID
				s["domain_id"] = scope
				stampSearchDate(s)
				out = append(out, s)
			}
		}
		return oneBatch(out, endpoint, q), nil

	case "search-volume-history":
		// PATCH(amend-2026-07-21: spec F9): monthly search volume per keyword.
		// The API field is `date` (the month); we map it to the model's `month`
		// watermark column. Windowed + dedup handle the overlap when the API
		// returns full history regardless of the requested window.
		q := url.Values{}
		q.Set("fields", "id,"+prefixFields("search_volume.history.", "date,search_volume"))
		q.Set("period_from", from)
		q.Set("period_to", to)
		endpoint := fmt.Sprintf("/api/v4/domains/%d/keywords/", scope)
		body, err := apiGet(ctx, cl, endpoint, q)
		if err != nil {
			return nil, err
		}
		var kw []struct {
			ID           int64 `json:"id"`
			SearchVolume struct {
				History []map[string]any `json:"history"`
			} `json:"search_volume"`
		}
		if err := json.Unmarshal(body, &kw); err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, 256)
		for _, k := range kw {
			for _, h := range k.SearchVolume.History {
				h["keyword_id"] = k.ID
				stampMonth(h)
				out = append(out, h)
			}
		}
		return oneBatch(out, endpoint, q), nil

	case "ai-search-volume-history":
		// PATCH(amend-2026-07-21: spec F9): monthly AI search volume per keyword.
		q := url.Values{}
		q.Set("fields", "id,"+prefixFields("ai_search_volume.history.", "date,search_volume,search_volume_total"))
		q.Set("period_from", from)
		q.Set("period_to", to)
		endpoint := fmt.Sprintf("/api/v4/domains/%d/keywords/", scope)
		body, err := apiGet(ctx, cl, endpoint, q)
		if err != nil {
			return nil, err
		}
		var kw []struct {
			ID             int64 `json:"id"`
			AiSearchVolume struct {
				History []map[string]any `json:"history"`
			} `json:"ai_search_volume"`
		}
		if err := json.Unmarshal(body, &kw); err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, 256)
		for _, k := range kw {
			for _, h := range k.AiSearchVolume.History {
				h["keyword_id"] = k.ID
				stampMonth(h)
				out = append(out, h)
			}
		}
		return oneBatch(out, endpoint, q), nil

	case "competitor-history":
		// PATCH(amend-2026-07-21: spec F9): per-competitor daily aggregate,
		// projected from the domain detail endpoint's competitors[].history[].
		q := url.Values{}
		q.Set("fields", "competitors.id,"+modelAPIFields(m, "competitor_history", "competitors.history.", competitorHistorySkip))
		q.Set("period_from", from)
		q.Set("period_to", to)
		endpoint := fmt.Sprintf("/api/v4/domains/%d/", scope)
		body, err := apiGet(ctx, cl, endpoint, q)
		if err != nil {
			return nil, err
		}
		var doc struct {
			Competitors []struct {
				ID      int64            `json:"id"`
				History []map[string]any `json:"history"`
			} `json:"competitors"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, 256)
		for _, c := range doc.Competitors {
			for _, h := range c.History {
				h["competitor_id"] = c.ID
				h["domain_id"] = scope
				out = append(out, h)
			}
		}
		return oneBatch(out, endpoint, q), nil

	case "landing-page-history":
		// PATCH(amend-2026-07-21: spec F9): per-landing-page daily aggregate.
		// The landing_pages LIST endpoint carries history inline (see model.yaml
		// landing_pages.default_fields), so no store-aware per-page enumeration
		// is needed — one call per (domain, chunk).
		q := url.Values{}
		q.Set("fields", "id,landing_page,"+modelAPIFields(m, "landing_page_history", "history.", landingPageHistorySkip))
		q.Set("period_from", from)
		q.Set("period_to", to)
		endpoint := fmt.Sprintf("/api/v4/domains/%d/landing_pages/", scope)
		body, err := apiGet(ctx, cl, endpoint, q)
		if err != nil {
			return nil, err
		}
		var raw []struct {
			ID          int64            `json:"id"`
			LandingPage string           `json:"landing_page"`
			Path        string           `json:"path"`
			History     []map[string]any `json:"history"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, 256)
		for _, lp := range raw {
			for _, h := range lp.History {
				h["landing_page_id"] = lp.ID
				h["domain_id"] = scope
				out = append(out, h)
			}
		}
		return oneBatch(out, endpoint, q), nil

	case "tag-history":
		// PATCH(amend-2026-07-21: spec F9): per-tag daily aggregate, projected
		// from the tags LIST endpoint's inline history. Tier-restricted on some
		// accounts — a 403/paywall degrades to exit 45 (see dumpResources.gated).
		q := url.Values{}
		q.Set("fields", "tag,"+modelAPIFields(m, "tag_history", "history.", tagHistorySkip))
		q.Set("period_from", from)
		q.Set("period_to", to)
		endpoint := fmt.Sprintf("/api/v4/domains/%d/tags/", scope)
		body, err := apiGet(ctx, cl, endpoint, q)
		if err != nil {
			return nil, err
		}
		var raw []struct {
			Tag     string           `json:"tag"`
			History []map[string]any `json:"history"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, 256)
		for _, t := range raw {
			if t.Tag == "" {
				continue // untagged {tag:null} aggregate bucket
			}
			for _, h := range t.History {
				h["domain_id"] = scope
				h["tag"] = t.Tag
				out = append(out, h)
			}
		}
		return oneBatch(out, endpoint, q), nil

	case "keywords":
		// PATCH(amend-2026-07-07: uniform dump surface — spec F2): full
		// keyword-dimension snapshot; cheap, designed to run daily as the
		// panel-diffing input for `keywords-diff --against`.
		q := url.Values{}
		q.Set("fields", m.DefaultFields("keywords"))
		endpoint := fmt.Sprintf("/api/v4/domains/%d/keywords/", scope)
		body, err := apiGet(ctx, cl, endpoint, q)
		if err != nil {
			return nil, err
		}
		var raw []map[string]any
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, err
		}
		rows := make([]map[string]any, 0, len(raw))
		for _, item := range raw {
			rows = append(rows, flattenKeywordRow(item, scope))
		}
		return oneBatch(rows, endpoint, q), nil

	case "competitors":
		// PATCH(amend-2026-07-07: uniform dump surface — spec F2): the
		// competitor dimension, projected from the domain detail endpoint.
		q := url.Values{}
		q.Set("fields", prefixFields("competitors.", m.DefaultFields("competitors")))
		endpoint := fmt.Sprintf("/api/v4/domains/%d/", scope)
		body, err := apiGet(ctx, cl, endpoint, q)
		if err != nil {
			return nil, err
		}
		var doc struct {
			Competitors []map[string]any `json:"competitors"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			return nil, err
		}
		for _, c := range doc.Competitors {
			c["domain_id"] = scope
		}
		return oneBatch(doc.Competitors, endpoint, q), nil

	case "landing-pages":
		// Live-API note (verified 2026-07-07): the landing_pages list endpoint
		// returns `path` per row on this tier — the modelled id/landing_page/
		// title projection is silently ignored. Request both spellings and alias
		// path→landing_page so rows stay useful whichever shape the tier returns.
		q := url.Values{}
		q.Set("fields", "id,path,landing_page,title")
		endpoint := fmt.Sprintf("/api/v4/domains/%d/landing_pages/", scope)
		body, err := apiGet(ctx, cl, endpoint, q)
		if err != nil {
			return nil, err
		}
		var raw []map[string]any
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, err
		}
		for _, item := range raw {
			item["domain_id"] = scope
			if _, ok := item["landing_page"]; !ok {
				if p, ok := item["path"].(string); ok {
					item["landing_page"] = p
				}
			}
		}
		return oneBatch(raw, endpoint, q), nil

	case "tags":
		q := url.Values{}
		q.Set("fields", "tag")
		endpoint := fmt.Sprintf("/api/v4/domains/%d/tags/", scope)
		body, err := apiGet(ctx, cl, endpoint, q)
		if err != nil {
			return nil, err
		}
		var raw []map[string]any
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, err
		}
		// The API prepends an untagged-bucket row {"tag": null} (verified live
		// 2026-07-07). It is an aggregate artifact, not a tag — drop it so rows
		// key cleanly on (domain_id, tag).
		rows := make([]map[string]any, 0, len(raw))
		for _, item := range raw {
			if t, ok := item["tag"].(string); !ok || t == "" {
				continue
			}
			item["domain_id"] = scope
			rows = append(rows, item)
		}
		return oneBatch(rows, endpoint, q), nil

	case "people-also-ask":
		// PATCH(amend-2026-07-21: spec F9): People-Also-Ask terms per keyword,
		// projected from the keywords list endpoint's people_also_ask array.
		q := url.Values{}
		q.Set("fields", "id,"+prefixFields("people_also_ask.", "term,last_seen_at"))
		endpoint := fmt.Sprintf("/api/v4/domains/%d/keywords/", scope)
		body, err := apiGet(ctx, cl, endpoint, q)
		if err != nil {
			return nil, err
		}
		var kw []struct {
			ID  int64            `json:"id"`
			PAA []map[string]any `json:"people_also_ask"`
		}
		if err := json.Unmarshal(body, &kw); err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, 256)
		for _, k := range kw {
			for _, p := range k.PAA {
				if t, ok := p["term"].(string); !ok || t == "" {
					continue
				}
				p["keyword_id"] = k.ID
				out = append(out, p)
			}
		}
		return oneBatch(out, endpoint, q), nil

	case "domains":
		// PATCH(amend-2026-07-21: spec F9): GLOBAL — dump every tracked domain
		// (no --domain). Carries created_at (per-domain backfill floor) and
		// group linkage.
		q := url.Values{}
		q.Set("fields", m.DefaultFields("domains"))
		endpoint := "/api/v4/domains/"
		body, err := apiGet(ctx, cl, endpoint, q)
		if err != nil {
			return nil, err
		}
		rows, err := decodeRowsFlexible(body)
		if err != nil {
			return nil, fmt.Errorf("decode domains: %w", err)
		}
		for _, item := range rows {
			if g, ok := item["group"].(map[string]any); ok {
				if gid, ok := extractInt64(g, "id"); ok {
					item["group_id"] = gid
				}
			}
		}
		return oneBatch(rows, endpoint, q), nil

	case "accounts":
		// PATCH(amend-2026-07-21: spec F9): GLOBAL — one row per visible account.
		q := url.Values{}
		q.Set("fields", m.DefaultFields("accounts"))
		endpoint := "/api/v4/accounts/"
		body, err := apiGet(ctx, cl, endpoint, q)
		if err != nil {
			return nil, err
		}
		rows, err := decodeRowsFlexible(body)
		if err != nil {
			return nil, fmt.Errorf("decode accounts: %w", err)
		}
		return oneBatch(rows, endpoint, q), nil

	case "groups":
		// PATCH(amend-2026-07-21: spec F9): GLOBAL — client groups, derived from
		// the domains list's group object (there is no standalone /groups/ list).
		q := url.Values{}
		q.Set("fields", "id,group.id,group.name,group.created_at,group.organization_id")
		endpoint := "/api/v4/domains/"
		body, err := apiGet(ctx, cl, endpoint, q)
		if err != nil {
			return nil, err
		}
		rows, err := decodeRowsFlexible(body)
		if err != nil {
			return nil, fmt.Errorf("decode groups: %w", err)
		}
		out := make([]map[string]any, 0, 16)
		for _, item := range rows {
			g, ok := item["group"].(map[string]any)
			if !ok {
				continue
			}
			if _, ok := extractInt64(g, "id"); !ok {
				continue
			}
			out = append(out, g)
		}
		return oneBatch(out, endpoint, q), nil

	case "brands":
		// PATCH(amend-2026-07-21: spec F10): GLOBAL LLM tier — AccuLLM brands.
		q := url.Values{}
		q.Set("fields", m.DefaultFields("brands"))
		endpoint := "/api/v4/brands/"
		body, err := apiGet(ctx, cl, endpoint, q)
		if err != nil {
			return nil, err
		}
		rows, err := decodeRowsFlexible(body)
		if err != nil {
			return nil, fmt.Errorf("decode brands: %w", err)
		}
		for _, item := range rows {
			if g, ok := item["group"].(map[string]any); ok {
				if gid, ok := extractInt64(g, "id"); ok {
					item["group_id"] = gid
				}
			}
		}
		return oneBatch(rows, endpoint, q), nil

	case "prompts":
		// PATCH(amend-2026-07-21: spec F10): GLOBAL LLM tier — prompts under
		// each brand. Fans out over brands (store-free), one batch per brand.
		bids, err := dumpEnumerateBrands(ctx, cl, m)
		if err != nil {
			return nil, err
		}
		batches := make([]dumpBatch, 0, len(bids))
		for _, bid := range bids {
			q := url.Values{}
			q.Set("fields", m.DefaultFields("prompts"))
			endpoint := fmt.Sprintf("/api/v4/brands/%d/prompts/", bid)
			body, err := apiGet(ctx, cl, endpoint, q)
			if err != nil {
				return nil, err
			}
			rows, err := decodeRowsFlexible(body)
			if err != nil {
				return nil, fmt.Errorf("decode prompts for brand %d: %w", bid, err)
			}
			for _, item := range rows {
				item["brand_id"] = bid
			}
			batches = append(batches, dumpBatch{rows: rows, endpoint: endpoint, params: q})
		}
		return batches, nil

	case "prompt-results":
		// PATCH(amend-2026-07-21: spec F10): GLOBAL LLM tier, windowed —
		// per-prompt LLM-search results. Fans out over brands→prompts.
		bids, err := dumpEnumerateBrands(ctx, cl, m)
		if err != nil {
			return nil, err
		}
		batches := make([]dumpBatch, 0, 32)
		for _, bid := range bids {
			prompts, err := dumpEnumeratePrompts(ctx, cl, m, bid)
			if err != nil {
				return nil, err
			}
			for _, pid := range prompts {
				q := url.Values{}
				q.Set("fields", m.DefaultFields("prompt_results"))
				q.Set("period_from", from)
				q.Set("period_to", to)
				endpoint := fmt.Sprintf("/api/v4/brands/%d/prompts/%d/", bid, pid)
				body, err := apiGet(ctx, cl, endpoint, q)
				if err != nil {
					return nil, err
				}
				var doc struct {
					Results []map[string]any `json:"results"`
				}
				if err := json.Unmarshal(body, &doc); err != nil {
					return nil, fmt.Errorf("decode prompt_results for prompt %d: %w", pid, err)
				}
				for _, r := range doc.Results {
					r["prompt_id"] = pid
					stampSearchDate(r)
				}
				batches = append(batches, dumpBatch{rows: doc.Results, endpoint: endpoint, params: q})
			}
		}
		return batches, nil

	default:
		return nil, fmt.Errorf("dump resource %q not supported; try: %s", resource, dumpResourceNames())
	}
}

// oneBatch is the common single-batch return for resources that map to exactly
// one API call per (scope, chunk).
func oneBatch(rows []map[string]any, endpoint string, params url.Values) []dumpBatch {
	return []dumpBatch{{rows: rows, endpoint: endpoint, params: params}}
}

// dumpEnumerateBrands lists AccuLLM brand ids straight from the API (store-free)
// so `dump prompts`/`dump prompt-results` can fan out. Propagates the paywall
// error unchanged so the gate can classify it as unentitled.
func dumpEnumerateBrands(ctx context.Context, cl *client.Client, m *schema.Model) ([]int64, error) {
	q := url.Values{}
	q.Set("fields", "id")
	body, err := apiGet(ctx, cl, "/api/v4/brands/", q)
	if err != nil {
		return nil, err
	}
	rows, err := decodeRowsFlexible(body)
	if err != nil {
		return nil, fmt.Errorf("decode brands: %w", err)
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		if id, ok := extractInt64(r, "id"); ok {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func dumpEnumeratePrompts(ctx context.Context, cl *client.Client, m *schema.Model, brandID int64) ([]int64, error) {
	q := url.Values{}
	q.Set("fields", "id")
	body, err := apiGet(ctx, cl, fmt.Sprintf("/api/v4/brands/%d/prompts/", brandID), q)
	if err != nil {
		return nil, err
	}
	rows, err := decodeRowsFlexible(body)
	if err != nil {
		return nil, fmt.Errorf("decode prompts for brand %d: %w", brandID, err)
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		if id, ok := extractInt64(r, "id"); ok {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// decodeRowsFlexible unmarshals a list-endpoint body whose envelope shape can
// drift per tier: a bare JSON array, a single object, or an object wrapping a
// rows array under a common key (accounts/results/data/items/domains). This
// keeps global resources robust against the account/organization response
// drift AccuRanker exhibits.
func decodeRowsFlexible(body []byte) ([]map[string]any, error) {
	var arr []map[string]any
	if err := json.Unmarshal(body, &arr); err == nil {
		return arr, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	for _, key := range []string{"accounts", "results", "data", "items", "domains", "groups", "brands"} {
		if raw, ok := obj[key]; ok {
			if list, ok := raw.([]any); ok {
				out := make([]map[string]any, 0, len(list))
				for _, el := range list {
					if row, ok := el.(map[string]any); ok {
						out = append(out, row)
					}
				}
				return out, nil
			}
		}
	}
	// A single object is one row.
	return []map[string]any{obj}, nil
}

// competitorHistorySkip / landingPageHistorySkip / tagHistorySkip are the model
// columns that are NOT API history sub-fields (parent FKs, natural keys) and so
// must be excluded when deriving the fields= projection from model.yaml.
var (
	competitorHistorySkip  = map[string]bool{"competitor_id": true, "domain_id": true}
	landingPageHistorySkip = map[string]bool{"landing_page_id": true, "domain_id": true}
	tagHistorySkip         = map[string]bool{"domain_id": true, "tag": true}
)

// modelAPIFields builds a prefixed fields= projection from a resource's
// model.yaml columns, skipping the synced_at default-fn column and any name in
// skip. Used for the history resources whose column set matches the API 1:1.
func modelAPIFields(m *schema.Model, resource, prefix string, skip map[string]bool) string {
	r := m.Resource(resource)
	if r == nil {
		return ""
	}
	parts := make([]string, 0, len(r.Columns))
	for _, c := range r.Columns {
		if c.DefaultFn == "now" || skip[c.Name] {
			continue
		}
		parts = append(parts, prefix+c.Name)
	}
	return strings.Join(parts, ",")
}

// stampSearchDate derives the search_date (UTC calendar date) from created_at
// when the row does not already carry it.
func stampSearchDate(row map[string]any) {
	if _, ok := row["search_date"]; ok {
		return
	}
	if cAt, ok := row["created_at"].(string); ok && len(cAt) >= 10 {
		row["search_date"] = cAt[:10]
	}
}

// stampMonth maps the API's monthly `date` field to the model's `month`
// watermark column.
func stampMonth(row map[string]any) {
	if _, ok := row["month"]; ok {
		return
	}
	if d, ok := row["date"].(string); ok && d != "" {
		row["month"] = d
	}
}

// hasAnyValue reports whether at least one of keys has a non-nil value in obj.
func hasAnyValue(obj map[string]any, keys ...string) bool {
	for _, k := range keys {
		if v, ok := obj[k]; ok && v != nil {
			return true
		}
	}
	return false
}

// flattenKeywordRow stamps domain_id and flattens the nested search_engine /
// search_volume / ai_search_volume objects into the keyword-dimension columns
// model.yaml declares. Shared by `dump keywords` and the mirror runner so both
// emit the same shape.
func flattenKeywordRow(item map[string]any, did int64) map[string]any {
	item["domain_id"] = did
	if se, ok := item["search_engine"].(map[string]any); ok {
		if sid, ok := extractInt64(se, "id"); ok {
			item["search_engine_id"] = sid
		}
		if name, ok := se["name"].(string); ok {
			item["search_engine_name"] = name
		}
	}
	if sv, ok := item["search_volume"].(map[string]any); ok {
		if v, ok := extractInt64(sv, "search_volume"); ok {
			item["search_volume_value"] = v
		}
		if v, ok := extractFloat(sv, "avg_cost_per_click"); ok {
			item["search_volume_avg_cpc"] = v
		}
		if v, ok := extractFloat(sv, "competition"); ok {
			item["search_volume_competition"] = v
		}
	}
	if av, ok := item["ai_search_volume"].(map[string]any); ok {
		if v, ok := extractInt64(av, "search_volume"); ok {
			item["ai_search_volume_value"] = v
		}
		if v, ok := extractInt64(av, "search_volume_total"); ok {
			item["ai_search_volume_total"] = v
		}
	}
	return item
}

// prefixFields rewrites "a,b,c" as "p.a,p.b,p.c" for nested-projection fields=
// values (e.g. competitors.* off the domain detail endpoint).
func prefixFields(prefix, fields string) string {
	parts := strings.Split(fields, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, prefix+p)
		}
	}
	return strings.Join(out, ",")
}

func dedupKey(resource string, row map[string]any) string {
	switch resource {
	case "keyword-ranks":
		kid, _ := extractInt64(row, "keyword_id")
		date, _ := row["created_at"].(string)
		init, _ := row["is_initial"].(bool)
		return fmt.Sprintf("kr:%d:%s:%t", kid, date, init)
	case "domain-history":
		did, _ := extractInt64(row, "domain_id")
		date, _ := row["date"].(string)
		return fmt.Sprintf("dh:%d:%s", did, date)
	case "competitor-ranks":
		kid, _ := extractInt64(row, "keyword_id")
		cid, _ := extractInt64(row, "competitor_id")
		date, _ := row["search_date"].(string)
		return fmt.Sprintf("cr:%d:%d:%s", kid, cid, date)
	case "full-serp":
		kid, _ := extractInt64(row, "keyword_id")
		date, _ := row["search_date"].(string)
		return fmt.Sprintf("fs:%d:%s", kid, date)
	case "search-volume-history":
		kid, _ := extractInt64(row, "keyword_id")
		month, _ := row["month"].(string)
		return fmt.Sprintf("svh:%d:%s", kid, month)
	case "ai-search-volume-history":
		kid, _ := extractInt64(row, "keyword_id")
		month, _ := row["month"].(string)
		return fmt.Sprintf("asvh:%d:%s", kid, month)
	case "competitor-history":
		cid, _ := extractInt64(row, "competitor_id")
		date, _ := row["date"].(string)
		return fmt.Sprintf("ch:%d:%s", cid, date)
	case "landing-page-history":
		lid, _ := extractInt64(row, "landing_page_id")
		date, _ := row["date"].(string)
		return fmt.Sprintf("lph:%d:%s", lid, date)
	case "tag-history":
		did, _ := extractInt64(row, "domain_id")
		tag, _ := row["tag"].(string)
		date, _ := row["date"].(string)
		return fmt.Sprintf("th:%d:%s:%s", did, tag, date)
	case "keywords":
		id, _ := extractInt64(row, "id")
		return fmt.Sprintf("kw:%d", id)
	case "competitors":
		id, _ := extractInt64(row, "id")
		return fmt.Sprintf("cp:%d", id)
	case "landing-pages":
		id, _ := extractInt64(row, "id")
		return fmt.Sprintf("lp:%d", id)
	case "tags":
		did, _ := extractInt64(row, "domain_id")
		tag, _ := row["tag"].(string)
		return fmt.Sprintf("tg:%d:%s", did, tag)
	case "people-also-ask":
		kid, _ := extractInt64(row, "keyword_id")
		term, _ := row["term"].(string)
		return fmt.Sprintf("paa:%d:%s", kid, term)
	case "domains":
		id, _ := extractInt64(row, "id")
		return fmt.Sprintf("dm:%d", id)
	case "accounts":
		id, _ := extractInt64(row, "id")
		return fmt.Sprintf("ac:%d", id)
	case "groups":
		id, _ := extractInt64(row, "id")
		return fmt.Sprintf("gr:%d", id)
	case "brands":
		id, _ := extractInt64(row, "id")
		return fmt.Sprintf("br:%d", id)
	case "prompts":
		id, _ := extractInt64(row, "id")
		return fmt.Sprintf("pr:%d", id)
	case "prompt-results":
		pid, _ := extractInt64(row, "prompt_id")
		date, _ := row["search_date"].(string)
		return fmt.Sprintf("prr:%d:%s", pid, date)
	}
	b, _ := json.Marshal(row)
	return string(b)
}
