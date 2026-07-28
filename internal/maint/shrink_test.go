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
	pl := maint.DecidePreShrink(indexes, nil, p)
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
	pl := maint.DecidePreShrink(nil, heaps, p)
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
	if pl := maint.DecidePreShrink(indexes, nil, p); len(pl.Reorganizes) != 0 {
		t.Errorf("override skip must drop the reorganize, got %+v", pl.Reorganizes)
	}
}

func TestDecidePreShrinkReorganizeCarriesLOBCompaction(t *testing.T) {
	p, err := maint.Parse([]byte("index:\n  page_count_floor: 1000\n  lob_compaction: true\nshrink:\n  enabled: true\n  type: data\n  files: all\n  targetfreespace: 10%\n"))
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	indexes := []maint.ShrinkIndexMeasurement{{Schema: "dbo", Table: "A", Index: "PK_A", PageCount: 5000, AvgPageSpaceUsedPercent: 40}}
	pl := maint.DecidePreShrink(indexes, nil, p)
	if len(pl.Reorganizes) != 1 || !pl.Reorganizes[0].LOBCompaction {
		t.Errorf("reorganize should carry LOBCompaction=true, got %+v", pl.Reorganizes)
	}
	_ = ddl.ReorganizeIndex{} // ensure ddl import is used
}

func TestDecidePreShrinkDensityBoundary(t *testing.T) {
	p := shrinkProfile(t) // threshold 65
	indexes := []maint.ShrinkIndexMeasurement{
		{Schema: "dbo", Table: "AtThreshold", Index: "IX", PageCount: 5000, AvgPageSpaceUsedPercent: 65}, // == threshold → skip (only below qualifies)
		{Schema: "dbo", Table: "Below", Index: "IX", PageCount: 5000, AvgPageSpaceUsedPercent: 64.9},     // below → reorganize
	}
	pl := maint.DecidePreShrink(indexes, nil, p)
	if len(pl.Reorganizes) != 1 || pl.Reorganizes[0].Table != "Below" {
		t.Errorf("boundary: density == threshold must be skipped; got %+v", pl.Reorganizes)
	}
}
