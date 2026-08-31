# Pre-shrink reorganize + heap advisory in the `plan` subcommand

Status: design approved, pending implementation plan.
Date: 2026-07-28.

## Motivation

A data shrink of a live database can stall short of its target: `DBCC SHRINKFILE`
makes no forward progress, or errors on a page it cannot relocate, and the engine
finalizes the manifest as `INCOMPLETE`/`FAILED` before reaching `targetfreespace`.
(The "could not adjust the space allocation" text seen on the PRODDB runs is *not*
Msg 5240 — per Microsoft's message catalog 5240 is "file … is either being shrunk by
another process or is empty". The driver's error matcher mislabels this; tracked
separately from this plan.) Two causes, seen on a real PRODDB campaign:

1. **Concurrency** — the shrink needs a `Sch-M` lock on IAM pages, contending with
   the workload's `Sch-S` locks; under `WAIT_AT_LOW_PRIORITY` it times out after a
   minute and yields, and the driver counts that as no-progress.
2. **Low page density** — after large deletes, allocated pages are spread across the
   file, so the shrink has more pages to relocate than the used-space total implies.

Microsoft's own guidance ("Reorganize indexes before shrink") is to compact the
low-density objects first: `ALTER INDEX ... REORGANIZE` is online and, unlike
`REBUILD`, does not grow the file. This design teaches the `plan` subcommand to emit
that `reorganize → shrink` sequence for low-density rowstore indexes, and to surface
the objects reorganize cannot fix in-row — heaps — as an advisory.

Cause (1) is not addressed here; it is a scheduling concern (run in a low-write
window). This feature targets cause (2) and makes the prep reproducible.

## Scope

- The `plan` subcommand and `internal/maint` currently have **no** concept of shrink.
  This is the first shrink support in the planner.
- Applies to the **connected database** only. Multi-database shrink is a non-goal.
- The `shrink` operation, the `reorganize_index`/`rebuild_index`/`rebuild_heap`
  operations, and the shrink driver already exist and are unchanged. This design only
  adds planner-side generation and an advisory.

## 1. Profile — new `shrink:` section

Added to `maintenance_profile.yaml`, parsed by `internal/maint/profile.go` into a
`ShrinkRules` struct (strict decode, unknown fields rejected, like the other rule
sections):

```yaml
shrink:
  enabled: true                        # off/absent = no shrink manifest (today's behavior)
  type: data                           # data | log
  files: all                           # all | <logical file name>
  targetfreespace: 10%                 # percent or absolute MB — parsed by ddl.ParseTargetFreeSpace
  pre_reorganize: true                 # off = shrink op only, no leading reorganizes
  reorganize_below_density_percent: 65 # reorganize rowstore indexes whose SAMPLED page
                                       #   density is below this (half-empty pages from deletes)
  max_block_minutes: 10                # optional; carried into the shrink op's options
```

Validation:
- `enabled: false` or the section absent → the feature is inert (current behavior).
- `type` ∈ {`data`, `log`} (case-insensitive).
- `files` non-empty (`all` or a logical file name; not resolved at plan time).
- `targetfreespace` must parse via `ddl.ParseTargetFreeSpace` (reuses the shrink op's
  parser, so percent/absolute-MB rules are identical).
- `reorganize_below_density_percent` ∈ 1..100; default 65 when `pre_reorganize` is on.
- `max_block_minutes` ≥ 0 (0/absent = omit the option).

The index page-count floor reuses the existing `index.page_count_floor` (default 1000
pages) — tiny indexes are skipped, no new knob.

Defaults: when `enabled` and `pre_reorganize` is omitted, `pre_reorganize` is `true`.

`pre_reorganize` and the heap advisory (§3) apply only to `type: data` — a log shrink
reclaims VLFs and has nothing to do with page density or heaps. For `type: log`,
`pre_reorganize` is ignored (no reorganizes, no advisory) and the manifest carries the
shrink op alone.

