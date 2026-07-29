package maint_test

import (
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/maint"
)

func shrinkProfile(t *testing.T) *maint.Profile {
	t.Helper()
	p, err := maint.Parse([]byte("index:\n  page_count_floor: 1000\nheap:\n  min_size_mb: 10\nshrink:\n  enabled: true\n  type: data\n  files: all\n  targetfreespace: 10%\n  reorganize_below_density_percent: 65\n"))
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	return p
}

func TestDecidePreShrinkSelectsLowDensityIndexes(t *testing.T) {
	p := shrinkProfile(t)
	indexes := []maint.ShrinkIndexMeasurement{
		{Schema: "dbo", Table: "A", Index: "PK_A", PageCount: 5000, AvgPageSpaceUsedPercent: 40}, // below 65 → reorganize
		{Schema: "dbo", Table: "B", Index: "PK_B", PageCount: 5000, AvgPageSpaceUsedPercent: 90}, // dense → skip
		{Schema: "dbo", Table: "C", Index: "PK_C", PageCount: 100, AvgPageSpaceUsedPercent: 10},  // below floor → skip
	}
	pl := maint.DecidePreShrink(indexes, nil, p, nil)
	if len(pl.Reorganizes) != 1 {
		t.Fatalf("got %d reorganizes, want 1: %+v", len(pl.Reorganizes), pl.Reorganizes)
	}
	ro := pl.Reorganizes[0]
	if ro.Schema != "dbo" || ro.Table != "A" || ro.Index != "PK_A" {
		t.Errorf("reorganize target = %+v, want dbo.A.PK_A", ro)
	}
}

func TestDecidePreShrinkHeapAdvisory(t *testing.T) {
	p := shrinkProfile(t)
	heaps := []maint.ShrinkHeapMeasurement{
		{Schema: "dbo", Table: "H", SizeMB: 3000, ForwardedRecordPercent: 12, AvgPageSpaceUsedPercent: 55}, // advise
		{Schema: "dbo", Table: "D", SizeMB: 3000, AvgPageSpaceUsedPercent: 80},                             // dense → skip
		{Schema: "dbo", Table: "S", SizeMB: 5, AvgPageSpaceUsedPercent: 10},                                // below min size → skip
	}
	pl := maint.DecidePreShrink(nil, heaps, p, nil)
	if len(pl.HeapAdvisories) != 1 || pl.HeapAdvisories[0].Table != "H" {
		t.Fatalf("got %+v, want one advisory for dbo.H", pl.HeapAdvisories)
	}
	// A heap is never emitted as a reorganize.
	if len(pl.Reorganizes) != 0 {
		t.Errorf("heaps must not produce reorganizes, got %+v", pl.Reorganizes)
	}
}

func TestDecidePreShrinkHonorsOverrideSkip(t *testing.T) {
	p, err := maint.Parse([]byte("index:\n  page_count_floor: 1000\nshrink:\n  enabled: true\n  type: data\n  files: all\n  targetfreespace: 10%\noverrides:\n  - match: dbo.A\n    skip: true\n"))
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	indexes := []maint.ShrinkIndexMeasurement{{Schema: "dbo", Table: "A", Index: "PK_A", PageCount: 5000, AvgPageSpaceUsedPercent: 40}}
	if pl := maint.DecidePreShrink(indexes, nil, p, nil); len(pl.Reorganizes) != 0 {
		t.Errorf("override skip must drop the reorganize, got %+v", pl.Reorganizes)
	}
}

func TestDecidePreShrinkReorganizeCarriesLOBCompaction(t *testing.T) {
	p, err := maint.Parse([]byte("index:\n  page_count_floor: 1000\n  lob_compaction: true\nshrink:\n  enabled: true\n  type: data\n  files: all\n  targetfreespace: 10%\n"))
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	indexes := []maint.ShrinkIndexMeasurement{{Schema: "dbo", Table: "A", Index: "PK_A", PageCount: 5000, AvgPageSpaceUsedPercent: 40}}
	pl := maint.DecidePreShrink(indexes, nil, p, nil)
	if len(pl.Reorganizes) != 1 || !pl.Reorganizes[0].LOBCompaction {
		t.Errorf("reorganize should carry LOBCompaction=true, got %+v", pl.Reorganizes)
	}
	_ = ddl.ReorganizeIndex{} // ensure ddl import is used
}

// profileWithShrink builds a *Profile with the given reorganize-below-density
// threshold and index page-count floor, and a heap min size low enough not to
// interfere with the confirmed-blocker tests.
func profileWithShrink(threshold, pageFloor int) *maint.Profile {
	return &maint.Profile{
		Index: maint.IndexRules{PageCountFloor: pageFloor},
		Heap:  maint.HeapRules{MinSizeMB: 10},
		Shrink: maint.ShrinkRules{
			Enabled: true, Type: "data", Files: "all", TargetFreeSpace: "10%",
			ReorganizeBelowDensityPercent: float64(threshold),
		},
	}
}

