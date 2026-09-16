package report_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/rudi-bruchez/SqlGoPace/internal/report"
)

func sampleReport() report.RunReport {
	return report.RunReport{
		Manifest:   "010_a.yaml",
		Outcome:    "SUCCESS",
		StartedAt:  "2026-06-10T12:00:00Z",
		FinishedAt: "2026-06-10T12:00:01Z",
		DurationMS: 1200,
		Operations: []report.OperationReport{{
			Index:       1,
			CommandType: "rebuild_index",
			Target:      "dbo.T.IX",
			SQL:         "ALTER INDEX [IX] ON [dbo].[T] REBUILD;",
			Outcome:     "success",
			DurationMS:  1100,
			PeakBlocked: 2,
			Options: []report.OptionDecision{
				{Option: "online", Value: "ON", Reason: "supported by target (auto)"},
			},
		}},
	}
}

func TestWriteHumanAndJSON(t *testing.T) {
	r := sampleReport()
	var buf bytes.Buffer
	if err := report.Write(&buf, r); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"manifest: 010_a.yaml",
		"SUCCESS",
		"rebuild_index dbo.T.IX",
		"peak blocked: 2 session(s)",
		"online = ON",
		"ALTER INDEX [IX] ON [dbo].[T] REBUILD;",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("human report missing %q\n%s", want, out)
		}
	}

	_, jsonPart, found := strings.Cut(out, report.JSONDelimiter)
	if !found {
		t.Fatalf("report has no JSON section")
	}
	var got report.RunReport
	if err := json.Unmarshal([]byte(jsonPart), &got); err != nil {
		t.Fatalf("JSON section does not parse: %v", err)
	}
	if diff := cmp.Diff(r, got); diff != "" {
		t.Errorf("JSON round-trip mismatch (-want +got):\n%s", diff)
	}
}

// TestReportRendersCancelOnlyLines pins CANCEL-ONLY.md §2/§3: the manifest-start
// notice and the end-of-run summary each appear once in the human text and round-trip
// through the JSON block, same as every other report field.
func TestReportRendersCancelOnlyLines(t *testing.T) {
	r := sampleReport()
	r.CancelOnlyNotice = "1 of 1 operation(s) can only be canceled under pressure; a cancel rolls back all their work and is retried up to max_retry_attempts (1)"
	r.CancelOnlySummary = "1 rollback-on-cancel operation(s) were canceled under pressure: 1 succeeded after a retry, 0 failed"

	var buf bytes.Buffer
	if err := report.Write(&buf, r); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	out := buf.String()

	for _, want := range []string{r.CancelOnlyNotice, r.CancelOnlySummary} {
		if !strings.Contains(out, want) {
			t.Errorf("human report missing %q\n%s", want, out)
		}
	}

	_, jsonPart, found := strings.Cut(out, report.JSONDelimiter)
	if !found {
		t.Fatalf("report has no JSON section")
	}
	var got report.RunReport
	if err := json.Unmarshal([]byte(jsonPart), &got); err != nil {
		t.Fatalf("JSON section does not parse: %v", err)
	}
	if diff := cmp.Diff(r, got); diff != "" {
		t.Errorf("JSON round-trip mismatch (-want +got):\n%s", diff)
	}
}

// TestReportOmitsCancelOnlyLinesWhenEmpty covers the common case (no rollback-on-cancel
// operation in the manifest, or none canceled): neither line appears.
func TestReportOmitsCancelOnlyLinesWhenEmpty(t *testing.T) {
	r := sampleReport()
	var buf bytes.Buffer
	if err := report.Write(&buf, r); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "can only be canceled") || strings.Contains(out, "rollback-on-cancel operation(s) were canceled") {
		t.Errorf("report has a cancel-only line where none was set:\n%s", out)
	}
}

func TestReportRendersContendedPointer(t *testing.T) {
	r := report.RunReport{
		Manifest: "020_shrink.yaml",
		Outcome:  "SUCCESS",
		Operations: []report.OperationReport{{
			Index:          1,
			CommandType:    "shrink_data",
			Target:         "PRODDB",
			Outcome:        "success",
			ContendedCount: 2,
			ContendedFile:  "020_shrink.yaml.contended.yaml",
		}},
	}
	var buf bytes.Buffer
	if err := report.Write(&buf, r); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "contended objects: 2 — see 020_shrink.yaml.contended.yaml") {
		t.Errorf("missing contended pointer line:\n%s", out)
	}
}

// TestHumanizeKB pins the boundaries, including the TB step: a 1.4 TB object exists in the
// field (TODO.md), and rendering it as "1433.6 GB" while the console header says TB for the
// same database reads as a bug.
func TestHumanizeKB(t *testing.T) {
	tests := []struct {
		kb   int64
		want string
	}{
		{0, "0 KB"},
		{812, "812 KB"},
		{1024, "1.0 MB"},
		{12_700, "12.4 MB"},
		{1024 * 1024, "1.0 GB"},
		{2_684_354, "2.6 GB"},
		{1024 * 1024 * 1024, "1.00 TB"},
	}
	for _, tt := range tests {
		if got := report.HumanizeKB(tt.kb); got != tt.want {
			t.Errorf("HumanizeKB(%d) = %q, want %q", tt.kb, got, tt.want)
		}
	}
}

