package report_test

import (
	"context"
	"database/sql"
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

// TestHistoryColumnMigration guards that a run-history DB predating the peak_blocked
// and skipped columns is migrated on open (ALTER TABLE ADD COLUMN) and that the values
// then persist — CREATE TABLE IF NOT EXISTS alone would not add a column.
func TestHistoryColumnMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	ctx := context.Background()

	// Simulate a pre-existing runs table without peak_blocked.
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.ExecContext(ctx, `CREATE TABLE runs (
	    id INTEGER PRIMARY KEY AUTOINCREMENT, manifest TEXT NOT NULL, outcome TEXT NOT NULL,
	    started_at TEXT, finished_at TEXT, operations INTEGER, duration_ms INTEGER, error TEXT);`); err != nil {
		t.Fatal(err)
	}
	_ = old.Close()

	h, err := report.OpenHistory(path) // must add the missing column, not fail
	if err != nil {
		t.Fatalf("OpenHistory() migrate error = %v", err)
	}
	if err := h.Record(ctx, report.RunRecord{Manifest: "m.yaml", Outcome: "SUCCESS", PeakBlocked: 4, Skipped: 3}); err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	_ = h.Close()

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var peak, skipped int
	if err := db.QueryRowContext(ctx, "SELECT peak_blocked, skipped FROM runs LIMIT 1").Scan(&peak, &skipped); err != nil {
		t.Fatalf("read migrated columns: %v", err)
	}
	if peak != 4 || skipped != 3 {
		t.Errorf("peak_blocked/skipped = %d/%d, want 4/3", peak, skipped)
	}
}

// TestHistoryReopenIsIdempotent guards #12: re-opening an already-migrated history DB must
// not re-issue a failing ALTER (the migration now checks column existence rather than matching
// the driver's "duplicate column" error text).
func TestHistoryReopenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	h1, err := report.OpenHistory(path)
	if err != nil {
		t.Fatalf("first OpenHistory() error = %v", err)
	}
	_ = h1.Close()

	h2, err := report.OpenHistory(path) // columns already present — must be a clean no-op
	if err != nil {
		t.Fatalf("re-open OpenHistory() error = %v", err)
	}
	t.Cleanup(func() { _ = h2.Close() })
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
