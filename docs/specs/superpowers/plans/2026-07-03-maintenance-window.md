# Maintenance Window Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a manifest declare a recurring server-time window; its operations run only inside the window, defer when outside, and stop cleanly (finishing the current op) when the window closes.

**Architecture:** A pure `ddl.Window` (parse/validate/`Contains`) evaluated against the SQL Server clock read via a new `run.ServerClock` (prod: `mssql.Conn.ServerNow` → `SELECT SYSDATETIME()`). The engine gates at two points: a pre-claim check in `ProcessAll` (defer), and the existing op-boundary graceful-stop check in `processOne` (window closed → reuse the drain/cursor path). No new execution model — it composes with drain, the resume cursor, and frozen ordering.

**Tech Stack:** Go, `gopkg.in/yaml.v3`, `github.com/microsoft/go-mssqldb`, standard `time`. Tests: standard `testing`, `-race`, fakes (no DB for unit tests).

## Global Constraints

- **Idiomatic Go, KISS.** No new layers/interfaces/generics beyond what a task needs.
- **English only** — all code, comments, identifiers, committed docs.
- **No query timeout** — never wrap executing DDL in `context.WithTimeout`. (Irrelevant here but do not introduce.)
- **Lint:** golangci-lint v2 (`.golangci.yml`); errcheck/govet/staticcheck/ineffassign/unused enforced. US spelling in comments/identifiers.
- **Tests run without a database** (`make test`, `-race`). DB-touching code (`internal/mssql`) is covered by integration tests guarded by the `integration` build tag; unit tests use fakes.
- **Version** lives in `internal/version/VERSION`; do not touch for this feature.
- Follow existing patterns: functional `WithX` engine options; `var _ Interface = (*mssql.Conn)(nil)` compile-time checks; pure functions in `internal/ddl`.

---

### Task 1: `ddl.Window` — struct, parsing, validation

**Files:**
- Create: `internal/ddl/window.go`
- Test: `internal/ddl/window_test.go`

**Interfaces:**
- Produces: `type Window struct { Start string; End string; Days []string }` (yaml tags `start`/`end`/`days`); `func (w *Window) Validate() error`; unexported helpers `parseHHMM(string) (int, error)` (minutes since midnight, 0–1439) and `parseWeekday(string) (time.Weekday, error)` (case-insensitive `Mon`..`Sun`). Consumed by Task 2 (`Contains`) and Task 3 (manifest validation).

- [ ] **Step 1: Write the failing test**

```go
package ddl

import "testing"

func TestWindowValidate(t *testing.T) {
	tests := []struct {
		name    string
		w       Window
		wantErr bool
	}{
		{"ok same-day", Window{Start: "01:00", End: "05:00"}, false},
		{"ok overnight", Window{Start: "22:00", End: "05:00"}, false},
		{"ok with days", Window{Start: "01:00", End: "05:00", Days: []string{"Sat", "sun"}}, false},
		{"bad start format", Window{Start: "1am", End: "05:00"}, true},
		{"bad hour", Window{Start: "24:00", End: "05:00"}, true},
		{"bad minute", Window{Start: "01:60", End: "05:00"}, true},
		{"equal start/end", Window{Start: "01:00", End: "01:00"}, true},
		{"unknown day", Window{Start: "01:00", End: "05:00", Days: []string{"Funday"}}, true},
		{"empty start", Window{Start: "", End: "05:00"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.w.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/ddl -run TestWindowValidate`
Expected: FAIL — `Window`/`Validate` undefined (build error).

- [ ] **Step 3: Write the implementation**

