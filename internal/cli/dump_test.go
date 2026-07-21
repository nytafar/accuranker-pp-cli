// Tests for the warehouse dump contract (spec F1/F2/F3): run report
// emission, next_cursor watermark semantics, envelope wrapping, dedup.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"accuranker-pp-cli/internal/client"
)

func testFetch(rowsPerChunk map[string][]map[string]any, failOn string) dumpFetchFn {
	return func(_ context.Context, did int64, from, to string) ([]dumpBatch, error) {
		key := from + ".." + to
		if key == failOn {
			return nil, fmt.Errorf("boom on %s", key)
		}
		q := url.Values{}
		q.Set("period_from", from)
		q.Set("period_to", to)
		return oneBatch(
			rowsPerChunk[key],
			fmt.Sprintf("/api/v4/domains/%d/keywords/", did),
			q,
		), nil
	}
}

func decodeReport(t *testing.T, errOut *bytes.Buffer) dumpRunReport {
	t.Helper()
	var rep dumpRunReport
	if err := json.Unmarshal(errOut.Bytes(), &rep); err != nil {
		t.Fatalf("run report is not a single JSON object: %v\nstderr: %s", err, errOut.String())
	}
	return rep
}

func TestRunDumpCleanExit(t *testing.T) {
	rows := map[string][]map[string]any{
		"2026-01-01..2026-01-02": {
			{"keyword_id": int64(1), "created_at": "2026-01-01T04:00:00Z"},
			{"keyword_id": int64(1), "created_at": "2026-01-02T04:00:00Z"},
		},
		"2026-01-03..2026-01-04": {
			// duplicate of chunk 1's last row — must dedup
			{"keyword_id": int64(1), "created_at": "2026-01-02T04:00:00Z"},
			{"keyword_id": int64(1), "created_at": "2026-01-03T04:00:00Z"},
		},
	}
	var out, errOut bytes.Buffer
	opts := dumpOptions{
		resource:  "keyword-ranks",
		domainIDs: []int64{7},
		windowed:  true,
		ws:        "2026-01-01", we: "2026-01-04",
		windowDays: 2,
	}
	err := runDump(context.Background(), testFetch(rows, ""), &out, &errOut, opts, func() (int64, int64) { return 2, 0 })
	if err != nil {
		t.Fatalf("runDump: %v", err)
	}
	rep := decodeReport(t, &errOut)
	if !rep.CleanExit {
		t.Errorf("clean_exit = false, want true")
	}
	if rep.NextCursor != "2026-01-04" {
		t.Errorf("next_cursor = %q, want 2026-01-04", rep.NextCursor)
	}
	if rep.RowsEmitted != 3 {
		t.Errorf("rows_emitted = %d, want 3 (dedup across chunk edge)", rep.RowsEmitted)
	}
	if rep.DomainID != 7 {
		t.Errorf("domain_id = %d, want 7", rep.DomainID)
	}
	if rep.RequestsMade != 2 {
		t.Errorf("requests_made = %d, want 2", rep.RequestsMade)
	}
	if got := strings.Count(out.String(), "\n"); got != 3 {
		t.Errorf("stdout NDJSON lines = %d, want 3", got)
	}
	// stdout must be pure NDJSON — every line a JSON object without report keys.
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("stdout line not JSON: %v", err)
		}
		if _, has := obj["clean_exit"]; has {
			t.Errorf("run report leaked into stdout: %s", line)
		}
	}
}

