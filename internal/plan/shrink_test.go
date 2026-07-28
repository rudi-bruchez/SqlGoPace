package plan_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/maint"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
	"github.com/rudi-bruchez/SqlGoPace/internal/plan"
)

// fakeShrinkReader returns canned inventory + sampled physical stats; the other Reader
// methods are unused by AnalyzePreShrink and return nil.
type fakeShrinkReader struct {
	inv        []mssql.InventoryObject
	density    map[int64]float64               // objectID → avg_page_space_used_in_percent
	pages      map[int64]int64                 // objectID → page_count
	errObjects map[int64]bool                  // objectID → PhysicalStats returns an error (per-object read failure)
	rows       map[int64][]mssql.PhysicalStats // objectID → explicit multi-partition rows (overrides density/pages)
}

func (f *fakeShrinkReader) ObjectInventory(context.Context) ([]mssql.InventoryObject, error) {
	return f.inv, nil
}
func (f *fakeShrinkReader) PhysicalStats(_ context.Context, objectID int64, _ int, _ *int, mode string) ([]mssql.PhysicalStats, error) {
	if f.errObjects[objectID] {
		return nil, fmt.Errorf("sampled scan failed for object %d", objectID)
	}
	if rows, ok := f.rows[objectID]; ok {
		return rows, nil
	}
	return []mssql.PhysicalStats{{
		PartitionNumber: 1, PageCount: f.pages[objectID],
		AvgPageSpaceUsedPercent: f.density[objectID], RecordCount: 100,
	}}, nil
}
func (f *fakeShrinkReader) EstimateCompression(context.Context, string, string, int, *int, string) ([]mssql.CompressionSaving, error) {
	return nil, nil
}
func (f *fakeShrinkReader) IndexOperationalStats(context.Context, int64, int, *int) ([]mssql.OperationalStats, error) {
	return nil, nil
}
func (f *fakeShrinkReader) StatsProperties(context.Context, int64) ([]mssql.StatProperty, error) {
	return nil, nil
}

func TestAnalyzePreShrink(t *testing.T) {
	p, err := maint.Parse([]byte("index:\n  page_count_floor: 1000\nheap:\n  min_size_mb: 10\nshrink:\n  enabled: true\n  type: data\n  files: all\n  targetfreespace: 10%\n  reorganize_below_density_percent: 65\n"))
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	r := &fakeShrinkReader{
		inv: []mssql.InventoryObject{
			{Schema: "dbo", Table: "A", ObjectID: 1, IndexID: 1, IndexName: "PK_A", Type: 1, PartitionNumber: 1, SizeMB: 100},
			{Schema: "dbo", Table: "H", ObjectID: 2, IndexID: 0, IndexName: "", Type: 0, PartitionNumber: 1, SizeMB: 3000},
		},
		density: map[int64]float64{1: 40, 2: 50},
		pages:   map[int64]int64{1: 5000, 2: 400000},
	}
	pl, err := plan.AnalyzePreShrink(context.Background(), r, p, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("AnalyzePreShrink: %v", err)
	}
	if len(pl.Reorganizes) != 1 || pl.Reorganizes[0].Index != "PK_A" {
		t.Errorf("reorganizes = %+v, want one for PK_A", pl.Reorganizes)
	}
	if len(pl.HeapAdvisories) != 1 || pl.HeapAdvisories[0].Table != "H" {
		t.Errorf("advisories = %+v, want one for dbo.H", pl.HeapAdvisories)
	}
}

func TestAnalyzePreShrinkSkipsFailedObjectNotFatal(t *testing.T) {
	p, err := maint.Parse([]byte("index:\n  page_count_floor: 1000\nshrink:\n  enabled: true\n  type: data\n  files: all\n  targetfreespace: 10%\n  reorganize_below_density_percent: 65\n"))
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	r := &fakeShrinkReader{
		inv: []mssql.InventoryObject{
			{Schema: "dbo", Table: "Bad", ObjectID: 1, IndexID: 1, IndexName: "IX_Bad", Type: 1, PartitionNumber: 1, SizeMB: 100},
			{Schema: "dbo", Table: "Good", ObjectID: 2, IndexID: 1, IndexName: "IX_Good", Type: 1, PartitionNumber: 1, SizeMB: 100},
		},
		density:    map[int64]float64{2: 40},
		pages:      map[int64]int64{2: 5000},
		errObjects: map[int64]bool{1: true}, // object 1's sampled scan errors; must be skipped, not fatal
	}
	pl, err := plan.AnalyzePreShrink(context.Background(), r, p, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("a per-object read error must not be fatal, got %v", err)
	}
	if len(pl.Reorganizes) != 1 || pl.Reorganizes[0].Table != "Good" {
		t.Errorf("failed object should be skipped, survivor kept; got %+v", pl.Reorganizes)
	}
}

// TestAnalyzePreShrinkAggregatesAcrossPartitions covers shrinkIndexMeasurement's
// multi-partition aggregation: page counts SUM across partitions and the worst (lowest)
// density wins. The floor and threshold are chosen so the test only passes under correct
// aggregation: neither partition's page count alone clears the floor (only their sum
// does), and neither the higher-density partition alone nor the average of the two would
// clear the reorganize threshold — only the true minimum does.
func TestAnalyzePreShrinkAggregatesAcrossPartitions(t *testing.T) {
	p, err := maint.Parse([]byte("index:\n  page_count_floor: 4000\nshrink:\n  enabled: true\n  type: data\n  files: all\n  targetfreespace: 10%\n  reorganize_below_density_percent: 50\n"))
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	r := &fakeShrinkReader{
		inv: []mssql.InventoryObject{
			{Schema: "dbo", Table: "P", ObjectID: 1, IndexID: 1, IndexName: "PK_P", Type: 1, PartitionNumber: 1, SizeMB: 100},
		},
		rows: map[int64][]mssql.PhysicalStats{
			1: {
				{PartitionNumber: 1, PageCount: 3000, AvgPageSpaceUsedPercent: 80, RecordCount: 100},
				{PartitionNumber: 2, PageCount: 2000, AvgPageSpaceUsedPercent: 40, RecordCount: 100},
			},
		},
	}
	pl, err := plan.AnalyzePreShrink(context.Background(), r, p, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("AnalyzePreShrink: %v", err)
	}
	if len(pl.Reorganizes) != 1 || pl.Reorganizes[0].Index != "PK_P" {
		t.Errorf("reorganizes = %+v, want PK_P selected (summed page count 5000 >= floor 4000, worst density 40 < threshold 50)", pl.Reorganizes)
	}
}
