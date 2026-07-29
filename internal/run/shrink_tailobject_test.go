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

func TestMaybeCaptureTailEmitsOn2019Plus(t *testing.T) {
	s := &fakeServer{
		tail:      mssql.TailObject{ObjectID: 5, Schema: "dbo", Table: "Big", IndexID: 1, PageFromEnd: 2},
		tailFound: true,
	}
	r := tailRunner(s, 15)

	var events []ReactionEvent
	sink := func(ev ReactionEvent) { events = append(events, ev) }
	warned := new(bool)
	r.maybeCaptureTail(context.Background(), mssql.FileSpace{Name: "d", FileID: 1, FreeMB: 10}, sink, warned)

	var tail *TailFinding
	for _, ev := range events {
		if ev.Tail != nil {
			tail = ev.Tail
		}
	}
	if tail == nil {
		t.Fatal("expected a Tail event on 2019+")
	}
	if tail.ObjectID != 5 || tail.PageFromEnd != 2 {
		t.Errorf("tail finding = %+v", tail)
	}
	if s.tailCalls != 1 {
		t.Errorf("FindTailObject calls = %d, want 1", s.tailCalls)
	}
}

func TestMaybeCaptureTailWarnsOnceBelow2019(t *testing.T) {
	s := &fakeServer{}
	r := tailRunner(s, 13)

	var warns int
	sink := func(ev ReactionEvent) {
		if ev.Kind == "warn" {
			warns++
		}
	}
	warned := new(bool)
	f := mssql.FileSpace{Name: "d", FileID: 1, FreeMB: 10}
	r.maybeCaptureTail(context.Background(), f, sink, warned)
	r.maybeCaptureTail(context.Background(), f, sink, warned)

	if warns != 1 {
		t.Errorf("warnings = %d, want 1 (once per operation)", warns)
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
// `if tp != nil { r.maybeCaptureTail(...) }` inserted right before this give-up's
// `return result, nil` — rather than calling maybeCaptureTail directly, so a regression that
// drops or misplaces that call would fail this test.
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
// unconditionally, so maybeCaptureTail is never reached from that path.
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

// TestChunkLoopProactiveWalkFiresAtEntry drives a data shrink that converges normally (no
// give-up at all — the only opportunity for a call is the proactive one at loop entry) with
// op.IdentifyTailObject set, and checks the walk fired exactly once despite several chunks.
func TestChunkLoopProactiveWalkFiresAtEntry(t *testing.T) {
	s := &fakeServer{
		fileType: mssql.FileTypeRows, name: "Data",
		sizeMB: 1000, usedMB: 400, floorMB: 400, // no truncate effect; converges via chunk loop
		tail:      mssql.TailObject{ObjectID: 11, Schema: "dbo", Table: "Proactive", IndexID: 1, PageFromEnd: 0},
		tailFound: true,
	}
	clk := NewManualClock(time.Unix(0, 0))
	r := newTestRunner(s, clk)
	r.major = 15

	op := ddl.Shrink{Type: "data", Files: "Data", TargetFreeSpace: "10%", IdentifyTailObject: true}
	res, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, discard)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if res[0].Chunks == 0 {
		t.Fatalf("got %+v, want a converged shrink with > 0 chunks", res)
	}
	if s.tailCalls != 1 {
		t.Errorf("FindTailObject calls = %d, want exactly 1 (proactive, once at entry — not per chunk)", s.tailCalls)
	}
}