```go
package ddl

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Window restricts a manifest's operations to a recurring time window evaluated
// against the SQL Server's local wall clock (SYSDATETIME). Absent = no constraint.
type Window struct {
	Start string   `yaml:"start"` // "HH:MM", 24h, server local
	End   string   `yaml:"end"`   // "HH:MM", 24h, server local
	Days  []string `yaml:"days"`  // optional; Mon..Sun (case-insensitive); empty = every day
}

// weekdayNames maps lowercase 3-letter names to time.Weekday.
var weekdayNames = map[string]time.Weekday{
	"mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday, "sun": time.Sunday,
}

// parseHHMM parses "HH:MM" into minutes since midnight (0–1439).
func parseHHMM(s string) (int, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("time %q is not HH:MM: %w", s, ErrInvalidManifest)
	}
	h, herr := strconv.Atoi(parts[0])
	m, merr := strconv.Atoi(parts[1])
	if herr != nil || merr != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("time %q is not a valid 24h HH:MM: %w", s, ErrInvalidManifest)
	}
	return h*60 + m, nil
}

// parseWeekday parses a case-insensitive 3-letter weekday name.
func parseWeekday(s string) (time.Weekday, error) {
	d, ok := weekdayNames[strings.ToLower(strings.TrimSpace(s))]
	if !ok {
		return 0, fmt.Errorf("unknown day %q (want Mon..Sun): %w", s, ErrInvalidManifest)
	}
	return d, nil
}

// Validate checks the window's times and day names, and rejects a zero-length
// (start == end) window as ambiguous.
func (w *Window) Validate() error {
	start, err := parseHHMM(w.Start)
	if err != nil {
		return err
	}
	end, err := parseHHMM(w.End)
	if err != nil {
		return err
	}
	if start == end {
		return fmt.Errorf("window start and end are equal (%q): %w", w.Start, ErrInvalidManifest)
	}
	for _, d := range w.Days {
		if _, err := parseWeekday(d); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -race ./internal/ddl -run TestWindowValidate`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ddl/window.go internal/ddl/window_test.go
