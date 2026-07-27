# TEMPDB-SHRINK — the `shrink_tempdb` operation

> **DRAFT** — source of truth for the intended behavior of the tempdb shrink operation.
> Nothing is implemented yet. Companion to `specs/TEMPDB-GUARD.md` (the inverse feature:
> watching tempdb pressure while our ops run). This one *shrinks* tempdb; that one *protects* it.

## Problem

Sometimes infra grows the physical drive for tempdb. But tempdb files can balloon to hundreds
of GB while staying mostly empty at the end, and shrinking them live is painful: transient
internal structures (work tables, sort/hash spills, version store) pin pages at the file end and
refuse to move, and every data file must be shrunk to the same size or the round-robin allocation
skews. A restart rebuilds tempdb clean from its configured sizes — but that means downtime.

The observed incident (`staging/platform/2026.07.27.tempdb/`): on a SQL Server 2019 staging
instance, tempdb grew to 400 GB and filled the disk. A `DBCC SHRINKFILE` sat at 99.99 % for ~14
minutes, blocked behind a **live application query** spilling a large sort into tempdb. The shrink
also reported `Page X could not be moved because it is a work table page`, and the reference
material warns of error **845** (buffer-latch time-out) under contention.

## 1. Goal and non-goals

**Goal.** A *best-effort, live* shrink of tempdb's **data files**, bringing every data file down
to a common absolute target size, under SqlGoPace's usual monitoring and reaction hierarchy, with
a **clean, re-entrant give-up** when the target is not reachable (work preserved; a re-run resumes
from the current size).

**Side benefit (documented).** `DBCC SHRINKFILE` below a file's *created* size also updates the
initial (boot) size in `sys.master_files`. So this operation not only reclaims disk now, it also
corrects what tempdb will be recreated at on the next restart — a manual `ALTER DATABASE ...
MODIFY FILE (SIZE = ...)` bump made during an incident is undone.

**Non-goals** (deliberate, documented):

1. **Not a monitor.** SqlGoPace is a maintenance tool, not a monitoring/diagnostic tool. There is
   **no** continuous tempdb surveillance and **no** query-plan capture here. The only link to "the
   queries that fill tempdb" is a **by-product of reacting**: the driver must already know *who*
   blocks the shrink to choose its reaction, so the run report lists those blockers. That is
   incident reporting on the operation we run, not tempdb monitoring.
2. **Not a guaranteed shrink.** Internal objects held by live queries can pin space and prevent
   reaching the target. The tool does its best and **stops cleanly**, saying so plainly. Bringing
   tempdb from 400 GB to 20 GB live is often impossible without a restart — that is expected.
3. **Data files only.** The tempdb log is out of scope here (a simpler, separate case).

## 2. Manifest surface

`shrink_tempdb` is a **dedicated operation** (parse → resolve → generate → plan), per the
project convention "one capability = one operation type end-to-end". It gets its own `--explain`
decisions. There is **no `database:` field** — the operation *is* tempdb (database_id 2).

```yaml
operation:
  shrink_tempdb:
    targetsizemb: 20480    # every data file is shrunk to 20 GB
    flushcaches: false     # opt-in for the cache-flush escalation (§5)
```

- `targetsizemb` (required, > 0): the common absolute target size, in MB, applied to **every**
  tempdb data file.
- `flushcaches` (optional, default `false`): opt-in for the instance-wide cache flush used as a
  stall escalation (§5). Off by default because it hurts instance performance.

## 3. Pure core — `internal/ddl`

- `ddl.ShrinkTempdb{ TargetSizeMB int; FlushCaches bool }`:
  - `Validate()`: `TargetSizeMB > 0`.
  - `CommandType()`: `"shrink_tempdb"` — feeds the compatibility matrix so WALP eligibility is
    version/edition-gated exactly like the other shrink command types.
  - `Target()`: `func (o ShrinkTempdb) Target() ObjectRef { return ObjectRef{Database: "tempdb"} }`
    — the `check_db` convention (database-scoped, **not** `schema.table`; do not abuse
    `ObjectRef.Table`). Empty `Schema`/`Table` is what makes preflight skip the existence check
    (fixed in 028602a), so this shape is load-bearing, not cosmetic.
