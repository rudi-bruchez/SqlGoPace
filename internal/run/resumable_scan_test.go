package run

import (
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// A resumable rebuild left paused on the server holds both the old and the new structure
// allocated and blocks every other rebuild on its table. It is invisible until something
// trips over it — which is how a rebuild 99.84% complete sat unnoticed overnight. The
// startup scan exists to say so before a run starts, and to say whether the queue will
// deal with it.

func TestScanClassifiesCoveredAndOrphanedResumables(t *testing.T) {
	server := []mssql.ResumableOp{
		{Schema: "dbo", Table: "T", Name: "IX", StateDesc: "PAUSED", PercentComplete: 72.37, ExecutionMinutes: 14},
		{Schema: "dbo", Table: "Other", Name: "PK_Other", StateDesc: "PAUSED", PercentComplete: 99.84},
	}
	queued := []ddl.ObjectRef{{Schema: "dbo", Table: "T", Name: "IX"}}

	got := classifyResumables(server, queued)
	if len(got) != 2 {
		t.Fatalf("classified %d, want 2", len(got))
	}
	if !got[0].Covered {
		t.Error("dbo.T.IX is targeted by a queued operation and must be reported as covered")
	}
	if got[1].Covered {
		t.Error("dbo.Other.PK_Other is in no queued operation: nothing will continue it")
	}
}

// A RUNNING resumable belongs to a live run, not to us: reporting it as abandoned work
// would push an operator to act on something already in hand.
func TestScanIgnoresRunningResumables(t *testing.T) {
	server := []mssql.ResumableOp{{Schema: "dbo", Table: "T", Name: "IX", StateDesc: "RUNNING"}}
	if got := classifyResumables(server, nil); len(got) != 0 {
		t.Errorf("classified %+v, want none (only paused operations are stranded)", got)
	}
}

func TestResumableWarningLineNamesStateAndCoverage(t *testing.T) {
	covered := resumableWarningLine(resumableWarning{
		Schema: "dbo", Table: "T", Index: "IX", PercentComplete: 72.37, ExecutionMinutes: 14, Covered: true,
	})
	for _, want := range []string{"dbo.T.IX", "72.4%", "queued"} {
		if !strings.Contains(covered, want) {
			t.Errorf("covered line %q missing %q", covered, want)
		}
	}

	orphan := resumableWarningLine(resumableWarning{
		Schema: "dbo", Table: "Other", Index: "PK_Other", PercentComplete: 99.84,
	})
	if !strings.Contains(orphan, "no queued operation") {
		t.Errorf("orphan line must say nothing will continue it, got %q", orphan)
	}
	if !strings.Contains(orphan, "RESUME") {
		t.Errorf("orphan line must name the way out, got %q", orphan)
	}
}

// TestScanMatchesOnTheWholeThreePartName: a reviewer noted the fixtures used distinct
// index names, so reducing the comparison to the index name alone would still pass. Same
// index name on a different table must NOT count as covered.
func TestScanMatchesOnTheWholeThreePartName(t *testing.T) {
	server := []mssql.ResumableOp{{Schema: "dbo", Table: "Other", Name: "IX", StateDesc: "PAUSED"}}
	queued := []ddl.ObjectRef{{Schema: "dbo", Table: "T", Name: "IX"}}
	got := classifyResumables(server, queued)
	if len(got) != 1 || got[0].Covered {
		t.Errorf("classified %+v; dbo.Other.IX is not covered by a queued dbo.T.IX", got)
	}
}

// TestScanCoverageIsCaseInsensitive: SQL Server object names compare under the database
// collation and a manifest may spell them differently from the DMV. Replacing EqualFold
// with == would otherwise slip through, since every other fixture matches exactly.
func TestScanCoverageIsCaseInsensitive(t *testing.T) {
	server := []mssql.ResumableOp{{Schema: "dbo", Table: "T", Name: "IX", StateDesc: "PAUSED"}}
	queued := []ddl.ObjectRef{{Schema: "DBO", Table: "t", Name: "ix"}}
	got := classifyResumables(server, queued)
	if len(got) != 1 || !got[0].Covered {
		t.Errorf("classified %+v; want covered despite case differences", got)
	}
}
