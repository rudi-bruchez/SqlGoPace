package run

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// blockedByMaint scripts an ActiveSessions snapshot where our shrink (SPID 99) is
// blocked by session 104 running the given command.
func blockedByMaint(command string) []mssql.Session {
	return []mssql.Session{
		{SPID: 99, BlockingSPID: 104, WaitType: "LCK_M_SCH_M", WaitMS: 90000},
		{SPID: 104, Command: command},
	}
}

func warnDetail(events []ReactionEvent) string {
	for _, e := range events {
		if e.Kind == "warn" {
			return e.Detail
		}
	}
	return ""
}

// TestMaintBlockWarnsOnceUnderMaintenance drives a no-gain give-up while ActiveSessions
// reports our shrink blocked by a concurrent ALTER INDEX. The driver must emit exactly one
// clear "concurrent maintenance" warning naming the verb, the blocker SPID, and the wait.
func TestMaintBlockWarnsOnceUnderMaintenance(t *testing.T) {
	s := &fakeServer{
		fileType: mssql.FileTypeRows, name: "Data",
		sizeMB: 1000, usedMB: 400, floorMB: 400, noProgress: true,
		sessions: blockedByMaint("ALTER INDEX"),
	}
	r := newTestRunner(s, NewManualClock(time.Unix(0, 0)))

	var events []ReactionEvent
	warns := 0
	sink := func(e ReactionEvent) {
		events = append(events, e)
		if e.Kind == "warn" && strings.Contains(e.Detail, "concurrent maintenance") {
			warns++
		}
	}
	op := ddl.Shrink{Type: "data", Files: "Data", TargetFreeSpace: "10%"}
	if _, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, sink); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if warns != 1 {
		t.Errorf("maintenance warnings = %d, want exactly 1", warns)
	}
	d := warnDetail(events)
	if !strings.Contains(d, "ALTER INDEX") || !strings.Contains(d, "104") {
		t.Errorf("warning must name the verb and blocker SPID: %q", d)
	}
}

// TestMaintBlockIgnoresApplicationBlocker: an application UPDATE blocking the shrink is NOT
// maintenance — no maintenance warning is emitted (today's behavior is preserved).
func TestMaintBlockIgnoresApplicationBlocker(t *testing.T) {
	s := &fakeServer{
		fileType: mssql.FileTypeRows, name: "Data",
		sizeMB: 1000, usedMB: 400, floorMB: 400, noProgress: true,
		sessions: blockedByMaint("UPDATE"),
	}
	r := newTestRunner(s, NewManualClock(time.Unix(0, 0)))

	var events []ReactionEvent
	sink := func(e ReactionEvent) { events = append(events, e) }
	op := ddl.Shrink{Type: "data", Files: "Data", TargetFreeSpace: "10%"}
	if _, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, sink); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if d := warnDetail(events); strings.Contains(d, "concurrent maintenance") {
		t.Errorf("application blocker must not emit a maintenance warning: %q", d)
	}
}

// TestTailAndMaintWarningsAreIndependent: below 2019 with IdentifyTailObject set AND a
// concurrent maintenance block, both once-per-operation warnings fire (separate guards).
func TestTailAndMaintWarningsAreIndependent(t *testing.T) {
	s := &fakeServer{
		fileType: mssql.FileTypeRows, name: "Data",
		sizeMB: 1000, usedMB: 400, floorMB: 400, noProgress: true,
		sessions: blockedByMaint("ALTER INDEX"),
	}
	r := newTestRunner(s, NewManualClock(time.Unix(0, 0)))
	r.major = 13 // below 2019: the proactive walk warns about the missing DMV

	var tailWarn, maintWarn bool
	sink := func(e ReactionEvent) {
		if e.Kind == "warn" && strings.Contains(e.Detail, "2019") {
			tailWarn = true
		}
		if e.Kind == "warn" && strings.Contains(e.Detail, "concurrent maintenance") {
			maintWarn = true
		}
	}
	op := ddl.Shrink{Type: "data", Files: "Data", TargetFreeSpace: "10%", IdentifyTailObject: true}
	if _, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, sink); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !tailWarn || !maintWarn {
		t.Errorf("both warnings must fire independently: tailWarn=%v maintWarn=%v", tailWarn, maintWarn)
	}
}

// TestMaintBlockRecordsTransientTail: a give-up under concurrent ALTER INDEX records the tail
// as transient (Transient + blocked-by set), and the give-up reason names the maintenance op.
func TestMaintBlockRecordsTransientTail(t *testing.T) {
	s := &fakeServer{
		fileType: mssql.FileTypeRows, name: "Data",
		sizeMB: 1000, usedMB: 400, floorMB: 400, noProgress: true,
		sessions:  blockedByMaint("ALTER INDEX"),
		tail:      mssql.TailObject{ObjectID: 21, Schema: "dbo", Table: "Rebuilt", IndexID: 1, PageFromEnd: 3},
		tailFound: true,
	}
	r := newTestRunner(s, NewManualClock(time.Unix(0, 0)))
	r.major = 15

	var events []ReactionEvent
	sink := func(e ReactionEvent) { events = append(events, e) }
	op := ddl.Shrink{Type: "data", Files: "Data", TargetFreeSpace: "10%"}
	res, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, sink)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(res) != 1 || res[0].Reason == "" {
		t.Fatalf("got %+v, want a give-up result with a reason", res)
	}
	tf := wantTail(events)
	if tf == nil || !tf.Transient {
		t.Fatalf("want a transient tail-bearing event, got %+v", tf)
	}
	if tf.BlockedByCommand != "ALTER INDEX" || tf.BlockedBySPID != 104 {
		t.Errorf("blocked-by = (%q, %d), want (ALTER INDEX, 104)", tf.BlockedByCommand, tf.BlockedBySPID)
	}
	if !strings.Contains(res[0].Reason, "maintenance") || !strings.Contains(res[0].Reason, "ALTER INDEX") {
		t.Errorf("give-up reason must name the maintenance op: %q", res[0].Reason)
	}
}

// TestApplicationBlockerStaysTailPosition: the SAME give-up, but the blocker is an application
// UPDATE, records a normal structural tail_position (Transient false) — unchanged behavior.
func TestApplicationBlockerStaysTailPosition(t *testing.T) {
	s := &fakeServer{
		fileType: mssql.FileTypeRows, name: "Data",
		sizeMB: 1000, usedMB: 400, floorMB: 400, noProgress: true,
		sessions:  blockedByMaint("UPDATE"),
		tail:      mssql.TailObject{ObjectID: 22, Schema: "dbo", Table: "Hot", IndexID: 1, PageFromEnd: 1},
		tailFound: true,
	}
	r := newTestRunner(s, NewManualClock(time.Unix(0, 0)))
	r.major = 15

	var events []ReactionEvent
	sink := func(e ReactionEvent) { events = append(events, e) }
	op := ddl.Shrink{Type: "data", Files: "Data", TargetFreeSpace: "10%"}
	if _, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, sink); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	tf := wantTail(events)
	if tf == nil || tf.Transient {
		t.Fatalf("want a non-transient tail_position record, got %+v", tf)
	}
}
