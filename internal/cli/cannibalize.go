// Hand-authored: cannibalize command.
//
// Flags keywords where multiple URLs from the same domain rank simultaneously
// (`extra_ranks` is a populated array per the AccuRanker schema) or where
// `highest_ranking_page` flips across the window. Pure local SQL — only works
// against the synced accuranker_keyword_ranks table.
package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

type cannibalRow struct {
	KeywordID      int64    `json:"keyword_id"`
	Keyword        string   `json:"keyword"`
	WindowStart    string   `json:"window_start"`
	WindowEnd      string   `json:"window_end"`
	PageFlips      int      `json:"page_flips"`
	MaxExtraRanks  int      `json:"max_extra_ranks"`
	CompetingPages []string `json:"competing_pages"`
}

func newCannibalizeCmd(flags *rootFlags) *cobra.Command {
	var (
		domainID  int64
		from      string
		to        string
		minFlips  int
		minExtras int
		dbPath    string
	)
	cmd := &cobra.Command{
		Use:   "cannibalize",
		Short: "Detect keyword cannibalization (multiple same-domain URLs ranking; URL flips across the window)",
		Long: `cannibalize joins accuranker_keyword_ranks against accuranker_keywords from
the local mirror to surface the keywords most likely to be suffering from
cannibalization. The output is a worklist, not a dashboard:

  - page_flips counts how many distinct URLs were the highest-ranking page
    over the window. > 1 means the SERP is undecided about which URL.
  - max_extra_ranks is the maximum length of extra_ranks across the window
    (multiple URLs from the same domain ranking simultaneously).

Run 'mirror' first to populate accuranker_keyword_ranks.`,
		Example: strings.Trim(`
  # Default: surface flips and extra-rank populations since the beginning of time
  accuranker-pp-cli cannibalize --domain 295242 --json

  # Tighter: 2+ flips AND extra_ranks >= 2 in the last 90 days
  accuranker-pp-cli cannibalize --domain 295242 --from 2026-02-20 --to 2026-05-20 \
    --min-flips 2 --min-extras 2 --json --select keyword,page_flips,competing_pages
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
			rows, err := computeCannibalize(cmd.Context(), st.DB(), domainID, from, to, minFlips, minExtras)
			if err != nil {
				// Graceful "no synced data" path so a fresh CLI can still
				// answer the question in JSON (empty array) without
				// surfacing a raw SQL error.
				if strings.Contains(err.Error(), "no such table") {
					rows = make([]cannibalRow, 0)
				} else {
					return err
				}
			}
			if flags.asJSON || !isTTY(cmd.OutOrStdout()) {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%-10s  %-30s  %s  %s  %s\n", "KEYWORD_ID", "KEYWORD", "FLIPS", "EXTRA", "PAGES")
			for _, r := range rows {
				fmt.Fprintf(cmd.OutOrStdout(), "%-10d  %-30s  %5d  %5d  %s\n", r.KeywordID, truncate(r.Keyword, 30), r.PageFlips, r.MaxExtraRanks, strings.Join(r.CompetingPages, " | "))
			}
			return nil
		},
	}
	cmd.Flags().Int64Var(&domainID, "domain", 0, "Domain ID (required)")
	cmd.Flags().StringVar(&from, "from", "", "Window start (YYYY-MM-DD, optional)")
	cmd.Flags().StringVar(&to, "to", "", "Window end (YYYY-MM-DD, optional)")
	cmd.Flags().IntVar(&minFlips, "min-flips", 2, "Minimum distinct highest_ranking_page values across the window")
	cmd.Flags().IntVar(&minExtras, "min-extras", 1, "Minimum extra_ranks array length")
	cmd.Flags().StringVar(&dbPath, "db", "", "Local SQLite DB path (default: ~/.local/share/accuranker-pp-cli/mirror.db)")
	return cmd
}

func computeCannibalize(ctx context.Context, db *sql.DB, domainID int64, from, to string, minFlips, minExtras int) ([]cannibalRow, error) {
	// Build the WHERE filter for the date window in a parameterized way.
	args := []any{domainID}
	dateFilter := ""
	if from != "" {
		dateFilter += " AND kr.search_date >= ?"
		args = append(args, from)
	}
	if to != "" {
		dateFilter += " AND kr.search_date <= ?"
		args = append(args, to)
	}
	q := fmt.Sprintf(`
SELECT
  k.id, k.keyword,
  COALESCE(MIN(kr.search_date), '') AS window_start,
  COALESCE(MAX(kr.search_date), '') AS window_end,
  COUNT(DISTINCT NULLIF(kr.highest_ranking_page, '')) AS page_flips,
  COALESCE(MAX(json_array_length(kr.extra_ranks)), 0) AS max_extras,
  COALESCE(json_group_array(DISTINCT kr.highest_ranking_page), '[]') AS pages
FROM accuranker_keywords k
JOIN accuranker_keyword_ranks kr ON kr.keyword_id = k.id
WHERE k.domain_id = ?%s
GROUP BY k.id, k.keyword
HAVING page_flips >= ? OR max_extras >= ?
ORDER BY page_flips DESC, max_extras DESC
LIMIT 500`, dateFilter)
	args = append(args, minFlips, minExtras)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("cannibalize query: %w", err)
	}
	defer rows.Close()
	out := make([]cannibalRow, 0)
	for rows.Next() {
		var r cannibalRow
		var pagesJSON string
		if err := rows.Scan(&r.KeywordID, &r.Keyword, &r.WindowStart, &r.WindowEnd, &r.PageFlips, &r.MaxExtraRanks, &pagesJSON); err != nil {
			return nil, err
		}
		var pages []string
		if err := json.Unmarshal([]byte(pagesJSON), &pages); err == nil {
			// drop empty strings
			r.CompetingPages = make([]string, 0, len(pages))
			for _, p := range pages {
				if p != "" {
					r.CompetingPages = append(r.CompetingPages, p)
				}
			}
		} else {
			r.CompetingPages = make([]string, 0)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
