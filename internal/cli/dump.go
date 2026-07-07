// Hand-authored: dump command.
//
// Streams NDJSON for every warehouse-facing resource. Windowed resources
// (keyword-ranks, domain-history, competitor-ranks) walk the 100-day chunks
// AccuRanker imposes and dedup by natural key; snapshot resources (keywords,
// competitors, landing-pages, tags) emit the current dimension state. One
// record per line; ready for `psql COPY ... FROM STDIN` or any other
// line-oriented loader.
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
package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"accuranker-pp-cli/internal/client"
	"accuranker-pp-cli/internal/config"
	"accuranker-pp-cli/internal/schema"
)

const dumpAPIVersion = "v4"

// dumpWindowedResources require --from; each (domain, chunk) is one API call.
var dumpWindowedResources = map[string]bool{
	"keyword-ranks":    true,
	"domain-history":   true,
	"competitor-ranks": true,
}

// dumpSnapshotResources are current-state dimension dumps; --from is ignored.
var dumpSnapshotResources = map[string]bool{
	"keywords":      true,
	"competitors":   true,
	"landing-pages": true,
	"tags":          true,
}

func dumpResourceNames() string {
	return "keyword-ranks, domain-history, competitor-ranks (windowed); keywords, competitors, landing-pages, tags (snapshot)"
}

func newDumpCmd(flags *rootFlags) *cobra.Command {
	var (
		domainSpec    string
		resource      string
		from          string
		until         string
		windowDaysArg int
		schemaPath    string
		envelope      bool
		reportFile    string
	)
	cmd := &cobra.Command{
		Use:   "dump [resource]",
		Short: "Stream NDJSON for any resource over an arbitrary date window (auto-chunks the 100-day API cap)",
		Long: `Dump streams one NDJSON record per line to stdout for any resource family.

Windowed resources (require --from): keyword-ranks, domain-history,
competitor-ranks. The CLI walks the window in 100-day chunks internally and
dedups across chunk edges.

Snapshot resources (current dimension state, no window): keywords,
competitors, landing-pages, tags.

Every run emits exactly one machine-readable run report as a single JSON
object on stderr (or to --report-file). stdout stays pure NDJSON. The report
carries clean_exit and next_cursor: on a fully-successful run next_cursor is
the window end; on any partial failure the exit code is non-zero and
next_cursor is the last chunk boundary that completed for every requested
domain. A sync spine should advance its watermark from next_cursor only.

--envelope wraps each record for lossless raw landing:
  {"source":"accuranker","endpoint":…,"params_hash":…,"api_version":"v4",
   "fetched_at":…,"payload":{…}}`,
		Example: strings.Trim(`
  # Two years of rank history, NDJSON, into Postgres COPY
  accuranker-pp-cli dump keyword-ranks --domain 295242 --from 2024-01-01 --to 2026-05-20 \
    | psql -c '\COPY accuranker_ranks FROM STDIN CSV'

  # Daily keyword-dimension snapshot (panel diffing input for keywords-diff --against)
  accuranker-pp-cli dump keywords --domain 295242 > keywords-$(date +%F).ndjson

  # Competitor ranks for a quarter, raw envelope mode, report to file
  accuranker-pp-cli dump competitor-ranks --domain 295242 --from 2026-01-01 --to 2026-03-31 \
    --envelope --report-file /tmp/run-report.json
`, "\n"),
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if flags.dryRun {
				return nil
			}
			if len(args) == 0 {
				return cmd.Help()
			}
			resource = args[0]
			windowed := dumpWindowedResources[resource]
			if !windowed && !dumpSnapshotResources[resource] {
				if resource == "landing-page-history" {
					return fmt.Errorf("dump landing-page-history: use `mirror --resources landing_page_history` (per-page enumeration is store-aware)")
				}
				return fmt.Errorf("dump resource %q not supported; try: %s", resource, dumpResourceNames())
			}
			ids, err := parseInt64CSV(domainSpec)
			if err != nil {
				return fmt.Errorf("--domain: %w", err)
			}
			if len(ids) == 0 {
				return fmt.Errorf("--domain is required")
			}
			var ws, we string
			if windowed {
				if from == "" {
					return fmt.Errorf("--from is required (YYYY-MM-DD) for windowed resource %q", resource)
				}
				ws, we, err = resolveWindow(from, until)
				if err != nil {
					return err
				}
			} else {
				// Snapshot resources: the "window" is the fetch date.
				today := time.Now().UTC().Format("2006-01-02")
				ws, we = today, today
			}
			if windowDaysArg <= 0 || windowDaysArg > 100 {
				windowDaysArg = 100
			}
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

			opts := dumpOptions{
				resource:   resource,
				domainIDs:  ids,
				windowed:   windowed,
				ws:         ws,
				we:         we,
				windowDays: windowDaysArg,
				envelope:   envelope,
				reportFile: reportFile,
			}
			fetch := func(ctx context.Context, did int64, from, to string) (dumpBatch, error) {
				return dumpFetch(ctx, cl, model, resource, did, from, to)
			}
			return runDump(ctx, fetch, cmd.OutOrStdout(), cmd.ErrOrStderr(), opts, cl.Metrics)
		},
	}
	cmd.Flags().StringVar(&domainSpec, "domain", "", "Domain ID(s), comma-separated (required)")
	cmd.Flags().StringVar(&from, "from", "", "Window start (YYYY-MM-DD; required for windowed resources)")
	cmd.Flags().StringVar(&until, "to", "", "Window end (YYYY-MM-DD, default: today)")
	cmd.Flags().IntVar(&windowDaysArg, "window-days", 100, "Days per chunk (max 100)")
	cmd.Flags().StringVar(&schemaPath, "schema-file", "", "Override schema/model.yaml path")
	cmd.Flags().BoolVar(&envelope, "envelope", false, "Wrap each record in the raw-landing envelope {source, endpoint, params_hash, api_version, fetched_at, payload}")
	cmd.Flags().StringVar(&reportFile, "report-file", "", "Write the run report JSON to this path instead of stderr")
	return cmd
}