- `resolve.go`: a dedicated branch, modelled on the existing shrink branch — resolve **only**
  `wait_at_low_priority` via the matrix; **do not** touch online/resumable/sort_in_tempdb and do
  not apply the "WALP requires ONLINE" rule. `ABORT_AFTER_WAIT = SELF` **always** (never
  `BLOCKERS`: consistent with "never kill a blocker", §4). On SQL Server 2019 the matrix disables
  WALP for this command type → the reaction degrades to plain waiting.
- `generate.go`: statements are built **at run time**, per file (like shrink — the only ops whose
  SQL is generated at run time, not up front): `USE tempdb; DBCC SHRINKFILE (<file>, <target>)
  [WITH WAIT_AT_LOW_PRIORITY (ABORT_AFTER_WAIT = SELF)]`.

## 4. Driver — `internal/run` (specialization, no duplication)

No new runner. The `*ShrinkRunner` is reused; the tempdb path is a **new orchestrator method on
it**, not a fork of the chunk primitives.

**Wiring the runner (the interface change).** `ShrinkDriver` currently exposes one method typed to
`ddl.Shrink`:

```go
type ShrinkDriver interface {
    Run(ctx, op ddl.Shrink, res ddl.ResolvedOptions, ignore IgnoreSource, sink ReactionSink) ([]ShrinkResult, error)
    RunTempdb(ctx, op ddl.ShrinkTempdb, res ddl.ResolvedOptions, ignore IgnoreSource, sink ReactionSink) ([]ShrinkResult, error) // new
}
```

- A **second method** `RunTempdb` is added (type-safe; no `any` union, no translating
  `ShrinkTempdb` into a sentinel `ddl.Shrink`). `*ShrinkRunner` implements both.
- `processOne` gains `case ddl.ShrinkTempdb:` next to the existing `case ddl.Shrink:`, calling
  `RunTempdb`.
- The existing **stop-short → `incomplete`** detection keys on `ddl.Shrink`; it must cover both op
  types. Extract a small predicate `isShrinkOp(step.Operation)` (true for `ddl.Shrink` **and**
  `ddl.ShrinkTempdb`) and use it there. Both paths already return `[]ShrinkResult`, so
  `shrinkStoppedShort` is unchanged.

**No duplication, made verifiable.** The per-file page-moving loop currently lives inside
`shrinkData`. Extract it into a shared primitive (e.g. `chunkLoop(ctx, f, final, res, ignore, sink,
profile)`) that both `shrinkData` (data/log path) and `runTempdb` call. `runTempdb` then reuses the
existing `runTruncateOnly` and this extracted `chunkLoop`; it adds **orchestration**, not copies of
the loop. `RunTempdb` builds a `TempdbProfile{TargetSizeMB, FlushCaches, NoProgressBeforeFlush}`
from the op and threads it into `chunkLoop`/`stall`, where the tempdb-specific escalation (§5)
reads it.

- **Per-file target**: `target = max(targetsizemb, file.UsedMB)`. A file whose used space already
  exceeds the common target stops at its used floor (clamp), and the result records why.
- **File resolution**: all tempdb data files (`FileTypeRows`), shrunk **sequentially** (never two
  of the single tempdb filegroup in parallel — the existing sequential loop guarantees it).
- **Two-phase order** — this is why `runTempdb` is a separate orchestrator and not a branch inside
  the per-file `shrinkData` (which interleaves truncate+chunk per file):
  - **Phase 0 — `TRUNCATEONLY` on *all* files first** (calling `runTruncateOnly` per file), before
    any page movement. Releasing every file's empty tail up front returns space to the OS
    immediately and can shift internal allocations, easing the subsequent page moves.
  - **Phase 1 — per-file chunk loop across all files** (calling the extracted `chunkLoop`),
    sequentially.
- **Log sampling**: tempdb is always SIMPLE recovery, so the FULL/BULK_LOGGED log-reuse wait path
  (`awaitLogReuse`, `shrinkLog`) is **never** taken here — data-only, no `BACKUP LOG` gate. Each
  chunk still writes tempdb transaction log, but SIMPLE self-truncates on checkpoint, so the log
  dimension of the sampler stays quiet; the reaction that matters for tempdb is blocking, not log
  drain.
