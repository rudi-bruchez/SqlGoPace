package run

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/maint"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// fakeHeldLocksReader returns a fixed set of held object locks for
// captureContended; the ActiveSessions half of BlockerReader is unused here.
type fakeHeldLocksReader struct{ held []mssql.LockedObject }

func (f fakeHeldLocksReader) ActiveSessions(context.Context) ([]mssql.Session, error) {
	return nil, nil
}
func (f fakeHeldLocksReader) HeldObjectLocks(context.Context, int) ([]mssql.LockedObject, error) {
	return f.held, nil
}

// TestCaptureContendedSkipsWriteWhenHeldEmpty covers FIX 6: a snapshot where the
// shrink holds NO Sch-M lock (e.g. a stall pause after the lock released) must not
// rewrite the sidecar. Once an object has been captured, an empty-held snapshot
// must leave the existing file alone rather than rewriting the identical content —
// the earlier "acc.len() > 0" guard rewrote on every call once anything had ever
// been captured.
func TestCaptureContendedSkipsWriteWhenHeldEmpty(t *testing.T) {
	dir := t.TempDir()
	e := &Engine{dirs: Dirs{Processing: dir}, clk: System}
	var acc contendedCapture

	// First snapshot: the shrink holds a lock. The sidecar must be written.
	e.blockers = fakeHeldLocksReader{held: []mssql.LockedObject{
		{ObjectID: 1, Schema: "dbo", Table: "T", Mode: "Sch-M"},
	}}
	e.captureContended(context.Background(), 42, &acc, "020_shrink", "DB")
	path := filepath.Join(dir, "020_shrink"+contendedCaptureSuffix)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected sidecar written on a non-empty held snapshot: %v", err)
	}

	// Remove the file, then take an EMPTY-held snapshot: must NOT rewrite it.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	e.blockers = fakeHeldLocksReader{held: nil}
	e.captureContended(context.Background(), 42, &acc, "020_shrink", "DB")
	if _, err := os.Stat(path); err == nil {
		t.Errorf("sidecar was rewritten on an empty held snapshot")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat: %v", err)
	}
}

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