Session policy (`ignore_blocked_sessions` / `kill_blocking_sessions`) is **not**
generated — it is situational. The operator adds it by editing the generated manifest.
Only `max_block_minutes` is carried through.

## 2. Plan flow — one dedicated, self-contained shrink manifest

When `shrink.enabled`, `plan` writes a single manifest
`NNN_shrink_<db>_<type>.yaml` into `01.to_run`, alongside (and independent of) the
normal maintenance manifests:

- **`pre_reorganize: true`** — select by **page density**: for each rowstore index
  (`index_id ≥ 1`) in the connected DB, read SAMPLED page density and emit a
  `reorganize_index` for those with `page_count ≥ index.page_count_floor` **and**
  `avg_page_space_used_in_percent < reorganize_below_density_percent`, sequenced first;
  then the `shrink` operation. The pass **never emits REBUILD** — a rebuild grows the
  file (`docs/specs/SHRINK.md` §400) — so there is no downgrade step; it generates
  `reorganize_index` directly. Low-density **heaps** cannot be compacted in-row by
  reorganize, so they go to the advisory (§3), not this list.
- **`pre_reorganize: false`** — emit only the `shrink` operation.

Notes:
- **Signal: page density, not fragmentation.** What most helps a shrink is compacting
  the half-empty pages large deletes leave behind — measured by
  `avg_page_space_used_in_percent`, not logical fragmentation. Microsoft's shrink
  playbook uses the same trigger (density < 60–70%, SAMPLED). Fragmentation-based
  selection would miss low-fragmentation/low-density tables, which are exactly the
  post-delete case a shrink cleans up.
- **Read: reuse `PhysicalStats` in SAMPLED mode — no new SQL, but a real scan.** The
  density column and SAMPLED mode already exist in `internal/mssql/analysis.go`
  (`avg_page_space_used_in_percent`, `PhysicalSampled`); the normal maintenance pass
  reads indexes in the cheaper `LIMITED` mode (density returns 0). The pre-shrink pass
  therefore adds a **SAMPLED scan of the target DB's indexes at plan time** — heavier
  than LIMITED, bounded (samples ~1% of pages), run once when `shrink.enabled`. Heap
  density is *already* gathered by the existing heap read (`PageSpaceUsedPercent`), so
  the advisory (§3) pays nothing extra.
- **Decision stays in the pure core.** The density → `reorganize_index` selection is a
  new pure function in `internal/maint` (unit-testable, no DB); the SAMPLED read lives
  in `internal/mssql`, wired in `plan.go`, mirroring how the existing measurements are
  gathered. `decide.go`'s fragmentation/heap logic is untouched.
- The manifest is **self-contained**. Its density-selected reorganizes may partially
  overlap the normal index-maintenance pass's fragmentation-selected reorganizes (a
  table can be both). Running `REORGANIZE` twice is safe (idempotent), and a *later*
  plan run re-measures density and emits nothing for the now-compacted indexes; a
  re-run of the same snapshot re-issues them (a bounded re-scan). The overlap is
  accepted rather than coupling the two passes.
- Cross-manifest run order is not correctness-critical: the reorganizes that must
  precede the shrink are inside this same manifest, executed sequentially.

Generated shape (illustrative):

```yaml
description: "Pre-shrink reorganize + reclaim: PRODDB data"
database: PRODDB
operations:
  - operation: reorganize_index      # one op per low-density index (not ALL)
    schema: dbo
    table: MEASUREMENT
    index: PK_MEASUREMENT
  # ... more reorganizes (density-selected: page density < reorganize_below_density_percent) ...
  - operation: shrink
    type: data
    files: all
    targetfreespace: 10%
    options:
      max_block_minutes: 10
```

## 3. Heap advisory (the diagnostic)

