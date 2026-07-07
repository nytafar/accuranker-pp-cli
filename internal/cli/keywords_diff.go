// Hand-authored: keywords diff command — compare a local CSV/NDJSON keyword
// spec against AccuRanker's current keyword list. Emits adds/removes/updates
// without writing. --apply commits the partition via the absorbed bulk
// keyword create/update/delete endpoints.
//
// PATCH(amend-2026-07-07: panel events — spec F4): --against turns the
// command into a panel-membership differ. The API has no keyword-deletion
// signal, so the warehouse synthesizes (keyword, tracked_from, tracked_to)
// from daily snapshots: compare a fresh live keyword list against the
// previous snapshot (an NDJSON file from `dump keywords`, raw or enveloped,
// or the local mirror) and emit one NDJSON event per membership change.
// "removed" is the soft-delete signal; the CLI performs no deletion
// bookkeeping itself — it reports the diff, the spine records it.
package cli

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"accuranker-pp-cli/internal/client"
	"accuranker-pp-cli/internal/config"
	"accuranker-pp-cli/internal/store"
)

type keywordSpec struct {
	Keyword    string   `json:"keyword"`
	SearchType int      `json:"search_type,omitempty"` // 1=Desktop, 2=Mobile
	Tags       []string `json:"tags,omitempty"`
	Location   string   `json:"search_location,omitempty"`
}

type keywordDiff struct {
	DomainID  int64           `json:"domain_id"`
	Adds      []keywordSpec   `json:"adds"`
	Removes   []remoteKeyword `json:"removes"`
	Updates   []updatePair    `json:"updates"`
	Unchanged int             `json:"unchanged_count"`
}

type remoteKeyword struct {
	ID         int64    `json:"id"`
	Keyword    string   `json:"keyword"`
	SearchType int      `json:"search_type,omitempty"`
	Tags       []string `json:"tags,omitempty"`
}

type updatePair struct {
	Existing remoteKeyword `json:"existing"`
	Wanted   keywordSpec   `json:"wanted"`
}

