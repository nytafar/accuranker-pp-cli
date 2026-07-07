// Per-resource mirror runners — one function per resource family.
// All runners share the same signature so they can be looked up via
// mirrorRunnerFor() in mirror.go.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"accuranker-pp-cli/internal/client"
	"accuranker-pp-cli/internal/schema"
	"accuranker-pp-cli/internal/store"
)

// mirrorDomains pulls /api/v4/domains/ once (the list is small and changes
// rarely). period_from/period_to are NOT included because /domains/ ignores
// them — the per-domain history endpoint is separate (domainHistory below).
func mirrorDomains(ctx context.Context, cl *client.Client, st *store.Store, m *schema.Model, opts *mirrorOptions, _, _ string, stat *mirrorResStat) error {
	q := url.Values{}
	q.Set("fields", m.DefaultFields("domains"))
	body, err := apiGet(ctx, cl, "/api/v4/domains/", q)
	if err != nil {
		return err
	}
	stat.APICalls++
	stat.Windows = 1

	var raw []map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return fmt.Errorf("decode domains: %w", err)
	}
	stat.RowsFetched = len(raw)

	// Filter to requested domain IDs.
	keep := make([]map[string]any, 0, len(raw))
	wanted := int64Set(opts.domainIDs)
	for _, item := range raw {
		id, ok := extractInt64(item, "id")
		if !ok {
			continue
		}
		if len(wanted) > 0 && !wanted[id] {
			continue
		}
		// Flatten group.{id,name} into group_id (parent FK).
		if g, ok := item["group"].(map[string]any); ok {
			if gid, ok := extractInt64(g, "id"); ok {
				item["group_id"] = gid
			}
		}
		keep = append(keep, item)
	}

	n, err := upsertJSON(ctx, st.DB(), "accuranker_domains", m, keep)
	if err != nil {
		return err
	}
	stat.RowsWritten = n
	return saveCursor(ctx, st, "domains", "global", "")
}

// mirrorKeywords fetches /api/v4/domains/{id}/keywords/ for each requested
// domain. No period_* because keywords themselves are slow-changing
// dimensional rows (rank data lives in keyword_ranks).
func mirrorKeywords(ctx context.Context, cl *client.Client, st *store.Store, m *schema.Model, opts *mirrorOptions, _, _ string, stat *mirrorResStat) error {
	stat.Windows = len(opts.domainIDs)
	for _, did := range opts.domainIDs {
		q := url.Values{}
		q.Set("fields", m.DefaultFields("keywords"))
		path := fmt.Sprintf("/api/v4/domains/%d/keywords/", did)
		body, err := apiGet(ctx, cl, path, q)
		if err != nil {
			return err
		}
		stat.APICalls++

		var raw []map[string]any
		if err := json.Unmarshal(body, &raw); err != nil {
			return fmt.Errorf("decode keywords for domain %d: %w", did, err)
		}
		stat.RowsFetched += len(raw)

		// Flatten and stamp domain_id, plus search_engine.id/name.
		// PATCH(amend-2026-07-07: spec F2): shared with `dump keywords`
		// via flattenKeywordRow so both surfaces emit the same shape.
		rows := make([]map[string]any, 0, len(raw))
		for _, item := range raw {
			rows = append(rows, flattenKeywordRow(item, did))
		}
		n, err := upsertJSON(ctx, st.DB(), "accuranker_keywords", m, rows)
		if err != nil {
			return err
		}
		stat.RowsWritten += n
	}
	return saveCursor(ctx, st, "keywords", csvDomainIDs(opts.domainIDs), "")
}

