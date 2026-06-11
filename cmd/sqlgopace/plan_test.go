package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/rudi-bruchez/SqlGoPace/internal/config"
	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/maint"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
	"github.com/rudi-bruchez/SqlGoPace/internal/report"
)

// fakeReader serves canned analysis results, keyed so buildInput's orchestration
// can be exercised without a database.
type fakeReader struct {
	inventory []mssql.InventoryObject
	physical  map[string][]mssql.PhysicalStats     // "objectID:indexID:mode"
	estimate  map[string][]mssql.CompressionSaving // "schema.table:indexID:setting"
	opstats   map[string][]mssql.OperationalStats  // "objectID:indexID"
	stats     map[int64][]mssql.StatProperty       // objectID
}

func (f *fakeReader) ObjectInventory(context.Context) ([]mssql.InventoryObject, error) {
	return f.inventory, nil
}
func (f *fakeReader) PhysicalStats(_ context.Context, objectID int64, indexID int, _ *int, mode string) ([]mssql.PhysicalStats, error) {
	return f.physical[fmt.Sprintf("%d:%d:%s", objectID, indexID, mode)], nil
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
	in, err := buildInput(context.Background(), scenarioReader(), p, categorySet{}, "MYDB", io.Discard)
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
	in, err := buildInput(context.Background(), scenarioReader(), p, categorySet{"checkdb": true}, "MYDB", io.Discard)
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

func TestManifestsFromPlanOrderingAndWrite(t *testing.T) {
	p := scenarioProfile(t)
	in, _ := buildInput(context.Background(), scenarioReader(), p, categorySet{}, "MYDB", io.Discard)
	manifests := manifestsFromPlan(maint.Decide(in, p), "MYDB")

	// checkdb (010) → index (020) → heap (030) → statistics (040).
	wantFiles := []string{
		"010_maint_MYDB_checkdb.yaml",
		"020_maint_MYDB_index.yaml",
		"030_maint_MYDB_heaps.yaml",
		"040_maint_MYDB_statistics.yaml",
	}
	if len(manifests) != len(wantFiles) {
		t.Fatalf("manifests = %d, want %d", len(manifests), len(wantFiles))
	}
	for i, nm := range manifests {
		if nm.filename != wantFiles[i] {
			t.Errorf("manifest[%d] = %q, want %q", i, nm.filename, wantFiles[i])
		}
	}

	// Written manifests must load back through the real parser.
	dir := t.TempDir()
	if err := writeManifests(io.Discard, dir, manifests); err != nil {
		t.Fatalf("writeManifests() error = %v", err)
	}
	for _, nm := range manifests {
		m, err := ddl.LoadManifestFile(filepath.Join(dir, nm.filename))
		if err != nil {
			t.Errorf("LoadManifestFile(%s) error = %v", nm.filename, err)
			continue
		}
		if len(m.Operations) != len(nm.manifest.Operations) {
			t.Errorf("%s: loaded %d operations, want %d", nm.filename, len(m.Operations), len(nm.manifest.Operations))
		}
	}
}

func TestAutoPlan(t *testing.T) {
	p := scenarioProfile(t)
	manifests, plan, err := autoPlan(context.Background(), scenarioReader(), p, categorySet{}, "MYDB", io.Discard)
	if err != nil {
		t.Fatalf("autoPlan() error = %v", err)
	}
	// Same shape as the plan subcommand: checkdb, index, heaps, statistics.
	if len(manifests) != 4 {
		t.Errorf("autoPlan manifests = %d, want 4", len(manifests))
	}
	if len(plan.Operations()) != 4 {
		t.Errorf("autoPlan operations = %d, want 4 (rebuild_index, rebuild_heap, update_statistics, check_db)", len(plan.Operations()))
	}
}

func TestRecordPlanHistory(t *testing.T) {
	p := scenarioProfile(t)
	in, _ := buildInput(context.Background(), scenarioReader(), p, categorySet{}, "MYDB", io.Discard)
	plan := maint.Decide(in, p)
	manifests := manifestsFromPlan(plan, "MYDB")

	dbPath := filepath.Join(t.TempDir(), "history.db")
	cfg := &config.Config{History: config.HistoryConfig{Enabled: true, Destination: "sqlite://" + dbPath}}

	recordPlanHistory(context.Background(), io.Discard, cfg, "MYDB", plan, manifests)

	h, err := report.OpenHistory(dbPath)
	if err != nil {
		t.Fatalf("OpenHistory() error = %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	plans, rows, err := h.MaintenanceCounts(context.Background())
	if err != nil {
		t.Fatalf("MaintenanceCounts() error = %v", err)
	}
	// One plan; one row per emitted op: rebuild_index + rebuild_heap + update_statistics + check_db.
	if plans != 1 || rows != 4 {
		t.Errorf("counts = (%d plans, %d rows), want (1, 4)", plans, rows)
	}
}

func TestRecordPlanHistoryDisabled(t *testing.T) {
	// With history disabled, recording is a no-op and must not error or create a file.
	cfg := &config.Config{History: config.HistoryConfig{Enabled: false}}
	recordPlanHistory(context.Background(), io.Discard, cfg, "MYDB", maint.Plan{}, nil)
}

func TestManifestsMultiDatabaseBlocks(t *testing.T) {
	p := scenarioProfile(t)
	in, _ := buildInput(context.Background(), scenarioReader(), p, categorySet{}, "DB1", io.Discard)
	plan := maint.Decide(in, p)

	width := prefixWidth(2)
	got := append(manifestsForDatabase(plan, "DB1", 0, width), manifestsForDatabase(plan, "DB2", 1, width)...)

	want := []string{
		"010_maint_DB1_checkdb.yaml", "020_maint_DB1_index.yaml",
		"030_maint_DB1_heaps.yaml", "040_maint_DB1_statistics.yaml",
		"050_maint_DB2_checkdb.yaml", "060_maint_DB2_index.yaml",
		"070_maint_DB2_heaps.yaml", "080_maint_DB2_statistics.yaml",
	}
	var gotNames []string
	for _, nm := range got {
		gotNames = append(gotNames, nm.filename)
	}
	if diff := cmp.Diff(want, gotNames); diff != "" {
		t.Errorf("multi-database manifest names mismatch (-want +got):\n%s", diff)
	}
}

func TestPrefixWidth(t *testing.T) {
	tests := []struct {
		databases int
		want      int
	}{
		{1, 3}, {2, 3}, {24, 3}, {25, 4}, {250, 5},
	}
	for _, tt := range tests {
		if got := prefixWidth(tt.databases); got != tt.want {
			t.Errorf("prefixWidth(%d) = %d, want %d", tt.databases, got, tt.want)
		}
	}
}

func TestParseCategories(t *testing.T) {
	if _, err := parseCategories("index, heaps , checkdb"); err != nil {
		t.Errorf("parseCategories(valid) error = %v", err)
	}
	if _, err := parseCategories("index,bogus"); err == nil {
		t.Errorf("parseCategories(bogus) error = nil, want an error")
	}
	set, err := parseCategories("")
	if err != nil || !set.has("anything") {
		t.Errorf("empty categories should mean all; has() = %t err = %v", set.has("anything"), err)
	}
}
