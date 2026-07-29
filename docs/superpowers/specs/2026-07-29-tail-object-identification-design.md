# Tail-object identification for shrink

**Status:** design approved, pre-implementation
**Date:** 2026-07-29
**Related:** `2026-07-28-contended-object-capture-design.md`, `2026-07-28-pre-shrink-reorganize-design.md`, `specs/SHRINK.md`

## Problem

`DBCC SHRINKFILE` relocates the highest-numbered allocated pages toward the front of
the file, then truncates the freed tail. The object that owns the physical tail page is
therefore the binding constraint: it determines how far the file can shrink and what the
shrink contends on. When that object cannot be moved cheaply (a heap, a LOB allocation, a
page pinned by another session), the shrink stalls short of target — the "tail-pinned
LOB/heap" stall observed in production.

The tool already learns the culprit in one narrow case. Contended-object capture
(`internal/run/contended.go`) records objects a shrink held a `Sch-M` lock on **while
blocking another session**, writing `<manifest>.contended.yaml` for `plan --confirmed` to
promote in `DecidePreShrink`. But a shrink frequently stalls with **no blocking victim** —
data pinned at the file end, a `WAIT_AT_LOW_PRIORITY` timeout, a self-wait. That is the
`stall()` path in `chunkLoop`, and the lock-based capture never sees it.

`docs/last-page-on-file.sql` closes that hole: it walks backward from the last page of a
data file and returns the first object owning an allocated page — the true tail object —
without needing any lock contention to surface it.

## Non-goals

- **No `sys.dm_db_database_page_allocations`.** The set-based DMF has catastrophic
  performance on large databases. The backward page walk is deliberate.
- **No `DBCC PAGE`.** It requires `sysadmin`; `sys.dm_db_page_info` gives the same page
  header with only `VIEW DATABASE STATE`, which the tool already mandates. No fallback,
  no permission probe.
- **No top-N.** A single tail object per run, matching the query. Iterative by design:
  fix the tail object, the next run reveals the next one.
- **No log or tempdb coverage.** Log shrinks truncate to a VLF boundary and tempdb data
  files have no meaningful relocation tail; neither walk applies there.

## Behaviour

### Modes

Two triggers, one shared helper on `*ShrinkRunner`:

- **Reactive — always on.** Runs exactly at the `chunkLoop` give-up point (`stop=true`,
  "no further progress, work preserved") for a data file. One walk per stalled file. The
  cost is negligible — the shrink has already failed — so no flag gates it.
- **Proactive — flag.** Runs once at `chunkLoop` entry, **after** the TRUNCATEONLY pass
  (which removes the trailing free space and reveals the real last allocated page). Gated
  by a new manifest flag `identify_tail_object: true` on the shrink operation.

Both write the tail object into the manifest's `.contended.yaml` sidecar.

### Version and permission gate

`sys.dm_db_page_info` requires SQL Server 2019 (major version 15) or later. Before either
walk, gate on the **already-detected** `Target` major version:

- major ≥ 15 → run.
- major < 15 → emit a warning `ReactionEvent` (`Kind: "warn"`) and skip. The shrink itself
  is unaffected.

No further permission check: `VIEW DATABASE STATE` / `VIEW SERVER STATE` is already
required for monitoring.

## Design

### 1. The read — `internal/mssql/shrink.go`

```go
// TailObject is the object owning the physically-last allocated page of a data file:
// the object DBCC SHRINKFILE must relocate past, and the binding constraint on how far
// the file can shrink. PageFromEnd is how many pages from the file end that page sits
// (0 = the very last page).
type TailObject struct {
    ObjectID    int64
    Schema      string
    Table       string
    IndexID     int
    PageFromEnd int
}

// TailObject walks backward from the last page of fileID via sys.dm_db_page_info,
// returning the first page owned by a user object (object_id IS NOT NULL). It stops
// after maxPagesBack pages without a hit, returning found=false. SQL 2019+ only — the
// caller gates on version before calling.
func (c *Conn) TailObject(ctx context.Context, fileID, maxPagesBack int) (TailObject, bool, error)
```

The T-SQL is the productized `last-page-on-file.sql`: parameterized by `@file_id`, reads
`size` for that file from `sys.database_files`, walks `@page_id` down from `size - 1`,
stops at the first non-null `object_id` or when it has scanned `@max_back` pages. It
resolves `OBJECT_SCHEMA_NAME(object_id)`, `OBJECT_NAME(object_id)`, and `index_id`.

**Walk cap.** `maxPagesBack` bounds the backward loop. Default constant
`defaultTailWalkPages = 262144` (≈ 2 GB of trailing scan at 8 KB/page). In practice the
last allocated page sits near the file end — an immovable tail page is *why* the shrink
stalled — so the walk is short; the cap is a runaway backstop. When the cap is reached
with no allocated page found, the driver logs a warning and records nothing (there is no
confirmed culprit to record).

**file_id plumbing.** `FileSpace` gains `FileID int`, populated by one extra column in
`fileSpaceSQL`. The shrink driver already holds the `FileSpace` for the file it is
shrinking, so it passes `f.FileID` directly — no name→id lookup.

### 2. The driver — `internal/run/shrink.go`

`ShrinkReader` gains:

