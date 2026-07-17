package maint_test

import (
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/maint"
)

func ptr[T any](v T) *T { return &v }

// baseProfile is the default profile with all sections enabled, for decider tests.
func baseProfile(t *testing.T) *maint.Profile {
	t.Helper()
	p, err := maint.Parse([]byte("compression:\n  enabled: true\nheap:\n  enabled: true\nstatistics:\n  enabled: true\ncheckdb:\n  enabled: true\n"))
	if err != nil {
		t.Fatalf("baseProfile: %v", err)
	}
	return p
}

// bigIndex is an index measurement above the page-count floor with a moderate size.
func bigIndex(frag float64) maint.IndexMeasurement {
	return maint.IndexMeasurement{
		Schema: "dbo", Table: "T", Index: "IX",
		PageCount: 5000, SizeMB: 100, FragmentationPercent: frag, Current: maint.CompressionNone,
	}
}

func TestDecideIndexFragmentation(t *testing.T) {
	p := baseProfile(t)
	tests := []struct {
		name     string
		frag     float64
		pageCnt  int64
		wantKind string
	}{
		{"rebuild above 30", 42, 5000, "rebuild_index"},
		{"reorganize in band", 12, 5000, "reorganize_index"},
		{"skip below reorganize", 2, 5000, "skip"},
		{"skip below page floor", 99, 500, "skip"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := bigIndex(tt.frag)
			m.PageCount = tt.pageCnt
			d := maint.DecideIndex(m, p)
			if d.Kind != tt.wantKind {
				t.Errorf("Kind = %q, want %q (reason: %s)", d.Kind, tt.wantKind, d.Reason)
			}
		})
	}
}

func TestDecideIndexCompression(t *testing.T) {
	p := baseProfile(t)
	tests := []struct {
		name     string
		m        maint.IndexMeasurement
		wantKind string
		wantDC   string
	}{
		{
			name: "page chosen on low frag promotes to rebuild",
			m: maint.IndexMeasurement{Schema: "dbo", Table: "T", Index: "IX", PageCount: 5000, SizeMB: 100,
				FragmentationPercent: 2, Current: maint.CompressionNone,
				Estimate: &maint.CompressionEstimate{CurrentKB: 100, RowKB: 70, PageKB: 50}},
			wantKind: "rebuild_index", wantDC: "PAGE",
		},
		{
			name: "row chosen when page not worth extra",
			m: maint.IndexMeasurement{Schema: "dbo", Table: "T", Index: "IX", PageCount: 5000, SizeMB: 100,
				FragmentationPercent: 2, Current: maint.CompressionNone,
				Estimate: &maint.CompressionEstimate{CurrentKB: 100, RowKB: 70, PageKB: 68}},
			wantKind: "rebuild_index", wantDC: "ROW",
		},
		{
			name: "negligible gain leaves it (skip on low frag)",
			m: maint.IndexMeasurement{Schema: "dbo", Table: "T", Index: "IX", PageCount: 5000, SizeMB: 100,
				FragmentationPercent: 2, Current: maint.CompressionNone,
				Estimate: &maint.CompressionEstimate{CurrentKB: 100, RowKB: 98, PageKB: 97}},
			wantKind: "skip",
		},
		{
			name: "already at target, no change",
			m: maint.IndexMeasurement{Schema: "dbo", Table: "T", Index: "IX", PageCount: 5000, SizeMB: 100,
				FragmentationPercent: 2, Current: maint.CompressionPage,
				Estimate: &maint.CompressionEstimate{CurrentKB: 100, RowKB: 130, PageKB: 100}},
			wantKind: "skip",
		},
		{
			name: "write-intensive caps page to row",
			m: maint.IndexMeasurement{Schema: "dbo", Table: "T", Index: "IX", PageCount: 5000, SizeMB: 100,
				FragmentationPercent: 2, Current: maint.CompressionNone,
				Estimate: &maint.CompressionEstimate{CurrentKB: 100, RowKB: 70, PageKB: 50},
				Write:    &maint.WriteActivity{Writes: 500, Reads: 500}},
			wantKind: "rebuild_index", wantDC: "ROW",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := maint.DecideIndex(tt.m, p)
			if d.Kind != tt.wantKind {
				t.Fatalf("Kind = %q, want %q (reason: %s)", d.Kind, tt.wantKind, d.Reason)
			}
			if d.Kind == "rebuild_index" {
				ri := d.Op.(ddl.RebuildIndex)
				if ri.DataCompression != tt.wantDC {
					t.Errorf("DataCompression = %q, want %q", ri.DataCompression, tt.wantDC)
				}
			}
		})
	}
}

