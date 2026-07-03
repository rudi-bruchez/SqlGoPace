package run_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