// dumpOptions carries everything runDump needs besides the fetcher.
type dumpOptions struct {
	resource   string
	domainIDs  []int64
	windowed   bool
	ws, we     string
	windowDays int
	envelope   bool
	reportFile string
}

// dumpBatch is one API call's worth of rows plus the request metadata the
// envelope (F3) and report (F1) need.
type dumpBatch struct {
	rows     []map[string]any
	endpoint string
	params   url.Values
}

// dumpFetchFn abstracts dumpFetch for tests.
type dumpFetchFn func(ctx context.Context, did int64, from, to string) (dumpBatch, error)

// dumpReportWindow is the requested window in the run report.
type dumpReportWindow struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// dumpRunReport is the F1 watermark handshake. One per run, stderr or
// --report-file, never stdout.
type dumpRunReport struct {
	Resource      string           `json:"resource"`
	DomainID      int64            `json:"domain_id,omitempty"`
	DomainIDs     []int64          `json:"domain_ids,omitempty"`
	Window        dumpReportWindow `json:"window"`
	RowsEmitted   int64            `json:"rows_emitted"`
	RequestsMade  int64            `json:"requests_made"`
	RateLimitHits int64            `json:"rate_limit_hits"`
	APIVersion    string           `json:"api_version"`
	StartedAt     string           `json:"started_at"`
	FinishedAt    string           `json:"finished_at"`
	CleanExit     bool             `json:"clean_exit"`
	NextCursor    string           `json:"next_cursor"`
	Error         string           `json:"error,omitempty"`
}

func runDump(ctx context.Context, fetch dumpFetchFn, out, errOut io.Writer, opts dumpOptions, metrics func() (int64, int64)) error {
	rep := &dumpRunReport{
		Resource:   opts.resource,
		Window:     dumpReportWindow{From: opts.ws, To: opts.we},
		APIVersion: dumpAPIVersion,
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	if len(opts.domainIDs) == 1 {
		rep.DomainID = opts.domainIDs[0]
	} else {
		rep.DomainIDs = opts.domainIDs
	}

	runErr := dumpAllChunks(ctx, fetch, out, opts, rep)

	rep.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	rep.CleanExit = runErr == nil
	if metrics != nil {
		rep.RequestsMade, rep.RateLimitHits = metrics()
	}
	if runErr != nil {
		rep.Error = runErr.Error()
	}
	if err := writeDumpReport(rep, errOut, opts.reportFile); err != nil {
		if runErr == nil {
			return err
		}
		fmt.Fprintf(errOut, "warning: writing run report failed: %v\n", err)
	}
	return runErr
}

// dumpAllChunks walks chunk-outer/domain-inner so "the last chunk boundary
// completed for ALL domains" — the report's next_cursor — is well-defined
// even for multi-domain runs. rep.RowsEmitted and rep.NextCursor are updated
// as the walk progresses so a partial failure reports honest progress.
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
		for _, did := range opts.domainIDs {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("dump %s domain=%d %s..%s: interrupted: %w", opts.resource, did, chunk[0], chunk[1], err)
			}
			batch, err := fetch(ctx, did, chunk[0], chunk[1])
			if err != nil {
				return fmt.Errorf("dump %s domain=%d %s..%s: %w", opts.resource, did, chunk[0], chunk[1], err)
			}
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
		// Chunk completed for every domain — this boundary is safe to
		// advance a watermark to.
		rep.NextCursor = chunk[1]
	}
	return nil
}

