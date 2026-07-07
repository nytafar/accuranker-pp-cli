// Tests for the panel-events mode of keywords-diff (spec F4).
package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestComputePanelEventsPartition(t *testing.T) {
	previous := []panelKeyword{
		{ID: 1, Keyword: "kept", CreatedAt: "2025-01-01T00:00:00Z"},
		{ID: 2, Keyword: "deleted", CreatedAt: "2025-02-01T00:00:00Z"},
	}
	current := []panelKeyword{
		{ID: 1, Keyword: "kept", CreatedAt: "2025-01-01T00:00:00Z"},
		{ID: 3, Keyword: "fresh", CreatedAt: "2026-07-01T00:00:00Z"},
	}
	events := computePanelEvents(current, previous, "2026-07-07T00:00:00Z")
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2: %v", len(events), events)
	}
	// Deterministic order: removed first, then added.
	if events[0].Event != "removed" || events[0].KeywordID != 2 || events[0].CreatedAt != "2025-02-01T00:00:00Z" {
		t.Errorf("removed event wrong: %+v", events[0])
	}
	if events[1].Event != "added" || events[1].KeywordID != 3 || events[1].Keyword != "fresh" {
		t.Errorf("added event wrong: %+v", events[1])
	}
	if events[0].ObservedAt != "2026-07-07T00:00:00Z" {
		t.Errorf("observed_at not stamped: %+v", events[0])
	}
}

func TestComputePanelEventsIdempotentOnIdenticalInputs(t *testing.T) {
	panel := []panelKeyword{{ID: 1, Keyword: "same"}, {ID: 2, Keyword: "also same"}}
	if events := computePanelEvents(panel, panel, "2026-07-07T00:00:00Z"); len(events) != 0 {
		t.Errorf("identical inputs must emit nothing, got %v", events)
	}
}

func TestComputePanelEventsDeterministicOrder(t *testing.T) {
	previous := []panelKeyword{{ID: 9}, {ID: 4}, {ID: 6}}
	current := []panelKeyword{{ID: 12}, {ID: 10}}
	a := computePanelEvents(current, previous, "t")
	b := computePanelEvents(current, previous, "t")
	if !reflect.DeepEqual(a, b) {
		t.Error("same inputs must produce identical event streams")
	}
	wantIDs := []int64{4, 6, 9, 10, 12} // removed sorted, then added sorted
	for i, ev := range a {
		if ev.KeywordID != wantIDs[i] {
			t.Fatalf("order[%d] = %d, want %d (%v)", i, ev.KeywordID, wantIDs[i], a)
		}
	}
}

func TestLoadPanelFromNDJSONRawAndEnveloped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snap.ndjson")
	content := `{"id": 1, "keyword": "raw row", "created_at": "2025-01-01T00:00:00Z"}
{"source":"accuranker","endpoint":"/api/v4/domains/7/keywords/","payload":{"id": 2, "keyword": "enveloped row", "created_at": "2025-02-01T00:00:00Z"}}

{"keyword": "no id — skipped"}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadPanelFromNDJSON(path)
	if err != nil {
		t.Fatalf("loadPanelFromNDJSON: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("rows = %d, want 2: %v", len(got), got)
	}
	if got[0].ID != 1 || got[0].Keyword != "raw row" {
		t.Errorf("raw row wrong: %+v", got[0])
	}
	if got[1].ID != 2 || got[1].Keyword != "enveloped row" || got[1].CreatedAt != "2025-02-01T00:00:00Z" {
		t.Errorf("enveloped row wrong: %+v", got[1])
	}
}