func TestDecidePreShrinkNilConfirmedUnchanged(t *testing.T) {
	// Baseline: with nil confirmed, output equals the pre-existing behavior.
	idx := []maint.ShrinkIndexMeasurement{{ObjectID: 1, Schema: "dbo", Table: "A", Index: "IX", PageCount: 5000, AvgPageSpaceUsedPercent: 40}}
	p := profileWithShrink(70, 1000) // helper: threshold 70, page_count_floor 1000
	pl := maint.DecidePreShrink(idx, nil, p, nil)
	if len(pl.Reorganizes) != 1 || len(pl.ReorganizeNotes) != 1 || pl.ReorganizeNotes[0] != "" {
		t.Fatalf("baseline = %+v notes %v", pl.Reorganizes, pl.ReorganizeNotes)
	}
}

func TestDecidePreShrinkConfirmedReordersToHead(t *testing.T) {
	idx := []maint.ShrinkIndexMeasurement{
		{ObjectID: 1, Schema: "dbo", Table: "A", Index: "IXA", PageCount: 5000, AvgPageSpaceUsedPercent: 40},
		{ObjectID: 2, Schema: "dbo", Table: "B", Index: "IXB", PageCount: 5000, AvgPageSpaceUsedPercent: 40},
	}
	p := profileWithShrink(70, 1000)
	pl := maint.DecidePreShrink(idx, nil, p, map[int64]int{2: 3}) // B confirmed
	if pl.Reorganizes[0].Table != "B" {
		t.Errorf("confirmed B not first: %+v", pl.Reorganizes)
	}
	if pl.ReorganizeNotes[0] != "confirmed blocker (times_blocked=3)" {
		t.Errorf("note = %q", pl.ReorganizeNotes[0])
	}
}

func TestDecidePreShrinkConfirmedDenseAddedDespiteDensity(t *testing.T) {
	// C is DENSE (85% >= threshold 70) so density skips it, but it is confirmed.
	idx := []maint.ShrinkIndexMeasurement{
		{ObjectID: 3, Schema: "dbo", Table: "C", Index: "IXC", PageCount: 5000, AvgPageSpaceUsedPercent: 85},
	}
	p := profileWithShrink(70, 1000)
	pl := maint.DecidePreShrink(idx, nil, p, map[int64]int{3: 1})
	if len(pl.Reorganizes) != 1 || pl.Reorganizes[0].Table != "C" {
		t.Fatalf("dense-confirmed not added: %+v", pl.Reorganizes)
	}
	if pl.ReorganizeNotes[0] != "confirmed blocker — added despite density" {
		t.Errorf("note = %q", pl.ReorganizeNotes[0])
	}
}

func TestDecidePreShrinkConfirmedHeapMarked(t *testing.T) {
	heaps := []maint.ShrinkHeapMeasurement{{ObjectID: 9, Schema: "dbo", Table: "H", SizeMB: 500, AvgPageSpaceUsedPercent: 40}}
	p := profileWithShrink(70, 1000) // heap.min_size_mb small enough in the helper
	pl := maint.DecidePreShrink(nil, heaps, p, map[int64]int{9: 4})
	if len(pl.HeapAdvisories) != 1 || !pl.HeapAdvisories[0].Confirmed || pl.HeapAdvisories[0].TimesBlocked != 4 {
		t.Errorf("heap not marked confirmed: %+v", pl.HeapAdvisories)
	}
}

// TestDecidePreShrinkConfirmedDenseHeapAddedDespiteDensity covers FIX 2: a
// confirmed heap must be surfaced regardless of density (mirroring the index
// path's "added despite density" behavior), because losing the empirical signal
// means an actual observed blocker never gets marked CONFIRMED. Density 80 is
// above the 70 threshold, so an unconfirmed heap at the same density must still be
// skipped.
func TestDecidePreShrinkConfirmedDenseHeapAddedDespiteDensity(t *testing.T) {
	p := profileWithShrink(70, 1000) // heap.min_size_mb small enough in the helper
	heaps := []maint.ShrinkHeapMeasurement{
		{ObjectID: 9, Schema: "dbo", Table: "H", SizeMB: 500, AvgPageSpaceUsedPercent: 80},  // dense, confirmed
		{ObjectID: 10, Schema: "dbo", Table: "D", SizeMB: 500, AvgPageSpaceUsedPercent: 80}, // dense, not confirmed
	}
	pl := maint.DecidePreShrink(nil, heaps, p, map[int64]int{9: 2})
	if len(pl.HeapAdvisories) != 1 {
		t.Fatalf("advisories = %+v, want exactly one (confirmed dense heap kept, unconfirmed dense heap skipped)", pl.HeapAdvisories)
	}
	a := pl.HeapAdvisories[0]
	if a.Table != "H" || !a.Confirmed || a.TimesBlocked != 2 {
		t.Errorf("advisory = %+v, want confirmed dbo.H times_blocked=2", a)
	}
}

func TestDecidePreShrinkDensityBoundary(t *testing.T) {
	p := shrinkProfile(t) // threshold 65
	indexes := []maint.ShrinkIndexMeasurement{
		{Schema: "dbo", Table: "AtThreshold", Index: "IX", PageCount: 5000, AvgPageSpaceUsedPercent: 65}, // == threshold → skip (only below qualifies)
		{Schema: "dbo", Table: "Below", Index: "IX", PageCount: 5000, AvgPageSpaceUsedPercent: 64.9},     // below → reorganize
	}
	pl := maint.DecidePreShrink(indexes, nil, p, nil)
	if len(pl.Reorganizes) != 1 || pl.Reorganizes[0].Table != "Below" {
		t.Errorf("boundary: density == threshold must be skipped; got %+v", pl.Reorganizes)
	}
}
