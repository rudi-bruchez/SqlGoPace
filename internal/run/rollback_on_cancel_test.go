package run

import (
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

// TestRollbackOnCancel pins the definition in docs/specs/CANCEL-ONLY.md: one of the
// five heavy builders, resolved without RESUMABLE. Every other operation type is
// never flagged, whatever Resumable says — including the four DDL types deliberately
// excluded (add_column, drop_column, drop_constraint, drop_index): in the common
// metadata-only case their whole cost is the wait for Sch-M, not a rollback.
func TestRollbackOnCancel(t *testing.T) {
	heavyBuilders := []ddl.Operation{
		ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "IX"},
		ddl.RebuildHeap{Schema: "dbo", Table: "T"},
		ddl.CreateIndex{Schema: "dbo", Table: "T", Index: "IX", Columns: []string{"C"}},
		ddl.AlterColumn{Schema: "dbo", Table: "T", Column: "C", DataType: "INT"},
		ddl.AddConstraint{Schema: "dbo", Table: "T", Constraint: "PK_T", Kind: "primary_key", Columns: []string{"C"}},
	}
	for _, op := range heavyBuilders {
		notResumable := ddl.PlannedOperation{Operation: op, Options: ddl.ResolvedOptions{Resumable: false}}
		if !RollbackOnCancel(notResumable) {
			t.Errorf("RollbackOnCancel(%s, Resumable=false) = false, want true (heavy builder)", op.CommandType())
		}
		resumable := ddl.PlannedOperation{Operation: op, Options: ddl.ResolvedOptions{Resumable: true}}
		if RollbackOnCancel(resumable) {
			t.Errorf("RollbackOnCancel(%s, Resumable=true) = true, want false (a resumable rebuild pauses, it does not roll back)", op.CommandType())
		}
	}

	excludedDDL := []ddl.Operation{
		// cancelSafe: incremental or cheap to redo, whatever Resumable says.
		ddl.ReorganizeIndex{Schema: "dbo", Table: "T", Index: "IX"},
		ddl.CheckDB{Database: "PRODDB"},
		ddl.UpdateStatistics{Schema: "dbo", Table: "T"},
		// Known gap (CANCEL-ONLY.md "Definitions"): a metadata-only ADD/DROP is cheap to
		// cancel in the common case, so these are never flagged even though a
		// size-of-data add_column or a clustered drop_index can be expensive.
		ddl.AddColumn{Schema: "dbo", Table: "T", Column: "C", DataType: "BIT"},
		ddl.DropColumn{Schema: "dbo", Table: "T", Column: "C"},
		ddl.DropConstraint{Schema: "dbo", Table: "T", Constraint: "DF_T"},
		ddl.DropIndex{Schema: "dbo", Table: "T", Index: "IX"},
	}
	for _, op := range excludedDDL {
		for _, resumable := range []bool{false, true} {
			planned := ddl.PlannedOperation{Operation: op, Options: ddl.ResolvedOptions{Resumable: resumable}}
			if RollbackOnCancel(planned) {
				t.Errorf("RollbackOnCancel(%s, Resumable=%t) = true, want false (excluded)", op.CommandType(), resumable)
			}
		}
	}

	// Deliberately excluded drivers: shrink and batch DML keep committed work on cancel.
	drivers := []ddl.Operation{
		ddl.Shrink{Type: "data", Files: "all"},
		ddl.BatchDML{Verb: "delete", Schema: "dbo", Table: "T"},
	}
	for _, op := range drivers {
		planned := ddl.PlannedOperation{Operation: op, Options: ddl.ResolvedOptions{Resumable: false}}
		if RollbackOnCancel(planned) {
			t.Errorf("RollbackOnCancel(%s) = true, want false (driver keeps committed work)", op.CommandType())
		}
	}
}