```go
TailObject(ctx context.Context, fileID, maxPagesBack int) (mssql.TailObject, bool, error)
```

A shared helper records the tail object into the capture accumulator and flushes the
sidecar:

```go
func (r *ShrinkRunner) captureTailObject(ctx context.Context, f mssql.FileSpace,
    acc *contendedCapture, name, database string, sink ReactionSink)
```

It is a no-op when the target major version is < 15 (warn once) or when the walk finds
nothing. Call sites:

- `chunkLoop` give-up (both no-gain and DBCC-error stall paths that return `stop=true`):
  reactive capture. `chunkLoop` is shared with `RunTempdb` (`prof != nil`), so both
  captures are gated on `prof == nil` — a tempdb shrink never walks (non-goal).
- `chunkLoop` entry, after TRUNCATEONLY, when `identify_tail_object` is set: proactive
  capture.

The runner needs the detected major version and the `identify_tail_object` flag; both are
threaded through `ShrinkRunnerConfig` / the op, alongside the existing wiring. The
accumulator (`*contendedCapture`) is already created per shrink for the lock capture; the
tail capture reuses it so both kinds land in one sidecar.

### 3. The sidecar — `internal/run/contended.go` + `internal/maint/contended.go`

`contendedCapture` gains a parallel `addTail(o mssql.TailObject, now string)` that records
a position-confirmed entry (keyed by object_id, deduped like the lock path).

`maint.ContendedObject` gains three fields, all `omitempty` so existing sidecars still
decode under `KnownFields(true)`:

```go
IndexID     int    `yaml:"index_id,omitempty"`
ConfirmedBy string `yaml:"confirmed_by,omitempty"` // "" or "lock" = lock-held; "tail_position" = tail walk
PageFromEnd int    `yaml:"page_from_end,omitempty"`
```

Lock-captured entries keep `confirmed_by` empty (back-compatible; read as "lock"). Tail
entries set `confirmed_by: tail_position`, `times_blocked`/`lock_mode`/`first_seen`/
`last_seen` left at their zero values (a tail object was never *blocked* — it was
*positioned*).

The header comment on the sidecar is extended to explain the two confirmation kinds.

### 4. Consumption — `internal/maint/shrink.go` + `cmd/sqlgopace/shrink_plan.go`

The `confirmed` map that `DecidePreShrink` and `confirmedSetFor` pass around changes from
`map[int64]int` to `map[int64]maint.Confirmation`:

```go
// Confirmation is how an object_id was confirmed as a shrink blocker, from a
// .contended.yaml sidecar. ByTail entries (position-confirmed) are the definitive
// blocker and sort ahead of lock-confirmed ones.
type Confirmation struct {
    TimesBlocked int  // lock captures; 0 for a tail-position entry
    ByTail       bool // confirmed_by == "tail_position"
}
```

`confirmedSetFor` builds it from `ConfirmedBy`/`TimesBlocked`. `DecidePreShrink`:

- still promotes every confirmed object to the head group ahead of density-only
  candidates (unchanged);
- annotates by kind: `ByTail` → `"confirmed tail-position blocker"`; otherwise the
  existing `"confirmed blocker (times_blocked=%d)"`;
- sorts the head group `ByTail` first, then by `TimesBlocked` desc (stable). A tail object
  is the object the shrink literally could not pass, so it leads.

Heap advisories: a `ByTail` confirmation marks a matching heap `CONFIRMED` the same way a
lock confirmation does today.

This is the one signature ripple; it is contained to `internal/maint`, `internal/plan`
(`AnalyzePreShrink`), and `cmd/sqlgopace/shrink_plan.go`, plus their tests.

### 5. The manifest flag — `internal/ddl`

`identify_tail_object bool` is added to the shrink operation's options (`ddl.Shrink`,
parsed in the manifest parser). The `plan` shrink profile carries it through
`shrinkManifest` when the profile enables it, mirroring `max_block_minutes`.

## Testing

- **`internal/mssql`** — the T-SQL is DB-touching, so behavioural coverage is
  integration-tagged (`SQLGOPACE_TEST_DSN`, a real 2019+ server): a table with a known
  object at the file tail returns that object; an empty/all-free file returns
  `found=false`. Any separable row-scan/parse logic is unit-tested.
- **`internal/run`** — a fake `ShrinkReader` implements `TailObject`. Unit tests assert:
  the reactive walk fires once at give-up; the proactive walk fires once at entry only
  when `identify_tail_object` is set; both are skipped for a log shrink and for major < 15
  (with a warning event); a found tail object produces a `tail_position` sidecar entry;
  a not-found walk records nothing.
- **`internal/maint`** — `DecidePreShrink` table tests gain `ByTail` cases: promotion
  ahead of density candidates, tail-first ordering within the confirmed group, and the
  tail-position annotation. `ParseContended` round-trips the new fields and still accepts
  a legacy sidecar.

## Rollout

`identify_tail_object` defaults to off; the reactive walk is on but only fires after an
already-failed shrink, so unflagged manifests behave as today plus a one-line warning
(<2019) or a richer `.contended.yaml` (≥2019) on a stall. No manifest migration needed.
Version bump in `internal/version/VERSION` on implementation.