// envelopeRecord wraps a row in the lossless raw-landing envelope (spec F3).
// payload is the emitted row: the API object plus the stamped parent ids
// (domain_id, keyword_id, …) that preserve the nesting context the flat
// NDJSON stream would otherwise lose.
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
// record: sha256 over the endpoint plus the canonically-encoded (sorted)
// query string.
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

func dumpFetch(ctx context.Context, cl *client.Client, m *schema.Model, resource string, did int64, from, to string) (dumpBatch, error) {
	switch resource {
	case "keyword-ranks":
		q := url.Values{}
		q.Set("fields", "id,"+m.DefaultFields("keyword_ranks"))
		q.Set("period_from", from)
		q.Set("period_to", to)
		endpoint := fmt.Sprintf("/api/v4/domains/%d/keywords/", did)
		body, err := apiGet(ctx, cl, endpoint, q)
		if err != nil {
			return dumpBatch{}, err
		}
		var kw []struct {
			ID    int64            `json:"id"`
			Ranks []map[string]any `json:"ranks"`
		}
		if err := json.Unmarshal(body, &kw); err != nil {
			return dumpBatch{}, err
		}
		out := make([]map[string]any, 0, 256)
		for _, k := range kw {
			for _, r := range k.Ranks {
				r["keyword_id"] = k.ID
				r["domain_id"] = did
				out = append(out, r)
			}
		}
		return dumpBatch{rows: out, endpoint: endpoint, params: q}, nil

	case "domain-history":
		q := url.Values{}
		q.Set("fields", m.DefaultFields("domain_history"))
		q.Set("period_from", from)
		q.Set("period_to", to)
		endpoint := fmt.Sprintf("/api/v4/domains/%d/", did)
		body, err := apiGet(ctx, cl, endpoint, q)
		if err != nil {
			return dumpBatch{}, err
		}
		var doc struct {
			History []map[string]any `json:"history"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			return dumpBatch{}, err
		}
		for _, h := range doc.History {
			h["domain_id"] = did
		}
		return dumpBatch{rows: doc.History, endpoint: endpoint, params: q}, nil

	case "competitor-ranks":
		// PATCH(amend-2026-07-07: uniform dump surface — spec F2): the
		// modelled-but-unwired keyword_competitor_ranks family, windowed
		// like ranks. Rows land in accuranker_keyword_competitor_ranks.
		q := url.Values{}
		q.Set("fields", "id,"+m.DefaultFields("keyword_competitor_ranks"))
		q.Set("period_from", from)
		q.Set("period_to", to)
		endpoint := fmt.Sprintf("/api/v4/domains/%d/keywords/", did)
		body, err := apiGet(ctx, cl, endpoint, q)
		if err != nil {
			return dumpBatch{}, err
		}
		var kw []struct {
			ID              int64            `json:"id"`
			CompetitorRanks []map[string]any `json:"competitor_ranks"`
		}
		if err := json.Unmarshal(body, &kw); err != nil {
			return dumpBatch{}, err
		}
		out := make([]map[string]any, 0, 256)
		for _, k := range kw {
			for _, r := range k.CompetitorRanks {
				r["keyword_id"] = k.ID
				r["domain_id"] = did
				if comp, ok := r["competitor"].(map[string]any); ok {
					if cid, ok := extractInt64(comp, "id"); ok {
						r["competitor_id"] = cid
					}
				}
				if cAt, ok := r["created_at"].(string); ok && len(cAt) >= 10 {
					r["search_date"] = cAt[:10]
				}
				out = append(out, r)
			}
		}
		return dumpBatch{rows: out, endpoint: endpoint, params: q}, nil

	case "keywords":
		// PATCH(amend-2026-07-07: uniform dump surface — spec F2): full
		// keyword-dimension snapshot; cheap, designed to run daily as the
		// panel-diffing input for `keywords-diff --against`.
		q := url.Values{}
		q.Set("fields", m.DefaultFields("keywords"))
		endpoint := fmt.Sprintf("/api/v4/domains/%d/keywords/", did)
		body, err := apiGet(ctx, cl, endpoint, q)
		if err != nil {
			return dumpBatch{}, err
		}
		var raw []map[string]any
		if err := json.Unmarshal(body, &raw); err != nil {
			return dumpBatch{}, err
		}
		rows := make([]map[string]any, 0, len(raw))
		for _, item := range raw {
			rows = append(rows, flattenKeywordRow(item, did))
		}
		return dumpBatch{rows: rows, endpoint: endpoint, params: q}, nil

	case "competitors":
		// PATCH(amend-2026-07-07: uniform dump surface — spec F2): the
		// competitor dimension, projected from the domain detail endpoint.
		// Rows land in accuranker_competitors with no schema edits.
		q := url.Values{}
		q.Set("fields", prefixFields("competitors.", m.DefaultFields("competitors")))
		endpoint := fmt.Sprintf("/api/v4/domains/%d/", did)
		body, err := apiGet(ctx, cl, endpoint, q)
		if err != nil {
			return dumpBatch{}, err
		}
		var doc struct {
			Competitors []map[string]any `json:"competitors"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			return dumpBatch{}, err
		}
		for _, c := range doc.Competitors {
			c["domain_id"] = did
		}
		return dumpBatch{rows: doc.Competitors, endpoint: endpoint, params: q}, nil

	case "landing-pages":
		// Live-API note (verified 2026-07-07): the landing_pages list
		// endpoint returns `path` per row on this tier — the modelled
		// id/landing_page/title projection is silently ignored. Request
		// both spellings and alias path→landing_page so rows stay useful
		// whichever shape the tier returns. (Full drift detection is the
		// deferred `doctor --warehouse` item.)
		q := url.Values{}
		q.Set("fields", "id,path,landing_page,title")
		endpoint := fmt.Sprintf("/api/v4/domains/%d/landing_pages/", did)
		body, err := apiGet(ctx, cl, endpoint, q)
		if err != nil {
			return dumpBatch{}, err
		}
		var raw []map[string]any
		if err := json.Unmarshal(body, &raw); err != nil {
			return dumpBatch{}, err
		}
		for _, item := range raw {
			item["domain_id"] = did
			if _, ok := item["landing_page"]; !ok {
				if p, ok := item["path"].(string); ok {
					item["landing_page"] = p
				}
			}
		}
		return dumpBatch{rows: raw, endpoint: endpoint, params: q}, nil

	case "tags":
		q := url.Values{}
		q.Set("fields", "tag")
		endpoint := fmt.Sprintf("/api/v4/domains/%d/tags/", did)
		body, err := apiGet(ctx, cl, endpoint, q)
		if err != nil {
			return dumpBatch{}, err
		}
		var raw []map[string]any
		if err := json.Unmarshal(body, &raw); err != nil {
			return dumpBatch{}, err
		}
		// The API prepends an untagged-bucket row {"tag": null}
		// (verified live 2026-07-07). It is an aggregate artifact, not a
		// tag — drop it so rows key cleanly on (domain_id, tag).
		rows := make([]map[string]any, 0, len(raw))
		for _, item := range raw {
			if t, ok := item["tag"].(string); !ok || t == "" {
				continue
			}
			item["domain_id"] = did
			rows = append(rows, item)
		}
		return dumpBatch{rows: rows, endpoint: endpoint, params: q}, nil

	default:
		return dumpBatch{}, fmt.Errorf("dump resource %q not supported; try: %s", resource, dumpResourceNames())
	}
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

// prefixFields rewrites "a,b,c" as "p.a,p.b,p.c" for nested-projection
// fields= values (e.g. competitors.* off the domain detail endpoint).
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
		return fmt.Sprintf("kr:%d:%s", kid, date)
	case "domain-history":
		did, _ := extractInt64(row, "domain_id")
		date, _ := row["date"].(string)
		return fmt.Sprintf("dh:%d:%s", did, date)
	case "competitor-ranks":
		kid, _ := extractInt64(row, "keyword_id")
		cid, _ := extractInt64(row, "competitor_id")
		date, _ := row["search_date"].(string)
		return fmt.Sprintf("cr:%d:%d:%s", kid, cid, date)
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
	}
	b, _ := json.Marshal(row)
	return string(b)
}
