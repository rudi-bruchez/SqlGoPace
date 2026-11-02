# Shrink stall causes: yield, backup, patience, and tail relocation

Status: design, approved for planning. Two lots, each shippable as its own minor version.

## Why

A data shrink that stops short of its target today says one thing whatever stopped it:
`no further progress after 3 attempts without a gain (shrink.max_no_progress)`. Three field
runs on one `PRODDB` data file (SQL Server 2019 Standard, ~7.6 TB, `SIMPLE` recovery) show
three different causes behind that sentence, each wanting a different answer.

| Case | What happened | What the driver did | What it should have done |
|---|---|---|---|
| Yield | 17 chunks, 394 916 MB reclaimed in 1 h 52, then three chunks in a row stopped by the supervisor at 99 % of the chunk because they blocked a writer (a loader inserting with open transactions, deliberately not in `ignore_blocked_sessions`) and two reporting `SELECT`s | counted each stopped chunk as "no progress", retried at the same step, gave up after the third | make the chunk smaller until it fits between the writer's inserts; wait only when even the smallest chunk cannot fit |
| Backup | 509 836 MB reclaimed in 16 chunks over 2 h 17, then three chunks in a row failed with `Msg 3140, Level 16, State 5` (`Could not adjust the space allocation for file`) while a differential backup of the same database ran from 11:00:01 to 11:16:09 UTC (`msdb.dbo.backupset`) | halved the step, backed off 30 s then 60 s, gave up at 11:15:51, eighteen seconds before the backup ended | wait for the backup to finish, then carry on at the same step |
| Tail object | the same tail object (`dbo.MEASUREMENT`, `index_id = 5`) named through five stall episodes | with `max_no_progress: 10` and `self_wait_timeout_minutes: 30` set by hand, every episode recovered on its own and the shrink reached 100 % (746 347 MB to 381 298 MB, 1 h 22) | the defaults would have stopped at the first episode; patience was the right answer |

