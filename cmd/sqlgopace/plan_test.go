package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/rudi-bruchez/SqlGoPace/internal/config"
	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/maint"
	"github.com/rudi-bruchez/SqlGoPace/internal/report"
)

// boolPtr builds the tri-state *bool the config fields use; package main cannot
// reach internal/config's unexported helper.
func boolPtr(b bool) *bool { return &b }

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
	cfg := &config.Config{History: config.HistoryConfig{Enabled: boolPtr(true), Destination: "sqlite://" + dbPath}}

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
	cfg := &config.Config{History: config.HistoryConfig{Enabled: boolPtr(false)}}
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

func TestWriteManifestsAppearsCompleteOrNotAtAll(t *testing.T) {
	// A run polling 01.to_run/ claims any *.yaml it sees, so a manifest must never be
	// visible under its final name while it is still being written: a truncation that
	// lands on an operation boundary is still valid YAML, and the run would execute a
	// prefix of the plan and report success. The write goes to a name the queue skips
	// (leading dot, and not a .yaml extension — internal/run/queue.go isManifest) and is
	// renamed into place, which is atomic within one directory.
	dir := t.TempDir()
	manifests := manifestsFromPlan(scenarioPlan(t), "MYDB")
	if err := writeManifests(io.Discard, dir, manifests); err != nil {
		t.Fatalf("writeManifests() error = %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("temp file %q left behind in the queue directory", e.Name())
		}
	}
	if len(entries) != len(manifests) {
		t.Errorf("directory holds %d files, want %d", len(entries), len(manifests))
	}

	// The staging name itself: dot-prefixed so Discover skips it even mid-write.
	if got := stagedName("020_maint_MYDB_index.yaml"); !strings.HasPrefix(got, ".") || strings.HasSuffix(got, ".yaml") {
		t.Errorf("stagedName = %q, want a dot-prefixed name that is not a .yaml", got)
	}
}
