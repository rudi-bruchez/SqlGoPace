# Maintenance window — per-manifest server-time execution windows

**Status:** design approved, pending implementation plan
**Date:** 2026-07-03
**Scope:** a single feature — restrict a working manifest's operations to a recurring
time window evaluated against the SQL Server's local clock.

## Motivation

On Standard edition an index rebuild is OFFLINE (holds a Sch-M lock) and cannot run
during business hours without blocking the workload and being cancelled. The operator
often knows there is a nightly administration window. This feature lets a manifest
declare that window so its operations only run inside it, and stop cleanly when it
closes — carrying progress to the next window.

It composes with two existing/adjacent mechanisms:
- the **graceful stop (drain)** path (`finalizeDrained` + resume cursor), reused verbatim
  for "window closed mid-run";
- the **frozen-materialized ordering** feature (separate spec): a stable op order keeps the
  resume fingerprint stable, so a large manifest processed across several nights resumes
  exactly where it stopped.

## Decisions (locked during brainstorming)

| Question | Decision |
|---|---|
| Granularity | **Per manifest (working file).** The "group of objects" is the file's operations. Different windows → different files. |
| Window closes mid-operation | **Finish the current operation, then stop** at the op boundary (never cancel a running op — Standard has no resumable, cancelling wastes all its work). |
| Launched outside the window | **Defer:** leave the manifest untouched in `01.to_run`, log it, move to the next manifest. External scheduling (cron / Windows Task Scheduler) opens the window. |
| Recurrence | **Time-of-day, daily, with an optional `days:` list**, and overnight (midnight-crossing) support. |
| Clock reference | **SQL Server local wall clock** via `SYSDATETIME()`. Independent of the operator machine's timezone. |
| Server-time source | **Read `SYSDATETIME()` on demand** (approach 1): once at the pre-claim gate, once per op boundary. Robust to long runs, DST, NTP adjustment. |

## Manifest surface

New optional top-level field. Absent = current behavior (no time constraint).

```yaml
description: "Compress EXAMPLEDB large indexes"
database: EXAMPLEDB
on_failure: continue
window:
  start: "01:00"      # HH:MM, server local time
  end:   "05:00"      # HH:MM
  days:  [Sat, Sun]   # optional; default = every day
operations:
  - ...
```

- `start`, `end`: `HH:MM` 24-hour, server-local.
- `days`: optional list of `Mon|Tue|Wed|Thu|Fri|Sat|Sun` (case-insensitive). Default: all days.

## Window semantics (pure)

`internal/ddl/window.go`:

```go
type Window struct {
    Start string   // "HH:MM"
    End   string   // "HH:MM"
    Days  []string // optional; Mon..Sun, case-insensitive
}

// Contains reports whether server wall-clock time t is inside the window.
func (w Window) Contains(t time.Time) bool
```

Rules:
- **`end > start`** → same-day window `[start, end)` on a matching day.
- **`end < start`** → overnight window crossing midnight: `[start, 24:00)` on day D **and**
  `[00:00, end)` on day D+1.
- **`end == start`** → **invalid** (rejected at validation; ambiguous zero/24h).
- Start bound inclusive, end bound exclusive.
- **`days` selects the day the window OPENS** (the `start` day). So `days: [Sat]` with
  `22:00–05:00` = opens Saturday 22:00, closes Sunday 05:00 ("Saturday night"). For a same-day
  window `days` simply selects that day.

**Clock detail (critical):** `SYSDATETIME()` returns server local time with no offset.
`go-mssqldb` scans `datetime2` into `time.Time` preserving the wall-clock components. The
implementation reads `t.Hour()`, `t.Minute()`, `t.Weekday()` **directly** — these are the
server wall-clock values regardless of the operator machine's timezone or the `time.Time`
`Location`. No timezone conversion is performed.

Validation (at manifest load, alongside existing manifest validation):
- `start`/`end` parse as `HH:MM`, 00:00–23:59;
- `end != start`;
- each `days` entry is a known weekday name.
Invalid window → manifest fails fast with a clear error; never reaches execution.

## Server clock component

Interface consumed by the engine (`internal/run`):

```go
type ServerClock interface {
    Now(ctx context.Context) (time.Time, error)
}
```

- **Production:** `mssql.Conn.ServerNow(ctx)` runs `SELECT SYSDATETIME()` on the **monitoring
  pool** (never the pinned execution connection, so it is never blocked by the DDL in flight).
- **Wiring:** functional option `WithServerClock(ServerClock)` on the engine, consistent with the
  existing `WithX` options. **Lazy:** never queried when no manifest in the run declares a window.
- **Tests:** a fake `ServerClock` (like `ManualClock`) returns a controllable server time.

## Enforcement points

Two gates, both using the server clock.

### 1. Pre-claim gate (defer)

In `ProcessAll`, for each manifest name before claiming it: read the manifest's `window`
(a targeted parse of the top-level field, before any expansion). If `window != nil` and
`!window.Contains(serverNow)`:
- log `deferred: outside window 01:00–05:00 [Sat,Sun]`;
- **do not claim** — the file stays intact in `01.to_run`;
- increment `Summary.Deferred`;
- continue to the next manifest.

