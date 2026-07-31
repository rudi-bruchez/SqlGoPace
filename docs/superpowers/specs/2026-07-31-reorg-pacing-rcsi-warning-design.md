# Paced reorg yielding + RCSI-off warning

Status: design approved, pending implementation plan.
Date: 2026-07-31.

## Motivation

A production incident showed an `ALTER INDEX ... REORGANIZE` (on `dbo.MEASUREMENT`
in PRODDB, clustered PK `PK_MEASUREMENT`) blocking ~20 queries in `LCK` waits.
REORGANIZE is *online* but not *lock-free*: it compacts and reorders leaf pages under
short-term page/row X locks, and under a busy workload those locks build a blocking
chain. The table has no LOB columns, so `LOB_COMPACTION` was irrelevant — the block
was pure rowstore leaf-page contention. See `docs/REORGANIZE.md` for the full
mechanism analysis.

Two gaps surfaced:

1. **Reorg yields, but weakly.** Reorg routes through `MonitoredRunner` and is flagged
   `CancelSafe`, so under blocking pressure `DecideReaction` returns `Cancel` — a clean
   stop whose committed work is preserved, since SQL Server persists reorganize
   progress. But `runLoop` returns `ErrCancelled` immediately and `Run` **re-issues
   without waiting for relief**, capped at `MaxRetries`. Under sustained load a
   day-scale reorg does run → block → cancel → *immediately* re-issue → block again →
   … → exhaust retries → **fail**. It yields but does not *pace*, and it gives up.
2. **No RCSI warning.** Whether readers block on a reorg's page X locks depends on the
   database's isolation: with `READ_COMMITTED_SNAPSHOT` (RCSI) off, readers take S
   locks and block; with it on, they version-read and do not. RCSI is already detected
   (`ServerInfo.RCSIEnabled`) but nothing warns the operator when it is off at the
   point a reorg starts.

This design makes paced (uncapped) yielding the **default** for `reorganize_index` and
emits an advisory warning when a reorg starts against a database with RCSI off.

## Scope