The third case also settles a question left open in `docs/specs/TODO.md` ("Found in a live
shrink, blocked on a tail object"): the identity of the tail object does not tell a structural
blocker from workload churn. The same object can be both. This design does not try to tell them
apart by signature. It waits first, and treats a tail object that is still there after the
patience budget as the structural case.

## Measured facts this design relies on

Measured on 2026-11-02, SQL Server 2022 CU26 (16.0.4265.3), Developer, Linux container, on a
throwaway 3 GB database with 1.5 GB free before 1.5 GB of data (dropped afterward):

- A `DBCC SHRINKFILE` issued while a `BACKUP DATABASE` (full or differential) of the same
  database runs fails after about 2 s with `Msg 3140, Level 16, State 5`, with `SET LOCK_TIMEOUT
  -1`, while the backup is still running. It does not wait for the backup. (Microsoft's
  `BACKUP` page says the second operation waits for the lock to time out per the session
  timeout; that is not what happens. Recorded separately as a documentation defect.)
- In a loop of 100 MB chunks against back-to-back data backups, roughly half the chunks gained
  nothing and a few advanced while a backup was running. A chunk can therefore succeed during a
  backup; failure is likely, not certain.
- `BACKUP LOG` does not make a chunk fail: 740 log backups in 40 s, every chunk advanced, each
  slowed from about 1 s to about 6 s.
- A running data backup is visible in `sys.dm_exec_requests` with `command = 'BACKUP DATABASE'`
  and the shrunk database's `database_id`, with `percent_complete` and
  `estimated_completion_time` populated. A log backup shows `command = 'BACKUP LOG'`.
- The other direction is harmless: a full `BACKUP DATABASE` started while a single large chunk
  was running (at 56 % of it) completed normally in 5 s, without Msg 3023, and the chunk then
  finished and reached its target. The shrink does not make a scheduled backup fail, so the
  driver has no reason to check for a backup before each chunk.
- `ALTER TABLE ... REBUILD WITH (ONLINE = ON (WAIT_AT_LOW_PRIORITY (MAX_DURATION = 1 MINUTES,
  ABORT_AFTER_WAIT = SELF)))` on a heap is accepted on 2022; `RESUMABLE` is not an `ALTER TABLE`
  option (Msg 155). The `rebuild_heap` row of `ddl_compatibility.yaml` ("no RESUMABLE and no
  WAIT_AT_LOW_PRIORITY for a heap rebuild, on any version") is therefore wrong about
  `WAIT_AT_LOW_PRIORITY`. See "Prerequisite" in lot 2.

## Goals

1. A chunk the driver stopped itself (yield) is never counted as a stall while the step can
   still get smaller.
2. A chunk that fails or gains nothing while a data backup of the same database runs waits
   for the backup, without spending the stall budget and without shrinking the step.
3. Every other stall gets one patience budget measured in time, long enough for an overnight
   shrink by default.
4. When the shrink gives up, its reason names the cause it saw: the sessions it kept yielding
   to, the backup, or the tail object read at give-up time.
5. Lot 2: a manifest can opt in to relocating the tail object found at give-up, under strict
   guard rails, then resume the shrink.

## Non-goals

- Starting, stopping, delaying or otherwise touching a backup. SqlGoPace waits; it never
  issues `BACKUP`, never kills a backup session, never changes a backup job. Same rule as the
  log shrink's `log_reuse_wait_timeout_minutes`.
- A signature-based structural-blocker detector (tail identity, page identity, write rate).
  Rejected by the 2026-09-20 measurement.
- Any change to the log shrink (`shrink_log`) or to the tempdb shrink beyond what the shared
  `stall` budget implies.
- Availability-group send/redo queue awareness during the relocation rebuild. It is its own
  backlog entry ("Availability group state as a pressure signal").
- Relocation on Standard edition. See lot 2.

## Anti-goals

- An unattended relocation that blocks production while nobody watches. Every guard rail in
  lot 2 exists to prevent this; none is optional.
- A shrink that waits forever on something that will never clear. Every wait in this design
  is bounded, except the backup wait, which is bounded by the backup itself making progress.
- A give-up reason that sends the operator to the wrong knob.

## Lot 1: stall classification

All of it lives in `internal/run/shrink.go` (`chunkLoop`, `stall`) and
`internal/run/shrink_calc.go`, plus one new read in `internal/mssql` behind `ShrinkReader`.

### The three outcomes of a chunk that gained nothing

After each chunk, the driver already knows whether it was stopped under pressure (`stopped`
from `runChunk`), whether it errored (`err`), and whether the file got smaller (`newSize`
versus `current`). The no-gain cases are classified in this order:

1. Backup. If a data backup of the target database is running (new read, below), the chunk
   is a backup wait, whatever else happened (error, no gain, or stopped).
2. Yield. Else, if `stopped` is true, the chunk is a yield.
3. Stall. Everything else: an error other than our own cancellation, or a clean chunk that
   gained nothing.

The backup check comes first because a chunk failing on Msg 3140 during a backup would
otherwise be treated as a stall and halve the step for nothing.

### Yield

- Halve the step (`halveStep`), as `AdjustStepMB` already does for a stopped chunk that
  progressed. The current code skips this on the no-gain path, so the same chunk is cut at the
  same place every time.
- If the step before halving was above `min_step_mb`: no stall count, no backoff. The driver
  has already waited for relief in `awaitRelief`; it retries at once with the smaller step.
- If the step was already at `min_step_mb`: the workload leaves no gap even for the smallest
  chunk. The yield is counted as a stall and goes through the patience budget below, with its
  backoff.
- The driver remembers who it yielded to. The engine already snapshots the sessions we block
  on each yield (it feeds `.blocked.yaml` and the "blocking N session(s)" narration); the
  give-up reason names the login seen most often across the consecutive yields of the current
  stall, and whether it had an open transaction. Where that snapshot is not reachable from the
  shrink driver, the builder threads it through the existing `ReactionEvent` path rather than
  adding a second DMV read.

Pages a stopped chunk already moved stay moved (`DBCC SHRINKFILE` keeps completed work), so
retrying with a target closer to the current size costs little.

### Backup wait

New read on `ShrinkReader`, implemented in `internal/mssql/shrink.go`: the running data backup
of the current database, if any, from `sys.dm_exec_requests` (`command = 'BACKUP DATABASE'`,
`database_id = DB_ID()`), returning its session id, `percent_complete`, `start_time` and
`estimated_completion_time`. Log backups are excluded on purpose (measured harmless).

Behavior while a data backup runs:

- No step change, no stall count, no addition to the stall budget.
- Poll the backup every `backoff` interval starting at `no_progress_backoff_seconds` and
  capped at `no_progress_backoff_max_seconds` (the existing keys), and emit a `pause`
  reaction naming it: session id, percent complete, estimated end. The console shows the same
  through the existing progress path.
- The wait lasts as long as the backup runs and makes progress. Decided by Rudi: no fixed
  ceiling. A backup whose `percent_complete` has not moved for 15 minutes is no longer treated
  as a reason to wait; the chunk then falls through to the ordinary stall path and its budget.
  (15 minutes is the designer's call, a constant, not a config key.)
- When the backup ends, retry the chunk at once at the same step, and reset the stall counters
  as after a successful chunk.
- Graceful stop (`ErrStopped`) and cancellation interrupt the wait like every other wait in
  the driver.
- The run report records each backup wait as a reaction: when it started, how long it lasted,
  which backup.

The check runs only after a chunk that gained nothing. It does not run before every chunk: the
cost of the cheap path is one failed 2-second chunk per backup, which is not worth a DMV read
per chunk.

### One patience budget

`stall` keeps its shape (count, cumulative wait, backoff doubling) but the defaults change so
that the time budget is the bound that normally trips:

| Key | Old default | New default | Role |
|---|---:|---:|---|
| `shrink.self_wait_timeout_minutes` | 5 | 30 | the patience budget: cumulative backoff waited in the current stall |
| `shrink.max_no_progress` | 3 | 15 | backstop only; with 30 s doubling to a 300 s ceiling, 30 minutes is about nine backoffs, so 15 does not trip first |
| `shrink.no_progress_backoff_seconds` | 30 | 30 | unchanged |
| `shrink.no_progress_backoff_max_seconds` | 300 | 300 | unchanged |

Decided by Rudi: one budget for every stall without an identified cause, 30 minutes, his
current field workaround. The tempdb path's `no_progress_before_flush` must stay below
`max_no_progress`; check its default still satisfies that.

Migration note required in `CHANGELOG.md`, `docs/configuration.md` and both `config.yaml`
copies: an operator whose `config.yaml` sets `max_no_progress: 3` or
`self_wait_timeout_minutes: 5` explicitly keeps the old, short behavior and should revisit it.
The shipped `config.yaml` comments that describe "about 90 s of waiting" must be rewritten.

### The give-up reason

The reason names the cause, the bound, and the fact that work is preserved. Shapes (wording is
the builder's):

- yield: `yielded 7 times to sessions we blocked (most often login=<login>, open transaction);
  step at min_step_mb (50 MB); 30m0s of waiting (shrink.self_wait_timeout_minutes); work
  preserved`
- stall with tail: `no further progress: 30m0s of waiting (shrink.self_wait_timeout_minutes);
  tail object at give-up: dbo.MEASUREMENT index_id=5, 0 pages from end; work preserved`
- stall without tail (pre-2019, or walk found nothing): as today, with the bound named.

The tail read for the reason must be taken at give-up time (`captureGiveUpTail`), not reused
from the proactive walk at shrink start. Check that `captureGiveUpTail` performs a fresh walk
and that the reason uses its result.

### Lot 1 tests

Unit tests with the existing fakes (`ShrinkReader` fakes, fake clock), no database:

- a stopped no-gain chunk above the floor halves the step and does not increment the stall
  count; at the floor it does;
- a no-gain chunk with a running data backup does not halve, does not count, waits until the
  fake backup disappears, then retries at the same step;
- a backup whose percent stays flat for 15 fake minutes falls back to the stall path;
- an errored chunk with a running backup is classified as backup, not stall;
- the give-up reason carries the most frequent yielded-to login, and the fresh tail object;
- default config: a stall gives up on the time bound, not the count.

One e2e test under the `integration` tag (it mutates its own throwaway database): start a
`BACKUP DATABASE ... COPY_ONLY` to a file during a shrink and assert the run reports a backup
wait and finishes. The backup file is deleted by the test.

## Lot 2: tail relocation (`unblock_tail`)

Requested after a live campaign did this by hand three rounds in a row; see `docs/specs/TODO.md`,
"Autonomous tail unblocking", for the measured rounds (33.6 GB reclaimed before the first
relocation, 115.4 GB after it; a 2.3 GB heap rebuild landed at 22.2 % of the file).

### Prerequisite: the heap row of the matrix

Measure on 2016, 2017, 2019 and 2022 containers (the field server is 2019) which versions
accept `WAIT_AT_LOW_PRIORITY` on `ALTER TABLE ... REBUILD WITH (ONLINE = ON (...))`, then add
`wait_at_low_priority` to `rebuild_heap` in `ddl_compatibility.yaml` with the measured
`min_major` and the Enterprise gate. This is a defect of the shipped tool independently of this
design (planner-generated `rebuild_heap` operations today never get a low-priority wait), so it
ships as its own fix with its own CHANGELOG entry, before lot 2. If a version the tool supports
rejects it, lot 2 refuses heaps on that version under guard rail 2.

### Unit of relocation

The whole index (or the whole heap with its nonclustered indexes, see guard rail 3), never a
partition. Decided by Rudi. `FindTailObject` does not return the partition, and the field cases
were not partitioned. On a large partitioned table the size ceiling stops the round with the
object named. Partition-level relocation (`REBUILD PARTITION = n`) is a follow-up for
`docs/specs/TODO.md`.

### Manifest surface

Declared per shrink operation, never on by default, because it is unrequested DDL on
production:

```yaml
- operation: shrink
  type: data
  files: PRODDB
  targetfreespace: 25%
  identify_tail_object: true
  unblock_tail:
    enabled: true
    max_rounds: 3
    max_object_mb: 5000
    max_unresumable_object_mb: 2500
```

`enabled: true` requires `identify_tail_object: true`, `type: data`, and a named file (not
`files: all`); anything else is a manifest validation error. A named file keeps a round
between two shrink attempts of one file, without changing how `ShrinkRunner.Run` iterates over
files. There is no `require_online`, `require_low_priority` or
`preserve_compression` key: those are not options, they are the feature (decided by Rudi, see
guard rails). Defaults when `unblock_tail` is present without them: `max_rounds: 3`,
`max_object_mb: 5000`, `max_unresumable_object_mb: 2500` (the 2.3 GB heap from the field must
fit; designer's call).

### When it fires

Only when all of these hold at the end of a shrink of one data file:

- the shrink gave up on the patience budget of lot 1 for a plain stall (not on a backup, not
  on a graceful stop, not on a log-drain timeout, not on an error, and not on yields at
  `min_step_mb`: a shrink that kept blocking a writer would only meet the same writer in an
  online rebuild, and moving the tail does nothing for that cause). `ShrinkResult` therefore
  records which cause exhausted the budget;
- the give-up tail walk (fresh, lot 1) named a user object, and the server is 2019 or later
  (the walk needs `sys.dm_db_page_info`);
- the tail is not `confirmed_by: transient_maintenance`: a concurrent maintenance operation
  pinning the tail is not ours to move;
- fewer than `max_rounds` relocations have run for this file in this operation.

### Guard rails

Each check failing stops the operation `INCOMPLETE`, work preserved, with the object and the
failed rule named in the reason. Nothing is relocated on a guess.

1. ONLINE is mandatory. The rebuild is resolved through the normal `Resolve` path against the
   `Matrix`; if `ONLINE = ON` is not eligible (Standard edition, an object type that cannot be
   rebuilt online), stop. On Standard this means `unblock_tail` never relocates. Preflight
   reports it as a `WARN`, not a failure, and `--dry-run --explain` says it ("unblock_tail is
   armed but cannot fire on this target: ONLINE rebuild unavailable on Standard"): the shrink
   itself still runs, with relocation inert.
2. `WAIT_AT_LOW_PRIORITY` is mandatory, and `ABORT_AFTER_WAIT` is forced to `SELF` whatever
   the policy or `Policy.AllowAbortBlockers` says: a relocation never kills a production
   session to make room for itself. Where the matrix has no `WAIT_AT_LOW_PRIORITY` for the
   object and version (after the prerequisite above), stop.
3. Size ceiling. For an index, its size. For a heap, the whole table: heap, every
   nonclustered index and LOB data, because `ALTER TABLE ... REBUILD` rebuilds all of them in
   one transaction. Above `max_object_mb` stops. Above `max_unresumable_object_mb` stops too
   when the rebuild has no `RESUMABLE` form: a heap never has one, and an index rebuild has one
   only where the matrix says so. Below the ceilings, `RESUMABLE = ON` is used wherever it is
   eligible. A heap with a disabled nonclustered index is refused (`ALTER TABLE ... REBUILD`
   would re-enable it, and `AllowReenableDisabledIndexes` is never set by a relocation).
4. Compression preserved. The rebuild keeps each partition's current `data_compression`. It is
   built with `intent: relocation` (`ddl.IntentRelocation`, already defined: it always runs and
   preserves compression).
5. Bounded rounds. After `max_rounds`, stop and name the latest tail object.
6. Progress per round. If a round's rebuild completes but the next shrink attempt gains nothing
   and the fresh tail names the same object again, stop: the move did not take it off the end.

### How a round runs

The shrink driver does not run DDL other than `DBCC SHRINKFILE`. A round is an ordinary
operation run by the engine:

1. `ShrinkRunner` returns its result with the fresh tail finding and a flag saying the give-up
   qualifies for a round (the conditions above that the driver can see).
2. The engine (`processOne`) builds the relocation operation from the finding: index rebuild
   for `index_id >= 1`, `ALTER TABLE ... REBUILD` for a heap (`index_id = 0`), with
   `intent: relocation`, and passes it through `Resolve` and `Plan` exactly as a manifest
   operation would be, so every option decision lands in the `Decision` list and in the report.
   The guard rails are checked on the resolved options.
3. The engine runs it through `MonitoredRunner`, under the manifest-level ignore lists and kill
   rules and the config's reaction policy. The shrink operation's own `options:` overrides
   (`max_block_minutes`, `abort_after_wait`, ...) are not inherited: they were written for a
   shrink. A relocation canceled under pressure ends the operation `INCOMPLETE` naming it; it
   is not retried by `max_retry_attempts` (a rollback of an unresumable rebuild is paid once,
   not twice).
4. Then the shrink resumes on the same file from its current size, with a fresh stall budget.
5. The run report shows each round as its own step: the object, size, options, duration, and
   the shrink's size before and after.

Interruption and recovery. The relocation is not one of the manifest's planned operations, so
the existing state (`internal/run/state.go`: `PlanFingerprint`, the resume cursor,
`State.Paused` matched on op index plus target) does not know it. Designer's call:

- The manifest's state records, under the shrink operation's index, the number of rounds
  already run and the relocation in flight (object, index, whether resumable). `max_rounds`
  is counted from that record, so a recovery or a re-armed run does not grant fresh rounds.
  The count resets only when the operator moves the manifest back to `01.to_run/` by hand,
  which is a deliberate new campaign.
- On recovery, a paused resumable relocation recorded there is owned by this manifest: it is
  resumed first, then the shrink resumes. A relocation that was not resumable has rolled back
  with the process; the round counts as spent and the shrink resumes.
- A graceful stop during a round pauses a resumable relocation the same way the engine pauses
  any resumable rebuild today, and records it as above.

### Lot 2 tests

- manifest validation: `unblock_tail` without `identify_tail_object`, on `type: log`, with
  negative values;
- resolve: Standard target refuses with the ONLINE reason; Enterprise 2019 index gets
  `ONLINE`, `RESUMABLE`, `WAIT_AT_LOW_PRIORITY` and the current compression; a heap gets no
  `RESUMABLE` and is checked against `max_unresumable_object_mb`;
- engine: give-up on budget with a tail runs one round then resumes; give-up on backup or
  graceful stop runs none; `max_rounds` honored; same object after a round stops;
- explain/dry-run text when armed on Standard.
- e2e on the Developer container (Enterprise features): a heap at the file end, shrink gives up
  quickly under a test-sized budget, one round relocates it, the shrink then reaches target.

## Documentation that moves with each lot

Per `CLAUDE.md`: `CHANGELOG.md` (new minor section per lot, migration note for lot 1's
defaults), `internal/version/VERSION`, `docs/specs/SHRINK.md` (§7.1 algorithm, §8.2, §8.3,
§8.6, the blocking table in §11, and a new section for the backup wait and for
`unblock_tail`), `docs/shrink.md`, `docs/configuration.md`, `docs/manifests.md` (lot 2),
`config.yaml` and `internal/scaffold/assets/config.yaml`, and `docs/specs/TODO.md`: move
"tail object does not steer the decision", "no-progress budget sized for a chunk",
"autonomous tail unblocking" to Shipped as their lot lands, and add the backup-aware stall,
whose Todoist task points at a `TODO.md` entry that was never committed. New config keys must
be read outside `internal/config` or `TestNoInertConfigKey` fails, which is the point.

## Left to the builder

- Names of types, functions and the new `ShrinkReader` method.
- Exact wording of reactions and give-up reasons, within the shapes above.
- How the yielded-to login reaches the driver (see Yield).
- Whether lot 2's qualifying flag lives on `ShrinkResult` or is derived by the engine.
- Splitting each lot into commits and tasks.

## Calls made by the designer, to check

- The backup check runs only after a no-gain chunk, not before every chunk.
- A backup whose percent is flat for 15 minutes stops being a reason to wait.
- `max_no_progress` default 15 as a backstop to the 30-minute budget.
- `max_unresumable_object_mb` as a second key rather than an implicit fraction of
  `max_object_mb`, default 2500.
- A relocation canceled under pressure is not retried by `max_retry_attempts`.
- No `require_*` keys in `unblock_tail`: the guard rails cannot be turned off.
- `unblock_tail` requires a named file.
- Yields at the floor do not qualify for a round.
- A heap's size is the whole table; a heap with a disabled nonclustered index is refused.
- `ABORT_AFTER_WAIT = SELF` forced; the shrink's `options:` are not inherited.
- Rounds and the in-flight relocation are recorded in the manifest's state; a paused
  resumable relocation is resumed on recovery.
- On Standard, armed `unblock_tail` is a preflight `WARN`, not a refusal.
