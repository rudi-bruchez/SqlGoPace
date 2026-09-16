package plan

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/maint"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// fakeReader serves canned analysis results, keyed so buildInput's orchestration
// can be exercised without a database.
type fakeReader struct {
	inventory   []mssql.InventoryObject
	physical    map[string][]mssql.PhysicalStats     // "objectID:indexID:mode"
	estimate    map[string][]mssql.CompressionSaving // "schema.table:indexID:setting"
	opstats     map[string][]mssql.OperationalStats  // "objectID:indexID"
	stats       map[int64][]mssql.StatProperty       // objectID
	disabled    map[int64][]string                   // objectID -> disabled index names
	disabledErr map[int64]error                      // objectID -> DisabledIndexes error

	// physicalStatsKeys records every PhysicalStats call, same key shape as physical,
	// so a test can assert a scan was never paid for.
	physicalStatsKeys []string
}

func (f *fakeReader) ObjectInventory(context.Context) ([]mssql.InventoryObject, error) {
	return f.inventory, nil
}
func (f *fakeReader) PhysicalStats(_ context.Context, objectID int64, indexID int, _ *int, mode string) ([]mssql.PhysicalStats, error) {
	key := fmt.Sprintf("%d:%d:%s", objectID, indexID, mode)
	f.physicalStatsKeys = append(f.physicalStatsKeys, key)
	return f.physical[key], nil
}
func (f *fakeReader) EstimateCompression(_ context.Context, schema, table string, indexID int, _ *int, setting string) ([]mssql.CompressionSaving, error) {
	return f.estimate[fmt.Sprintf("%s.%s:%d:%s", schema, table, indexID, setting)], nil
}
func (f *fakeReader) IndexOperationalStats(_ context.Context, objectID int64, indexID int, _ *int) ([]mssql.OperationalStats, error) {
	return f.opstats[fmt.Sprintf("%d:%d", objectID, indexID)], nil
}
func (f *fakeReader) StatsProperties(_ context.Context, objectID int64) ([]mssql.StatProperty, error) {
	return f.stats[objectID], nil
}
func (f *fakeReader) DisabledIndexes(_ context.Context, objectID int64) ([]string, error) {
	if err := f.disabledErr[objectID]; err != nil {
		return nil, err
	}
	return f.disabled[objectID], nil
}

// scenarioReader builds a small but representative database state.
func scenarioReader() *fakeReader {
	return &fakeReader{
		inventory: []mssql.InventoryObject{
			{Schema: "dbo", Table: "ORDERS", ObjectID: 10, IndexID: 1, IndexName: "PK_ORDERS", Type: 1, TypeDesc: "CLUSTERED", PartitionNumber: 1, Compression: "NONE", Rows: 50_000_000, SizeMB: 8192},
			{Schema: "dbo", Table: "STAGING", ObjectID: 20, IndexID: 0, IndexName: "", Type: 0, TypeDesc: "HEAP", PartitionNumber: 1, Compression: "NONE", Rows: 1_000_000, SizeMB: 500},
		},
		physical: map[string][]mssql.PhysicalStats{
			"10:1:LIMITED": {{PartitionNumber: 1, AvgFragmentationPercent: 42, PageCount: 1_000_000}},
			"20:0:SAMPLED": {{PartitionNumber: 1, AvgFragmentationPercent: 5, PageCount: 60_000, RecordCount: 1_000_000, ForwardedRecordCount: 200_000, AvgPageSpaceUsedPercent: 90}},
		},
		estimate: map[string][]mssql.CompressionSaving{
			"dbo.ORDERS:1:ROW":  {{PartitionNumber: 1, CurrentKB: 8_000_000, RequestedKB: 5_500_000}},
			"dbo.ORDERS:1:PAGE": {{PartitionNumber: 1, CurrentKB: 8_000_000, RequestedKB: 4_000_000}},
		},
		opstats: map[string][]mssql.OperationalStats{
			"10:1": {{PartitionNumber: 1, LeafInsert: 100, RangeScan: 100_000}}, // read-heavy → no cap
		},
		stats: map[int64][]mssql.StatProperty{
			10: {
				{Name: "CustStats", Rows: 50_000_000, ModificationCounter: 9_000_000}, // stale, kept
				{Name: "PK_ORDERS", Rows: 50_000_000, ModificationCounter: 9_000_000}, // backs the rebuilt index → suppressed
			},
		},
	}
}

