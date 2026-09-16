package run

import (
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/report"
)

// TestSizeTotalsCountsEachStructureOnce: a hand-written manifest can rebuild a heap and
// then one of its indexes. The index must count once — its first "before" and its last
// "after" — or the manifest total double-counts it.
func TestSizeTotalsCountsEachStructureOnce(t *testing.T) {
	var tot sizeTotals
	tot.add("dbo", "MEASUREMENT", []report.SizeLine{
		{Name: "heap", BeforeKB: 5000, AfterKB: 3000},
		{Name: "IX_TS", BeforeKB: 2000, AfterKB: 1500},
	})
	tot.add("dbo", "MEASUREMENT", []report.SizeLine{
		{Name: "IX_TS", BeforeKB: 1500, AfterKB: 1000},
	})

	before, after, structures := tot.totals()
	if structures != 2 {
		t.Fatalf("structures = %d, want 2 (heap + IX_TS)", structures)
	}
	if before != 7000 || after != 4000 {
		t.Errorf("totals = (%d, %d), want (7000, 4000): first before, last after", before, after)
	}
}

// TestSizeTotalsSkipsUnmeasuredSides: a structure missing either side contributes nothing,
// so the manifest figure is never a sum of halves.
func TestSizeTotalsSkipsUnmeasuredSides(t *testing.T) {
	var st sizeTotals
	st.add("dbo", "T", []report.SizeLine{{Name: "IX", BeforeKB: 100, AfterKB: report.SizeUnknown}})
	before, after, structures := st.totals()
	if structures != 0 || before != 0 || after != 0 {
		t.Errorf("totals = (%d, %d, %d), want zeros", before, after, structures)
	}
}