Heaps are the blind spot — precisely, not absolutely. `ALTER INDEX ALL … REORGANIZE`
*does* run on a heap and compacts its LOB allocation unit (via the underlying-table
path), but it cannot touch the heap's *in-row* structure or forwarded records — the
part a shrink needs relocated. So with no LOB (this DB's case) reorganize does nothing
for a heap, and even with LOB it leaves the in-row data in place. A heap near the tail
therefore survives the reorganize pass; the lever is `rebuild_heap` (offline on
Standard), or, as the durable fix, giving the table a clustered index. When
`shrink.enabled`, `plan` prints an advisory and writes a `.heaps.yaml` sidecar next to
the shrink manifest (mirroring the `.blocked.yaml` advisory convention — advisory only,
SqlGoPace never reads it back):

- Reuses the heap measurements the planner already gathers (`HeapMeasurement`, which
  already carries `PageSpaceUsedPercent` from a SAMPLED read); **no new read** — the
  heap advisory is free.
- Lists heaps in the connected DB above `heap.min_size_mb` whose density is below
  `reorganize_below_density_percent` (the actionable ones — a dense heap won't help the
  shrink), with size, forwarded-record %, and page density.
- **Candidates, not confirmed blockers.** The advisory identifies heaps by *property*
  (size/density), not by *position* in the file. Only a heap in the file's tail extents
  actually impedes the shrink; a large heap in the middle may not. Confirming tail
  position needs `sys.dm_db_database_page_allocations`, which is heavy at database
  scope (deliberately avoided — see the metadata-only rationale in the campaign notes).
  A **deferred enhancement** is a *scoped* `page_allocations(DB_ID(), @object_id, …)`
  on the top few candidates only (cheap because single-object) to flag which actually
  sit in the tail. This iteration ships the property-based list and states this limit
  in the advisory text so operators do not chase the wrong heap.
- States that reorganize cannot help them and points to `rebuild_heap` in a
  maintenance window (offline/blocking on Standard edition).
- **Advisory only** — heaps are never auto-added to the generated manifest.

Accepted consequence: if a heap is the actual tail blocker, the shrink will stay short
of target on every run (finalizing `INCOMPLETE`, work preserved) until an operator
rebuilds it in a window. The pipeline stays safe, but closing the loop relies on
someone reading the advisory. A future enhancement could echo the advisory into the
shrink's `INCOMPLETE` run report so it surfaces at the point of failure; out of scope
here.

## 4. rebuild_heap — already implemented

The `rebuild_heap` operation exists end-to-end: parsed (`manifest.go`), the
`RebuildHeap` struct, generated by `generateRebuildHeap` (`generate.go`), and emitted
by the planner's `DecideHeap` (`internal/maint/decide.go`), with `RESUMABLE`/
`WAIT_AT_LOW_PRIORITY` correctly disallowed for a heap rebuild. No work required. The
advisory in §3 points operators at it for the objects reorganize cannot fix.

## Testing

All decision logic lands in the pure `maint` core (no database):

- `ShrinkRules` parse + validate (valid, each rejection path, defaults incl.
  `reorganize_below_density_percent` = 65 and its 1..100 range).
- Density selection: an index at/above the density threshold is skipped; below it and
  at/above `index.page_count_floor` is emitted; a heap (`index_id 0`) is **never**
  emitted as a reorganize regardless of density (it routes to the advisory).
- Manifest assembly: reorganizes precede the shrink; one op per low-density index (not
  `ALL`); `pre_reorganize: false` yields the shrink op alone; `type: log` yields the
  shrink op alone; `max_block_minutes` carried through / omitted when 0.
- Heap advisory selection (filter by `heap.min_size_mb` and density threshold; empty
  when no qualifying heaps).

## Non-goals

- Multi-database shrink from `plan`.
- Addressing shrink stalls caused by concurrency (a scheduling concern).
- Auto-generating session-policy rules or `rebuild_heap` ops.
- Any change to the shrink driver, the shrink operation, or the reorganize/rebuild
  operations themselves.
```
