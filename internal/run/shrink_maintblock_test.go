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
