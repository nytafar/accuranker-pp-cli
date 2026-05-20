// Hand-authored: dump command.
//
// Streams NDJSON for any time-windowed resource. The CLI internally walks the
// 100-day chunks AccuRanker imposes and dedups by (id, search_date|date|month).
// One record per line; ready for `psql COPY ... FROM STDIN` or any other
// line-oriented loader.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"accuranker-pp-cli/internal/client"
	"accuranker-pp-cli/internal/config"
	"accuranker-pp-cli/internal/schema"
)

func newDumpCmd(flags *rootFlags) *cobra.Command {
	var (
		domainSpec    string
		resource      string
		from          string
		until         string
		windowDaysArg int
		schemaPath    string
	)
	cmd := &cobra.Command{
		Use:   "dump [resource]",
		Short: "Stream NDJSON over an arbitrary date window (auto-chunks the 100-day API cap)",
		Example: strings.Trim(`
  # Two years of rank history, NDJSON, into Postgres COPY
  accuranker-pp-cli dump keyword-ranks --domain 295242 --from 2024-01-01 --to 2026-05-20 \
    | psql -c '\COPY accuranker_ranks FROM STDIN CSV'

  # Domain history for a quarter
  accuranker-pp-cli dump domain-history --domain 295242 --from 2026-01-01 --to 2026-03-31 --json
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
			ids, err := parseInt64CSV(domainSpec)
			if err != nil {
				return fmt.Errorf("--domain: %w", err)
			}
			if len(ids) == 0 {
				return fmt.Errorf("--domain is required")
			}
			if from == "" {
				return fmt.Errorf("--from is required (YYYY-MM-DD)")
			}
			ws, we, err := resolveWindow(from, until)
			if err != nil {
				return err
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
			return runDump(cmd.Context(), cl, model, cmd.OutOrStdout(), resource, ids, ws, we, windowDaysArg)
		},
	}
	cmd.Flags().StringVar(&domainSpec, "domain", "", "Domain ID(s), comma-separated (required)")
	cmd.Flags().StringVar(&from, "from", "", "Window start (YYYY-MM-DD, required)")
	cmd.Flags().StringVar(&until, "to", "", "Window end (YYYY-MM-DD, default: today)")
	cmd.Flags().IntVar(&windowDaysArg, "window-days", 100, "Days per chunk (max 100)")
	cmd.Flags().StringVar(&schemaPath, "schema-file", "", "Override schema/model.yaml path")
	return cmd
}

func runDump(ctx context.Context, cl *client.Client, model *schema.Model, out io.Writer, resource string, domainIDs []int64, ws, we string, windowDays int) error {
	chunks, err := chunkDateWindow(ws, we, windowDays)
	if err != nil {
		return err
	}
	seen := make(map[string]bool, 4096)
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)

	for _, did := range domainIDs {
		for _, chunk := range chunks {
			rows, err := dumpFetch(ctx, cl, model, resource, did, chunk[0], chunk[1])
			if err != nil {
				return fmt.Errorf("dump %s domain=%d %s..%s: %w", resource, did, chunk[0], chunk[1], err)
			}
			for _, row := range rows {
				key := dedupKey(resource, row)
				if seen[key] {
					continue
				}
				seen[key] = true
				if err := enc.Encode(row); err != nil {
					return fmt.Errorf("encode: %w", err)
				}
			}
		}
	}
	return nil
}

func dumpFetch(ctx context.Context, cl *client.Client, m *schema.Model, resource string, did int64, from, to string) ([]map[string]any, error) {
	switch resource {
	case "keyword-ranks":
		q := url.Values{}
		q.Set("fields", "id,"+m.DefaultFields("keyword_ranks"))
		q.Set("period_from", from)
		q.Set("period_to", to)
		body, err := apiGet(ctx, cl, fmt.Sprintf("/api/v4/domains/%d/keywords/", did), q)
		if err != nil {
			return nil, err
		}
		var kw []struct {
			ID    int64            `json:"id"`
			Ranks []map[string]any `json:"ranks"`
		}
		if err := json.Unmarshal(body, &kw); err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, 256)
		for _, k := range kw {
			for _, r := range k.Ranks {
				r["keyword_id"] = k.ID
				r["domain_id"] = did
				out = append(out, r)
			}
		}
		return out, nil
	case "domain-history":
		q := url.Values{}
		q.Set("fields", m.DefaultFields("domain_history"))
		q.Set("period_from", from)
		q.Set("period_to", to)
		body, err := apiGet(ctx, cl, fmt.Sprintf("/api/v4/domains/%d/", did), q)
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
			h["domain_id"] = did
		}
		return doc.History, nil
	case "landing-page-history":
		// Requires per-landing-page enumeration; defer to the mirror command.
		return nil, fmt.Errorf("dump landing-page-history: use `mirror --resources landing_page_history` (per-page enumeration is store-aware)")
	default:
		return nil, fmt.Errorf("dump resource %q not supported; try: keyword-ranks, domain-history", resource)
	}
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
	}
	b, _ := json.Marshal(row)
	return string(b)
}
