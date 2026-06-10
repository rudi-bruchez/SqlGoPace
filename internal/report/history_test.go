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
