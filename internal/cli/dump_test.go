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
)

func testFetch(rowsPerChunk map[string][]map[string]any, failOn string) dumpFetchFn {
	return func(_ context.Context, did int64, from, to string) (dumpBatch, error) {
		key := from + ".." + to
		if key == failOn {
			return dumpBatch{}, fmt.Errorf("boom on %s", key)
		}
		q := url.Values{}
		q.Set("period_from", from)
		q.Set("period_to", to)
		return dumpBatch{
			rows:     rowsPerChunk[key],
			endpoint: fmt.Sprintf("/api/v4/domains/%d/keywords/", did),
			params:   q,
		}, nil
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
	fetch := func(_ context.Context, did int64, from, to string) (dumpBatch, error) {
		if did == 8 && from == "2026-01-03" {
			return dumpBatch{}, fmt.Errorf("boom domain 8")
		}
		return dumpBatch{rows: []map[string]any{
			{"keyword_id": did, "created_at": from + "T04:00:00Z"},
		}, endpoint: "/x/", params: url.Values{}}, nil
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
	}
	for _, c := range cases {
		if got := dedupKey(c.resource, c.row); got != c.want {
			t.Errorf("dedupKey(%s) = %q, want %q", c.resource, got, c.want)
		}
	}
}