// mirrorKeywordRanks fetches rank time-series for each keyword the local
// store already has, walking the date window in 100-day chunks.
func mirrorKeywordRanks(ctx context.Context, cl *client.Client, st *store.Store, m *schema.Model, opts *mirrorOptions, ws, we string, stat *mirrorResStat) error {
	chunks, err := chunkDateWindow(ws, we, opts.maxWindowDays)
	if err != nil {
		return err
	}
	stat.Windows = len(chunks) * len(opts.domainIDs)
	for _, did := range opts.domainIDs {
		for _, chunk := range chunks {
			q := url.Values{}
			q.Set("fields", "id,"+m.DefaultFields("keyword_ranks"))
			q.Set("period_from", chunk[0])
			q.Set("period_to", chunk[1])
			path := fmt.Sprintf("/api/v4/domains/%d/keywords/", did)
			body, err := apiGet(ctx, cl, path, q)
			if err != nil {
				return err
			}
			stat.APICalls++
			var kw []struct {
				ID    int64            `json:"id"`
				Ranks []map[string]any `json:"ranks"`
			}
			if err := json.Unmarshal(body, &kw); err != nil {
				return fmt.Errorf("decode ranks chunk %s..%s for domain %d: %w", chunk[0], chunk[1], did, err)
			}
			rows := make([]map[string]any, 0, 256)
			for _, k := range kw {
				for _, r := range k.Ranks {
					r["keyword_id"] = k.ID
					if cAt, ok := r["created_at"].(string); ok && len(cAt) >= 10 {
						r["search_date"] = cAt[:10]
					}
					r["is_initial"] = false
					rows = append(rows, r)
					stat.RowsFetched++
				}
			}
			n, err := upsertJSON(ctx, st.DB(), "accuranker_keyword_ranks", m, rows)
			if err != nil {
				return err
			}
			stat.RowsWritten += n
		}
	}
	return saveCursor(ctx, st, "keyword_ranks", csvDomainIDs(opts.domainIDs), we)
}

// mirrorDomainHistory fetches /api/v4/domains/{id}/?fields=history.*
// for each domain, chunking by date window.
func mirrorDomainHistory(ctx context.Context, cl *client.Client, st *store.Store, m *schema.Model, opts *mirrorOptions, ws, we string, stat *mirrorResStat) error {
	chunks, err := chunkDateWindow(ws, we, opts.maxWindowDays)
	if err != nil {
		return err
	}
	stat.Windows = len(chunks) * len(opts.domainIDs)
	for _, did := range opts.domainIDs {
		for _, chunk := range chunks {
			q := url.Values{}
			q.Set("fields", m.DefaultFields("domain_history"))
			q.Set("period_from", chunk[0])
			q.Set("period_to", chunk[1])
			path := fmt.Sprintf("/api/v4/domains/%d/", did)
			body, err := apiGet(ctx, cl, path, q)
			if err != nil {
				return err
			}
			stat.APICalls++

			var doc struct {
				History []map[string]any `json:"history"`
			}
			if err := json.Unmarshal(body, &doc); err != nil {
				return fmt.Errorf("decode domain_history chunk %s..%s for %d: %w", chunk[0], chunk[1], did, err)
			}
			rows := make([]map[string]any, 0, len(doc.History))
			for _, h := range doc.History {
				h["domain_id"] = did
				rows = append(rows, h)
				stat.RowsFetched++
			}
			n, err := upsertJSON(ctx, st.DB(), "accuranker_domain_history", m, rows)
			if err != nil {
				return err
			}
			stat.RowsWritten += n
		}
	}
	return saveCursor(ctx, st, "domain_history", csvDomainIDs(opts.domainIDs), we)
}