func scenarioProfile(t *testing.T) *maint.Profile {
	t.Helper()
	p, err := maint.Parse([]byte("compression:\n  enabled: true\nheap:\n  enabled: true\nstatistics:\n  enabled: true\ncheckdb:\n  enabled: true\n"))
	if err != nil {
		t.Fatalf("scenarioProfile: %v", err)
	}
	return p
}

func TestBuildInputAndDecide(t *testing.T) {
	p := scenarioProfile(t)
	in, err := buildInput(context.Background(), scenarioReader(), p, Categories{}, "MYDB", io.Discard)
	if err != nil {
		t.Fatalf("buildInput() error = %v", err)
	}
	if len(in.Indexes) != 1 || len(in.Heaps) != 1 {
		t.Fatalf("measurements: indexes=%d heaps=%d, want 1 and 1", len(in.Indexes), len(in.Heaps))
	}
	if in.Indexes[0].Estimate == nil {
		t.Errorf("ORDERS index has no compression estimate")
	}

	plan := maint.Decide(in, p)

	index := plan.OperationsByCategory("index")
	if len(index) != 1 {
		t.Fatalf("index ops = %d, want 1", len(index))
	}
	if ri, ok := index[0].(ddl.RebuildIndex); !ok || ri.DataCompression != "PAGE" || ri.Index != "PK_ORDERS" {
		t.Errorf("index op = %#v, want rebuild PK_ORDERS PAGE", index[0])
	}

	if heap := plan.OperationsByCategory("heap"); len(heap) != 1 {
		t.Errorf("heap ops = %d, want 1 (STAGING forwarded records)", len(heap))
	}

	stats := plan.OperationsByCategory("statistics")
	if len(stats) != 1 {
		t.Fatalf("statistics ops = %d, want 1 (CustStats kept, PK_ORDERS suppressed)", len(stats))
	}
	if us := stats[0].(ddl.UpdateStatistics); us.Statistic != "CustStats" {
		t.Errorf("statistic = %q, want CustStats (PK_ORDERS should be suppressed)", us.Statistic)
	}

	if cdb := plan.OperationsByCategory("checkdb"); len(cdb) != 1 {
		t.Errorf("checkdb ops = %d, want 1", len(cdb))
	}
}

func TestCategoryFilterRestrictsAnalysis(t *testing.T) {
	p := scenarioProfile(t)
	in, err := buildInput(context.Background(), scenarioReader(), p, Categories{"checkdb": true}, "MYDB", io.Discard)
	if err != nil {
		t.Fatalf("buildInput() error = %v", err)
	}
	if len(in.Indexes) != 0 || len(in.Heaps) != 0 || len(in.Statistics) != 0 {
		t.Errorf("with categories=checkdb: indexes=%d heaps=%d stats=%d, want all 0",
			len(in.Indexes), len(in.Heaps), len(in.Statistics))
	}
	plan := maint.Decide(in, p)
	if len(plan.OperationsByCategory("checkdb")) != 1 {
		t.Errorf("checkdb still expected")
	}
}

func TestAnalyze(t *testing.T) {
	p := scenarioProfile(t)
	plan, err := Analyze(context.Background(), scenarioReader(), p, Categories{}, "MYDB", io.Discard)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	// checkdb, index, heap, statistics: one emitted operation each.
	if got := len(plan.Operations()); got != 4 {
		t.Errorf("Analyze operations = %d, want 4 (rebuild_index, rebuild_heap, update_statistics, check_db)", got)
	}
}