func TestRunDumpPartialFailureReportsLastCompletedBoundary(t *testing.T) {
	rows := map[string][]map[string]any{
		"2026-01-01..2026-01-02": {{"keyword_id": int64(1), "created_at": "2026-01-01T04:00:00Z"}},
	}
	var out, errOut bytes.Buffer
	opts := dumpOptions{
		resource:  "keyword-ranks",
		domainIDs: []int64{7},
		windowed:  true,
		ws:        "2026-01-01", we: "2026-01-04",
		windowDays: 2,
	}
	err := runDump(context.Background(), testFetch(rows, "2026-01-03..2026-01-04"), &out, &errOut, opts, nil)
	if err == nil {
		t.Fatal("runDump should fail when a chunk fails")
	}
	rep := decodeReport(t, &errOut)
	if rep.CleanExit {
		t.Error("clean_exit = true on partial failure, want false")
	}
	if rep.NextCursor != "2026-01-02" {
		t.Errorf("next_cursor = %q, want 2026-01-02 (last fully-completed chunk boundary)", rep.NextCursor)
	}
	if rep.Error == "" {
		t.Error("report should carry the error message")
	}
	if rep.RowsEmitted != 1 {
		t.Errorf("rows_emitted = %d, want 1 (honest partial progress)", rep.RowsEmitted)
	}
}

func TestRunDumpMultiDomainCursorAdvancesOnlyWhenAllDomainsComplete(t *testing.T) {
	// Domain 8 fails on chunk 2 — the boundary must stay at chunk 1's end
	// even though domain 7 completed chunk 2 first in the walk order.
	fetch := func(_ context.Context, did int64, from, to string) ([]dumpBatch, error) {
		if did == 8 && from == "2026-01-03" {
			return nil, fmt.Errorf("boom domain 8")
		}
		return oneBatch([]map[string]any{
			{"keyword_id": did, "created_at": from + "T04:00:00Z"},
		}, "/x/", url.Values{}), nil
	}
	var out, errOut bytes.Buffer
	opts := dumpOptions{
		resource:  "keyword-ranks",
		domainIDs: []int64{7, 8},
		windowed:  true,
		ws:        "2026-01-01", we: "2026-01-04",
		windowDays: 2,
	}
	err := runDump(context.Background(), fetch, &out, &errOut, opts, nil)
	if err == nil {
		t.Fatal("expected failure")
	}
	rep := decodeReport(t, &errOut)
	if rep.NextCursor != "2026-01-02" {
		t.Errorf("next_cursor = %q, want 2026-01-02", rep.NextCursor)
	}
	if len(rep.DomainIDs) != 2 {
		t.Errorf("domain_ids = %v, want both domains listed", rep.DomainIDs)
	}
}