// mirrorLandingPages walks /api/v4/domains/{id}/landing_pages/ for each domain.
func mirrorLandingPages(ctx context.Context, cl *client.Client, st *store.Store, m *schema.Model, opts *mirrorOptions, _, _ string, stat *mirrorResStat) error {
	stat.Windows = len(opts.domainIDs)
	for _, did := range opts.domainIDs {
		q := url.Values{}
		q.Set("fields", "id,landing_page,title")
		path := fmt.Sprintf("/api/v4/domains/%d/landing_pages/", did)
		body, err := apiGet(ctx, cl, path, q)
		if err != nil {
			return err
		}
		stat.APICalls++
		var raw []map[string]any
		if err := json.Unmarshal(body, &raw); err != nil {
			return fmt.Errorf("decode landing_pages for %d: %w", did, err)
		}
		stat.RowsFetched += len(raw)
		rows := make([]map[string]any, 0, len(raw))
		for _, item := range raw {
			item["domain_id"] = did
			rows = append(rows, item)
		}
		n, err := upsertJSON(ctx, st.DB(), "accuranker_landing_pages", m, rows)
		if err != nil {
			return err
		}
		stat.RowsWritten += n
	}
	return saveCursor(ctx, st, "landing_pages", csvDomainIDs(opts.domainIDs), "")
}

// mirrorLandingPageHistory pulls per-landing-page history with date chunking.
func mirrorLandingPageHistory(ctx context.Context, cl *client.Client, st *store.Store, m *schema.Model, opts *mirrorOptions, ws, we string, stat *mirrorResStat) error {
	chunks, err := chunkDateWindow(ws, we, opts.maxWindowDays)
	if err != nil {
		return err
	}
	// First, enumerate landing pages from local store so we know what to ask for.
	lps, err := loadLandingPageIDs(ctx, st, opts.domainIDs)
	if err != nil {
		return err
	}
	stat.Windows = len(chunks) * len(lps)
	for _, lp := range lps {
		for _, chunk := range chunks {
			q := url.Values{}
			q.Set("fields", m.DefaultFields("landing_page_history"))
			q.Set("period_from", chunk[0])
			q.Set("period_to", chunk[1])
			path := fmt.Sprintf("/api/v4/domains/%d/landing_pages/%d/", lp.DomainID, lp.ID)
			body, err := apiGet(ctx, cl, path, q)
			if err != nil {
				return err
			}
			stat.APICalls++
			var doc struct {
				History []map[string]any `json:"history"`
			}
			if err := json.Unmarshal(body, &doc); err != nil {
				return fmt.Errorf("decode landing_page_history: %w", err)
			}
			rows := make([]map[string]any, 0, len(doc.History))
			for _, h := range doc.History {
				h["landing_page_id"] = lp.ID
				rows = append(rows, h)
				stat.RowsFetched++
			}
			n, err := upsertJSON(ctx, st.DB(), "accuranker_landing_page_history", m, rows)
			if err != nil {
				return err
			}
			stat.RowsWritten += n
		}
	}
	return saveCursor(ctx, st, "landing_page_history", csvDomainIDs(opts.domainIDs), we)
}

// mirrorTags pulls /api/v4/domains/{id}/tags/ as a list of tag strings.
func mirrorTags(ctx context.Context, cl *client.Client, st *store.Store, m *schema.Model, opts *mirrorOptions, _, _ string, stat *mirrorResStat) error {
	stat.Windows = len(opts.domainIDs)
	for _, did := range opts.domainIDs {
		q := url.Values{}
		q.Set("fields", "tag")
		path := fmt.Sprintf("/api/v4/domains/%d/tags/", did)
		body, err := apiGet(ctx, cl, path, q)
		if err != nil {
			return err
		}
		stat.APICalls++
		var raw []map[string]any
		if err := json.Unmarshal(body, &raw); err != nil {
			return fmt.Errorf("decode tags: %w", err)
		}
		stat.RowsFetched += len(raw)
		rows := make([]map[string]any, 0, len(raw))
		for _, item := range raw {
			item["domain_id"] = did
			rows = append(rows, item)
		}
		n, err := upsertJSON(ctx, st.DB(), "accuranker_tags", m, rows)
		if err != nil {
			return err
		}
		stat.RowsWritten += n
	}
	return saveCursor(ctx, st, "tags", csvDomainIDs(opts.domainIDs), "")
}