// TestShrinkMeasurementsCarryObjectID exercises shrinkIndexMeasurement and
// shrinkHeapMeasurement directly (unexported, so this lives in the internal
// package plan test) and asserts the built measurement carries the inventory
// head's ObjectID, so a later pass can join the confirmed-blocker set by
// object id.
func TestShrinkMeasurementsCarryObjectID(t *testing.T) {
	r := &fakeReader{
		physical: map[string][]mssql.PhysicalStats{
			"10:1:SAMPLED": {{PartitionNumber: 1, PageCount: 5000, AvgPageSpaceUsedPercent: 40, RecordCount: 100}},
			"20:0:SAMPLED": {{PartitionNumber: 1, PageCount: 100, AvgPageSpaceUsedPercent: 50, RecordCount: 100, ForwardedRecordCount: 10}},
		},
	}
	p, err := maint.Parse([]byte("heap:\n  min_size_mb: 1\n"))
	if err != nil {
		t.Fatalf("profile: %v", err)
	}

	indexHead := mssql.InventoryObject{Schema: "dbo", Table: "ORDERS", ObjectID: 10, IndexID: 1, IndexName: "PK_ORDERS", Type: 1}
	im, ok := shrinkIndexMeasurement(context.Background(), r, indexHead, io.Discard)
	if !ok {
		t.Fatalf("shrinkIndexMeasurement() ok = false, want true")
	}
	if im.ObjectID != 10 {
		t.Errorf("index measurement ObjectID = %d, want 10 (the inventory head's object id)", im.ObjectID)
	}

	heapHead := mssql.InventoryObject{Schema: "dbo", Table: "STAGING", ObjectID: 20, IndexID: 0, SizeMB: 100}
	hm, ok := shrinkHeapMeasurement(context.Background(), r, p, []mssql.InventoryObject{heapHead}, io.Discard)
	if !ok {
		t.Fatalf("shrinkHeapMeasurement() ok = false, want true")
	}
	if hm.ObjectID != 20 {
		t.Errorf("heap measurement ObjectID = %d, want 20 (the inventory head's object id)", hm.ObjectID)
	}
}

// profileWithHeapBounds returns a profile with heap maintenance enabled and the given
// min/max size bounds, for the heap-measurement tests below.
func profileWithHeapBounds(minMB, maxMB int64) *maint.Profile {
	p, err := maint.Parse([]byte(fmt.Sprintf("heap:\n  enabled: true\n  min_size_mb: %d\n  max_size_mb: %d\n", minMB, maxMB)))
	if err != nil {
		panic(err)
	}
	return p
}

// TestHeapMeasurementSumsPartitionsAndIndexes: the fixed defect. The heap's own size is the
// sum over its partitions (plan.go read partition 1 only), and the rewrite adds every
// nonclustered index of the table.
func TestHeapMeasurementSumsPartitionsAndIndexes(t *testing.T) {
	r := &fakeReader{
		inventory: []mssql.InventoryObject{
			{Schema: "dbo", Table: "MEASUREMENT", ObjectID: 1, IndexID: 0, Type: 0, PartitionNumber: 1, SizeMB: 3000},
			{Schema: "dbo", Table: "MEASUREMENT", ObjectID: 1, IndexID: 0, Type: 0, PartitionNumber: 2, SizeMB: 2000},
			{Schema: "dbo", Table: "MEASUREMENT", ObjectID: 1, IndexID: 2, IndexName: "IX_A", Type: 2, PartitionNumber: 1, SizeMB: 1500},
		},
		// The SAMPLED scan (forwarded records) runs after the size and disabled-index
		// checks pass; a heap needs at least one row here to survive buildInput.
		physical: map[string][]mssql.PhysicalStats{
			"1:0:SAMPLED": {{PartitionNumber: 1, RecordCount: 100, ForwardedRecordCount: 10, AvgPageSpaceUsedPercent: 90}},
		},
	}
	in, err := buildInput(context.Background(), r, profileWithHeapBounds(10, 100000), Categories{}, "PRODDB", io.Discard)
	if err != nil {
		t.Fatalf("buildInput() error = %v", err)
	}
	if len(in.Heaps) != 1 {
		t.Fatalf("heaps = %d, want 1", len(in.Heaps))
	}
	h := in.Heaps[0]
	if h.SizeMB != 5000 || h.NonclusteredMB != 1500 || h.RewriteMB != 6500 || h.NonclusteredCount != 1 {
		t.Errorf("heap measurement = %+v, want heap 5000, nonclustered 1500 (1 index), rewrite 6500", h)
	}
}