- Changes are confined to the pure, testable core of `internal/run`
  (`monitored_runner.go`'s `runLoop`, plus a small hook and helper for the engine),
  plus one engine wiring option and its call site in `cmd/sqlgopace/main.go`.
- No new manifest fields and no new config knobs. Yielding-by-default and the warning
  are both automatic.
- The compatibility matrix (`reorganize_index: {}` stays optionless), the generator,
  the shrink driver, and the resumable pause/resume path are unchanged.

## Background: how reorg flows today

- The engine processes only manifests whose `database:` matches the connection
  (`engine.go`), so a reorg always runs against the connected database. Therefore
  `info.RCSIEnabled` (detected once via `is_read_committed_snapshot_on` for `DB_ID()`)
  is the correct RCSI value for every reorg the engine runs — no per-database
  re-detection is needed.
- `reorganize_index` has no injectable options in `ddl_compatibility.yaml`. It cannot
  use `WAIT_AT_LOW_PRIORITY` or `RESUMABLE` (those are `REBUILD ... ONLINE` options),
  so the only pacing lever is cancel-and-reissue.
- `Capabilities.CancelSafe` already marks operations whose cancellation is a clean stop
  with no expensive rollback: `ReorganizeIndex`, `CheckDB`, `UpdateStatistics`
  (`cancelSafe` in `engine.go`, mirrored in `reaction.go`). The paced path deliberately
  does **not** key off this flag (see §1) — `CancelSafe` refines reaction *narration*,
  not control flow, and only `reorganize_index` has the property the paced loop relies
  on (re-issue resumes from persisted progress).

## 1. Paced reorg yielding (`runLoop`)

The pause/resume state machine in `runLoop` gains one behavior. Today:

```
Cancel  -> return ErrCancelled          (Run bounded-retries)
Stop    -> return ErrStopped
Pause   -> waitForRelief(); resumeSQL(); loop
Continue-> return err                    (statement finished)
```

New: `runLoop` takes a `paced bool`, and the `Cancel` branch splits:

```
Cancel & paced   -> waitForRelief(); stmt = sql; loop   (re-issue same REORGANIZE, UNCAPPED)
Cancel & !paced  -> return ErrCancelled                 (unchanged; Run bounded-retries)
```

`paced` is set **only for `reorganize_index`** — a type switch on the operation, not a
capability bit. It is *not* `caps.CancelSafe`: only reorg re-issues from persisted
progress, so only reorg's uncapped loop is guaranteed to converge (see "Scope" below).
`check_db` and `update_statistics` — the other two cancel-safe ops — keep today's
bounded-retry `ErrCancelled` behavior.

Details:

- The re-issued statement is the **original** operation SQL (the same
  `ALTER INDEX ... REORGANIZE`), *not* a resume statement — SQL Server continues from
  its persisted progress. This is distinct from the Pause branch, which issues
  `ALTER INDEX ... RESUME`.
- The statement is still physically stopped the same way before the wait
  (`runStatement`: context-abort first, KILL as a fallback if it does not stop within
  `killGrace`). The cancel is still narrated via `reactionEvent` with the existing
  "incremental — committed work preserved, no rollback" detail, so the log reads as a
  clean cancel, not a rollback-bearing kill.
- `waitForRelief()` is the **exact** primitive the Pause branch already uses
  (`MonitoredRunner.awaitRelief` → pure `waitForRelief`): it samples via its own pump
  until blocking and log pressure clear.
- `Run`'s `MaxRetries` loop is untouched. Reorg (the paced op) never returns
  `ErrCancelled`, so it never consumes retries; the cap continues to govern every other
  cancel — including the other cancel-safe ops (`check_db`, `update_statistics`) and
  non-cancel-safe cancels such as a non-resumable REBUILD, whose re-issue restarts from
  scratch and must be bounded.

### Escape hatches (already present, unchanged)

"Uncapped" does not mean "cannot terminate." Two existing mechanisms still bound the
loop:

- **Graceful stop.** When `caps.Stop()` becomes true the supervisor returns
  `Action Stop`, which `runLoop` turns into `ErrStopped` — the loop ends at the next
  poll, leaving the reorg's committed work in place for a later run.
- **Log-drain timeout.** Inside `waitForRelief`, if the transaction log stays over cap
  for longer than `logDrainTimeout`, it returns `ErrLogDrainTimeout` and the loop ends.
  So a reorg that relieves blocking but drives the log over cap still terminates.

A reorg therefore loops only while it is *making progress under tolerable pressure*; it
terminates on completion, operator stop, or a log emergency — but never *fails merely
for being slow*.

### Scope of the paced path — `reorganize_index` only

The paced path applies to **`reorganize_index` only**, keyed by a type switch on the
operation, not by `CancelSafe`. The reasoning:

- **`reorganize_index`** — re-issue resumes from SQL Server's persisted progress, so
  each attempt makes cumulative forward progress and the uncapped loop is guaranteed to
  converge. This *persisted-progress* property is what licenses "uncapped."
- **`update_statistics`** — cancel-safe, but re-issue **restarts** from scratch. An
  uncapped loop on a perpetually-blocked stats update would spin forever, discarding a
  full/sampled scan each attempt with zero cumulative progress. Bounded retry is the
  better behavior here: it fails fast with a clear "could not get a window" signal in
  the `.log`. Kept on today's path.
- **`check_db`** — cancel-safe, but runs against an internal read-only snapshot and
  essentially never triggers `BlockingOthers`, so it rarely reaches the Cancel path at
  all. No reason to change it. Kept on today's path.

Why not reuse `CancelSafe`: that flag was introduced to refine reaction *narration*
(its own comment states it "does not change which Action is chosen"). Driving the
retry/pacing control flow from it would silently expand its role and, worse, would
extend uncapped pacing to `update_statistics`, where it is unsafe. A reorg-only type
switch keeps the paced path exactly coincident with the persisted-progress property
that justifies it. Cost: one `case ddl.ReorganizeIndex:` instead of a bool — trivial,
and a one-line add if a future op ever gains the same persisted-progress property.

## 2. RCSI-off warning at reorg start

- New engine option `WithRCSI(bool)`, wired in `cmd/sqlgopace/main.go` from
  `info.RCSIEnabled`, alongside the existing `WithADR(info.ADREnabled)`.
- The decision is a small pure helper so it is unit-testable without the engine:

  ```go
  // reorgRCSIWarning returns the warning text to emit before running op, and whether
  // to emit it: only for a REORGANIZE against a database with RCSI off (readers block
  // on its page locks). Empty/false for every other operation or when RCSI is on.
  func reorgRCSIWarning(op ddl.Operation, rcsi bool) (string, bool)
  ```

- In `processOne`, before the reorg executes, call the helper; when it returns true,
  emit one warning that names `schema.table` and the database, states that readers may
  block on the REORGANIZE's page locks with RCSI off, and notes that the pacing loop
  will still yield on blocking. It goes to the existing progress output and is recorded
  in the operation's `.log` sidecar via the existing note/report path — no new report
  plumbing.
- **Advisory only.** It never blocks or skips the operation (per the tool's
  "act, don't nanny" philosophy). Only `ReorganizeIndex` triggers it; the warning is
  specifically about reader-blocking page locks and would be noise on `check_db` or
  `update_statistics`.

Illustrative output:

```
[warn] dbo.MEASUREMENT: RCSI is OFF on PRODDB — readers may block on this
       REORGANIZE's page locks; the pacing loop will still yield on blocking.
```

## 3. Testing

All logic lands in the pure core (no database):

- **`runLoop`** (table-driven, deterministic via injected `runStatement` /
  `waitForRelief` / `resumeSQL`), driving the `paced` flag:
  - paced + pressure → waits for relief, then re-issues the **same** SQL (assert the
    statement passed to the next `runStatement` equals the original, not a RESUME),
    loops N times, then completes.
  - paced + relief never clears + log over cap past timeout → returns
    `ErrLogDrainTimeout`.
  - paced + graceful stop → returns `ErrStopped`.
  - **not** paced + cancel → returns `ErrCancelled` (unchanged) — covers both a
    non-cancel-safe op and a cancel-safe-but-not-paced op (`check_db`,
    `update_statistics`).
  - paced never consumes `MaxRetries` (drive many cancels; assert no
    "retries exhausted" error).
  - the `paced` flag is set only for `ReorganizeIndex`: a small test on the
    engine-side mapping (op → paced) asserts true for `ReorganizeIndex` and false for
    `CheckDB` / `UpdateStatistics` / a non-cancel-safe op.
- **`reorgRCSIWarning`**: warns (true, non-empty) only for `ReorganizeIndex` with
  `rcsi == false`; silent (false) for `ReorganizeIndex` with `rcsi == true`, and for
  `CheckDB` / `UpdateStatistics` / other ops regardless of `rcsi`.

## 4. Non-goals

- No new manifest fields, config knobs, or the ability to skip/fail a reorg on RCSI off
  (warn-and-proceed only).
- No change to the matrix, the generator, the shrink driver, or the resumable
  pause/resume path.
- No dedicated paced reorg driver: `MonitoredRunner` already monitors, so the paced
  path is a `runLoop` refinement, not a new driver. (This reconciles the earlier
  suggestion in `docs/REORGANIZE.md` of a ShrinkRunner-style driver — unnecessary given
  the existing monitoring.)
- No per-database RCSI re-detection (the engine runs only same-database manifests, so
  the detected value already applies).
