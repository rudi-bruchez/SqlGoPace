package run

import (
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

func TestReorgRCSIWarning(t *testing.T) {
	reorg := ddl.ReorganizeIndex{Schema: "dbo", Table: "MEASUREMENT", Index: "PK_MEASUREMENT"}

	// Reorg + RCSI off → warn, message carries schema.table and the database name.
	msg, ok := reorgRCSIWarning(reorg, "PRODDB", false)
	if !ok {
		t.Fatal("reorgRCSIWarning(reorg, off) ok = false, want true")
	}
	for _, want := range []string{"dbo.MEASUREMENT", "PRODDB", "RCSI is OFF"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}

	// Reorg + RCSI on → silent.
	if _, ok := reorgRCSIWarning(reorg, "PRODDB", true); ok {
		t.Error("reorgRCSIWarning(reorg, on) ok = true, want false")
	}

	// Non-reorg ops → silent regardless of RCSI.
	for _, op := range []ddl.Operation{
		ddl.CheckDB{Database: "DB"},
		ddl.UpdateStatistics{Schema: "dbo", Table: "T"},
		ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "IX"},
	} {
		if _, ok := reorgRCSIWarning(op, "DB", false); ok {
			t.Errorf("reorgRCSIWarning(%T, off) ok = true, want false (only reorganize_index warns)", op)
		}
	}
}