git commit -m "feat(ddl): add Window type with HH:MM/day parsing and validation"
```

---

### Task 2: `Window.Contains` — same-day, overnight, day-of-week

**Files:**
- Modify: `internal/ddl/window.go`
- Test: `internal/ddl/window_test.go`

**Interfaces:**
- Consumes: `parseHHMM`, `parseWeekday` (Task 1).
- Produces: `func (w Window) Contains(t time.Time) bool` — reports whether the server wall-clock time `t` is inside the window. Consumed by the engine (Tasks 6, 7).

Semantics: start inclusive, end exclusive. Overnight (`start > end`) crosses midnight; the `days` list selects the day the window OPENS (the `start` day), so the pre-dawn tail belongs to the previous day. Reads `t.Hour()`, `t.Minute()`, `t.Weekday()` directly — these are server wall-clock components regardless of `t`'s `Location`.

- [ ] **Step 1: Write the failing test**

```go
func TestWindowContains(t *testing.T) {
	// A fixed Saturday: 2022-01-01 is a Saturday.
	at := func(weekdayOffset, hh, mm int) time.Time {
		base := time.Date(2022, 1, 1, hh, mm, 0, 0, time.UTC) // Sat
		return base.AddDate(0, 0, weekdayOffset)
	}
	sameDay := Window{Start: "01:00", End: "05:00"}
	overnight := Window{Start: "22:00", End: "05:00"}
	satNight := Window{Start: "22:00", End: "05:00", Days: []string{"Sat"}}

	tests := []struct {
		name string
		w    Window
		t    time.Time
		want bool
	}{
		{"same-day inside", sameDay, at(0, 3, 0), true},
		{"same-day at start (inclusive)", sameDay, at(0, 1, 0), true},
		{"same-day at end (exclusive)", sameDay, at(0, 5, 0), false},
		{"same-day before", sameDay, at(0, 0, 59), false},
		{"overnight evening", overnight, at(0, 23, 0), true},
		{"overnight past midnight", overnight, at(1, 2, 0), true}, // Sunday 02:00
		{"overnight at end (exclusive)", overnight, at(1, 5, 0), false},
		{"overnight dead zone", overnight, at(0, 12, 0), false},
		{"sat-night opens Sat evening", satNight, at(0, 23, 0), true},   // Sat 23:00
		{"sat-night tail Sun morning", satNight, at(1, 2, 0), true},     // Sun 02:00 (opened Sat)
		{"sat-night Sun evening excluded", satNight, at(1, 23, 0), false}, // opens Sun -> not allowed
		{"sat-night Mon morning excluded", satNight, at(2, 2, 0), false},  // opened Sun -> not allowed
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.w.Contains(tc.t); got != tc.want {
				t.Errorf("Contains(%s) = %v, want %v", tc.t.Format("Mon 15:04"), got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/ddl -run TestWindowContains`
Expected: FAIL — `Contains` undefined.

- [ ] **Step 3: Write the implementation**

```go
// Contains reports whether server wall-clock time t falls inside the window.
// It is defensive: an unvalidated window (start==end or unparseable) returns false.
func (w Window) Contains(t time.Time) bool {
	start, serr := parseHHMM(w.Start)
	end, eerr := parseHHMM(w.End)
	if serr != nil || eerr != nil || start == end {
		return false
	}
	now := t.Hour()*60 + t.Minute()
	today := t.Weekday()
	yesterday := time.Weekday((int(today) + 6) % 7)

	dayAllowed := func(d time.Weekday) bool {
		if len(w.Days) == 0 {
			return true
		}
		for _, name := range w.Days {
			if wd, err := parseWeekday(name); err == nil && wd == d {
				return true
			}
		}
		return false
	}

	if start < end { // same-day window [start, end)
		return now >= start && now < end && dayAllowed(today)
	}
	// overnight window: [start, 24:00) opens today, [00:00, end) is yesterday's tail
	switch {
	case now >= start:
		return dayAllowed(today)
	case now < end:
		return dayAllowed(yesterday)
	default:
		return false
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -race ./internal/ddl -run TestWindowContains`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ddl/window.go internal/ddl/window_test.go
git commit -m "feat(ddl): add Window.Contains with overnight and day-of-week semantics"
```

---

### Task 3: Wire `Window` into `Manifest`

**Files:**
- Modify: `internal/ddl/manifest.go` (struct `Manifest` ~line 191; `UnmarshalYAML` raw struct ~line 243; `Validate` ~line 217)
- Test: `internal/ddl/manifest_test.go`

**Interfaces:**
- Consumes: `Window`, `Window.Validate` (Tasks 1–2).
- Produces: `Manifest.Window *Window` field (nil when absent). Consumed by the engine (Tasks 6, 7) and main.go (Task 8).

- [ ] **Step 1: Write the failing test**

```go
func TestParseManifestWindow(t *testing.T) {
	const withWindow = `
description: windowed
window:
  start: "01:00"
  end: "05:00"
  days: [Sat, Sun]
operations:
  - operation: rebuild_index
    schema: dbo
    table: T
    index: IX
`
	m, err := ParseManifest(strings.NewReader(withWindow))
	if err != nil {
		t.Fatalf("ParseManifest() error = %v", err)
	}
	if m.Window == nil || m.Window.Start != "01:00" || m.Window.End != "05:00" || len(m.Window.Days) != 2 {
		t.Fatalf("Window = %+v, want start 01:00 end 05:00 with 2 days", m.Window)
	}

	const badWindow = `
description: bad
window:
  start: "01:00"
  end: "01:00"
operations:
  - operation: rebuild_index
    schema: dbo
    table: T
    index: IX
`
	if _, err := ParseManifest(strings.NewReader(badWindow)); err == nil {
		t.Fatal("ParseManifest() with equal start/end: want error, got nil")
	}

	const noWindow = `
description: none
operations:
  - operation: rebuild_index
    schema: dbo
    table: T
    index: IX
`
	m2, err := ParseManifest(strings.NewReader(noWindow))
	if err != nil {
		t.Fatalf("ParseManifest(no window) error = %v", err)
	}
	if m2.Window != nil {
		t.Fatalf("Window = %+v, want nil when absent", m2.Window)
	}
}
```

(Ensure `strings` is imported in `manifest_test.go`; add it if missing.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/ddl -run TestParseManifestWindow`
Expected: FAIL — `m.Window` undefined.

- [ ] **Step 3: Add the field, decode, and validate**

In the `Manifest` struct (after `AbortBlockingResumable bool`, before `Operations`):

```go
	// Window, when set, restricts this manifest's operations to a recurring
	// server-time window: outside it the manifest is deferred; a window closing
	// mid-run stops cleanly at the next operation boundary (see run engine).
	Window *Window
```

In `UnmarshalYAML`'s `raw` struct, add the field:

```go
		Window                 *Window          `yaml:"window"`
```

and after `m.AbortBlockingResumable = raw.AbortBlockingResumable`:

```go
	m.Window = raw.Window
```

In `Validate`, before the `len(m.Operations) == 0` check:

```go
	if m.Window != nil {
		if err := m.Window.Validate(); err != nil {
			return fmt.Errorf("window: %w", err)
		}
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/ddl -run 'TestParseManifestWindow|TestWindow'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ddl/manifest.go internal/ddl/manifest_test.go
git commit -m "feat(ddl): accept optional window block in manifests"
```

---

### Task 4: `mssql.Conn.ServerNow` — read `SYSDATETIME()`

**Files:**
- Create: `internal/mssql/clock.go`
- Test: `internal/mssql/clock_integration_test.go` (guarded by `//go:build integration`)

**Interfaces:**
- Produces: `func (c *Conn) ServerNow(ctx context.Context) (time.Time, error)` — reads the server local time on the monitoring pool. Satisfies `run.ServerClock` (Task 5).

Note: `internal/mssql` is DB-touching; unit tests use fakes elsewhere, so the real read is covered only by an integration test. Keep the method thin.

- [ ] **Step 1: Write the implementation**

```go
package mssql

import (
	"context"
	"fmt"
	"time"
)

// ServerNow returns the SQL Server's local wall-clock time (SYSDATETIME()). It is
// read on the monitoring pool, never the pinned execution connection, so it is
// never blocked by the DDL in flight. The returned time carries the server's
// wall-clock components (datetime2 has no offset); callers read Hour/Minute/Weekday
// directly and must not treat its Location as meaningful.
func (c *Conn) ServerNow(ctx context.Context) (time.Time, error) {
	var t time.Time
	if err := c.pool.QueryRowContext(ctx, "SELECT SYSDATETIME()").Scan(&t); err != nil {
		return time.Time{}, fmt.Errorf("read server time: %w", err)
	}
	return t, nil
}
```

- [ ] **Step 2: Write the integration test**

```go
//go:build integration

package mssql_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

func TestServerNowIntegration(t *testing.T) {
	dsn := os.Getenv("SQLGOPACE_TEST_DSN")
	if dsn == "" {
		t.Skip("SQLGOPACE_TEST_DSN not set")
	}
	ctx := context.Background()
	conn, err := mssql.Open(ctx, dsn, "test")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer conn.Close()

	got, err := conn.ServerNow(ctx)
	if err != nil {
		t.Fatalf("ServerNow: %v", err)
	}
	if got.IsZero() || got.Year() < 2020 {
		t.Fatalf("ServerNow returned implausible time %v", got)
	}
	_ = time.Now()
}
```

(Confirm the module path with `head -1 go.mod` and use it in the import.)

- [ ] **Step 3: Verify it builds (unit lane) and the integration test compiles**

Run: `go build ./internal/mssql && go vet -tags integration ./internal/mssql`
Expected: no errors.

- [ ] **Step 4: Commit**

```bash
git add internal/mssql/clock.go internal/mssql/clock_integration_test.go
git commit -m "feat(mssql): add Conn.ServerNow reading SYSDATETIME on the monitoring pool"
```

---

### Task 5: `run.ServerClock` interface, option, and stop helper

**Files:**
- Create: `internal/run/window.go`
- Modify: `internal/run/engine.go` (`Engine` struct ~line 133 add field; `Summary` struct ~line 115 add `Deferred`)
- Test: `internal/run/window_test.go`

**Interfaces:**
- Consumes: `ddl.Window`, `ddl.Window.Contains` (Tasks 2–3); `finalizeDrained` pattern.
- Produces:
  - `type ServerClock interface { ServerNow(ctx context.Context) (time.Time, error) }` with `var _ ServerClock = (*mssql.Conn)(nil)`.
  - `func WithServerClock(c ServerClock) EngineOption`.
  - `Engine.serverClock ServerClock` field.
  - `Summary.Deferred int` field.
  - `func (e *Engine) windowOpen(ctx context.Context, w *ddl.Window) (open bool, err error)` — nil window ⇒ (true, nil); reads the server clock and returns `w.Contains(now)`.
  - `func (e *Engine) finalizeWindowClosed(ctx context.Context, name string, rep *report.RunReport, start time.Time, done, total int) runOutcome`.

- [ ] **Step 1: Write the failing test**

```go
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
```

Add a tiny test-only export in a new file `internal/run/export_test.go`:

```go
package run

import (
	"context"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

// ExportWindowOpen exposes windowOpen to external tests.
func ExportWindowOpen(e *Engine, ctx context.Context, w *ddl.Window) (bool, error) {
	return e.windowOpen(ctx, w)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/run -run TestWindowOpen`
Expected: FAIL — `WithServerClock`, `windowOpen` undefined.

- [ ] **Step 3: Add the field, summary counter, interface, option, and helpers**

In `internal/run/engine.go`, add to `Summary`:

```go
	Deferred    int // manifests skipped this run because they were outside their window
```

Add to the `Engine` struct (near `drain`):

```go
	serverClock      ServerClock       // reads SQL Server local time for manifest windows
```

Create `internal/run/window.go`:

```go
package run

import (
	"context"
	"fmt"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
	"github.com/rudi-bruchez/SqlGoPace/internal/report"
)

// ServerClock reads the SQL Server's local wall-clock time, used to evaluate a
// manifest's execution window. *mssql.Conn satisfies it via ServerNow.
type ServerClock interface {
	ServerNow(ctx context.Context) (time.Time, error)
}

var _ ServerClock = (*mssql.Conn)(nil)

// WithServerClock wires the server clock so manifests carrying a window are gated
// against server time. Without it, a windowed manifest fails with a clear error.
func WithServerClock(c ServerClock) EngineOption { return func(e *Engine) { e.serverClock = c } }

// windowOpen reports whether window w is currently open in server time. A nil
// window is always open. A non-nil window with no server clock wired is a
// configuration error. A clock read error is returned to the caller, which
// applies the conservative fallback (defer / stop).
func (e *Engine) windowOpen(ctx context.Context, w *ddl.Window) (bool, error) {
	if w == nil {
		return true, nil
	}
	if e.serverClock == nil {
		return false, fmt.Errorf("manifest declares a window but no server clock is configured")
	}
	now, err := e.serverClock.ServerNow(ctx)
	if err != nil {
		// One retry: a transient scan/connection blip often clears immediately.
		now, err = e.serverClock.ServerNow(ctx)
		if err != nil {
			return false, err
		}
	}
	return w.Contains(now), nil
}

// finalizeWindowClosed records a stop because the manifest's window closed after
// `done` of `total` operations. Like a graceful drain, the manifest stays in
// processing with its resume cursor so the next run inside the window continues.
func (e *Engine) finalizeWindowClosed(ctx context.Context, name string, rep *report.RunReport, start time.Time, done, total int) runOutcome {
	rep.Error = fmt.Sprintf("window closed after operation %d/%d — resumes in the next window", done, total)
	e.recordInterrupted(ctx, name, rep, start)
	fmt.Fprintf(e.out, "-- window closed after operation %d/%d on %s — left in processing, resumes next window\n", done, total, name)
	return outcomeInterrupted
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/run -run TestWindowOpen`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/run/window.go internal/run/export_test.go internal/run/window_test.go internal/run/engine.go
git commit -m "feat(run): add ServerClock, WithServerClock, and window stop helper"
```

---

### Task 6: Pre-claim defer gate in `ProcessAll`

**Files:**
- Modify: `internal/run/engine.go` (`ProcessAll` ~line 308–327; add helper near `ownsManifest` ~line 335)
- Test: `internal/run/window_test.go`

**Interfaces:**
- Consumes: `windowOpen` (Task 5), `Summary.Deferred` (Task 5), `Queue.InToRun`, `ddl.LoadManifestFile`.
- Produces: `func (e *Engine) deferredByWindow(ctx context.Context, name string) bool` — loads the manifest from `to_run`; if it has a window that is closed (or the clock read fails), logs and returns true.

- [ ] **Step 1: Write the failing test**

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/run -run TestProcessAllDefersOutsideWindow`
Expected: FAIL — `sum.Deferred` stays 0 / manifest is claimed and run.

- [ ] **Step 3: Add the gate**

Add the helper after `ownsManifest`:

```go
// deferredByWindow reports whether a manifest waiting in to_run should be skipped
// this run because its server-time window is closed. It is conservative: a manifest
// with a window whose server-clock read fails is deferred (never run at an unknown
// time). A manifest that cannot be loaded is not deferred here — processOne surfaces
// the load error.
func (e *Engine) deferredByWindow(ctx context.Context, name string) bool {
	m, err := ddl.LoadManifestFile(filepath.Join(e.dirs.ToRun, name))
	if err != nil || m.Window == nil {
		return false
	}
	open, err := e.windowOpen(ctx, m.Window)
	if err != nil {
		fmt.Fprintf(e.out, "-- defer %s: cannot read server time (%v) — left in queue\n", name, err)
		return true
	}
	if !open {
		fmt.Fprintf(e.out, "-- defer %s: outside window %s–%s %v — left in queue\n", name, m.Window.Start, m.Window.End, m.Window.Days)
		return true
	}
	return false
}
```

In `ProcessAll`, inside the loop, after the `ownsManifest` block and before the `switch e.processOne(...)`:

```go
		if e.deferredByWindow(ctx, name) {
			sum.Deferred++
			continue
		}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/run -run 'TestProcessAll'`
Expected: PASS (existing ProcessAll tests unaffected — no window ⇒ `deferredByWindow` returns false).

- [ ] **Step 5: Commit**

```bash
git add internal/run/engine.go internal/run/window_test.go
git commit -m "feat(run): defer manifests whose server-time window is closed"
```

---

### Task 7: Op-boundary + entry window-closed stop in `processOne`

**Files:**
- Modify: `internal/run/engine.go` (`processOne`: entry check after `LoadManifestFile`/expand ~line 378; loop-top check at the `draining()` gate ~line 420)
- Test: `internal/run/window_test.go`

**Interfaces:**
- Consumes: `windowOpen` (Task 5), `finalizeWindowClosed` (Task 5), `finalizeDrained`, the `cursor` variable.
- Produces: no new exported symbols; behavior — a window closing mid-run stops at the next op boundary via `finalizeWindowClosed` (manifest stays in processing with cursor).

Design note: the entry check handles a windowed manifest resumed while already outside the window (requeued by recovery, then found closed) — stop before preflight. The loop-top check handles the window closing between operations. A clock-read error at either point is treated conservatively as "closed" (stop), matching the defer gate.

- [ ] **Step 1: Write the failing test**

```go
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
```

Note: this test enters `processOne` because the pre-claim gate reads the clock first (inside → not deferred), so the manifest is claimed; the loop-top check then reads again (closed → stop). Ensure the `togglingClock` order matches: read 1 = pre-claim gate (inside), read 2 = loop-top boundary (closed).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/run -run TestProcessOneStopsWhenWindowCloses`
Expected: FAIL — op runs / manifest completes instead of staying in processing.

- [ ] **Step 3: Add the entry and loop-top checks**

In `processOne`, immediately after the expand block (after line ~378, before `pfReport, err := e.pf.Check`):

```go
	// A windowed manifest resumed while already outside its window stops before
	// preflight — nothing to do until the window reopens. A clock error is treated
	// conservatively as closed.
	if open, err := e.windowOpen(ctx, manifest.Window); err != nil || !open {
		return e.finalizeWindowClosed(ctx, name, rep, start, resumeFrom, len(manifest.Operations))
	}
```

In the operation loop, extend the existing graceful-stop gate. Replace:

```go
		if e.draining() {
			return e.finalizeDrained(ctx, name, rep, start, cursor, len(planned))
		}
```

with:

```go
		if e.draining() {
			return e.finalizeDrained(ctx, name, rep, start, cursor, len(planned))
		}
		if open, err := e.windowOpen(ctx, manifest.Window); err != nil || !open {
			return e.finalizeWindowClosed(ctx, name, rep, start, cursor, len(planned))
		}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/run -run 'TestProcessOne|TestProcessAll|TestDrain'`
Expected: PASS (no-window manifests: `windowOpen(nil)` returns true, so behavior is unchanged).

- [ ] **Step 5: Commit**

```bash
git add internal/run/engine.go internal/run/window_test.go
git commit -m "feat(run): stop a windowed manifest cleanly when its window closes"
```

---

### Task 8: Wire the server clock in `main.go` + dry-run/explain annotation

**Files:**
- Modify: `cmd/sqlgopace/main.go` (`buildEngine` ~line 348–420 add `WithServerClock`; `renderPlan`/`dryRunManifest` ~line 843–882 add annotation)
- Test: `cmd/sqlgopace/main_test.go`

**Interfaces:**
- Consumes: `run.WithServerClock` (Task 5); `*mssql.Conn.ServerNow` (Task 4); `Manifest.Window` (Task 3).

- [ ] **Step 1: Write the failing test (offline dry-run annotation)**

```go
func TestDryRunAnnotatesWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "win.yaml")
	const m = `
description: windowed
window:
  start: "01:00"
  end: "05:00"
  days: [Sat, Sun]
operations:
  - operation: rebuild_index
    schema: dbo
    table: T
    index: IX
`
	if err := os.WriteFile(path, []byte(m), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	var out bytes.Buffer
	args := []string{"--dry-run", "--assume-version=15", "--assume-edition=standard",
		"--matrix", filepath.FromSlash("../../ddl_compatibility.yaml"), path}
	if err := run(context.Background(), &out, args); err != nil {
		t.Fatalf("run(dry-run) error = %v", err)
	}
	if !strings.Contains(out.String(), "window 01:00–05:00") {
		t.Errorf("dry-run output missing window annotation:\n%s", out.String())
	}
}
```

(Match the existing `main_test.go` calling convention for `run(...)`; adjust the function name/signature to whatever the package’s entrypoint test helper uses — see `TestRunDryRunStandardOmitsOnline`.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/sqlgopace -run TestDryRunAnnotatesWindow`
Expected: FAIL — no window annotation in output.

- [ ] **Step 3: Add the annotation and the wiring**

In `renderPlan(w io.Writer, source string, manifest *ddl.Manifest, ...)` (after the manifest header line, near the `explain` handling ~line 863–882), add — `w` is the existing writer parameter, `win` is the window local (no shadowing):

```go
	if manifest.Window != nil {
		win := manifest.Window
		days := ""
		if len(win.Days) > 0 {
			days = " " + strings.Join(win.Days, ",")
		}
		fmt.Fprintf(w, "-- window %s–%s%s — enforced at runtime (server time; not evaluated in offline dry-run)\n", win.Start, win.End, days)
	}
```

In `buildEngine`, where other `WithX` options are appended for a live run, add (guarded by a non-nil connection):

```go
	if conn != nil {
		extra = append(extra, run.WithServerClock(conn))
	}
```

(Match the real parameter name for the `*mssql.Conn` in `buildEngine` — it is `conn` in the signature `buildEngine(cfg, matrix, conn *mssql.Conn, ...)`. Confirm and use it.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./cmd/sqlgopace -run 'TestDryRun|TestRun'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/sqlgopace/main.go cmd/sqlgopace/main_test.go
git commit -m "feat(cli): wire server clock for windows and annotate window in dry-run"
```

---

### Task 9: Documentation

**Files:**
- Modify: `README.md` (manifest reference — add the `window` block near `on_failure`/`skip_if_satisfied`)
- Modify: `docs/specs/SPECS.md` (window semantics + defer/stop invariant)

**Interfaces:** none (docs).

- [ ] **Step 1: Add the README section**

Add under the manifest fields documentation:

```markdown
### `window` (optional)

Restrict a manifest's operations to a recurring window, evaluated against the SQL
Server's **local** clock (`SYSDATETIME()`):

```yaml
window:
  start: "01:00"      # HH:MM, 24h, server local time
  end:   "05:00"      # HH:MM
  days:  [Sat, Sun]   # optional; Mon..Sun; default = every day
```

- `end < start` is an overnight window that crosses midnight (e.g. `22:00`–`05:00`).
  `days` selects the day the window **opens**.
- Outside the window, the manifest is **deferred** (left in `01.to_run`, not run) —
  schedule the run (cron / Task Scheduler) to launch during the window.
- If the window closes while the manifest is running, the **current operation
  finishes**, then the run stops and the manifest stays in `02.processing` with its
  resume cursor, continuing in the next window.
- `start == end` is rejected. Offline `--dry-run` cannot evaluate the window (no
  connection) and annotates it instead.
```

- [ ] **Step 2: Add the SPECS.md invariant**

Add a short subsection stating: manifests may declare a server-time `window`; enforcement is at two points (pre-claim defer in `ProcessAll`, op-boundary stop in `processOne` reusing the drain/cursor path); the clock is `SYSDATETIME()` read on the monitoring pool; a clock-read failure is treated conservatively (defer/stop); the feature composes with the frozen-materialized order so a large manifest resumes across windows without discarding the cursor.

- [ ] **Step 3: Commit**

```bash
git add README.md docs/specs/SPECS.md
git commit -m "docs: document the manifest window (server-time execution window)"
```

---

### Task 10: Full verification

**Files:** none (verification only).

- [ ] **Step 1: Run the full unit suite with race**

Run: `make test`
Expected: PASS, no data races.

- [ ] **Step 2: Vet and lint**

Run: `make vet && make lint`
Expected: no findings.

- [ ] **Step 3: Offline dry-run smoke test on a real windowed manifest**

Run: `./sqlgopace.exe --dry-run --assume-version=15 --assume-edition=standard --matrix ./ddl_compatibility.yaml <a windowed manifest>`
Expected: SQL rendered plus the `-- window …` annotation line; exit 0.

---

## Notes on interactions (for the implementer)

- **No-window manifests are unaffected:** `windowOpen(nil)` returns `(true, nil)`, so every gate is a no-op when `manifest.Window == nil`. Confirm existing `ProcessAll`/drain/resume tests still pass after Tasks 6–7.
- **Resume across windows:** `finalizeWindowClosed` leaves the manifest in `02.processing` with the durable cursor (persisted by `advanceCursor`). The next in-window run resumes at the cursor. With the frozen-materialized ordering feature, the plan fingerprint is stable across runs, so `reconcileResumePlan` honors the cursor.
- **Conservative on clock failure:** both the defer gate and the stop checks treat a clock-read error as "do not run" (defer / stop). Never run windowed DDL at an unknown time.