- **Live blockers**: reuse the existing `awaitRelief` → bounded wait → clean give-up at timeout.
  **Never KILL a blocker** (they are legitimate application queries). WALP, when available (2022+),
  only makes *our* chunk yield (`ABORT_AFTER_WAIT = SELF`); it never aborts blockers.

## 5. Unified no-progress escalation (bricks: work-table retry, cache flush, 845)

One "no-progress" path, triggered by **any** of: a no-gain chunk, `Msg 5240` (work-table page
could not be moved), or **error 845** (buffer-latch time-out). None of these is a hard run failure.

```
no-gain chunk / Msg 5240 / 845
  → noProgress++, back off (doubling)                 # internal objects often clear on their own
  → if noProgress ≥ NoProgressBeforeFlush AND flushcaches AND not yet flushed this run:
        CHECKPOINT;
        DBCC FREESYSTEMCACHE ('Temporary Tables & Table Variables');   # targeted, not instance-wide
        reset noProgress; retry
  → else, when the budget (count or total self-wait time) is exhausted:
        stop cleanly, work preserved (a re-run resumes from the current size)
```

- The flush is deliberately **targeted, not instance-wide**. It frees only the temp-object
  cachestore (`CACHESTORE_TEMPTABLES`, named `'Temporary Tables & Table Variables'`), which
  releases cached temp tables/table variables that pin tempdb pages, preceded by a `CHECKPOINT` to
  stabilize state. It **avoids** the sledgehammers a naive "soft restart" recipe reaches for:
  - `DBCC FREEPROCCACHE` / `DBCC FREESYSTEMCACHE ('ALL')` empty the **whole** plan cache, triggering
    an instance-wide recompilation storm and CPU spike that can time out application connections —
    far too costly on the busy production instance this feature targets.
  - `DBCC DROPCLEANBUFFERS` empties the buffer pool (clean pages) without freeing any tempdb
    allocation: severe perf cost for zero tempdb gain.
  - Widening the flush to `('ALL')` (an `aggressive` escape hatch) is **deferred out of v1** to keep
    the surface small and unambiguous. If ever added, it would require `flushcaches: true` (it only
    widens the same escalation, never a separate trigger).

`NoProgressBeforeFlush` **default = `2`** no-progress events. It must be **`< MaxNoProgress`** (the
existing give-up count, `3`) so the flush fires *before* the clean give-up **and leaves retry room
after** — at `2`, the flush fires on the second stall, the counter resets, and there is still budget
before the give-up at `3`. Config validation rejects `NoProgressBeforeFlush >= MaxNoProgress`.
- **Flush runs at most once per run** (the caches are instance-wide; flushing per file would repeat
  the perf hit for nothing). A shared `flushed` flag spans all files.
- **845** is folded in as a retryable no-progress event; the flush (which frees internal caches) is
  a plausible response to the contention that raised it.

## 6. Preflight and report

- **Preflight**: database/file-scoped, so it **skips** the `schema.table` existence check (already
  the case for shrink — empty `Schema`/`Table`). It verifies tempdb is reachable and the target
  files exist. It states the **total** target explicitly, because `targetsizemb` is *per file* and
  is easy to misread as a whole-tempdb size — e.g. `Shrinking 8 tempdb data files to 20 GB each
  (total target 160 GB)`. Optionally warn when tempdb free space is low.
- **Report**: one `ShrinkResult` per file (initial / final / target / chunks / reason) plus the
  **blockers observed** during the shrink, written to the `.log` sidecar. The email
  fail/incomplete events already emitted by the engine cover notification.
- **Unbalanced-files warning**: file sizes are read in whole MB, so the check is **exact equality
  in MB** — any MB difference between final data-file sizes trips the warning (no tolerance band).
  If the data files do **not** all end at the same size (some clamped
  to used, or stalled on work-table pages above target), the report emits a clear
  `Unbalanced tempdb files` warning. Uneven files defeat SQL Server's proportional-fill balancing:
  a file that later frees its pinned pages ends up with far more free space than the others, so new
  allocations skew toward it and concentrate `PAGELATCH` contention on a single file. The warning
  tells the DBA tempdb is asymmetric and needs a follow-up (re-run or manual intervention).

