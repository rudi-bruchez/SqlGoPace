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