// mirrorTagHistory placeholder — same dimension+history shape as
// landing_page_history but the /tags/{name}/ detail endpoint may not be
// exposed in the public spec for every tier. Implemented as a best-effort
// no-op that records that we tried.
func mirrorTagHistory(ctx context.Context, cl *client.Client, st *store.Store, m *schema.Model, opts *mirrorOptions, _, _ string, stat *mirrorResStat) error {
	stat.Reason = "tag_history is fetched per-tag and may require tag-name encoding the public spec does not document; skipping until evidence shows a per-tier endpoint"
	stat.Status = "skipped"
	return saveCursor(ctx, st, "tag_history", csvDomainIDs(opts.domainIDs), "")
}

// LLM tier — these will return paywall errors on plans without AccuLLM.
// runMirror() catches and translates that into status="paywalled".

func mirrorBrands(ctx context.Context, cl *client.Client, st *store.Store, m *schema.Model, opts *mirrorOptions, _, _ string, stat *mirrorResStat) error {
	q := url.Values{}
	q.Set("fields", m.DefaultFields("brands"))
	body, err := apiGet(ctx, cl, "/api/v4/brands/", q)
	if err != nil {
		return err
	}
	stat.APICalls++
	stat.Windows = 1
	var raw []map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return fmt.Errorf("decode brands: %w", err)
	}
	stat.RowsFetched = len(raw)
	for _, item := range raw {
		if g, ok := item["group"].(map[string]any); ok {
			if gid, ok := extractInt64(g, "id"); ok {
				item["group_id"] = gid
			}
		}
	}
	n, err := upsertJSON(ctx, st.DB(), "accuranker_brands", m, raw)
	if err != nil {
		return err
	}
	stat.RowsWritten = n
	return saveCursor(ctx, st, "brands", "global", "")
}

func mirrorPrompts(ctx context.Context, cl *client.Client, st *store.Store, m *schema.Model, opts *mirrorOptions, _, _ string, stat *mirrorResStat) error {
	bids, err := loadBrandIDs(ctx, st)
	if err != nil {
		return err
	}
	stat.Windows = len(bids)
	for _, bid := range bids {
		q := url.Values{}
		q.Set("fields", m.DefaultFields("prompts"))
		path := fmt.Sprintf("/api/v4/brands/%d/prompts/", bid)
		body, err := apiGet(ctx, cl, path, q)
		if err != nil {
			return err
		}
		stat.APICalls++
		var raw []map[string]any
		if err := json.Unmarshal(body, &raw); err != nil {
			return fmt.Errorf("decode prompts: %w", err)
		}
		stat.RowsFetched += len(raw)
		for _, item := range raw {
			item["brand_id"] = bid
		}
		n, err := upsertJSON(ctx, st.DB(), "accuranker_prompts", m, raw)
		if err != nil {
			return err
		}
		stat.RowsWritten += n
	}
	return saveCursor(ctx, st, "prompts", "global", "")
}

func mirrorPromptResults(ctx context.Context, cl *client.Client, st *store.Store, m *schema.Model, opts *mirrorOptions, ws, we string, stat *mirrorResStat) error {
	chunks, err := chunkDateWindow(ws, we, opts.maxWindowDays)
	if err != nil {
		return err
	}
	prompts, err := loadPromptIDs(ctx, st)
	if err != nil {
		return err
	}
	stat.Windows = len(chunks) * len(prompts)
	for _, p := range prompts {
		for _, chunk := range chunks {
			q := url.Values{}
			q.Set("fields", m.DefaultFields("prompt_results"))
			q.Set("period_from", chunk[0])
			q.Set("period_to", chunk[1])
			path := fmt.Sprintf("/api/v4/brands/%d/prompts/%d/", p.BrandID, p.ID)
			body, err := apiGet(ctx, cl, path, q)
			if err != nil {
				return err
			}
			stat.APICalls++
			var doc struct {
				Results []map[string]any `json:"results"`
			}
			if err := json.Unmarshal(body, &doc); err != nil {
				return fmt.Errorf("decode prompt_results: %w", err)
			}
			rows := make([]map[string]any, 0, len(doc.Results))
			for _, r := range doc.Results {
				r["prompt_id"] = p.ID
				if cAt, ok := r["created_at"].(string); ok && len(cAt) >= 10 {
					r["search_date"] = cAt[:10]
				}
				rows = append(rows, r)
				stat.RowsFetched++
			}
			n, err := upsertJSON(ctx, st.DB(), "accuranker_prompt_results", m, rows)
			if err != nil {
				return err
			}
			stat.RowsWritten += n
		}
	}
	return saveCursor(ctx, st, "prompt_results", "global", we)
}

