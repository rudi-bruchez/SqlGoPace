package run

import (
	"context"
	"testing"
	"time"

	mssqldb "github.com/microsoft/go-mssqldb"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// tailRunner builds a ShrinkRunner for the tail tests: only reader + major version matter.
func tailRunner(s *fakeServer, major int) *ShrinkRunner {
	return NewShrinkRunner(s, s, noPressureSampler{}, &ManualClock{}, ShrinkRunnerConfig{
		Tuning:          testTuning(),
		SQLMajorVersion: major,
	})
}

func TestWalkTailAndEmitOn2019Plus(t *testing.T) {
	s := &fakeServer{
		tail:      mssql.TailObject{ObjectID: 5, Schema: "dbo", Table: "Big", IndexID: 1, PageFromEnd: 2},
		tailFound: true,
	}
	r := tailRunner(s, 15)

	var events []ReactionEvent
	sink := func(ev ReactionEvent) { events = append(events, ev) }
	tf := r.walkTail(context.Background(), mssql.FileSpace{Name: "d", FileID: 1, FreeMB: 10}, sink, new(bool), false)
	if tf == nil {
		t.Fatal("expected a finding on 2019+")
	}
	r.emitTail(sink, tf, "d", true) // record=true attaches the finding to the event

	tail := wantTail(events)
	if tail == nil {
		t.Fatal("expected a Tail event when emitTail records")
	}
	if tail.ObjectID != 5 || tail.PageFromEnd != 2 {
		t.Errorf("tail finding = %+v", tail)
	}
	if s.tailCalls != 1 {
		t.Errorf("FindTailObject calls = %d, want 1", s.tailCalls)
	}
}

// TestEmitTailDiagnosticOnly proves record=false logs but does not attach the finding (so the
// engine sink won't record it) — the proactive walk's log-only mode (#1).
func TestEmitTailDiagnosticOnly(t *testing.T) {
	r := tailRunner(&fakeServer{}, 15)
	var events []ReactionEvent
	r.emitTail(func(ev ReactionEvent) { events = append(events, ev) },
		&TailFinding{ObjectID: 5}, "d", false)
	if len(events) != 1 || events[0].Kind != "info" {
		t.Fatalf("want one info event, got %+v", events)
	}
	if wantTail(events) != nil {
		t.Error("record=false must not attach a Tail (would be recorded as a blocker)")
	}
}

func TestWalkTailWarnsOnlyWhenOptedInBelow2019(t *testing.T) {
	s := &fakeServer{}
	r := tailRunner(s, 13)

	var warns int
	sink := func(ev ReactionEvent) {
		if ev.Kind == "warn" {
			warns++
		}
	}
	f := mssql.FileSpace{Name: "d", FileID: 1, FreeMB: 10}

	// Proactive (opt-in, warn=true): warns, once per operation.
	warned := new(bool)
	r.walkTail(context.Background(), f, sink, warned, true)
	r.walkTail(context.Background(), f, sink, warned, true)
	if warns != 1 {
		t.Errorf("opt-in warnings = %d, want 1 (once per operation)", warns)
	}

	// Reactive (always-on, warn=false): must stay silent below 2019 — #4, a run that never
	// asked for the feature is not nagged just for reaching a give-up on an old server.
	warns = 0
	r.walkTail(context.Background(), f, sink, new(bool), false)
	if warns != 0 {
		t.Errorf("reactive warnings = %d, want 0 below 2019 (#4)", warns)
	}
	if s.tailCalls != 0 {
		t.Errorf("FindTailObject must not be called below 2019, got %d calls", s.tailCalls)
	}
}

// wantTail finds the (single expected) Tail-bearing event among a captured sink, or nil.
func wantTail(events []ReactionEvent) *TailFinding {
	for _, ev := range events {
		if ev.Tail != nil {
			return ev.Tail
		}
	}
	return nil
}

// TestChunkLoopCapturesTailOnNoGainGiveUp drives a real chunkLoop (via Run) to the no-gain
// give-up path (chunks execute but never move a page — mirrors TestShrinkDataNoProgressStops)
// with SQLMajorVersion 15 and a scripted tail hit. It exercises the actual wiring — the
// `r.captureGiveUpTail(...)` call right before this give-up's `return result, nil` — rather
// than calling it directly, so a regression that drops or misplaces that call would fail this
// test. IdentifyTailObject is unset, so the record comes from the reactive give-up walk.
func TestChunkLoopCapturesTailOnNoGainGiveUp(t *testing.T) {
	s := &fakeServer{
		fileType: mssql.FileTypeRows, name: "Data",
		sizeMB: 1000, usedMB: 400, floorMB: 400, noProgress: true, // chunks never shrink
		tail:      mssql.TailObject{ObjectID: 5, Schema: "dbo", Table: "Big", IndexID: 1, PageFromEnd: 2},
		tailFound: true,
	}
	clk := NewManualClock(time.Unix(0, 0))
	r := newTestRunner(s, clk)
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
	if s.tailCalls == 0 {
		t.Error("FindTailObject was never called at give-up")
	}
	tail := wantTail(events)
	if tail == nil {
		t.Fatal("expected a Tail-bearing ReactionEvent at give-up")
	}
	if tail.ObjectID != 5 || tail.PageFromEnd != 2 {
		t.Errorf("tail finding = %+v", tail)
	}
}

// TestChunkLoopCapturesTailOnDBCCErrorGiveUp is the sibling of the no-gain test above, but
// drives the OTHER give-up return (the DBCC-error path, "no further progress: %v (work
// preserved)") — the two give-up returns are separate code paths in chunkLoop, so a
// regression could drop the capture from one while leaving it in the other.
func TestChunkLoopCapturesTailOnDBCCErrorGiveUp(t *testing.T) {
	s := &fakeServer{
		fileType: mssql.FileTypeRows, name: "Data",
		sizeMB: 1000, usedMB: 400, floorMB: 400,
		chunkErr:  mssqldb.Error{Number: 3140, Message: "Could not adjust the space allocation for file 'Data'."},
		tail:      mssql.TailObject{ObjectID: 7, Schema: "dbo", Table: "Wide", IndexID: 2, PageFromEnd: 1},
		tailFound: true,
	}
	clk := NewManualClock(time.Unix(0, 0))
	r := newTestRunner(s, clk)
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
	if s.tailCalls == 0 {
		t.Error("FindTailObject was never called at give-up")
	}
	tail := wantTail(events)
	if tail == nil {
		t.Fatal("expected a Tail-bearing ReactionEvent at give-up")
	}
	if tail.ObjectID != 7 || tail.PageFromEnd != 1 {
		t.Errorf("tail finding = %+v", tail)
	}
}

// TestRunTempdbNeverWalksTail proves the tempdb path never runs the tail-object walk, even
// on SQL 2019+ with a tail scripted to be found: RunTempdb's chunkLoop call passes tp == nil
// unconditionally, so neither the proactive nor the give-up walk is ever reached from it.
func TestRunTempdbNeverWalksTail(t *testing.T) {
	s := &fakeServer{
		fileType: mssql.FileTypeRows, name: "tempdev",
		sizeMB: 1000, usedMB: 500, floorMB: 500,
		tail:      mssql.TailObject{ObjectID: 9, Schema: "dbo", Table: "T", IndexID: 1, PageFromEnd: 3},
		tailFound: true,
	}
	clk := NewManualClock(time.Unix(0, 0))
	r := newTestRunner(s, clk)
	r.major = 15

	res, err := r.RunTempdb(context.Background(), ddl.ShrinkTempdb{TargetSizeMB: 200}, ddl.ResolvedOptions{}, nil, discard)
	if err != nil {
		t.Fatalf("RunTempdb() error = %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("got %d results, want 1", len(res))
	}
	if s.tailCalls != 0 {
		t.Errorf("FindTailObject calls = %d, want 0 (tempdb never walks)", s.tailCalls)
	}
}

// TestProactiveWalkNotRecordedOnSuccess drives a data shrink that converges normally (no
// give-up — the only walk is the proactive one at loop entry) with op.IdentifyTailObject set.
// The walk must fire exactly once (at entry, not per chunk), and — #1 — because the shrink
// reaches target, its finding must NOT be recorded as a blocker (no Tail-bearing event): a
// tail object a successful shrink relocated was never a blocker, and recording it would
// mislead `plan --confirmed`.
func TestProactiveWalkNotRecordedOnSuccess(t *testing.T) {
	s := &fakeServer{
		fileType: mssql.FileTypeRows, name: "Data",
		sizeMB: 1000, usedMB: 400, floorMB: 400, // no truncate effect; converges via chunk loop
		tail:      mssql.TailObject{ObjectID: 11, Schema: "dbo", Table: "Proactive", IndexID: 1, PageFromEnd: 0},
		tailFound: true,
	}
	clk := NewManualClock(time.Unix(0, 0))
	r := newTestRunner(s, clk)
	r.major = 15

	var events []ReactionEvent
	sink := func(e ReactionEvent) { events = append(events, e) }

	op := ddl.Shrink{Type: "data", Files: "Data", TargetFreeSpace: "10%", IdentifyTailObject: true}
	res, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, sink)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if res[0].Chunks == 0 {
		t.Fatalf("got %+v, want a converged shrink with > 0 chunks", res)
	}
	if s.tailCalls != 1 {
		t.Errorf("FindTailObject calls = %d, want exactly 1 (proactive, once at entry — not per chunk)", s.tailCalls)
	}
	if tail := wantTail(events); tail != nil {
		t.Errorf("#1: a successful shrink must not record a proactive tail as a blocker, got %+v", tail)
	}
}

// TestProactiveTailRecordedOnGiveUp is the failure counterpart of the success test: with
// op.IdentifyTailObject set AND the shrink stalling to a give-up (misses target), the
// proactively-identified tail IS recorded (a real, missed-target blocker). The walk still runs
// once (the give-up path skips a second walk because the proactive finding is already stashed).
func TestProactiveTailRecordedOnGiveUp(t *testing.T) {
	s := &fakeServer{
		fileType: mssql.FileTypeRows, name: "Data",
		sizeMB: 1000, usedMB: 400, floorMB: 400, noProgress: true, // stalls → give-up
		tail:      mssql.TailObject{ObjectID: 13, Schema: "dbo", Table: "Stuck", IndexID: 1, PageFromEnd: 4},
		tailFound: true,
	}
	clk := NewManualClock(time.Unix(0, 0))
	r := newTestRunner(s, clk)
	r.major = 15

	var events []ReactionEvent
	sink := func(e ReactionEvent) { events = append(events, e) }

	op := ddl.Shrink{Type: "data", Files: "Data", TargetFreeSpace: "10%", IdentifyTailObject: true}
	if _, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, sink); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	tail := wantTail(events)
	if tail == nil {
		t.Fatal("#1: a missed-target shrink must record the proactively-identified tail")
	}
	if tail.ObjectID != 13 {
		t.Errorf("tail finding = %+v, want object 13", tail)
	}
	if s.tailCalls != 1 {
		t.Errorf("FindTailObject calls = %d, want 1 (proactive stash reused at give-up, no second walk)", s.tailCalls)
	}
}