// TestWriteRendersSizes covers the three shapes: one structure, a heap block with a total,
// and the states that have no percentage (unknown, a re-enabled index that had no pages).
func TestWriteRendersSizes(t *testing.T) {
	r := report.RunReport{
		Manifest: "100_h.yaml", Outcome: "SUCCESS",
		HeapScopeNotices: []string{"operation 1 rebuild_heap dbo.MEASUREMENT also rebuilds 2 nonclustered index(es)"},
		Operations: []report.OperationReport{
			{Index: 1, CommandType: "rebuild_index", Target: "dbo.MEASUREMENT.IX_TS", Outcome: "success",
				Sizes: []report.SizeLine{{Name: "IX_TS", Type: "NONCLUSTERED", BeforeKB: 2_097_152, AfterKB: 1_468_006}}},
			{Index: 2, CommandType: "rebuild_heap", Target: "dbo.MEASUREMENT", Outcome: "success",
				Sizes: []report.SizeLine{
					{Name: "heap", Type: "HEAP", BeforeKB: 5_242_880, AfterKB: 3_250_586},
					{Name: "IX_OLD", Type: "NONCLUSTERED", WasDisabled: true, BeforeKB: 0, AfterKB: 462_848},
					{Name: "IX_TS", Type: "NONCLUSTERED", BeforeKB: 2_097_152, AfterKB: report.SizeUnknown},
				}},
		},
		SizeBeforeKB: 7_340_032, SizeAfterKB: 4_718_592, SizeStructures: 2,
	}
	var b strings.Builder
	if err := report.Write(&b, r); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	out := b.String()
	for _, want := range []string{
		"also rebuilds 2 nonclustered index(es)",
		"size: IX_TS 2.0 GB -> 1.4 GB (-30.0%)",
		"size (heap and 2 nonclustered index(es)):",
		"IX_OLD (was disabled)",
		"-> unknown",
		// IX_TS's after size is unknown, so the total (H7, REVIEW-2026-09-16-harm.md) sums
		// only heap + IX_OLD (both known) and says it skipped one of the three structures.
		"total (2 of 3 measured)",
		"size: 7.0 GB -> 4.5 GB (-35.7%) over 2 structure(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "IX_OLD (was disabled)   0 KB -> 452.0 MB (") {
		t.Error("a structure with no pages before must not get a percentage")
	}
}

// TestRenderSizesTotalSkipsHalfMeasuredStructures pins H7, first bullet
// (REVIEW-2026-09-16-harm.md): the total row must use the same rule as sizeTotals.add
// (internal/run/sizes.go) — skip a structure unless both sides are known — or it can show
// growth that is an artifact of a half-measured set. When it skips any structure, the total
// line says so rather than presenting a silently partial number.
func TestRenderSizesTotalSkipsHalfMeasuredStructures(t *testing.T) {
	r := report.RunReport{
		Manifest: "100_h.yaml", Outcome: "SUCCESS",
		Operations: []report.OperationReport{
			{Index: 1, CommandType: "rebuild_heap", Target: "dbo.MEASUREMENT", Outcome: "success",
				Sizes: []report.SizeLine{
					{Name: "heap", Type: "HEAP", BeforeKB: 1000, AfterKB: 800},
					{Name: "IX_A", Type: "NONCLUSTERED", BeforeKB: 500, AfterKB: 400},
					{Name: "IX_B", Type: "NONCLUSTERED", BeforeKB: 2000, AfterKB: report.SizeUnknown},
				}},
		},
	}
	var b strings.Builder
	if err := report.Write(&b, r); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	out := b.String()
	if !strings.Contains(out, "total (2 of 3 measured)") {
		t.Errorf("report missing the partial-total marker:\n%s", out)
	}
	// heap (1000->800) + IX_A (500->400) = 1500->1200. IX_B's known "before" (2000) must
	// not inflate the total the way the old unconditional accumulation did.
	if !strings.Contains(out, "1.5 MB -> 1.2 MB") {
		t.Errorf("total should sum only the two fully-measured structures:\n%s", out)
	}
}

// TestWriteSizesUnread: when nothing could be measured, the manifest says so once instead
// of printing "unknown -> unknown" under every operation.
func TestWriteSizesUnread(t *testing.T) {
	var b strings.Builder
	err := report.Write(&b, report.RunReport{
		Manifest: "100_h.yaml", Outcome: "SUCCESS",
		SizesUnread: "structure sizes dbo.MEASUREMENT: permission denied",
		Operations:  []report.OperationReport{{Index: 1, CommandType: "rebuild_index", Outcome: "success"}},
	})
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if !strings.Contains(b.String(), "sizes not measured: structure sizes dbo.MEASUREMENT: permission denied") {
		t.Errorf("report missing the manifest-level unread line:\n%s", b.String())
	}
}