// --- helpers ---------------------------------------------------------------

func extractInt64(m map[string]any, key string) (int64, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false
	}
	switch x := v.(type) {
	case float64:
		return int64(x), true
	case int64:
		return x, true
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i, true
		}
	case string:
		var i int64
		if _, err := fmt.Sscanf(x, "%d", &i); err == nil {
			return i, true
		}
	}
	return 0, false
}

func extractFloat(m map[string]any, key string) (float64, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false
	}
	if f, ok := v.(float64); ok {
		return f, true
	}
	if n, ok := v.(json.Number); ok {
		if f, err := n.Float64(); err == nil {
			return f, true
		}
	}
	return 0, false
}

func int64Set(ids []int64) map[int64]bool {
	m := make(map[int64]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func csvDomainIDs(ids []int64) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("%d", id))
	}
	return "domain_id=" + strings.Join(parts, ",")
}

type landingPageRef struct {
	ID       int64
	DomainID int64
}

func loadLandingPageIDs(ctx context.Context, st *store.Store, domainIDs []int64) ([]landingPageRef, error) {
	rows, err := st.DB().QueryContext(ctx, `SELECT id, domain_id FROM accuranker_landing_pages WHERE domain_id IN (`+csvPlaceholders(len(domainIDs))+`)`, anyInt64Slice(domainIDs)...)
	if err != nil {
		return nil, fmt.Errorf("load landing_pages: %w", err)
	}
	defer rows.Close()
	out := make([]landingPageRef, 0, 32)
	for rows.Next() {
		var lp landingPageRef
		if err := rows.Scan(&lp.ID, &lp.DomainID); err != nil {
			return nil, err
		}
		out = append(out, lp)
	}
	return out, rows.Err()
}

func loadBrandIDs(ctx context.Context, st *store.Store) ([]int64, error) {
	rows, err := st.DB().QueryContext(ctx, `SELECT id FROM accuranker_brands ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]int64, 0, 8)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

type promptRef struct {
	ID      int64
	BrandID int64
}

func loadPromptIDs(ctx context.Context, st *store.Store) ([]promptRef, error) {
	rows, err := st.DB().QueryContext(ctx, `SELECT id, brand_id FROM accuranker_prompts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]promptRef, 0, 8)
	for rows.Next() {
		var p promptRef
		if err := rows.Scan(&p.ID, &p.BrandID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func anyInt64Slice(ids []int64) []any {
	out := make([]any, len(ids))
	for i, v := range ids {
		out[i] = v
	}
	return out
}

func csvPlaceholders(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "?"
	}
	return strings.Join(parts, ",")
}

// saveCursor writes (or updates) the per-(resource, scope) cursor row.
func saveCursor(ctx context.Context, st *store.Store, resource, scope, lastPeriodTo string) error {
	stmt := `INSERT OR REPLACE INTO accuranker_sync_cursor (resource, scope, last_period_to, last_synced_at, rows_seen, last_error) VALUES (?, ?, ?, CURRENT_TIMESTAMP, 0, NULL)`
	if _, err := st.DB().ExecContext(ctx, stmt, resource, scope, nullIfEmpty(lastPeriodTo)); err != nil {
		return fmt.Errorf("save cursor (%s/%s): %w", resource, scope, err)
	}
	return nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
