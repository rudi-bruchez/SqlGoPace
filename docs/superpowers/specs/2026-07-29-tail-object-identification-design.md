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
  cost is negligible — the shrink has already failed — so no flag gates it. Note this walk
  runs *after* the shrink has released its own locks, so allocation can shift slightly in
  that window: the reported page is the file's *current* last allocated page — a strong
  proxy for the stall blocker, not a guaranteed-identical snapshot of it. That is
  acceptable for a diagnostic that feeds the *next* run's plan.
- **Proactive — flag.** Runs once at `chunkLoop` entry, **after** the TRUNCATEONLY pass
  (which removes the trailing free space and reveals the real last allocated page). Gated
  by a new manifest flag `identify_tail_object: true` on the shrink operation. This is the
  one walk that runs on a *healthy* shrink, so its latency matters: it is bounded by the
  post-TRUNCATEONLY free-page count (§1), which is small once the trailing free band is
  gone, and hard-capped by `tailWalkAbsCap`.

On a **found** object, the driver emits an informational `ReactionEvent` naming the tail
object (schema.table, index_id, pages-from-end) so the operator sees the culprit live in
the console/`.log`, not only indirectly via the sidecar. Both modes then write the object
into the manifest's `.contended.yaml` sidecar.

### Version and permission gate

`sys.dm_db_page_info` requires SQL Server 2019 (major version 15) or later. Before either
walk, gate on the **already-detected** `Target` major version:

- major ≥ 15 → run.
- major < 15 → emit a warning `ReactionEvent` (`Kind: "warn"`) and skip. The shrink itself
  is unaffected. The warning fires **once per shrink operation** (one manifest run), not
  per file or per call site — guarded by a `warnedNoPageInfo bool` on the runner, like the
  tempdb `flushed` guard — so a multi-file shrink on SQL 2017 logs one line, not N.

No further permission check. `sys.dm_db_page_info` requires `VIEW DATABASE STATE` (on SQL
2022+ the renamed `VIEW DATABASE PERFORMANCE STATE`); both are implied by the server-level
`VIEW SERVER STATE` the tool already mandates for monitoring.

### Cost and concurrency

The walk takes **no transaction locks**. Each `sys.dm_db_page_info(..., 'LIMITED')` call
takes only a brief buffer latch to read the page header and releases it immediately —
latches are held for the physical read, not for a transaction — so the scan cannot block
other sessions the way a lock would, at worst contending momentarily on a page latch on a
very busy file. Each iteration is one such call (a buffer hit, or a single-page physical
read if the page is cold). The walk is expected to be short — the last allocated page sits
near the file end (in reactive mode that immovable tail page is *why* the shrink stalled;
in proactive mode TRUNCATEONLY has already removed the trailing free band) — so typical
cost is a handful of latched header reads. The free-space-derived cap (§1) bounds the
worst case; hitting it warns and records nothing rather than grinding indefinitely.

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

