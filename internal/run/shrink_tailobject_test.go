package run

import (
	"context"
	"testing"

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