## 7. Wiring (by layer)

- **`internal/ddl`** — `ShrinkTempdb` op (manifest struct, `Validate`, `CommandType`, `Target`);
  resolve branch (WALP only, `SELF`); generate branch (run-time per-file DBCC SHRINKFILE).
- **`internal/run`** — the `RunTempdb` method + `ShrinkDriver` interface addition; the extracted
  `chunkLoop` primitive and the `runTempdb` two-phase orchestrator; `TempdbProfile` on
  `ShrinkRunner`; `processOne` `case ddl.ShrinkTempdb` + the `isShrinkOp` predicate for the
  stop-short/`incomplete` path; per-file equal-size target + clamp; the unified no-progress
  escalation with the optional flush; 845 in the retryable set.
- **`internal/mssql`** — **tempdb-scoped connection.** `FileSpace`/`FileSizeMB` read
  `sys.database_files` and `FILEPROPERTY(..., 'SpaceUsed')` in the **connected** database, and
  `DBCC SHRINKFILE` shrinks the **current** database's file. So the tempdb shrink cannot run on the
  main config-DB connection: `RunTempdb` needs an exec + reader whose context **is tempdb**. Open a
  second `*mssql.Conn` against tempdb (clone the config DSN with `database=tempdb`) and use it as
  **both** the `Executor` and `ShrinkReader` for the tempdb path; the `Sampler` stays on the main
  connection (its DMVs are instance-wide). No new read SQL is needed — the existing reads are
  correct once the context is tempdb. Add only the flush exec helper. tempdb is always SIMPLE
  recovery, so no log-reuse gate on the data path.
- **`internal/preflight`** — tempdb reachability/file checks on the existing shrink model.
- **`internal/config`** — default for `NoProgressBeforeFlush` (`3`, `< MaxNoProgress`) and the
  shrink tuning already used. (`aggressive` widening is deferred out of v1, §5.)
- **README / operator skill** — document the operation, its non-goals, and the `flushcaches`
  trade-off.

## 8. Tests (no database; `-race`)

- **`ddl` (pure)**: YAML decode of `shrink_tempdb`; `Validate` (reject `targetsizemb ≤ 0`);
  generate (per-file DBCC SHRINKFILE); resolve (WALP `SELF`; disabled on the 2019 matrix row).
- **`run` (pure)**: equal-size target + clamp to each file's used; **two-phase order** —
  `TRUNCATEONLY` runs on *all* files before any chunk moves a page; the unified escalation —
  `Msg 5240` / no-gain / `845` → backoff → flush **once** at `NoProgressBeforeFlush` (when enabled)
  → clean give-up when the budget trips; **never** a blocker KILL; flush executed a single time
  across multiple files; `isShrinkOp` true for both `ddl.Shrink` and `ddl.ShrinkTempdb` (stop-short
  → `incomplete`).
- **`mssql`**: reads and flush behind the `integration` tag.

## 9. Limits (deliberate, documented)

1. **Live internal objects can pin space** → the shrink may stop above the target. Reported, not
   an error. A restart remains the only guaranteed reset.
2. **Uneven final sizes (skew)** → a file stalled above target leaves tempdb asymmetric, which
   defeats proportional fill (§6). We do not force a common floor (that would under-shrink every
   file to match the worst one); instead we shrink each as far as it can go and **warn**.
3. **Even the targeted cache flush has a cost** → freeing the temp-object cachestore drops cached
   temp objects instance-wide (they recreate on next use). Opt-in, escalation-only (never a routine
   pre-pass), run at most once. The `('ALL')` widening is a separate, off-by-default `aggressive`
   escalation.
4. **`NoProgressBeforeFlush` and the wait budgets are heuristics** → configurable, conservative
   defaults.
5. **2019 has no WALP for `DBCC SHRINKFILE`** → on 2019 the blocker reaction is plain bounded wait
   then clean give-up; WALP (yield-self only) applies from 2022+.