func TestDecideIndexOverridePinsCompression(t *testing.T) {
	p, err := maint.Parse([]byte("compression:\n  enabled: true\noverrides:\n  - match: dbo.T\n    compression: page\n"))
	if err != nil {
		t.Fatal(err)
	}
	m := bigIndex(2) // low frag; only the pin should drive a rebuild
	d := maint.DecideIndex(m, p)
	if d.Kind != "rebuild_index" {
		t.Fatalf("Kind = %q, want rebuild_index (reason: %s)", d.Kind, d.Reason)
	}
	if ri := d.Op.(ddl.RebuildIndex); ri.DataCompression != "PAGE" {
		t.Errorf("DataCompression = %q, want PAGE", ri.DataCompression)
	}
}

func TestDecideIndexCompressionScope(t *testing.T) {
	// An index that, in full scope, compresses to PAGE on its estimated gain.
	pageGain := &maint.CompressionEstimate{CurrentKB: 100, RowKB: 70, PageKB: 50}

	t.Run("out of include scope skips compression on low frag", func(t *testing.T) {
		p, err := maint.Parse([]byte("compression:\n  enabled: true\n  objects:\n    include: [dbo.Other]\n"))
		if err != nil {
			t.Fatal(err)
		}
		m := bigIndex(2)
		m.Estimate = pageGain
		d := maint.DecideIndex(m, p)
		if d.Kind != "skip" {
			t.Fatalf("Kind = %q, want skip (reason: %s)", d.Kind, d.Reason)
		}
	})

	t.Run("excluded but fragmented still rebuilds without compression", func(t *testing.T) {
		p, err := maint.Parse([]byte("compression:\n  enabled: true\n  objects:\n    exclude: [dbo.T]\n"))
		if err != nil {
			t.Fatal(err)
		}
		m := bigIndex(50) // above rebuild threshold: defrag must still happen
		m.Estimate = pageGain
		d := maint.DecideIndex(m, p)
		if d.Kind != "rebuild_index" {
			t.Fatalf("Kind = %q, want rebuild_index (reason: %s)", d.Kind, d.Reason)
		}
		if ri := d.Op.(ddl.RebuildIndex); ri.DataCompression != "" {
			t.Errorf("DataCompression = %q, want \"\" (compression out of scope)", ri.DataCompression)
		}
	})

	t.Run("excluding one index spares another", func(t *testing.T) {
		p, err := maint.Parse([]byte("compression:\n  enabled: true\n  objects:\n    exclude: [dbo.T.OTHER]\n"))
		if err != nil {
			t.Fatal(err)
		}
		m := bigIndex(2) // low frag: only compression can promote it to a rebuild
		m.Estimate = pageGain
		d := maint.DecideIndex(m, p)
		if d.Kind != "rebuild_index" {
			t.Fatalf("Kind = %q, want rebuild_index (reason: %s)", d.Kind, d.Reason)
		}
		if ri := d.Op.(ddl.RebuildIndex); ri.DataCompression != "PAGE" {
			t.Errorf("DataCompression = %q, want PAGE", ri.DataCompression)
		}
	})

	t.Run("override pin wins over exclude", func(t *testing.T) {
		p, err := maint.Parse([]byte("compression:\n  enabled: true\n  objects:\n    exclude: [dbo.T]\noverrides:\n  - match: dbo.T\n    compression: page\n"))
		if err != nil {
			t.Fatal(err)
		}
		m := bigIndex(2)
		d := maint.DecideIndex(m, p)
		if d.Kind != "rebuild_index" {
			t.Fatalf("Kind = %q, want rebuild_index (reason: %s)", d.Kind, d.Reason)
		}
		if ri := d.Op.(ddl.RebuildIndex); ri.DataCompression != "PAGE" {
			t.Errorf("DataCompression = %q, want PAGE (override pin must beat exclude)", ri.DataCompression)
		}
	})
}