// FindTailObject walks backward from the last page of fileID via sys.dm_db_page_info,
// returning the first page owned by a user object (object_id IS NOT NULL). It stops
// after maxPagesBack pages without a hit, returning found=false. SQL 2019+ only — the
// caller gates on version before calling. (Named FindTailObject, not TailObject, to
// disambiguate from the TailObject result type at call sites.)
func (c *Conn) FindTailObject(ctx context.Context, fileID, maxPagesBack int) (TailObject, bool, error)
```

The T-SQL is the productized `last-page-on-file.sql`: parameterized by `@file_id`, reads
`size` for that file from `sys.database_files`, walks `@page_id` down from `size - 1`,
stops at the first non-null `object_id` or when it has scanned `@max_back` pages. It
resolves `OBJECT_SCHEMA_NAME(object_id)`, `OBJECT_NAME(object_id)`, and `index_id`.

**Concurrency edge cases.** The `size` read and the walk run in **one batch**, so the
file size is consistent for the walk — no cross-statement race. Concurrent *growth* only
adds pages above the stale start point (ignored; a brand-new allocation is not the stall
blocker). The file cannot shrink underneath the walk: SqlGoPace is the only shrinker and
runs one file at a time. If the tail object is **dropped between the walk and name
resolution**, `OBJECT_NAME`/`OBJECT_SCHEMA_NAME` return `NULL`; the row is still recorded
with its `object_id` and empty schema/table (best-effort) — a dropped object simply
no-ops in the next `DecidePreShrink` (nothing to reorganize), so it is harmless, not an
error.

**Walk cap.** `maxPagesBack` bounds the backward loop. It is **derived from free space**,
not a fixed constant: the number of trailing unallocated pages the walk must skip before
it reaches the last allocated page can never exceed the file's total free-page count (a
trailing unallocated run is a subset of all unallocated pages). So the driver passes

```
maxPagesBack = min(f.FreeMB*128 + tailWalkMargin, tailWalkAbsCap)
```

where `f.FreeMB*128` is the file's free pages (8 KB/page), `tailWalkMargin` (a few hundred
pages) absorbs concurrent allocation churn, and `tailWalkAbsCap` (const, ≈ 262 144 pages /
2 GB) is an absolute backstop for the pathological case — a mostly-free file whose last
allocated page sits near the *front*, where the free-page bound is large. In proactive
mode the free-page count is read **after** TRUNCATEONLY, so the trailing free band is
already gone and the bound is tight. When the walk reaches its cap with no allocated page
found, the driver logs a warning and records nothing (no confirmed culprit to record).

Pages with no resolvable user object — free/unallocated pages and allocation-bitmap pages
(`object_id IS NULL`) — are skipped, exactly as the source query does; the first page
owning a user object is the tail object.

**file_id plumbing.** `FileSpace` gains `FileID int`, populated by one extra column in
`fileSpaceSQL`. The shrink driver already holds the `FileSpace` for the file it is
shrinking, so it passes `f.FileID` directly — no name→id lookup.

### 2. The driver — `internal/run/shrink.go`

The capture must follow the **existing lock-capture wiring**: the lock capture is *not*
done inside `ShrinkRunner`; the runner emits `ReactionEvent`s through the per-operation
`sink`, and the engine's sink closure (`engine.go`, which owns the `*contendedCapture`
accumulator and writes the sidecar) does the recording. The tail capture uses the same
channel.

`ShrinkReader` gains:

```go
FindTailObject(ctx context.Context, fileID, maxPagesBack int) (mssql.TailObject, bool, error)
```

`ReactionEvent` (`internal/run/reaction.go`) gains one optional field carrying a found
tail object, so the engine sink can recognise and record it without coupling the event to
`internal/mssql`:

```go
type TailFinding struct {
    ObjectID     int64
    Schema, Table string
    IndexID      int
    PageFromEnd  int
}
// in ReactionEvent:
Tail *TailFinding // non-nil only on a "tail object found" info event
```

The runner does the **read** (it holds the `ShrinkReader`, the file, and knows the
give-up/entry moments); the engine does the **record**. A runner helper:

```go
func (r *ShrinkRunner) maybeCaptureTail(ctx context.Context, f mssql.FileSpace,
    sink ReactionSink, warned *bool)
