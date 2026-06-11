package report_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/report"
)

func TestHistoryRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	h, err := report.OpenHistory(path)
	if err != nil {
		t.Fatalf("OpenHistory() error = %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	ctx := context.Background()
	rec := report.RunRecord{
		Manifest:   "010_a.yaml",
		Outcome:    "SUCCESS",
		StartedAt:  "2026-06-10T12:00:00Z",
		FinishedAt: "2026-06-10T12:00:01Z",
		Operations: 2,
		DurationMS: 1200,
	}
	if err := h.Record(ctx, rec); err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	if err := h.Record(ctx, rec); err != nil {
		t.Fatalf("Record() error = %v", err)
	}

	n, err := h.Count(ctx)
	if err != nil {
		t.Fatalf("Count() error = %v", err)
	}
	if n != 2 {
		t.Errorf("Count() = %d, want 2", n)
	}
}

func TestHistoryRecordMaintenance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	h, err := report.OpenHistory(path)
	if err != nil {
		t.Fatalf("OpenHistory() error = %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	ctx := context.Background()
	run := report.MaintenancePlanRecord{Database: "MYDB", GeneratedAt: "2026-06-11T10:00:00Z", Manifests: 2, Operations: 2}
	decisions := []report.MaintenanceDecisionRecord{
		{Category: "index", Target: "dbo.ORDERS.PK_ORDERS", Decision: "rebuild_index", Reason: "fragmentation 42%",
			SizeMB: 8192, FragmentationPercent: 42, CurrentCompression: "NONE", ChosenCompression: "PAGE"},
		{Category: "heap", Target: "dbo.STAGING", Decision: "rebuild_heap", Reason: "forwarded 20%",
			SizeMB: 500, ForwardedPercent: 20},
	}
	if err := h.RecordMaintenance(ctx, run, decisions); err != nil {
		t.Fatalf("RecordMaintenance() error = %v", err)
	}

	plans, rows, err := h.MaintenanceCounts(ctx)
	if err != nil {
		t.Fatalf("MaintenanceCounts() error = %v", err)
	}
	if plans != 1 || rows != 2 {
		t.Errorf("counts = (%d plans, %d rows), want (1, 2)", plans, rows)
	}

	// A second run is additive (the schema migration is idempotent across opens).
	if err := h.RecordMaintenance(ctx, run, decisions[:1]); err != nil {
		t.Fatalf("RecordMaintenance() second error = %v", err)
	}
	if plans, rows, _ = h.MaintenanceCounts(ctx); plans != 2 || rows != 3 {
		t.Errorf("after 2nd run: counts = (%d, %d), want (2, 3)", plans, rows)
	}
}

// TestHistoryReopenIsAdditive guards that opening an existing run-history DB adds
// the maintenance tables without disturbing existing data.
func TestHistoryReopenIsAdditive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	h1, err := report.OpenHistory(path)
	if err != nil {
		t.Fatalf("OpenHistory() error = %v", err)
	}
	ctx := context.Background()
	if err := h1.Record(ctx, report.RunRecord{Manifest: "x.yaml", Outcome: "SUCCESS"}); err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	_ = h1.Close()

	h2, err := report.OpenHistory(path) // reopen: maintenance tables must already/again exist
	if err != nil {
		t.Fatalf("re-OpenHistory() error = %v", err)
	}
	t.Cleanup(func() { _ = h2.Close() })
	if n, _ := h2.Count(ctx); n != 1 {
		t.Errorf("runs after reopen = %d, want 1 (existing data preserved)", n)
	}
	if err := h2.RecordMaintenance(ctx, report.MaintenancePlanRecord{Database: "D"}, nil); err != nil {
		t.Errorf("RecordMaintenance after reopen error = %v", err)
	}
}