func TestDecideIndexForbidAndCeiling(t *testing.T) {
	forbid, err := maint.Parse([]byte("overrides:\n  - match: dbo.T\n    rebuild: forbid\n"))
	if err != nil {
		t.Fatal(err)
	}
	skipCeiling, err := maint.Parse([]byte("index:\n  rebuild_max_size_mb: 50\n  rebuild_over_ceiling: skip\n"))
	if err != nil {
		t.Fatal(err)
	}
	reorgCeiling, err := maint.Parse([]byte("index:\n  rebuild_max_size_mb: 50\n"))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("forbid downgrades to reorganize", func(t *testing.T) {
		d := maint.DecideIndex(bigIndex(50), forbid)
		if d.Kind != "reorganize_index" {
			t.Errorf("Kind = %q, want reorganize_index (reason: %s)", d.Kind, d.Reason)
		}
	})
	t.Run("over ceiling skip", func(t *testing.T) {
		m := bigIndex(50)
		m.SizeMB = 100
		d := maint.DecideIndex(m, skipCeiling)
		if d.Kind != "skip" {
			t.Errorf("Kind = %q, want skip (reason: %s)", d.Kind, d.Reason)
		}
	})
	t.Run("over ceiling reorganize", func(t *testing.T) {
		m := bigIndex(50)
		m.SizeMB = 100
		d := maint.DecideIndex(m, reorgCeiling)
		if d.Kind != "reorganize_index" {
			t.Errorf("Kind = %q, want reorganize_index (reason: %s)", d.Kind, d.Reason)
		}
	})
	t.Run("forbid with only compression motivation skips", func(t *testing.T) {
		p, err := maint.Parse([]byte("compression:\n  enabled: true\noverrides:\n  - match: dbo.T\n    rebuild: forbid\n"))
		if err != nil {
			t.Fatal(err)
		}
		m := bigIndex(2) // low frag, but compression wants PAGE
		m.Estimate = &maint.CompressionEstimate{CurrentKB: 100, RowKB: 70, PageKB: 50}
		d := maint.DecideIndex(m, p)
		if d.Kind != "skip" {
			t.Errorf("Kind = %q, want skip (reorganize cannot apply compression) (reason: %s)", d.Kind, d.Reason)
		}
	})
}

func TestDecideIndexPartitionCarried(t *testing.T) {
	p := baseProfile(t)
	m := bigIndex(50)
	m.Partition = ptr(3)
	d := maint.DecideIndex(m, p)
	ri, ok := d.Op.(ddl.RebuildIndex)
	if !ok {
		t.Fatalf("Op type = %T, want ddl.RebuildIndex", d.Op)
	}
	if ri.Partition == nil || *ri.Partition != 3 {
		t.Errorf("Partition = %v, want 3", ri.Partition)
	}
}

func TestDecideHeap(t *testing.T) {
	p := baseProfile(t)
	rebuildable := maint.HeapMeasurement{
		Schema: "dbo", Table: "H", SizeMB: 500, RecordCount: 1000,
		ForwardedRecordCount: 200, PageSpaceUsedPercent: 90, FragmentationPercent: 1,
	}
	tests := []struct {
		name     string
		mutate   func(*maint.HeapMeasurement)
		wantKind string
	}{
		{"forwarded trigger", func(*maint.HeapMeasurement) {}, "rebuild_heap"},
		{"below min size", func(m *maint.HeapMeasurement) { m.SizeMB = 1 }, "skip"},
		{"above max size", func(m *maint.HeapMeasurement) { m.SizeMB = 99999 }, "skip"},
		{"no trigger", func(m *maint.HeapMeasurement) {
			m.ForwardedRecordCount = 0
			m.PageSpaceUsedPercent = 95
		}, "skip"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := rebuildable
			tt.mutate(&m)
			d := maint.DecideHeap(m, p)
			if d.Kind != tt.wantKind {
				t.Errorf("Kind = %q, want %q (reason: %s)", d.Kind, tt.wantKind, d.Reason)
			}
		})
	}
}