```

gates on the detected major version (`r.major`, new `ShrinkRunnerConfig.SQLMajorVersion`);
on < 15 it emits one `Kind:"warn"` event via the `warned *bool` guard and returns; on ≥ 15
it computes `maxPagesBack` from `f.FreeMB` (§1), calls `FindTailObject`, and on a hit emits
`ReactionEvent{Kind:"info", Detail:"tail object …", Tail:&TailFinding{…}}`. Not-found /
read-error records nothing.

Call sites, all gated on a normal data shrink (`prof == nil` — `chunkLoop` is shared with
`RunTempdb`, whose tempdb shrink never walks):

- **Reactive** — at each `chunkLoop` give-up (the two `stop=true` returns from `stall`,
  both no-gain and DBCC-error), call `maybeCaptureTail`.
- **Proactive** — at `chunkLoop` entry, before the loop, when `identify_tail_object` is
  set on the op.

`chunkLoop`/`shrinkData` gain a `*tailProbe{proactive bool; warned *bool}` param (nil on
the tempdb path). The `warned` guard is created once per operation in `Run`, so a
multi-file data shrink on < 2019 warns once. The engine sink gains one branch: on
`ev.Tail != nil`, call a new `Engine.captureTail(contended, name, database, *ev.Tail)`
that records into the same accumulator and writes the sidecar.

### 3. The sidecar — `internal/run/contended.go` + `internal/maint/contended.go`

`contendedCapture` gains a parallel `addTail(f TailFinding, now string)`, keyed by
object_id like the lock path (and `capturedObject` gains `byTail bool`, `indexID int`,
`pageFromEnd int`). `Engine.captureContended` and the new `Engine.captureTail` share an
extracted `writeContended(name, database, acc)` sidecar-write helper. **Merge semantics**
when the key already exists — i.e. the same object was lock-captured mid-run and then
tail-captured at give-up:

- The entry is **upgraded** to `confirmed_by: tail_position` and its `index_id` /
  `page_from_end` are filled from the walk.
- Its lock stats (`times_blocked`, `lock_mode`, `first_seen`, `last_seen`) are
  **preserved**, not cleared — the object was both blocked *and* the positional tail, and
  `Confirmation` (§4) carries both facets.

A fresh key creates a tail-only entry (lock stats at their zero values — a tail object was
never *blocked*, it was *positioned*).

`maint.ContendedObject` gains three fields, all `omitempty` so existing sidecars still
decode under `KnownFields(true)`:

```go
IndexID     int    `yaml:"index_id,omitempty"`
ConfirmedBy string `yaml:"confirmed_by,omitempty"` // "lock" = lock-held; "tail_position" = tail walk; "" = legacy (read as lock)
PageFromEnd int    `yaml:"page_from_end,omitempty"`
```

New lock-captured entries write `confirmed_by: lock` **explicitly**; an empty value is only
ever a legacy sidecar written before this change and is read as `lock`. Tail entries write
`confirmed_by: tail_position`. (Writing the value explicitly resolves the earlier
ambiguity where nothing emitted `"lock"`.)

**Empty sidecar.** The tail capture never creates or rewrites the sidecar on a not-found
walk — it writes only when it recorded an object, mirroring the lock capture's existing
"write only when this snapshot captured something" rule. A run whose only capture
opportunity was a tail walk that found nothing leaves no `.contended.yaml`, so consumers
that stat the file see the same "nothing captured" as today.

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
    IndexID      int  // tail entries; the allocation unit at the tail
    PageFromEnd  int  // tail entries; distance from file end (smaller = more binding)
}
```

`confirmedSetFor` builds it from `ConfirmedBy`/`TimesBlocked`/`IndexID`/`PageFromEnd`.
`DecidePreShrink`:

- still promotes every confirmed object to the head group ahead of density-only
  candidates (unchanged);
- annotates by kind: `ByTail` → `"confirmed tail-position blocker (index_id=%d, %d pages
  from end)"`; otherwise the existing `"confirmed blocker (times_blocked=%d)"`;
- sorts the head group `ByTail` first; **ties among tail entries** (both `TimesBlocked` 0)
  break on `PageFromEnd` ascending — the page closest to the file end is the most binding
  — then original order (stable). Lock-only entries follow, by `TimesBlocked` desc. A tail
  object is the object the shrink literally could not pass, so it leads.

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
- **`internal/run`** — a fake `ShrinkReader` implements `FindTailObject`. Unit tests
  assert: the reactive walk fires once at give-up; the proactive walk fires once at entry
  only when `identify_tail_object` is set; both are skipped for a log shrink, for a tempdb
  shrink (`prof != nil`), and for major < 15 (one warning event per operation); a found
  tail object produces a `tail_position` sidecar entry and an informational event; a
  not-found walk records nothing and leaves no sidecar; a lock-then-tail capture of the
  same object_id merges (upgraded to `tail_position`, lock stats preserved).
- **`internal/maint`** — `DecidePreShrink` table tests gain `ByTail` cases: promotion
  ahead of density candidates, tail-first ordering within the confirmed group, and the
  tail-position annotation. `ParseContended` round-trips the new fields and still accepts
  a legacy sidecar.

## Rollout

`identify_tail_object` defaults to off; the reactive walk is on but only fires after an
already-failed shrink, so unflagged manifests behave as today plus a one-line warning
(<2019) or a richer `.contended.yaml` (≥2019) on a stall. No manifest migration needed.
Version bump in `internal/version/VERSION` on implementation.