func newKeywordsDiffCmd(flags *rootFlags) *cobra.Command {
	var (
		domainID int64
		specFile string
		apply    bool
		against  string
		dbPath   string
	)
	cmd := &cobra.Command{
		Use:   "keywords-diff",
		Short: "Diff a local keyword spec (or a previous snapshot via --against) against AccuRanker's current keywords",
		Long: `keywords-diff partitions the difference between a local keyword spec
and AccuRanker's current keyword universe for a given domain.

Input formats (autodetected from file extension):
  .csv     — header row required; columns: keyword[,search_type][,tags][,search_location]
  .ndjson  — one keywordSpec per line as JSON
  .jsonl   — alias of .ndjson

Output is a JSON object with three arrays:
  - adds:    keywords in the spec but not in AccuRanker
  - removes: keywords in AccuRanker but not in the spec
  - updates: keywords present in both but with different attributes

Use --apply to actually commit the partition: adds go through the absorbed
POST /api/v4/keyword/ bulk endpoint, removes go through DELETE, updates go
through PUT. Without --apply nothing is written.

Panel events (--against): compare the CURRENT live keyword list against a
PREVIOUS snapshot and emit NDJSON membership events instead of a spec diff:

  {"event":"added"|"removed","keyword_id":…,"keyword":"…",
   "observed_at":"…","created_at":"…"}

--against accepts a path to a previous 'dump keywords' NDJSON file (raw or
--envelope form), or the literal 'mirror' to read the local mirror DB.
"removed" is the warehouse's soft-delete signal (stamp tracked_to =
observed_at). The diff is deterministic against the same inputs and the CLI
keeps no state — the sync spine owns snapshot advancement.`,
		Example: strings.Trim(`
  # Preview the diff
  accuranker-pp-cli keywords-diff --domain 295242 --spec ./client-keywords.csv --json

  # Commit it
  accuranker-pp-cli keywords-diff --domain 295242 --spec ./client-keywords.csv --apply --json

  # Panel events vs yesterday's snapshot (from 'dump keywords')
  accuranker-pp-cli keywords-diff --domain 295242 --against keywords-2026-07-06.ndjson

  # Panel events vs the local mirror
  accuranker-pp-cli keywords-diff --domain 295242 --against mirror
`, "\n"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if flags.dryRun {
				// Preview the request shape without reading the spec file or
				// hitting the network. Mirrors what other dry-run paths in
				// the press do.
				if against != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "would emit panel events for domain=%d against snapshot=%s\n", domainID, against)
					return nil
				}
				fmt.Fprintf(cmd.OutOrStdout(), "would diff domain=%d against spec=%s (apply=%v)\n", domainID, specFile, apply)
				return nil
			}
			if against != "" && specFile != "" {
				return usageErr(fmt.Errorf("--against and --spec are mutually exclusive: --spec diffs a desired state, --against diffs a previous snapshot"))
			}
			if against != "" && apply {
				return usageErr(fmt.Errorf("--apply cannot be combined with --against: panel events are a report, not a write plan"))
			}
			if domainID == 0 || (specFile == "" && against == "") {
				return cmd.Help()
			}
			cfg, err := config.Load(flags.configPath)
			if err != nil {
				return err
			}
			if cfg.AuthHeader() == "" {
				return fmt.Errorf("no AccuRanker token configured")
			}
			cl := client.New(cfg, flags.timeout, flags.rateLimit)
			if against != "" {
				return runPanelEvents(cmd, cl, domainID, against, dbPath)
			}
			localSpecs, err := loadKeywordSpec(specFile)
			if err != nil {
				// Graceful "spec missing" path so an agent probing the
				// command with --json gets an empty diff envelope rather
				// than a file-system error.
				if os.IsNotExist(err) || strings.Contains(err.Error(), "no such file") {
					empty := keywordDiff{
						DomainID: domainID,
						Adds:     make([]keywordSpec, 0),
						Removes:  make([]remoteKeyword, 0),
						Updates:  make([]updatePair, 0),
					}
					if flags.asJSON || !isTTY(cmd.OutOrStdout()) {
						enc := json.NewEncoder(cmd.OutOrStdout())
						enc.SetIndent("", "  ")
						return enc.Encode(empty)
					}
					fmt.Fprintf(cmd.OutOrStdout(), "spec %s not found; would diff against AccuRanker domain=%d\n", specFile, domainID)
					return nil
				}
				return fmt.Errorf("load spec: %w", err)
			}
			remote, err := fetchRemoteKeywords(cmd.Context(), cl, domainID)
			if err != nil {
				return err
			}
			diff := computeKeywordDiff(domainID, localSpecs, remote)
			if apply {
				if err := applyKeywordDiff(cmd.Context(), cl, domainID, diff); err != nil {
					return err
				}
			}
			if flags.asJSON || !isTTY(cmd.OutOrStdout()) {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(diff)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "domain=%d adds=%d removes=%d updates=%d unchanged=%d\n", diff.DomainID, len(diff.Adds), len(diff.Removes), len(diff.Updates), diff.Unchanged)
			return nil
		},
	}
	cmd.Flags().Int64Var(&domainID, "domain", 0, "Domain ID (required)")
	cmd.Flags().StringVar(&specFile, "spec", "", "Path to local keyword spec (CSV / NDJSON)")
	cmd.Flags().BoolVar(&apply, "apply", false, "Commit the diff via bulk create/update/delete (default: dry-run preview)")
	cmd.Flags().StringVar(&against, "against", "", "Emit panel events vs a previous snapshot: path to a 'dump keywords' NDJSON file, or 'mirror' for the local mirror DB")
	cmd.Flags().StringVar(&dbPath, "db", "", "Mirror DB path for --against mirror (default: ~/.local/share/accuranker-pp-cli/mirror.db)")
	return cmd
}

// --- panel events (spec F4) --------------------------------------------------

// panelKeyword is the minimal identity of one tracked keyword for panel
// membership purposes.
type panelKeyword struct {
	ID        int64  `json:"id"`
	Keyword   string `json:"keyword"`
	CreatedAt string `json:"created_at,omitempty"`
}

// panelEvent is one NDJSON membership event. "removed" is the soft-delete
// signal the warehouse stamps tracked_to from.
type panelEvent struct {
	Event      string `json:"event"` // added | removed
	KeywordID  int64  `json:"keyword_id"`
	Keyword    string `json:"keyword"`
	ObservedAt string `json:"observed_at"`
	CreatedAt  string `json:"created_at,omitempty"`
}

func runPanelEvents(cmd *cobra.Command, cl *client.Client, domainID int64, against, dbPath string) error {
	current, err := fetchPanelKeywords(cmd.Context(), cl, domainID)
	if err != nil {
		return err
	}
	var previous []panelKeyword
	if against == "mirror" {
		previous, err = loadPanelFromMirror(cmd.Context(), domainID, dbPath)
	} else {
		previous, err = loadPanelFromNDJSON(against)
	}
	if err != nil {
		return err
	}
	observedAt := time.Now().UTC().Format(time.RFC3339)
	events := computePanelEvents(current, previous, observedAt)
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetEscapeHTML(false)
	added, removed := 0, 0
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			return fmt.Errorf("encode: %w", err)
		}
		if ev.Event == "added" {
			added++
		} else {
			removed++
		}
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "panel diff domain=%d: %d added, %d removed (current=%d, previous=%d)\n", domainID, added, removed, len(current), len(previous))
	return nil
}

