package main

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/rudi-bruchez/SqlGoPace/internal/config"
	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/maint"
	"github.com/rudi-bruchez/SqlGoPace/internal/report"
)

func scenarioProfile(t *testing.T) *maint.Profile {
	t.Helper()
	p, err := maint.Parse([]byte("compression:\n  enabled: true\nheap:\n  enabled: true\nstatistics:\n  enabled: true\ncheckdb:\n  enabled: true\n"))
	if err != nil {
		t.Fatalf("scenarioProfile: %v", err)
	}
	return p
}

// scenarioPlan builds the decision plan the analysis layer produces for a small,
// representative database: a fragmented clustered index (rebuild + PAGE), a heap
// with forwarded records (rebuild), a stale statistic (update, plus one suppressed
// because it backs the rebuilt index), and the database integrity check. This is
// the same shape internal/plan.Analyze yields for its scenarioReader, built here
// as a maint.Input literal so the manifest/history tests need no database reader.
func scenarioPlan(t *testing.T) maint.Plan {
	t.Helper()
	in := maint.Input{
		ConnDatabase: "MYDB",
		Indexes: []maint.IndexMeasurement{{
			Schema: "dbo", Table: "ORDERS", Index: "PK_ORDERS", Clustered: true,
			PageCount: 1_000_000, SizeMB: 8192, FragmentationPercent: 42, Current: maint.CompressionNone,
			Estimate: &maint.CompressionEstimate{CurrentKB: 8_000_000, RowKB: 5_500_000, PageKB: 4_000_000},
			Write:    &maint.WriteActivity{Writes: 100, Reads: 100_000},
		}},
		Heaps: []maint.HeapMeasurement{{
			Schema: "dbo", Table: "STAGING", SizeMB: 500,
			ForwardedRecordCount: 200_000, RecordCount: 1_000_000,
			FragmentationPercent: 5, PageSpaceUsedPercent: 90, Current: maint.CompressionNone,
		}},
		Statistics: []maint.StatMeasurement{
			{Schema: "dbo", Table: "ORDERS", Statistic: "CustStats", Rows: 50_000_000, ModificationCounter: 9_000_000},
			{Schema: "dbo", Table: "ORDERS", Statistic: "PK_ORDERS", Rows: 50_000_000, ModificationCounter: 9_000_000},
		},
	}
	return maint.Decide(in, scenarioProfile(t))
}

func TestManifestsFromPlanOrderingAndWrite(t *testing.T) {
	manifests := manifestsFromPlan(scenarioPlan(t), "MYDB")

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

func TestRecordPlanHistory(t *testing.T) {
	plan := scenarioPlan(t)
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
	plan := scenarioPlan(t)

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