A deferred manifest never enters `02.processing`. It is retried on the next run (when the
external scheduler has opened the window).

### 2. Op-boundary stop (finish current, then stop)

In the `processOne` loop, at the existing graceful-stop check:

```go
if e.draining() || e.windowClosed(ctx, manifest.Window) {
    return e.finalizeDrained(ctx, name, rep, start, cursor, len(planned))
}
```

`windowClosed` returns true when the manifest has a window and `!Contains(serverNow)`. A
closed window mid-run takes the **exact drain path**: the current operation has already
finished (the check is at the loop top), the manifest stays in `02.processing` with its
resume cursor, and the next run inside the window resumes at the cursor. One `SYSDATETIME()`
read per op boundary (negligible next to monitoring).

The same `windowClosed` check is also evaluated **once at `processOne` entry, after loading
the manifest but before preflight.** This handles a windowed manifest already in
`02.processing` (stopped by a previous window close) that is resumed by a run which is itself
outside the window: it stops immediately via `finalizeDrained` without running preflight
pointlessly. The pre-claim gate (point 1) only sees files still in `01.to_run`; this entry
check is what gates resumed `02.processing` manifests.

## Interactions

- **Drain/cursor:** window-close stop reuses `finalizeDrained`, so the resume cursor is
  persisted and the next window resumes exactly where it stopped.
- **Frozen-materialized ordering (separate spec):** a stable op order keeps the plan
  fingerprint stable across nights, so `reconcileResumePlan` honors the cursor rather than
  discarding it. Window + frozen order + cursor together give clean multi-night processing of
  a large manifest with no rework.
- **Resume of a windowed manifest:** already-in-`processing` windowed manifests are gated by the
  `processOne`-entry check; if resumed outside the window they stop immediately (having done
  nothing, no preflight), which is safe. The pre-claim gate covers files still in `01.to_run`;
  the entry check covers resumed `02.processing` manifests.

## Error handling (conservative — never run DDL at an unknown time)

- **Server-clock read fails:** one retry, then conservative fallback. At the pre-claim gate →
  **defer**. Mid-run at an op boundary → **graceful stop** (`finalizeDrained`). Always logged.
  A persistent failure means the connection is dead (monitoring would fail too).
- **Invalid window:** rejected at manifest validation; fails fast, never executes.
- **Window declared but no `ServerClock` wired (misconfiguration):** the manifest **fails with a
  clear error** (`window requires a server clock`) — never a silent bypass. `main.go` always
  wires the clock in normal runs.

## Dry-run / --explain

- `--dry-run` is offline (`--assume-*`, no connection) → the window cannot be evaluated. Render
  the SQL as today **plus an annotation**: `window 01:00–05:00 [Sat,Sun] — enforced at runtime
  (not evaluated in offline dry-run)`. No silent omission.
- `--explain` adds a one-line window note to the plan header.

## Summary / reporting

- New `Summary.Deferred int` counter (deferred manifests).
- Deferred and window-stopped events appear in the run log with the window spec, so the log
  makes clear why a manifest did not run / stopped.

## File layout

- `internal/ddl/window.go` (+ `window_test.go`) — `Window`, `Contains`, validation. Pure.
- `internal/ddl/manifest.go` — add `Window *Window` field; wire validation.
- `internal/run/` — `ServerClock` interface, `windowClosed` helper, `WithServerClock` option,
  `Summary.Deferred`, pre-claim gate in `ProcessAll`, op-boundary check in `processOne`.
- `internal/mssql/` — `Conn.ServerNow(ctx)` = `SELECT SYSDATETIME()` on the monitoring pool.
- `cmd/sqlgopace/main.go` — wire `WithServerClock`; dry-run annotation; `--explain` line.
- Docs — `README.md` (`window` block), `docs/specs/SPECS.md` (semantics + defer/stop invariant).

## Testing

- **`Window.Contains`** (pure, table-driven): same-day, overnight, day-of-week membership
  (including overnight opening-day semantics), boundary inclusivity (start inclusive / end
  exclusive), and validation of invalid inputs.
- **Engine** (fake `ServerClock`, existing `drain_test.go` / `resume_test.go` harness):
  - defer: out-of-window manifest is not claimed, stays in `01.to_run`, `Summary.Deferred`
    increments; advancing the fake clock into the window runs it;
  - mid-run stop: window closes between ops → `finalizeDrained`, manifest in `02.processing`
    with cursor, resumes next window;
  - clock-read failure → conservative defer/stop;
  - no-window manifest unaffected (regression).

## Out of scope (YAGNI — revisit only if needed)

- Absolute date ranges (one-off `from`/`to` datetimes).
- Per-operation or per-group windows (per-file only).
- A "wait until the window opens" blocking mode (defer only; external scheduler opens it).
- An explicit timezone field (server-local only).
