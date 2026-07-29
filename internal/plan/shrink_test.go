package plan_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
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
	pl, err := plan.AnalyzePreShrink(context.Background(), r, p, nil, &bytes.Buffer{})
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
	pl, err := plan.AnalyzePreShrink(context.Background(), r, p, nil, &bytes.Buffer{})
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
	pl, err := plan.AnalyzePreShrink(context.Background(), r, p, nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("AnalyzePreShrink: %v", err)
	}
	if len(pl.Reorganizes) != 1 || pl.Reorganizes[0].Index != "PK_P" {
		t.Errorf("reorganizes = %+v, want PK_P selected (summed page count 5000 >= floor 4000, worst density 40 < threshold 50)", pl.Reorganizes)
	}
}

// TestAnalyzePreShrinkIndexIgnoresEmptyPartitionDensity covers FIX 1: an empty
// partition (page_count=0) reports density 0 (ISNULL-mapped), which must not drag
// the index's worst-density down and force a spurious reorganize of an otherwise
// dense index. Only partitions with page_count > 0 should count toward the
// minimum. Density 80 is well above the 50 threshold, so with the empty partition
// correctly ignored the index must NOT be reorganized.
func TestAnalyzePreShrinkIndexIgnoresEmptyPartitionDensity(t *testing.T) {
	p, err := maint.Parse([]byte("index:\n  page_count_floor: 1000\nshrink:\n  enabled: true\n  type: data\n  files: all\n  targetfreespace: 10%\n  reorganize_below_density_percent: 50\n"))
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	r := &fakeShrinkReader{
		inv: []mssql.InventoryObject{
			{Schema: "dbo", Table: "P", ObjectID: 1, IndexID: 1, IndexName: "PK_P", Type: 1, PartitionNumber: 1, SizeMB: 100},
		},
		rows: map[int64][]mssql.PhysicalStats{
			1: {
				{PartitionNumber: 1, PageCount: 5000, AvgPageSpaceUsedPercent: 80, RecordCount: 100},
				{PartitionNumber: 2, PageCount: 0, AvgPageSpaceUsedPercent: 0, RecordCount: 0}, // empty partition
			},
		},
	}
	pl, err := plan.AnalyzePreShrink(context.Background(), r, p, nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("AnalyzePreShrink: %v", err)
	}
	if len(pl.Reorganizes) != 0 {
		t.Errorf("reorganizes = %+v, want none: empty partition's density=0 must not drag down the true 80%% density", pl.Reorganizes)
	}
}

// TestAnalyzePreShrinkHeapIgnoresEmptyPartitionDensity is the heap analogue of the
// above: an empty partition's density=0 must not force a spuriously dense-below-
// threshold heap advisory.
func TestAnalyzePreShrinkHeapIgnoresEmptyPartitionDensity(t *testing.T) {
	p, err := maint.Parse([]byte("heap:\n  min_size_mb: 10\nshrink:\n  enabled: true\n  type: data\n  files: all\n  targetfreespace: 10%\n  reorganize_below_density_percent: 50\n"))
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	r := &fakeShrinkReader{
		inv: []mssql.InventoryObject{
			{Schema: "dbo", Table: "H", ObjectID: 2, IndexID: 0, IndexName: "", Type: 0, PartitionNumber: 1, SizeMB: 3000},
		},
		rows: map[int64][]mssql.PhysicalStats{
			2: {
				{PartitionNumber: 1, PageCount: 5000, AvgPageSpaceUsedPercent: 80, RecordCount: 100},
				{PartitionNumber: 2, PageCount: 0, AvgPageSpaceUsedPercent: 0, RecordCount: 0}, // empty partition
			},
		},
	}
	pl, err := plan.AnalyzePreShrink(context.Background(), r, p, nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("AnalyzePreShrink: %v", err)
	}
	if len(pl.HeapAdvisories) != 0 {
		t.Errorf("advisories = %+v, want none: empty partition's density=0 must not drag down the true 80%% density", pl.HeapAdvisories)
	}
}