func TestRunDumpEnvelope(t *testing.T) {
	rows := map[string][]map[string]any{
		"2026-01-01..2026-01-01": {{"id": int64(11), "keyword": "espresso"}},
	}
	var out, errOut bytes.Buffer
	opts := dumpOptions{
		resource:  "keywords",
		domainIDs: []int64{7},
		windowed:  false,
		ws:        "2026-01-01", we: "2026-01-01",
		envelope: true,
	}
	if err := runDump(context.Background(), testFetch(rows, ""), &out, &errOut, opts, nil); err != nil {
		t.Fatalf("runDump: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("envelope line not JSON: %v", err)
	}
	for _, key := range []string{"source", "endpoint", "params_hash", "api_version", "fetched_at", "payload"} {
		if _, ok := env[key]; !ok {
			t.Errorf("envelope missing %q: %v", key, env)
		}
	}
	if env["source"] != "accuranker" || env["api_version"] != "v4" {
		t.Errorf("envelope constants wrong: %v", env)
	}
	payload, ok := env["payload"].(map[string]any)
	if !ok || payload["keyword"] != "espresso" {
		t.Errorf("payload should carry the untouched row: %v", env["payload"])
	}
	if !strings.HasPrefix(env["params_hash"].(string), "sha256:") {
		t.Errorf("params_hash should be sha256-prefixed: %v", env["params_hash"])
	}
}

func TestDumpParamsHashStable(t *testing.T) {
	a := url.Values{}
	a.Set("period_from", "2026-01-01")
	a.Set("fields", "id,rank")
	b := url.Values{}
	b.Set("fields", "id,rank")
	b.Set("period_from", "2026-01-01")
	if dumpParamsHash("/api/v4/domains/1/keywords/", a) != dumpParamsHash("/api/v4/domains/1/keywords/", b) {
		t.Error("params_hash must be insertion-order independent")
	}
	if dumpParamsHash("/api/v4/domains/1/keywords/", a) == dumpParamsHash("/api/v4/domains/2/keywords/", a) {
		t.Error("params_hash must incorporate the endpoint")
	}
}

func TestPrefixFields(t *testing.T) {
	got := prefixFields("competitors.", "id, domain ,display_name")
	want := "competitors.id,competitors.domain,competitors.display_name"
	if got != want {
		t.Errorf("prefixFields = %q, want %q", got, want)
	}
}

func TestDedupKeyPerResource(t *testing.T) {
	cases := []struct {
		resource string
		row      map[string]any
		want     string
	}{
		{"keywords", map[string]any{"id": int64(5)}, "kw:5"},
		{"competitors", map[string]any{"id": int64(9)}, "cp:9"},
		{"landing-pages", map[string]any{"id": int64(3)}, "lp:3"},
		{"tags", map[string]any{"domain_id": int64(7), "tag": "brand"}, "tg:7:brand"},
		{"competitor-ranks", map[string]any{"keyword_id": int64(1), "competitor_id": int64(2), "search_date": "2026-01-01"}, "cr:1:2:2026-01-01"},
		// keyword-ranks keys on is_initial too so the baseline row never
		// collides with the same-day daily rank (PK: keyword_id,search_date,is_initial).
		{"keyword-ranks", map[string]any{"keyword_id": int64(1), "created_at": "2026-01-01T04:00:00Z", "is_initial": false}, "kr:1:2026-01-01T04:00:00Z:false"},
		{"keyword-ranks", map[string]any{"keyword_id": int64(1), "created_at": "2026-01-01T04:00:00Z", "is_initial": true}, "kr:1:2026-01-01T04:00:00Z:true"},
		{"full-serp", map[string]any{"keyword_id": int64(4), "search_date": "2026-02-02"}, "fs:4:2026-02-02"},
		{"search-volume-history", map[string]any{"keyword_id": int64(4), "month": "2026-02-01"}, "svh:4:2026-02-01"},
		{"ai-search-volume-history", map[string]any{"keyword_id": int64(4), "month": "2026-02-01"}, "asvh:4:2026-02-01"},
		{"competitor-history", map[string]any{"competitor_id": int64(8), "date": "2026-02-02"}, "ch:8:2026-02-02"},
		{"landing-page-history", map[string]any{"landing_page_id": int64(6), "date": "2026-02-02"}, "lph:6:2026-02-02"},
		{"tag-history", map[string]any{"domain_id": int64(7), "tag": "brand", "date": "2026-02-02"}, "th:7:brand:2026-02-02"},
		{"people-also-ask", map[string]any{"keyword_id": int64(4), "term": "how to brew"}, "paa:4:how to brew"},
		{"domains", map[string]any{"id": int64(295242)}, "dm:295242"},
		{"accounts", map[string]any{"id": int64(11)}, "ac:11"},
		{"groups", map[string]any{"id": int64(22)}, "gr:22"},
		{"brands", map[string]any{"id": int64(33)}, "br:33"},
		{"prompts", map[string]any{"id": int64(44)}, "pr:44"},
		{"prompt-results", map[string]any{"prompt_id": int64(44), "search_date": "2026-02-02"}, "prr:44:2026-02-02"},
	}
	for _, c := range cases {
		if got := dedupKey(c.resource, c.row); got != c.want {
			t.Errorf("dedupKey(%s) = %q, want %q", c.resource, got, c.want)
		}
	}
}

// --- amend-2026-07-21: dump completeness (spec F9/F10/F11) ---

// errFetch returns err on the first call — models an unentitled tier.
func errFetch(err error) dumpFetchFn {
	return func(_ context.Context, _ int64, _, _ string) ([]dumpBatch, error) {
		return nil, err
	}
}

func TestBuildDumpOptionsGlobalNeedsNoDomain(t *testing.T) {
	// A global resource (domains) must resolve with an empty --domain and drive
	// a single sentinel scope.
	opts, err := buildDumpOptions("domains", dumpResources["domains"], dumpBaseOptions{windowDays: 100})
	if err != nil {
		t.Fatalf("global resource should not require --domain: %v", err)
	}
	if !opts.global {
		t.Error("domains should classify as global")
	}
	if len(opts.domainIDs) != 1 || opts.domainIDs[0] != 0 {
		t.Errorf("global scope = %v, want [0] sentinel", opts.domainIDs)
	}
}

func TestBuildDumpOptionsDomainScopedRequiresDomain(t *testing.T) {
	_, err := buildDumpOptions("keywords", dumpResources["keywords"], dumpBaseOptions{windowDays: 100})
	if err == nil {
		t.Fatal("domain-scoped snapshot should require --domain")
	}
}

func TestBuildDumpOptionsWindowedRequiresFrom(t *testing.T) {
	_, err := buildDumpOptions("full-serp", dumpResources["full-serp"], dumpBaseOptions{domainIDs: []int64{7}, windowDays: 100})
	if err == nil {
		t.Fatal("windowed resource should require --from")
	}
}

func TestRunDumpGlobalReportOmitsDomain(t *testing.T) {
	rows := map[string][]map[string]any{}
	today := "2026-07-21"
	rows[today+".."+today] = []map[string]any{{"id": int64(295242), "created_at": "2020-01-01T00:00:00Z"}}
	fetch := func(_ context.Context, _ int64, from, to string) ([]dumpBatch, error) {
		return oneBatch(rows[from+".."+to], "/api/v4/domains/", url.Values{}), nil
	}
	var out, errOut bytes.Buffer
	opts := dumpOptions{resource: "domains", domainIDs: []int64{0}, global: true, ws: today, we: today}
	if err := runDump(context.Background(), fetch, &out, &errOut, opts, nil); err != nil {
		t.Fatalf("runDump: %v", err)
	}
	rep := decodeReport(t, &errOut)
	if rep.DomainID != 0 || rep.DomainIDs != nil {
		t.Errorf("global report leaked domain fields: domain_id=%d domain_ids=%v", rep.DomainID, rep.DomainIDs)
	}
	if rep.RowsEmitted != 1 || !rep.CleanExit {
		t.Errorf("rows=%d clean=%v, want 1 / true", rep.RowsEmitted, rep.CleanExit)
	}
}

func TestRunDumpGatedUnentitledExit45(t *testing.T) {
	// A gated resource whose fetch fails with a paywall body degrades to exit
	// 45 (skipped_unentitled), not a hard failure, and the report records it.
	paywall := fmt.Errorf("your plan does not include LLM API access")
	var out, errOut bytes.Buffer
	opts := dumpOptions{resource: "brands", domainIDs: []int64{0}, global: true, gated: true, ws: "2026-07-21", we: "2026-07-21"}
	err := runDump(context.Background(), errFetch(paywall), &out, &errOut, opts, nil)
	if err == nil {
		t.Fatal("gated+unentitled should return an error")
	}
	if code := ExitCode(err); code != 45 {
		t.Errorf("exit code = %d, want 45", code)
	}
	rep := decodeReport(t, &errOut)
	if rep.Status != "skipped_unentitled" {
		t.Errorf("report status = %q, want skipped_unentitled", rep.Status)
	}
	if rep.CleanExit {
		t.Error("clean_exit should be false for an unentitled dump")
	}
}

func TestRunDumpGatedRealErrorStaysHard(t *testing.T) {
	// A gated resource that fails for a NON-entitlement reason is a hard error,
	// not exit 45.
	boom := fmt.Errorf("connection refused")
	var out, errOut bytes.Buffer
	opts := dumpOptions{resource: "tag-history", domainIDs: []int64{7}, gated: true, windowed: true, ws: "2026-01-01", we: "2026-01-01", windowDays: 100}
	err := runDump(context.Background(), errFetch(boom), &out, &errOut, opts, nil)
	if err == nil {
		t.Fatal("expected hard error")
	}
	if code := ExitCode(err); code == 45 {
		t.Error("a non-entitlement failure must not map to exit 45")
	}
}

func TestIsUnentitledErr(t *testing.T) {
	if !isUnentitledErr(fmt.Errorf("plan does not include this")) {
		t.Error("paywall body should be unentitled")
	}
	if !isUnentitledErr(&client.APIError{StatusCode: 403, Body: "forbidden"}) {
		t.Error("403 should be unentitled")
	}
	if isUnentitledErr(fmt.Errorf("some network error")) {
		t.Error("generic error should not be unentitled")
	}
	if isUnentitledErr(&client.APIError{StatusCode: 500, Body: "boom"}) {
		t.Error("500 should not be unentitled")
	}
}

func TestKeywordRanksInitialRowEmittedAndDeduped(t *testing.T) {
	// Two chunks return the same daily row (must dedup) plus a distinct
	// is_initial baseline row (must survive because its key includes is_initial).
	rows := map[string][]map[string]any{
		"2026-01-01..2026-01-01": {
			{"keyword_id": int64(1), "created_at": "2026-01-01T04:00:00Z", "is_initial": false},
			{"keyword_id": int64(1), "created_at": "2025-06-01T00:00:00Z", "is_initial": true},
		},
		"2026-01-02..2026-01-02": {
			{"keyword_id": int64(1), "created_at": "2025-06-01T00:00:00Z", "is_initial": true}, // dup baseline
			{"keyword_id": int64(1), "created_at": "2026-01-02T04:00:00Z", "is_initial": false},
		},
	}
	fetch := func(_ context.Context, _ int64, from, to string) ([]dumpBatch, error) {
		return oneBatch(rows[from+".."+to], "/x/", url.Values{}), nil
	}
	var out, errOut bytes.Buffer
	opts := dumpOptions{resource: "keyword-ranks", domainIDs: []int64{7}, windowed: true, ws: "2026-01-01", we: "2026-01-02", windowDays: 1}
	if err := runDump(context.Background(), fetch, &out, &errOut, opts, nil); err != nil {
		t.Fatalf("runDump: %v", err)
	}
	rep := decodeReport(t, &errOut)
	// 2 daily rows + 1 deduped baseline = 3.
	if rep.RowsEmitted != 3 {
		t.Errorf("rows_emitted = %d, want 3 (2 daily + 1 baseline)", rep.RowsEmitted)
	}
	initialCount := 0
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var row map[string]any
		_ = json.Unmarshal([]byte(line), &row)
		if init, _ := row["is_initial"].(bool); init {
			initialCount++
		}
	}
	if initialCount != 1 {
		t.Errorf("is_initial=true rows in output = %d, want exactly 1", initialCount)
	}
}

