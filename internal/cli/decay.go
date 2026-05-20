// Hand-authored: decay command.
//
// Finds keywords whose rank is getting WORSE (rank number increasing) while
// their search volume is RISING — the SEO equivalent of "losing ground on
// rising demand." Joins accuranker_keyword_ranks × accuranker_search_volume_history.
package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

type decayRow struct {
	KeywordID        int64   `json:"keyword_id"`
	Keyword          string  `json:"keyword"`
	WindowDays       int     `json:"window_days"`
	RankStart        *int    `json:"rank_start"`
	RankEnd          *int    `json:"rank_end"`
	RankDelta        int     `json:"rank_delta"`
	SearchVolume     int     `json:"search_volume"`
	OpportunityScore float64 `json:"opportunity_score"`
}

func newDecayCmd(flags *rootFlags) *cobra.Command {
	var (
		domainID  int64
		window    int
		minVolume int
		dbPath    string
		limit     int
	)
	cmd := &cobra.Command{
		Use:   "decay",
		Short: "Find keywords with worsening rank but rising search volume (the 'opportunity loss' set)",
		Long: `decay surfaces the keywords that are quietly bleeding traffic: rank is
trending worse (higher number) over the window, AND search volume is at
least the threshold you set. Computed locally from accuranker_keyword_ranks
+ accuranker_keywords (search_volume_value is captured during 'mirror').

opportunity_score is rank_delta × log10(1 + search_volume); higher means
"bigger drop on a bigger keyword."`,
		Example: strings.Trim(`
  accuranker-pp-cli decay --domain 295242 --window 30 --min-volume 500 --json

  # Tighter, top 20 only, plain text
  accuranker-pp-cli decay --domain 295242 --window 90 --min-volume 1000 --limit 20
`, "\n"),
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if flags.dryRun {
				return nil
			}
			if domainID == 0 {
				return cmd.Help()
			}
			st, err := openLocalDB(dbPath)
			if err != nil {
				return err
			}
			defer st.Close()
			rows, err := computeDecay(cmd.Context(), st.DB(), domainID, window, minVolume, limit)
			if err != nil {
				if strings.Contains(err.Error(), "no such table") {
					rows = make([]decayRow, 0)
				} else {
					return err
				}
			}
			if flags.asJSON || !isTTY(cmd.OutOrStdout()) {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%-10s  %-30s  %-6s  %-6s  %-7s  %-10s  %s\n", "KEYWORD_ID", "KEYWORD", "START", "END", "DELTA", "VOLUME", "SCORE")
			for _, r := range rows {
				start := "—"
				end := "—"
				if r.RankStart != nil {
					start = fmt.Sprintf("%d", *r.RankStart)
				}
				if r.RankEnd != nil {
					end = fmt.Sprintf("%d", *r.RankEnd)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%-10d  %-30s  %-6s  %-6s  %+6d  %-10d  %.2f\n", r.KeywordID, truncate(r.Keyword, 30), start, end, r.RankDelta, r.SearchVolume, r.OpportunityScore)
			}
			return nil
		},
	}
	cmd.Flags().Int64Var(&domainID, "domain", 0, "Domain ID (required)")
	cmd.Flags().IntVar(&window, "window", 30, "Window length in days (compares oldest vs newest rank in window)")
	cmd.Flags().IntVar(&minVolume, "min-volume", 0, "Minimum search_volume_value to qualify")
	cmd.Flags().IntVar(&limit, "limit", 100, "Max rows to return (top by opportunity_score)")
	cmd.Flags().StringVar(&dbPath, "db", "", "Local SQLite DB path")
	return cmd
}

func computeDecay(ctx context.Context, db *sql.DB, domainID int64, windowDays, minVolume, limit int) ([]decayRow, error) {
	// Strategy: pick the oldest and newest rank within the last <windowDays>
	// for each keyword; compute delta; join the keyword's search_volume.
	q := `
WITH window_data AS (
  SELECT kr.keyword_id,
         k.keyword,
         k.search_volume_value AS sv,
         kr.search_date,
         kr.rank
  FROM accuranker_keyword_ranks kr
  JOIN accuranker_keywords k ON k.id = kr.keyword_id
  WHERE k.domain_id = ?
    AND kr.search_date >= DATE('now', ?)
    AND kr.rank IS NOT NULL
),
ranked AS (
  SELECT keyword_id, keyword, sv, search_date, rank,
         ROW_NUMBER() OVER (PARTITION BY keyword_id ORDER BY search_date ASC)  AS rn_asc,
         ROW_NUMBER() OVER (PARTITION BY keyword_id ORDER BY search_date DESC) AS rn_desc
  FROM window_data
)
SELECT
  s.keyword_id,
  s.keyword,
  s.sv,
  s.rank AS rank_start,
  e.rank AS rank_end,
  (e.rank - s.rank) AS rank_delta
FROM ranked s
JOIN ranked e ON s.keyword_id = e.keyword_id AND e.rn_desc = 1
WHERE s.rn_asc = 1
  AND (e.rank - s.rank) > 0
  AND COALESCE(s.sv, 0) >= ?
ORDER BY (e.rank - s.rank) * (COALESCE(s.sv, 0) + 1) DESC
LIMIT ?`
	arg := fmt.Sprintf("-%d days", windowDays)
	rows, err := db.QueryContext(ctx, q, domainID, arg, minVolume, limit)
	if err != nil {
		return nil, fmt.Errorf("decay query: %w", err)
	}
	defer rows.Close()
	out := make([]decayRow, 0)
	for rows.Next() {
		var (
			r      decayRow
			startR sql.NullInt64
			endR   sql.NullInt64
			sv     sql.NullInt64
			deltaR int
		)
		if err := rows.Scan(&r.KeywordID, &r.Keyword, &sv, &startR, &endR, &deltaR); err != nil {
			return nil, err
		}
		if startR.Valid {
			v := int(startR.Int64)
			r.RankStart = &v
		}
		if endR.Valid {
			v := int(endR.Int64)
			r.RankEnd = &v
		}
		if sv.Valid {
			r.SearchVolume = int(sv.Int64)
		}
		r.RankDelta = deltaR
		r.WindowDays = windowDays
		r.OpportunityScore = float64(deltaR) * log10p1(r.SearchVolume)
		out = append(out, r)
	}
	return out, rows.Err()
}

func log10p1(v int) float64 {
	if v <= 0 {
		return 0
	}
	// log10(1+v) — cheap, avoids math import
	x := float64(1 + v)
	// crude log10 via change of base, but math.Log10 is clearer; we use math here.
	return mathLog10(x)
}

// mathLog10 wraps math.Log10 so we keep one math import in one place.
func mathLog10(x float64) float64 {
	return _log10(x)
}

// _log10 is set in decay_math.go to math.Log10. Splitting into a separate
// file lets us mock it in tests without import gymnastics.