func TestDecideHeapOverrides(t *testing.T) {
	skip, err := maint.Parse([]byte("heap:\n  enabled: true\noverrides:\n  - match: dbo.H\n    skip: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	forbid, err := maint.Parse([]byte("heap:\n  enabled: true\noverrides:\n  - match: dbo.H\n    rebuild: forbid\n"))
	if err != nil {
		t.Fatal(err)
	}
	m := maint.HeapMeasurement{Schema: "dbo", Table: "H", SizeMB: 500, RecordCount: 1000, ForwardedRecordCount: 500, PageSpaceUsedPercent: 90}
	if d := maint.DecideHeap(m, skip); d.Kind != "skip" {
		t.Errorf("skip override: Kind = %q, want skip", d.Kind)
	}
	if d := maint.DecideHeap(m, forbid); d.Kind != "skip" {
		t.Errorf("forbid override: Kind = %q, want skip (heap cannot reorganize)", d.Kind)
	}
}

func TestDecideStatistic(t *testing.T) {
	p := baseProfile(t)
	t.Run("stale dynamic threshold", func(t *testing.T) {
		d := maint.DecideStatistic(maint.StatMeasurement{Schema: "dbo", Table: "T", Statistic: "ST", Rows: 50_000_000, ModificationCounter: 900_000}, p)
		if d.Kind != "update_statistics" {
			t.Fatalf("Kind = %q, want update_statistics (reason: %s)", d.Kind, d.Reason)
		}
		if us := d.Op.(ddl.UpdateStatistics); !us.FullScan {
			t.Errorf("FullScan = false, want true")
		}
	})
	t.Run("not stale", func(t *testing.T) {
		d := maint.DecideStatistic(maint.StatMeasurement{Schema: "dbo", Table: "T", Statistic: "ST", Rows: 1_000_000, ModificationCounter: 1000}, p)
		if d.Kind != "skip" {
			t.Errorf("Kind = %q, want skip (reason: %s)", d.Kind, d.Reason)
		}
	})
	t.Run("percent threshold", func(t *testing.T) {
		pp, err := maint.Parse([]byte("statistics:\n  enabled: true\n  modification_percent: 20\n  sample:\n    percent: 30\n"))
		if err != nil {
			t.Fatal(err)
		}
		d := maint.DecideStatistic(maint.StatMeasurement{Schema: "dbo", Table: "T", Statistic: "ST", Rows: 1000, ModificationCounter: 250}, pp)
		if d.Kind != "update_statistics" {
			t.Fatalf("Kind = %q, want update_statistics (reason: %s)", d.Kind, d.Reason)
		}
		us := d.Op.(ddl.UpdateStatistics)
		if us.SamplePercent == nil || *us.SamplePercent != 30 || us.FullScan {
			t.Errorf("sample = {full=%t pct=%v}, want SAMPLE 30 PERCENT", us.FullScan, us.SamplePercent)
		}
	})
}

func TestDecideSuppression(t *testing.T) {
	p := baseProfile(t)

	t.Run("stat backing rebuilt index suppressed", func(t *testing.T) {
		in := maint.Input{
			Indexes:    []maint.IndexMeasurement{bigIndex(50)}, // dbo.T.IX rebuild
			Statistics: []maint.StatMeasurement{{Schema: "dbo", Table: "T", Statistic: "IX", Rows: 50_000_000, ModificationCounter: 9_000_000}},
		}
		pl := maint.Decide(in, p)
		if got := pl.OperationsByCategory("statistics"); len(got) != 0 {
			t.Errorf("statistics ops = %d, want 0 (suppressed by index rebuild)", len(got))
		}
	})

	t.Run("partition rebuild does not suppress stats", func(t *testing.T) {
		m := bigIndex(50)
		m.Partition = ptr(2)
		in := maint.Input{
			Indexes:    []maint.IndexMeasurement{m},
			Statistics: []maint.StatMeasurement{{Schema: "dbo", Table: "T", Statistic: "IX", Rows: 50_000_000, ModificationCounter: 9_000_000}},
		}
		pl := maint.Decide(in, p)
		if got := pl.OperationsByCategory("statistics"); len(got) != 1 {
			t.Errorf("statistics ops = %d, want 1 (a single-partition rebuild does not refresh full stats)", len(got))
		}
	})

	t.Run("index op on heap-rebuilt table suppressed", func(t *testing.T) {
		idx := maint.IndexMeasurement{Schema: "dbo", Table: "H", Index: "IX_H", PageCount: 5000, SizeMB: 100, FragmentationPercent: 50}
		heap := maint.HeapMeasurement{Schema: "dbo", Table: "H", SizeMB: 500, RecordCount: 1000, ForwardedRecordCount: 500, PageSpaceUsedPercent: 90}
		pl := maint.Decide(maint.Input{Indexes: []maint.IndexMeasurement{idx}, Heaps: []maint.HeapMeasurement{heap}}, p)
		if got := pl.OperationsByCategory("index"); len(got) != 0 {
			t.Errorf("index ops = %d, want 0 (suppressed by heap rebuild)", len(got))
		}
		if got := pl.OperationsByCategory("heap"); len(got) != 1 {
			t.Errorf("heap ops = %d, want 1", len(got))
		}
	})
}

func TestDecideCheckDB(t *testing.T) {
	p := baseProfile(t)
	pl := maint.Decide(maint.Input{ConnDatabase: "MYDB"}, p)
	ops := pl.OperationsByCategory("checkdb")
	if len(ops) != 1 {
		t.Fatalf("checkdb ops = %d, want 1", len(ops))
	}
	if cdb := ops[0].(ddl.CheckDB); cdb.Database != "MYDB" {
		t.Errorf("CheckDB.Database = %q, want MYDB", cdb.Database)
	}

	listed, err := maint.Parse([]byte("checkdb:\n  enabled: true\n  databases: [A, B]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(maint.Decide(maint.Input{ConnDatabase: "MYDB"}, listed).OperationsByCategory("checkdb")); got != 2 {
		t.Errorf("checkdb ops = %d, want 2 (the profile's database list)", got)
	}
}

// TestGoldenPath reproduces spec §16: a measured database state must yield exactly
// the documented operations. It is the canonical acceptance test for the core.
func TestGoldenPath(t *testing.T) {
	p, err := maint.LoadFile(filepath.FromSlash("../../maintenance_profile.yaml"))
	if err != nil {
		t.Fatalf("load shipped profile: %v", err)
	}

	in := maint.Input{
		ConnDatabase: "MYDB",
		Indexes: []maint.IndexMeasurement{
			{Schema: "dbo", Table: "ORDERS", Index: "PK_ORDERS", Clustered: true, PageCount: 1_000_000, SizeMB: 8192,
				FragmentationPercent: 42, Current: maint.CompressionNone,
				Estimate: &maint.CompressionEstimate{CurrentKB: 8_000_000, RowKB: 5_500_000, PageKB: 4_000_000},
				Write:    &maint.WriteActivity{Writes: 5, Reads: 95}},
			{Schema: "dbo", Table: "ORDERS", Index: "IX_ORDERS_CUST", PageCount: 150_000, SizeMB: 1228,
				FragmentationPercent: 12, Current: maint.CompressionRow,
				Estimate: &maint.CompressionEstimate{CurrentKB: 900_000, RowKB: 900_000, PageKB: 850_000}},
			{Schema: "dbo", Table: "AUDIT_2024", Index: "PK_AUDIT_2024", Clustered: true, PageCount: 400_000, SizeMB: 3072,
				FragmentationPercent: 50, Current: maint.CompressionNone,
				Estimate: &maint.CompressionEstimate{CurrentKB: 3_000_000, RowKB: 1_100_000, PageKB: 700_000}},
			{Schema: "dbo", Table: "LEDGER", Index: "PK_LEDGER", Clustered: true, PageCount: 15_000_000, SizeMB: 122880,
				FragmentationPercent: 38, Current: maint.CompressionNone},
		},
		Heaps: []maint.HeapMeasurement{
			{Schema: "dbo", Table: "STAGING", SizeMB: 4096, RecordCount: 1_000_000, ForwardedRecordCount: 180_000, PageSpaceUsedPercent: 80},
		},
		Statistics: []maint.StatMeasurement{
			{Schema: "dbo", Table: "ORDERS", Statistic: "CustStats", Rows: 50_000_000, ModificationCounter: 900_000},
		},
	}

	pl := maint.Decide(in, p)

	wantIndex := []ddl.Operation{
		ddl.RebuildIndex{Schema: "dbo", Table: "ORDERS", Index: "PK_ORDERS", DataCompression: "PAGE", Intent: ddl.IntentFragmentation},
		ddl.ReorganizeIndex{Schema: "dbo", Table: "ORDERS", Index: "IX_ORDERS_CUST", LOBCompaction: true},
		ddl.ReorganizeIndex{Schema: "dbo", Table: "AUDIT_2024", Index: "PK_AUDIT_2024", LOBCompaction: true},
		ddl.ReorganizeIndex{Schema: "dbo", Table: "LEDGER", Index: "PK_LEDGER", LOBCompaction: true},
	}
	if diff := cmp.Diff(wantIndex, pl.OperationsByCategory("index")); diff != "" {
		t.Errorf("index operations mismatch (-want +got):\n%s", diff)
	}

	wantStats := []ddl.Operation{ddl.UpdateStatistics{Schema: "dbo", Table: "ORDERS", Statistic: "CustStats", FullScan: true}}
	if diff := cmp.Diff(wantStats, pl.OperationsByCategory("statistics")); diff != "" {
		t.Errorf("statistics operations mismatch (-want +got):\n%s", diff)
	}

	wantCheckDB := []ddl.Operation{ddl.CheckDB{Database: "MYDB"}}
	if diff := cmp.Diff(wantCheckDB, pl.OperationsByCategory("checkdb")); diff != "" {
		t.Errorf("checkdb operations mismatch (-want +got):\n%s", diff)
	}

	// STAGING (heap) is excluded by the skip override → no heap operation.
	if got := pl.OperationsByCategory("heap"); len(got) != 0 {
		t.Errorf("heap operations = %d, want 0 (STAGING is skipped)", len(got))
	}
}
