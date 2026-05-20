// Hand-authored: serp-features delta — partition keywords by gained/lost/held
// SERP feature membership between two dates. Works only against the local
// mirror (server-side AccuRanker filter answers "has feature now," not
// "gained/lost between").
package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func newSerpFeaturesCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:         "serp-features",
		Short:       "Local SQL queries over synced SERP-feature data (AI Overview, featured snippets, etc.)",
		Annotations: map[string]string{"mcp:read-only": "true"},
	}
	cmd.AddCommand(newSerpFeaturesDeltaCmd(flags))
	return cmd
}

func newSerpFeaturesDeltaCmd(flags *rootFlags) *cobra.Command {
	var (
		domainID  int64
		feature   string
		from      string
		to        string
		dbPath    string
		partition string
	)
	cmd := &cobra.Command{
		Use:   "delta",
		Short: "Partition keywords by gained/lost/held SERP feature membership between two dates",
		Long: `serp-features delta is the answer to "which keywords gained an AI Overview
in the last 14 days?" — a question AccuRanker's server-side filters cannot
answer directly (their ` + "`has_ai_overview`" + ` filter is point-in-time).

This command reads two snapshots of accuranker_keyword_ranks from the local
mirror, intersects the keywords that had the feature on date A vs date B,
and partitions them into gained / lost / held.

Run 'mirror' first to populate the local store.`,
		Example: strings.Trim(`
  # AI Overview wins in the last 14 days
  accuranker-pp-cli serp-features delta --domain 295242 --feature ai_overview --from 2026-05-06 --to 2026-05-20 --json

  # Local pack losses
  accuranker-pp-cli serp-features delta --domain 295242 --feature local_pack --from 2026-04-01 --to 2026-05-01 --partition lost
`, "\n"),
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if flags.dryRun {
				return nil
			}
			if domainID == 0 || feature == "" || from == "" || to == "" {
				return cmd.Help()
			}
			st, err := openLocalDB(dbPath)
			if err != nil {
				return err
			}
			defer st.Close()
			result, err := computeSerpDelta(cmd.Context(), st.DB(), domainID, feature, from, to)
			if err != nil {
				if strings.Contains(err.Error(), "no such table") {
					result = &serpDelta{Feature: feature, FromDate: from, ToDate: to,
						Gained: make([]serpKeyKey, 0), Lost: make([]serpKeyKey, 0), Held: make([]serpKeyKey, 0)}
				} else {
					return err
				}
			}
			return emitSerpDelta(cmd, result, partition, flags)
		},
	}
	cmd.Flags().Int64Var(&domainID, "domain", 0, "Domain ID (required)")
	cmd.Flags().StringVar(&feature, "feature", "", "SERP feature name (e.g. ai_overview, local_pack, featured_snippet)")
	cmd.Flags().StringVar(&from, "from", "", "Snapshot A date (YYYY-MM-DD)")
	cmd.Flags().StringVar(&to, "to", "", "Snapshot B date (YYYY-MM-DD)")
	cmd.Flags().StringVar(&dbPath, "db", "", "Local SQLite DB path (default: ~/.local/share/accuranker-pp-cli/mirror.db)")
	cmd.Flags().StringVar(&partition, "partition", "all", "Output partition: all | gained | lost | held")
	return cmd
}

type serpDelta struct {
	Feature  string       `json:"feature"`
	FromDate string       `json:"from"`
	ToDate   string       `json:"to"`
	Gained   []serpKeyKey `json:"gained"`
	Lost     []serpKeyKey `json:"lost"`
	Held     []serpKeyKey `json:"held"`
	GainedN  int          `json:"gained_count"`
	LostN    int          `json:"lost_count"`
	HeldN    int          `json:"held_count"`
}

type serpKeyKey struct {
	KeywordID int64  `json:"keyword_id"`
	Keyword   string `json:"keyword"`
}