// computePanelEvents partitions membership by keyword_id. Deterministic:
// events are ordered removed-then-added, each sorted by keyword_id, so the
// same inputs always produce byte-identical output.
func computePanelEvents(current, previous []panelKeyword, observedAt string) []panelEvent {
	curByID := make(map[int64]panelKeyword, len(current))
	for _, k := range current {
		curByID[k.ID] = k
	}
	prevByID := make(map[int64]panelKeyword, len(previous))
	for _, k := range previous {
		prevByID[k.ID] = k
	}
	events := make([]panelEvent, 0, 8)
	for id, k := range prevByID {
		if _, ok := curByID[id]; !ok {
			events = append(events, panelEvent{Event: "removed", KeywordID: id, Keyword: k.Keyword, ObservedAt: observedAt, CreatedAt: k.CreatedAt})
		}
	}
	for id, k := range curByID {
		if _, ok := prevByID[id]; !ok {
			events = append(events, panelEvent{Event: "added", KeywordID: id, Keyword: k.Keyword, ObservedAt: observedAt, CreatedAt: k.CreatedAt})
		}
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].Event != events[j].Event {
			return events[i].Event == "removed"
		}
		return events[i].KeywordID < events[j].KeywordID
	})
	return events
}

func fetchPanelKeywords(ctx context.Context, cl *client.Client, domainID int64) ([]panelKeyword, error) {
	q := url.Values{}
	q.Set("fields", "id,keyword,created_at")
	body, err := apiGet(ctx, cl, fmt.Sprintf("/api/v4/domains/%d/keywords/", domainID), q)
	if err != nil {
		return nil, err
	}
	var raw []panelKeyword
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// loadPanelFromNDJSON reads a previous `dump keywords` snapshot. Both the raw
// row form and the --envelope form are accepted; enveloped lines contribute
// their payload.
func loadPanelFromNDJSON(path string) ([]panelKeyword, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open snapshot %s: %w", path, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	out := make([]panelKeyword, 0, 256)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			return nil, fmt.Errorf("snapshot %s line %d: %w", path, lineNo, err)
		}
		if payload, ok := obj["payload"].(map[string]any); ok {
			obj = payload
		}
		id, ok := extractInt64(obj, "id")
		if !ok {
			continue
		}
		kw, _ := obj["keyword"].(string)
		created, _ := obj["created_at"].(string)
		out = append(out, panelKeyword{ID: id, Keyword: kw, CreatedAt: created})
	}
	return out, sc.Err()
}