func TestProbeEarliestFindsBoundary(t *testing.T) {
	// Data exists from 2020-01-01 onward; the probe (100-day windows) must find
	// a floor at or before that date and never after it.
	fetch := func(_ context.Context, _ int64, from, _ string) ([]dumpBatch, error) {
		if from >= "2020-01-01" {
			return oneBatch([]map[string]any{{"keyword_id": int64(1), "created_at": from}}, "/x/", url.Values{}), nil
		}
		return oneBatch(nil, "/x/", url.Values{}), nil
	}
	var out, errOut bytes.Buffer
	opts := dumpOptions{resource: "keyword-ranks", domainIDs: []int64{7}, windowed: true, we: "2026-07-21", windowDays: 100, probeEarliest: true}
	if err := runDump(context.Background(), fetch, &out, &errOut, opts, nil); err != nil {
		t.Fatalf("runDump probe: %v", err)
	}
	rep := decodeReport(t, &errOut)
	got := rep.EarliestAvailable["7"]
	if got == "" || got == "none" {
		t.Fatalf("earliest_available[7] = %q, want a date", got)
	}
	if got > "2020-01-01" {
		t.Errorf("earliest_available[7] = %q, must be <= 2020-01-01 (conservative floor)", got)
	}
	if out.Len() != 0 {
		t.Errorf("probe mode must not stream rows, got %d bytes on stdout", out.Len())
	}
}
