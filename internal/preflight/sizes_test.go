package preflight_test

import (
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
	"github.com/rudi-bruchez/SqlGoPace/internal/preflight"
)

var probeSizes = []mssql.StructureSize{
	{IndexID: 0, Name: "", TypeDesc: "HEAP", UsedKB: 5_000_000},
	{IndexID: 2, Name: "IX_MEASUREMENT_TS", TypeDesc: "NONCLUSTERED", UsedKB: 2_000_000},
	{IndexID: 3, Name: "IX_MEASUREMENT_OLD", TypeDesc: "NONCLUSTERED", Disabled: true},
	{IndexID: 4, Name: "NCCI_MEASUREMENT", TypeDesc: "NONCLUSTERED COLUMNSTORE", UsedKB: 900_000},
}

// TestRewritten pins the rule the whole feature rests on: ALTER TABLE ... REBUILD on a
// heap rewrites the heap AND every other structure of the table (verified on a server,
// OBJECT-SIZES-ANALYSIS.md), while an index operation rewrites only the index it names.
func TestRewritten(t *testing.T) {
	tests := []struct {
		name string
		op   ddl.Operation
		want []string // Name of each returned structure, in order
	}{
		{"rebuild_heap takes everything", ddl.RebuildHeap{Schema: "dbo", Table: "MEASUREMENT"},
			[]string{"", "IX_MEASUREMENT_TS", "IX_MEASUREMENT_OLD", "NCCI_MEASUREMENT"}},
		{"rebuild_index takes its index", ddl.RebuildIndex{Schema: "dbo", Table: "MEASUREMENT", Index: "IX_MEASUREMENT_TS"},
			[]string{"IX_MEASUREMENT_TS"}},
		{"reorganize_index takes its index", ddl.ReorganizeIndex{Schema: "dbo", Table: "MEASUREMENT", Index: "NCCI_MEASUREMENT"},
			[]string{"NCCI_MEASUREMENT"}},
		{"unknown index name takes nothing", ddl.RebuildIndex{Schema: "dbo", Table: "MEASUREMENT", Index: "IX_absent"}, nil},
		{"another operation takes nothing", ddl.UpdateStatistics{Schema: "dbo", Table: "MEASUREMENT"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := preflight.Rewritten(tt.op, probeSizes)
			if len(got) != len(tt.want) {
				t.Fatalf("Rewritten() = %d structures, want %d: %+v", len(got), len(tt.want), got)
			}
			for i, w := range tt.want {
				if got[i].Name != w {
					t.Errorf("structure %d = %q, want %q", i, got[i].Name, w)
				}
			}
		})
	}
}

// TestSumKBIgnoresNothing: the sum is what the rewrite needs room for; a disabled index
// contributes 0 because it has no pages to copy, not because it is excluded.
func TestSumKB(t *testing.T) {
	if got, want := preflight.SumKB(preflight.Rewritten(ddl.RebuildHeap{Schema: "dbo", Table: "MEASUREMENT"}, probeSizes)), int64(7_900_000); got != want {
		t.Errorf("SumKB() = %d, want %d", got, want)
	}
}

func TestDisabledNames(t *testing.T) {
	got := preflight.DisabledNames(probeSizes)
	if len(got) != 1 || got[0] != "IX_MEASUREMENT_OLD" {
		t.Errorf("DisabledNames() = %v, want [IX_MEASUREMENT_OLD]", got)
	}
}

// TestSizedOperation names the operations worth a size read at all.
func TestSizedOperation(t *testing.T) {
	part := 3
	tests := []struct {
		op        ddl.Operation
		wantTable string
		wantPart  *int
		wantOK    bool
	}{
		{ddl.RebuildIndex{Schema: "dbo", Table: "MEASUREMENT", Index: "IX", Partition: &part}, "MEASUREMENT", &part, true},
		{ddl.RebuildHeap{Schema: "dbo", Table: "MEASUREMENT"}, "MEASUREMENT", nil, true},
		{ddl.ReorganizeIndex{Schema: "dbo", Table: "MEASUREMENT", Index: "IX"}, "MEASUREMENT", nil, true},
		{ddl.UpdateStatistics{Schema: "dbo", Table: "MEASUREMENT"}, "", nil, false},
	}
	for _, tt := range tests {
		_, table, gotPart, ok := preflight.SizedOperation(tt.op)
		if ok != tt.wantOK || table != tt.wantTable {
			t.Errorf("SizedOperation(%T) = (%q, %t), want (%q, %t)", tt.op, table, ok, tt.wantTable, tt.wantOK)
		}
		if tt.wantPart == nil && gotPart != nil {
			t.Errorf("SizedOperation(%T) partition = %v, want nil", tt.op, *gotPart)
		}
		if tt.wantPart != nil && (gotPart == nil || *gotPart != *tt.wantPart) {
			t.Errorf("SizedOperation(%T) partition = %v, want %d", tt.op, gotPart, *tt.wantPart)
		}
	}
}