// TestAnalyzePreShrinkHeapSizeSumsAcrossPartitions covers FIX 5: a heap's SizeMB in
// InventoryObject is per-PARTITION, not per-object, so shrinkHeapMeasurement must
// sum SizeMB across the whole partition group before applying the min_size_mb
// pre-filter. Each partition is below min_size_mb alone but their sum clears it.
func TestAnalyzePreShrinkHeapSizeSumsAcrossPartitions(t *testing.T) {
	p, err := maint.Parse([]byte("heap:\n  min_size_mb: 10\nshrink:\n  enabled: true\n  type: data\n  files: all\n  targetfreespace: 10%\n  reorganize_below_density_percent: 65\n"))
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	r := &fakeShrinkReader{
		inv: []mssql.InventoryObject{
			{Schema: "dbo", Table: "H", ObjectID: 2, IndexID: 0, IndexName: "", Type: 0, PartitionNumber: 1, SizeMB: 6},
			{Schema: "dbo", Table: "H", ObjectID: 2, IndexID: 0, IndexName: "", Type: 0, PartitionNumber: 2, SizeMB: 6},
		},
		rows: map[int64][]mssql.PhysicalStats{
			2: {
				{PartitionNumber: 1, PageCount: 1000, AvgPageSpaceUsedPercent: 40, RecordCount: 100},
				{PartitionNumber: 2, PageCount: 1000, AvgPageSpaceUsedPercent: 40, RecordCount: 100},
			},
		},
	}
	pl, err := plan.AnalyzePreShrink(context.Background(), r, p, nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("AnalyzePreShrink: %v", err)
	}
	if len(pl.HeapAdvisories) != 1 {
		t.Fatalf("advisories = %+v, want one: partitions (6+6=12 MB) sum above min_size_mb 10, though each alone is below", pl.HeapAdvisories)
	}
	if pl.HeapAdvisories[0].SizeMB != 12 {
		t.Errorf("SizeMB = %d, want 12 (summed across partitions)", pl.HeapAdvisories[0].SizeMB)
	}
}

// TestAnalyzePreShrinkNotFoundOnlyForGenuinelyMissing covers FIX 4: "confirmed
// object not found" must mean genuinely absent from the database (dropped/renamed),
// not merely filtered out by a downstream rule (e.g. a heap below min_size_mb). id 2
// is present in the raw inventory but filtered out by the heap size floor, so it
// must NOT be logged "not found"; id 99 is absent from the inventory entirely and
// must be logged.
func TestAnalyzePreShrinkNotFoundOnlyForGenuinelyMissing(t *testing.T) {
	p, err := maint.Parse([]byte("heap:\n  min_size_mb: 1000\nshrink:\n  enabled: true\n  type: data\n  files: all\n  targetfreespace: 10%\n"))
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	r := &fakeShrinkReader{
		inv: []mssql.InventoryObject{
			{Schema: "dbo", Table: "H", ObjectID: 2, IndexID: 0, IndexName: "", Type: 0, PartitionNumber: 1, SizeMB: 5}, // below min_size_mb 1000
		},
		density: map[int64]float64{2: 40},
		pages:   map[int64]int64{2: 100},
	}
	var logbuf bytes.Buffer
	_, err = plan.AnalyzePreShrink(context.Background(), r, p, map[int64]maint.Confirmation{2: {TimesBlocked: 1}, 99: {TimesBlocked: 1}}, &logbuf)
	if err != nil {
		t.Fatalf("AnalyzePreShrink: %v", err)
	}
	log := logbuf.String()
	if strings.Contains(log, "confirmed object 2 not found") {
		t.Errorf("object 2 is present in inventory (just filtered by size); must not be logged not-found:\n%s", log)
	}
	if !strings.Contains(log, "confirmed object 99 not found") {
		t.Errorf("object 99 is absent from the inventory entirely; must be logged not-found:\n%s", log)
	}
}
