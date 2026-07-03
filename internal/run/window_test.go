package run_test

import (
	"context"
	"errors"
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