// TestHeapMeasurementDisabledIndexReachesTheDecision: a heap carrying a disabled
// nonclustered index is measured, not dropped by the planner — the list travels in
// the measurement so DecideHeap refuses it and states the remedy in the analysis
// trail. The SAMPLED scan is not paid for a heap that will be refused.
func TestHeapMeasurementDisabledIndexReachesTheDecision(t *testing.T) {
	r := &fakeReader{
		inventory: []mssql.InventoryObject{
			{Schema: "dbo", Table: "MEASUREMENT", ObjectID: 1, IndexID: 0, Type: 0,
				PartitionNumber: 1, SizeMB: 500},
			{Schema: "dbo", Table: "MEASUREMENT", ObjectID: 1, IndexID: 2, IndexName: "IX_A",
				Type: 2, PartitionNumber: 1, SizeMB: 150},
		},
		disabled: map[int64][]string{1: {"IX_MEASUREMENT_OLD"}},
	}
	p := profileWithHeapBounds(10, 100000)
	var logbuf bytes.Buffer
	in, err := buildInput(context.Background(), r, p, Categories{}, "PRODDB", &logbuf)
	if err != nil {
		t.Fatalf("buildInput() error = %v", err)
	}
	if len(in.Heaps) != 1 {
		t.Fatalf("heaps = %d, want 1 (measured, with the disabled index listed)", len(in.Heaps))
	}
	h := in.Heaps[0]
	if !slices.Equal(h.DisabledIndexes, []string{"IX_MEASUREMENT_OLD"}) {
		t.Errorf("heap.DisabledIndexes = %v, want [IX_MEASUREMENT_OLD]", h.DisabledIndexes)
	}
	if h.SizeMB != 500 || h.NonclusteredMB != 150 || h.RewriteMB != 650 {
		t.Errorf("heap sizing: sizeMB=%d nonclusteredMB=%d rewriteMB=%d, want 500, 150, 650",
			h.SizeMB, h.NonclusteredMB, h.RewriteMB)
	}
	if !strings.Contains(logbuf.String(), "IX_MEASUREMENT_OLD") {
		t.Errorf("log = %q, want it to name the disabled index", logbuf.String())
	}
	if slices.Contains(r.physicalStatsKeys, "1:0:"+mssql.PhysicalSampled) {
		t.Errorf("PhysicalStats(SAMPLED) = called, want skipped for a heap that will be refused")
	}

	d := maint.DecideHeap(h, p)
	if d.Op != nil || d.Kind != "skip" {
		t.Errorf("decision = %q %#v, want a skip with no operation", d.Kind, d.Op)
	}
	if !strings.Contains(d.Reason, "IX_MEASUREMENT_OLD") ||
		!strings.Contains(d.Reason, "allow_reenable_disabled_indexes") {
		t.Errorf("decision.Reason = %q, want the index name and the remedy", d.Reason)
	}
}

// TestHeapMeasurementSkipsOnDisabledIndexReadError: an unverifiable heap is not planned.
func TestHeapMeasurementSkipsOnDisabledIndexReadError(t *testing.T) {
	r := &fakeReader{
		inventory: []mssql.InventoryObject{
			{Schema: "dbo", Table: "MEASUREMENT", ObjectID: 1, IndexID: 0, Type: 0, PartitionNumber: 1, SizeMB: 500},
		},
		disabledErr: map[int64]error{1: fmt.Errorf("permission denied")},
	}
	var logbuf bytes.Buffer
	in, err := buildInput(context.Background(), r, profileWithHeapBounds(10, 100000), Categories{}, "PRODDB", &logbuf)
	if err != nil {
		t.Fatalf("buildInput() error = %v", err)
	}
	if len(in.Heaps) != 0 {
		t.Fatalf("heaps = %d, want 0 (unverifiable heap)", len(in.Heaps))
	}
	if !strings.Contains(logbuf.String(), "permission denied") {
		t.Errorf("skip log = %q, want it to name the read error", logbuf.String())
	}
}

func TestParseCategories(t *testing.T) {
	if _, err := ParseCategories("index, heaps , checkdb"); err != nil {
		t.Errorf("ParseCategories(valid) error = %v", err)
	}
	if _, err := ParseCategories("index,bogus"); err == nil {
		t.Errorf("ParseCategories(bogus) error = nil, want an error")
	}
	set, err := ParseCategories("")
	if err != nil || !set.Has("anything") {
		t.Errorf("empty categories should mean all; Has() = %t err = %v", set.Has("anything"), err)
	}
}
