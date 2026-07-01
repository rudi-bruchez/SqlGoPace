package run_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/run"
)

// stepRecorder collects the StepEvents the engine emits. It is mutex-guarded because
// a Finished event may be emitted from the run goroutine while the test reads the
// slice after ProcessAll returns.
type stepRecorder struct {
	mu     sync.Mutex
	events []run.StepEvent
}

func (r *stepRecorder) record(ev run.StepEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

const twoOpManifest = `
description: step-sink test
operations:
  - operation: rebuild_index
    schema: dbo
    table: T
    index: IX
  - operation: rebuild_index
    schema: dbo
    table: T
    index: IX2
`

func TestStepSinkEmitsStartedAndFinishedPerOp(t *testing.T) {
	rec := &stepRecorder{}
	runner := &fakeOpRunner{}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner, run.WithStepSink(rec.record))
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_a.yaml"), []byte(twoOpManifest), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}

	want := []struct {
		idx, total int
		phase      run.StepPhase
		outcome    string
	}{
		{1, 2, run.StepStarted, ""},
		{1, 2, run.StepFinished, "success"},
		{2, 2, run.StepStarted, ""},
		{2, 2, run.StepFinished, "success"},
	}
	if len(rec.events) != len(want) {
		t.Fatalf("events = %d, want %d: %+v", len(rec.events), len(want), rec.events)
	}
	for i, w := range want {
		ev := rec.events[i]
		if ev.Index != w.idx || ev.Total != w.total || ev.Phase != w.phase || ev.Outcome != w.outcome {
			t.Errorf("event[%d] = {Index:%d Total:%d Phase:%d Outcome:%q}, want {%d %d %d %q}",
				i, ev.Index, ev.Total, ev.Phase, ev.Outcome, w.idx, w.total, w.phase, w.outcome)
		}
		if ev.Command != "rebuild_index" || ev.Target == "" {
			t.Errorf("event[%d] Command=%q Target=%q, want rebuild_index and non-empty target", i, ev.Command, ev.Target)
		}
		if ev.StartedAt.IsZero() {
			t.Errorf("event[%d] StartedAt is zero", i)
		}
		if w.phase == run.StepFinished && ev.Duration < 0 {
			t.Errorf("event[%d] Duration = %v, want >= 0", i, ev.Duration)
		}
	}
}

func TestStepSinkReportsFailedOutcome(t *testing.T) {
	rec := &stepRecorder{}
	runner := &fakeOpRunner{err: io.ErrUnexpectedEOF}
	eng, _ := setupEngine(t, fakePreflighter{}, runner, run.WithStepSink(rec.record))

	// The default manifest (010_a.yaml) has one op and no on_failure → fail-fast.
	// A failed operation is recorded in the run summary, not returned as an error.
	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if sum.Failed != 1 {
		t.Fatalf("Summary = %+v, want Failed:1", sum)
	}

	if len(rec.events) != 2 {
		t.Fatalf("events = %d, want 2: %+v", len(rec.events), rec.events)
	}
	if rec.events[0].Phase != run.StepStarted {
		t.Errorf("event[0].Phase = %d, want StepStarted", rec.events[0].Phase)
	}
	if rec.events[1].Phase != run.StepFinished || rec.events[1].Outcome != "failed" {
		t.Errorf("finished event = %+v, want Phase=StepFinished Outcome=failed", rec.events[1])
	}
}