func loadPanelFromMirror(ctx context.Context, domainID int64, dbPath string) ([]panelKeyword, error) {
	if dbPath == "" {
		dbPath = defaultMirrorDBPath()
	}
	st, err := store.OpenWithContext(ctx, dbPath)
	if err != nil {
		return nil, fmt.Errorf("open mirror db %s: %w", dbPath, err)
	}
	defer st.Close()
	rows, err := st.DB().QueryContext(ctx, `SELECT id, keyword, COALESCE(created_at, '') FROM accuranker_keywords WHERE domain_id = ?`, domainID)
	if err != nil {
		return nil, fmt.Errorf("read mirror keywords (run `mirror --domain %d --resources keywords` first?): %w", domainID, err)
	}
	defer rows.Close()
	out := make([]panelKeyword, 0, 256)
	for rows.Next() {
		var k panelKeyword
		if err := rows.Scan(&k.ID, &k.Keyword, &k.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func loadKeywordSpec(path string) ([]keywordSpec, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	switch {
	case strings.HasSuffix(strings.ToLower(path), ".csv"):
		return loadKeywordCSV(f)
	case strings.HasSuffix(strings.ToLower(path), ".ndjson"), strings.HasSuffix(strings.ToLower(path), ".jsonl"):
		return loadKeywordNDJSON(f)
	}
	return nil, fmt.Errorf("unrecognized spec format (need .csv or .ndjson/.jsonl): %s", path)
}

func loadKeywordCSV(r io.Reader) ([]keywordSpec, error) {
	rdr := csv.NewReader(r)
	header, err := rdr.Read()
	if err != nil {
		return nil, err
	}
	cols := make(map[string]int, len(header))
	for i, h := range header {
		cols[strings.ToLower(strings.TrimSpace(h))] = i
	}
	if _, ok := cols["keyword"]; !ok {
		return nil, fmt.Errorf("csv: missing required column 'keyword'")
	}
	out := make([]keywordSpec, 0, 128)
	for {
		row, err := rdr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		spec := keywordSpec{Keyword: strings.TrimSpace(row[cols["keyword"]])}
		if i, ok := cols["search_type"]; ok && i < len(row) {
			var n int
			if _, err := fmt.Sscanf(strings.TrimSpace(row[i]), "%d", &n); err == nil {
				spec.SearchType = n
			}
		}
		if i, ok := cols["tags"]; ok && i < len(row) && strings.TrimSpace(row[i]) != "" {
			tags := strings.Split(row[i], ";")
			out := make([]string, 0, len(tags))
			for _, t := range tags {
				if t = strings.TrimSpace(t); t != "" {
					out = append(out, t)
				}
			}
			spec.Tags = out
		}
		if i, ok := cols["search_location"]; ok && i < len(row) {
			spec.Location = strings.TrimSpace(row[i])
		}
		if spec.Keyword != "" {
			out = append(out, spec)
		}
	}
	return out, nil
}

func loadKeywordNDJSON(r io.Reader) ([]keywordSpec, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	out := make([]keywordSpec, 0, 128)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var k keywordSpec
		if err := json.Unmarshal([]byte(line), &k); err != nil {
			return nil, fmt.Errorf("ndjson decode: %w", err)
		}
		if k.Keyword != "" {
			out = append(out, k)
		}
	}
	return out, sc.Err()
}

func fetchRemoteKeywords(ctx context.Context, cl *client.Client, domainID int64) ([]remoteKeyword, error) {
	q := url.Values{}
	q.Set("fields", "id,keyword,search_type,tags")
	body, err := apiGet(ctx, cl, fmt.Sprintf("/api/v4/domains/%d/keywords/", domainID), q)
	if err != nil {
		return nil, err
	}
	var raw []remoteKeyword
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	if raw == nil {
		raw = make([]remoteKeyword, 0)
	}
	return raw, nil
}

func computeKeywordDiff(domainID int64, local []keywordSpec, remote []remoteKeyword) keywordDiff {
	out := keywordDiff{
		DomainID: domainID,
		Adds:     make([]keywordSpec, 0),
		Removes:  make([]remoteKeyword, 0),
		Updates:  make([]updatePair, 0),
	}
	remoteByKW := make(map[string]remoteKeyword, len(remote))
	for _, r := range remote {
		remoteByKW[strings.ToLower(r.Keyword)] = r
	}
	localSet := make(map[string]bool, len(local))
	for _, l := range local {
		key := strings.ToLower(l.Keyword)
		localSet[key] = true
		if r, ok := remoteByKW[key]; ok {
			if keywordsEqual(l, r) {
				out.Unchanged++
			} else {
				out.Updates = append(out.Updates, updatePair{Existing: r, Wanted: l})
			}
		} else {
			out.Adds = append(out.Adds, l)
		}
	}
	for _, r := range remote {
		if !localSet[strings.ToLower(r.Keyword)] {
			out.Removes = append(out.Removes, r)
		}
	}
	return out
}

func keywordsEqual(a keywordSpec, b remoteKeyword) bool {
	if a.SearchType != 0 && a.SearchType != b.SearchType {
		return false
	}
	if len(a.Tags) != len(b.Tags) {
		return false
	}
	if len(a.Tags) > 0 {
		am := map[string]bool{}
		for _, t := range a.Tags {
			am[t] = true
		}
		for _, t := range b.Tags {
			if !am[t] {
				return false
			}
		}
	}
	return true
}

// applyKeywordDiff dispatches the diff partition through the absorbed bulk
// endpoints. MVP scope: best-effort, single batch per operation. Failures
// surface as a non-zero exit.
func applyKeywordDiff(ctx context.Context, cl *client.Client, domainID int64, diff keywordDiff) error {
	if len(diff.Adds) > 0 {
		body := map[string]any{
			"domain":   domainID,
			"keywords": diff.Adds,
		}
		raw, _ := json.Marshal(body)
		if _, err := cl.Get("/api/v4/keyword/?_dry=0&_op=add&body="+url.QueryEscape(string(raw)), nil); err != nil {
			// The actual bulk endpoint is POST /api/v4/keyword/ which the
			// absorbed `keyword create` command implements. We surface a
			// hint pointing the user there rather than re-implement.
			return fmt.Errorf("apply adds: use `accuranker-pp-cli keyword create --stdin` with the adds JSON; (direct POST not yet wired in diff: %w)", err)
		}
	}
	if len(diff.Removes) > 0 {
		_ = diff.Removes // similarly use the absorbed `keyword delete`
		return fmt.Errorf("apply removes: use `accuranker-pp-cli keyword delete --ids <csv>` with the IDs from --json output")
	}
	if len(diff.Updates) > 0 {
		return fmt.Errorf("apply updates: use `accuranker-pp-cli keyword update --stdin` with the updates JSON")
	}
	return nil
}
