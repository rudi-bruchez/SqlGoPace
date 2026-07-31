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

// newSelfBlockTestRunner wires a ShrinkRunner with a short poll interval and an onExec hook
// that holds the first page-moving DBCC SHRINKFILE chunk open on the returned release channel.
// The in-flight self-block sampler (pumpSelfBlock) reads immediately when the chunk starts, so
// holding the chunk open gives the test a deterministic window to observe s.sampledOnce (set by
// fakeServer.ActiveSessions) before releasing it — no time.Sleep, no timing-dependence. Mirrors
// the wiring in TestShrinkEmitsServerPercentComplete.
func newSelfBlockTestRunner(s *fakeServer) (*ShrinkRunner, chan struct{}) {
	release := make(chan struct{})
	s.onExec = func(sql string) {
		if strings.Contains(sql, "DBCC SHRINKFILE") && !strings.Contains(sql, "TRUNCATEONLY") {
			<-release
		}
	}
	clk := NewManualClock(time.Unix(0, 0))
	r := NewShrinkRunner(s, s, noPressureSampler{}, clk, ShrinkRunnerConfig{
		Tuning:          testTuning(),
		PollInterval:    2 * time.Millisecond, // fast enough for pumpSelfBlock's ticker, if ever needed
		LogPollInterval: time.Hour,
		BlockingTimeout: time.Minute,
		LogDrainTimeout: time.Minute,
		KillGrace:       time.Second,
	})
	r.wait = func(_ context.Context, d time.Duration) error { clk.Advance(d); return nil }
	return r, release
}

// runHeld runs op in a goroutine, waits for the fake's ActiveSessions to be sampled (the
// in-flight self-block sampler's immediate read), releases the held chunk, then waits for the
// run to finish. It fails the test on either wait timing out, rather than hanging forever.
func runHeld(t *testing.T, r *ShrinkRunner, s *fakeServer, release chan struct{}, op ddl.Shrink, sink ReactionSink) []ShrinkResult {
	t.Helper()
	type outcome struct {
		res []ShrinkResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, sink)
		done <- outcome{res, err}
	}()

	select {
	case <-s.sampledOnce:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("self-block sampler never called ActiveSessions within 2s")
	}
	close(release)

	select {
	case o := <-done:
		if o.err != nil {
			t.Fatalf("Run() error = %v", o.err)
		}
		return o.res
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not finish after releasing the held chunk")
		return nil
	}
}

// TestMaintBlockWarnsOnceUnderMaintenance drives a no-gain give-up while ActiveSessions
// reports our shrink blocked by a concurrent ALTER INDEX. The driver must emit exactly one
// clear "concurrent maintenance" warning naming the verb, the blocker SPID, and the wait.
func TestMaintBlockWarnsOnceUnderMaintenance(t *testing.T) {
	s := &fakeServer{
		fileType: mssql.FileTypeRows, name: "Data",
		sizeMB: 1000, usedMB: 400, floorMB: 400, noProgress: true,
		sessions:    blockedByMaint("ALTER INDEX"),
		sampledOnce: make(chan struct{}),
	}
	r, release := newSelfBlockTestRunner(s)

	var events []ReactionEvent
	warns := 0
	sink := func(e ReactionEvent) {
		events = append(events, e)
		if e.Kind == "warn" && strings.Contains(e.Detail, "concurrent maintenance") {
			warns++
		}
	}
	op := ddl.Shrink{Type: "data", Files: "Data", TargetFreeSpace: "10%"}
	runHeld(t, r, s, release, op, sink)

	if warns != 1 {
		t.Errorf("maintenance warnings = %d, want exactly 1", warns)
	}
	d := warnDetail(events)
	if !strings.Contains(d, "ALTER INDEX") || !strings.Contains(d, "104") {
		t.Errorf("warning must name the verb and blocker SPID: %q", d)
	}
}

// TestMaintBlockIgnoresApplicationBlocker: an application UPDATE blocking the shrink is NOT
// maintenance — no maintenance warning is emitted (today's behavior is preserved). pumpSelfBlock
// still samples immediately, but a non-maintenance blocker never sends, so no coordination with
// a held chunk is needed here.
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
		sessions:    blockedByMaint("ALTER INDEX"),
		sampledOnce: make(chan struct{}),
	}
	r, release := newSelfBlockTestRunner(s)
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
	runHeld(t, r, s, release, op, sink)

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
		sessions:    blockedByMaint("ALTER INDEX"),
		sampledOnce: make(chan struct{}),
		tail:        mssql.TailObject{ObjectID: 21, Schema: "dbo", Table: "Rebuilt", IndexID: 1, PageFromEnd: 3},
		tailFound:   true,
	}
	r, release := newSelfBlockTestRunner(s)
	r.major = 15

	var events []ReactionEvent
	sink := func(e ReactionEvent) { events = append(events, e) }
	op := ddl.Shrink{Type: "data", Files: "Data", TargetFreeSpace: "10%"}
	res := runHeld(t, r, s, release, op, sink)

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
