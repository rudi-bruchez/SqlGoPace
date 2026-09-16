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
