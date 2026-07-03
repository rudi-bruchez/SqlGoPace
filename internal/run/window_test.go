package run_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/run"
)

type fakeServerClock struct {
	now time.Time
	err error
}

func (f fakeServerClock) ServerNow(context.Context) (time.Time, error) {
	return f.now, f.err
}

func TestWindowOpen(t *testing.T) {
	// Saturday 2022-01-01 03:00.
	sat0300 := time.Date(2022, 1, 1, 3, 0, 0, 0, time.UTC)
	win := &ddl.Window{Start: "01:00", End: "05:00"}

	eng, _ := setupEngine(t, fakePreflighter{}, &fakeOpRunner{},
		run.WithServerClock(fakeServerClock{now: sat0300}))

	open, err := run.ExportWindowOpen(eng, context.Background(), win)
	if err != nil || !open {
		t.Fatalf("windowOpen inside = (%v, %v), want (true, nil)", open, err)
	}

	// nil window is always open.
	open, err = run.ExportWindowOpen(eng, context.Background(), nil)
	if err != nil || !open {
		t.Fatalf("windowOpen(nil) = (%v, %v), want (true, nil)", open, err)
	}

	// clock error propagates.
	engErr, _ := setupEngine(t, fakePreflighter{}, &fakeOpRunner{},
		run.WithServerClock(fakeServerClock{err: errors.New("boom")}))
	if _, err := run.ExportWindowOpen(engErr, context.Background(), win); err == nil {
		t.Fatal("windowOpen with clock error: want error, got nil")
	}

	// window set but no server clock wired: a configuration error, not a clock read.
	engNoClock, _ := setupEngine(t, fakePreflighter{}, &fakeOpRunner{})
	open, err = run.ExportWindowOpen(engNoClock, context.Background(), win)
	if err == nil || open {
		t.Fatalf("windowOpen with no server clock = (%v, %v), want (false, non-nil error)", open, err)
	}
}

func TestProcessAllDefersOutsideWindow(t *testing.T) {
	// Saturday 2022-01-01 12:00 — outside a 01:00–05:00 window.
	satNoon := time.Date(2022, 1, 1, 12, 0, 0, 0, time.UTC)
	runner := &fakeOpRunner{}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner,
		run.WithServerClock(fakeServerClock{now: satNoon}))

	// Overwrite the default manifest with a windowed one.
	const windowed = `
description: windowed
window:
  start: "01:00"
  end: "05:00"
operations:
  - operation: rebuild_index
    schema: dbo
    table: T
    index: IX
`
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_a.yaml"), []byte(windowed), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll: %v", err)
	}
	if sum.Deferred != 1 || sum.Done != 0 {
		t.Fatalf("Summary = %+v, want Deferred:1 Done:0", sum)
	}
	if runner.calls != 0 {
		t.Fatalf("runner.calls = %d, want 0 (nothing executed)", runner.calls)
	}
	// Left untouched in to_run, not claimed into processing.
	if _, err := os.Stat(filepath.Join(dirs.ToRun, "010_a.yaml")); err != nil {
		t.Fatalf("manifest should remain in to_run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dirs.Processing, "010_a.yaml")); err == nil {
		t.Fatal("manifest must not be in processing")
	}
}

func TestProcessOneStopsWhenWindowCloses(t *testing.T) {
	// A clock that reports inside the window for the first read (entry), then
	// outside for subsequent reads (op boundaries), simulating the window closing.
	clk := &togglingClock{
		times: []time.Time{
			time.Date(2022, 1, 1, 4, 59, 0, 0, time.UTC), // entry: inside 01:00–05:00
			time.Date(2022, 1, 1, 5, 1, 0, 0, time.UTC),  // boundary before op 0: closed
		},
	}
	runner := &fakeOpRunner{}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner, run.WithServerClock(clk))

	const windowed = `
description: windowed
window:
  start: "01:00"
  end: "05:00"
operations:
  - operation: rebuild_index
    schema: dbo
    table: T
    index: IX
`
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_a.yaml"), []byte(windowed), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll: %v", err)
	}
	if sum.Interrupted != 1 {
		t.Fatalf("Summary = %+v, want Interrupted:1", sum)
	}
	if runner.calls != 0 {
		t.Fatalf("runner.calls = %d, want 0 (window closed before op 0)", runner.calls)
	}
	// Left in processing for the next window (not done, not failed).
	if _, err := os.Stat(filepath.Join(dirs.Processing, "010_a.yaml")); err != nil {
		t.Fatalf("manifest should remain in processing: %v", err)
	}
}

func TestProcessOneStopsAtLoopTopWhenWindowClosesBetweenOps(t *testing.T) {
	// A clock that reports inside the window for the pre-claim gate, the entry check,
	// and the loop-top check before op 0, then closed for the loop-top check before op
	// 1 — simulating the window closing while op 0 was running. Each windowOpen call
	// performs exactly one ServerNow read on success, so the reads are, in order:
	// (1) deferredByWindow, (2) processOne entry, (3) loop-top before op 0, (4) loop-top
	// before op 1.
	inside := time.Date(2022, 1, 1, 4, 0, 0, 0, time.UTC)
	closed := time.Date(2022, 1, 1, 5, 1, 0, 0, time.UTC)
	clk := &togglingClock{times: []time.Time{inside, inside, inside, closed}}

	runner := &fakeOpRunner{}
	out := &syncBuffer{}
	// A session must be wired for the engine to write the resume-cursor sidecar at all
	// (writeSidecar is a no-op without one); the cursor value itself doesn't depend on it.
	eng, dirs := setupEngine(t, fakePreflighter{}, runner, run.WithServerClock(clk), run.WithOutput(out),
		run.WithSession(fakeSession{spid: 71}))

	const windowedTwoOps = `
description: windowed two-op
window:
  start: "01:00"
  end: "05:00"
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
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "010_a.yaml"), []byte(windowedTwoOps), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll: %v", err)
	}
	if sum.Interrupted != 1 {
		t.Fatalf("Summary = %+v, want Interrupted:1", sum)
	}
	if runner.calls != 1 {
		t.Fatalf("runner.calls = %d, want 1 (op 0 ran; op 1 did not start — window closed at the loop top)", runner.calls)
	}
	// Left in processing for the next window, not moved to done or failed.
	if _, err := os.Stat(filepath.Join(dirs.Processing, "010_a.yaml")); err != nil {
		t.Fatalf("manifest should remain in processing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dirs.Done, "010_a.yaml")); err == nil {
		t.Fatal("manifest must not be in done")
	}
	// The resume cursor advanced past op 0, so the next run resumes at op 1.
	if st := readSidecarState(t, dirs, "010_a.yaml"); st.ResumeFromOp != 1 {
		t.Fatalf("ResumeFromOp = %d, want 1 (op 0 done, resume before op 1)", st.ResumeFromOp)
	}
	if !strings.Contains(out.String(), "window closed after operation") {
		t.Fatalf("output = %q, want it to contain %q", out.String(), "window closed after operation")
	}
}

// togglingClock returns successive times, repeating the last one once exhausted.
type togglingClock struct {
	times []time.Time
	i     int
}

func (c *togglingClock) ServerNow(context.Context) (time.Time, error) {
	t := c.times[c.i]
	if c.i < len(c.times)-1 {
		c.i++
	}
	return t, nil
}
