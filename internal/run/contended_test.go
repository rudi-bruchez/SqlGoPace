package run

import (
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/maint"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

func TestContendedCaptureDedupsAndCounts(t *testing.T) {
	var acc contendedCapture
	obj := mssql.LockedObject{ObjectID: 100, Schema: "dbo", Table: "MEASUREMENT", Mode: "Sch-M"}
	acc.add(obj, "2026-07-28T11:10:09Z")
	acc.add(obj, "2026-07-28T11:14:29Z") // same object, later snapshot
	acc.add(mssql.LockedObject{ObjectID: 200, Schema: "dbo", Table: "OTHER", Mode: "Sch-M"}, "2026-07-28T11:14:29Z")

	doc := acc.doc("PRODDB")
	if len(doc.Observed) != 2 {
		t.Fatalf("observed = %d, want 2", len(doc.Observed))
	}
	first := doc.Observed[0] // first-seen order
	if first.ObjectID != 100 || first.TimesBlocked != 2 {
		t.Errorf("first = %+v, want id 100 times_blocked 2", first)
	}
	if first.FirstSeen != "2026-07-28T11:10:09Z" || first.LastSeen != "2026-07-28T11:14:29Z" {
		t.Errorf("first seen/last = %q/%q", first.FirstSeen, first.LastSeen)
	}
}

func TestRenderContendedRoundTrips(t *testing.T) {
	var acc contendedCapture
	acc.add(mssql.LockedObject{ObjectID: 261575970, Schema: "dbo", Table: "MEASUREMENT", Mode: "Sch-M"}, "2026-07-28T11:10:09Z")

	out := renderContended("020_shrink.yaml", "PRODDB", &acc)
	if !strings.HasPrefix(string(out), "# Contended-object capture") {
		t.Errorf("missing comment header:\n%s", out)
	}
	doc, err := maint.ParseContended(out) // guards format drift against the parser
	if err != nil {
		t.Fatalf("ParseContended(renderContended output): %v", err)
	}
	if doc.Database != "PRODDB" || len(doc.Observed) != 1 || doc.Observed[0].ObjectID != 261575970 {
		t.Errorf("round-tripped doc = %+v", doc)
	}
}