func computeSerpDelta(ctx context.Context, db *sql.DB, domainID int64, feature, from, to string) (*serpDelta, error) {
	// Each accuranker_keyword_ranks row has a JSON column page_serp_features.
	// We use json_extract to read the membership flag for the requested feature.
	// AccuRanker stores page_serp_features as an object whose keys are the
	// feature names; we accept either a boolean true OR a non-null object.
	q := `
WITH a AS (
  SELECT kr.keyword_id, k.keyword,
         json_extract(kr.page_serp_features, '$.' || ?) AS feat
  FROM accuranker_keyword_ranks kr
  JOIN accuranker_keywords k ON k.id = kr.keyword_id
  WHERE k.domain_id = ? AND kr.search_date = ?
),
b AS (
  SELECT kr.keyword_id, k.keyword,
         json_extract(kr.page_serp_features, '$.' || ?) AS feat
  FROM accuranker_keyword_ranks kr
  JOIN accuranker_keywords k ON k.id = kr.keyword_id
  WHERE k.domain_id = ? AND kr.search_date = ?
)
SELECT
  COALESCE(b.keyword_id, a.keyword_id) AS kid,
  COALESCE(b.keyword, a.keyword)       AS kw,
  CASE WHEN a.feat IS NOT NULL AND b.feat IS NOT NULL THEN 'held'
       WHEN a.feat IS NULL    AND b.feat IS NOT NULL THEN 'gained'
       WHEN a.feat IS NOT NULL AND b.feat IS NULL    THEN 'lost'
       ELSE 'neither' END AS bucket
FROM a FULL OUTER JOIN b ON a.keyword_id = b.keyword_id
WHERE COALESCE(a.feat, b.feat) IS NOT NULL`
	// SQLite has no FULL OUTER JOIN; emulate with two LEFT JOINs unioned.
	// Rewrite query for SQLite compatibility.
	q = `
WITH a AS (
  SELECT kr.keyword_id, k.keyword,
         json_extract(kr.page_serp_features, '$.' || ?) AS feat
  FROM accuranker_keyword_ranks kr
  JOIN accuranker_keywords k ON k.id = kr.keyword_id
  WHERE k.domain_id = ? AND kr.search_date = ?
),
b AS (
  SELECT kr.keyword_id, k.keyword,
         json_extract(kr.page_serp_features, '$.' || ?) AS feat
  FROM accuranker_keyword_ranks kr
  JOIN accuranker_keywords k ON k.id = kr.keyword_id
  WHERE k.domain_id = ? AND kr.search_date = ?
)
SELECT a.keyword_id, a.keyword, a.feat AS a_feat, b.feat AS b_feat
  FROM a LEFT JOIN b ON a.keyword_id = b.keyword_id
UNION
SELECT b.keyword_id, b.keyword, a.feat AS a_feat, b.feat AS b_feat
  FROM b LEFT JOIN a ON a.keyword_id = b.keyword_id
  WHERE a.keyword_id IS NULL`
	rows, err := db.QueryContext(ctx, q, feature, domainID, from, feature, domainID, to)
	if err != nil {
		return nil, fmt.Errorf("query serp-features delta: %w", err)
	}
	defer rows.Close()
	out := &serpDelta{Feature: feature, FromDate: from, ToDate: to}
	out.Gained = make([]serpKeyKey, 0)
	out.Lost = make([]serpKeyKey, 0)
	out.Held = make([]serpKeyKey, 0)
	for rows.Next() {
		var (
			kid          int64
			kw           string
			aFeat, bFeat sql.NullString
		)
		if err := rows.Scan(&kid, &kw, &aFeat, &bFeat); err != nil {
			return nil, err
		}
		entry := serpKeyKey{KeywordID: kid, Keyword: kw}
		hasA := aFeat.Valid && aFeat.String != "" && aFeat.String != "null"
		hasB := bFeat.Valid && bFeat.String != "" && bFeat.String != "null"
		switch {
		case !hasA && hasB:
			out.Gained = append(out.Gained, entry)
		case hasA && !hasB:
			out.Lost = append(out.Lost, entry)
		case hasA && hasB:
			out.Held = append(out.Held, entry)
		}
	}
	out.GainedN = len(out.Gained)
	out.LostN = len(out.Lost)
	out.HeldN = len(out.Held)
	return out, rows.Err()
}

func emitSerpDelta(cmd *cobra.Command, d *serpDelta, partition string, flags *rootFlags) error {
	var payload any = d
	switch partition {
	case "gained":
		payload = d.Gained
	case "lost":
		payload = d.Lost
	case "held":
		payload = d.Held
	case "", "all":
		// default: full envelope
	default:
		return fmt.Errorf("--partition must be all|gained|lost|held, got %q", partition)
	}
	if flags.asJSON || !isTTY(cmd.OutOrStdout()) {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(payload)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "SERP feature %s: %s → %s (gained=%d, lost=%d, held=%d)\n", d.Feature, d.FromDate, d.ToDate, d.GainedN, d.LostN, d.HeldN)
	return nil
}
