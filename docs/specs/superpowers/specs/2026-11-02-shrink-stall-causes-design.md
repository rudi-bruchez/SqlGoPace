# Shrink stall causes: yield, backup, and patience

Status: design, approved for planning. Revised the same day after a seven-reader review panel;
see "Revision after review" at the end for what changed and why. One lot, one minor version.

## Why

A data shrink that stops short of its target today says one thing whatever stopped it:
`no further progress after 3 attempts without a gain (shrink.max_no_progress)`. Three field
runs on one `PRODDB` data file (SQL Server 2019 Standard, ~7.6 TB, `SIMPLE` recovery) show
three different causes behind that sentence, each wanting a different answer.

| Case | What happened | What the driver did | What it should have done |
|---|---|---|---|
| Yield | 17 chunks, 394 916 MB reclaimed in 1 h 52, then three chunks in a row stopped by the supervisor at 99 % of the chunk because they blocked a writer (a loader inserting with open transactions, deliberately not in `ignore_blocked_sessions`) and two reporting `SELECT`s | counted each stopped chunk as "no progress", retried at the same step, gave up after the third | make the chunk smaller until it fits between the writer's inserts; wait only when even the smallest chunk cannot fit |
| Backup | 509 836 MB reclaimed in 16 chunks over 2 h 17, then three chunks in a row failed with `Msg 3140, Level 16, State 5` (`Could not adjust the space allocation for file`) while a differential backup of the same database ran from 11:00:01 to 11:16:09 UTC (`msdb.dbo.backupset`) | halved the step, backed off 30 s then 60 s, gave up at 11:15:51, eighteen seconds before the backup ended | wait for the backup to finish, then carry on at the same step |
| Patience | the same tail object (`dbo.MEASUREMENT`, `index_id = 5`) named through five stall episodes | with `max_no_progress: 10` and `self_wait_timeout_minutes: 30` set by hand, every episode recovered on its own and the shrink reached 100 % (746 347 MB to 381 298 MB, 1 h 22) | the defaults would have stopped at the first episode; patience was the right answer |

The third case also settles a question left open in `docs/specs/TODO.md` ("Found in a live
shrink, blocked on a tail object"): the identity of the tail object does not tell a structural
blocker from workload churn. The same object can be both. This design does not try to tell them
apart. It waits, and names the tail object read at give-up time so the operator can decide.

## Measured facts this design relies on

Measured on 2026-11-02, SQL Server 2022 CU26 (16.0.4265.3), Developer, Linux container, on
throwaway databases (dropped afterward), and reproduced the same day by four reviewers on their
own databases. The script is in the appendix.

- A `DBCC SHRINKFILE` chunk that runs while a `BACKUP DATABASE` (full or differential) of the
  same database runs ends in one of two shapes. Either it fails with `Msg 3140, Level 16,
  State 5` (`Could not adjust the space allocation`), whatever `SET LOCK_TIMEOUT` says, or it
  returns without error and the file size has not changed. In a loop against back-to-back
  backups, both shapes occur, and a few chunks still advance. The chunk does not wait for the
  backup. (Microsoft's `BACKUP` page says the second operation waits for the lock to time out
  per the session timeout; that is not what happens. Recorded separately as a documentation
  defect.)
- A failing chunk does its page movement first and fails at the final truncation: after a
  failed chunk, the last allocated page was below the chunk's target while the file size was
  unchanged (one reviewer, 3.0 s and 7.8 s chunks). So the retry after the backup only has to
  truncate, and a failed chunk is not cheap: it costs up to `max_chunk_seconds` of moving.
- `BACKUP LOG` does not make a chunk fail: 740 log backups in 40 s, every chunk advanced, each
  slowed from about 1 s to about 6 s.
- A backup started while a chunk is running completes normally (no Msg 3023), and the chunk
  then finishes. The shrink does not make a scheduled backup fail.
- A running data backup is visible in `sys.dm_exec_requests` with `command = 'BACKUP
  DATABASE'` and `database_id` equal to the backed-up database, even when the backup was issued
  from a session in `master`. `percent_complete` is populated. A log backup shows
  `command = 'BACKUP LOG'`. The shrink driver's reads run on a connection whose context is the
  target database (`mssql.OpenDatabase`), so `DB_ID()` there names it.

## Goals

1. A chunk the driver stopped itself under pressure (yield) is never counted as a stall while
   the step can still get smaller.
2. A chunk that fails or gains nothing while a data backup of the same database runs waits for
   the backup, without spending the stall budget and without shrinking the step.
3. Every other stall gets one patience budget measured in time, long enough for an overnight
   shrink by default.
4. When the shrink gives up, its reason names the cause it saw: who it kept yielding to, or the
   tail object read at give-up time.

## Non-goals

- Starting, stopping, delaying or otherwise touching a backup. SqlGoPace waits; it never
  issues `BACKUP`, never kills a backup session, never changes a backup job. Same rule as the
  log shrink's `log_reuse_wait_timeout_minutes`.
- A signature-based structural-blocker detector (tail identity, page identity, write rate).
  Rejected by the 2026-09-20 measurement.
- Any change to the log shrink (`shrink_log`).
- Relocating the tail object automatically. Designed as a second lot, then withdrawn after
  review; see `docs/specs/TODO.md`, "Autonomous tail unblocking".

## Anti-goals

- A shrink that waits forever on something that will never clear. Every wait here is bounded,
  except the backup wait, which lasts exactly as long as the backup exists.
- A give-up reason that sends the operator to the wrong knob.
- A wait that a graceful stop cannot interrupt promptly.

## The design

All of it lives in `internal/run/shrink.go` (`chunkLoop`, `stall`, `captureGiveUpTail`) and
`internal/run/shrink_calc.go`, plus one new read in `internal/mssql/shrink.go` behind
`ShrinkReader`.

### Reading the outcome of a chunk

Today an errored chunk goes straight to `halveStep` and `stall` without reading the file size.
That changes: after every chunk, errored or not, the driver reads `FileSizeMB`. A size read that
fails is returned as an error, as it is today on the clean path. If the file got smaller, the
chunk made progress, whatever else happened, and goes through the progress path (step
adjustment, counters reset). Only a chunk that gained nothing is classified, in this order:

1. Yield. `stopped` is true: the supervisor stopped the chunk under pressure (blocking other
   sessions, or the log over its cap).
2. Backup. Else, a data backup of the target database is running (new read, below).
3. Stall. Everything else: an error other than our own cancellation, or a clean chunk that
   gained nothing.

Yield comes first because the driver knows for certain that it stopped the chunk itself, and
why; a backup running at the same moment is incidental to that stop. Backup comes before stall
so that a chunk failing on Msg 3140 during a backup does not halve the step for nothing.

On the tempdb path (`RunTempdb`, `prof != nil`) the backup check is skipped, since tempdb
cannot be backed up; yield and stall apply as on a user database.

### Yield

- Halve the step (`halveStep`), as `AdjustStepMB` already does for a stopped chunk that
  progressed. The current no-gain path skips this, so the same chunk is cut at the same place
  every time.
- If the step before halving was above `min_step_mb`: no stall count, no backoff. The driver
  has already waited for relief in `awaitRelief`; it retries at once with the smaller step.
- If the step was already at `min_step_mb`: the workload leaves no gap even for the smallest
  chunk. The yield is counted as a stall and goes through the patience budget below, with its
  backoff. Each such retry is a chunk that may block the same session again before the
  supervisor stops it; at the default budget that is about ten more episodes of up to
  `blocking_timeout_minutes` each over half an hour. Accepted: it is the operator's choice in
  `config.yaml`, and the reason below names who paid for it.
- At the moment of each yield, the driver takes its own snapshot through
  `ShrinkReader.ActiveSessions` (already in the interface): the sessions the chunk was blocking,
  and the session blocking the chunk itself if there was one (its `blocking_session_id`, as
  `mssql.FindSelfBlock` reads it). It keeps a tally for the current stall, reset whenever a
  chunk makes progress. The engine's `.blocked.yaml` capture is not reused: it accumulates over
  the whole operation and its snapshot is taken after the cancel, when the blocked session may
  already be free.

Pages a stopped chunk already moved stay moved (`DBCC SHRINKFILE` keeps completed work), so
retrying with a target closer to the current size costs little.

### Backup wait

New read on `ShrinkReader`, in `internal/mssql/shrink.go`: the running data backup of the
current database, if any, from `sys.dm_exec_requests` (`command = 'BACKUP DATABASE' AND
database_id = DB_ID()`), returning its session id, `percent_complete` and `start_time`. Not
`estimated_completion_time`: `internal/mssql/dmv.go` records why that column is never shown
(documented "Internal only", and it once announced "ETA: 5s" for a rebuild 72 % done). Where an
estimate is shown, it comes from `ProgressRate`, as for every other operation. Log backups are
excluded on purpose (measured harmless).

Behavior while a data backup runs:

- No step change, no stall count, and the stall counters (`noProgress`, `stallWaited`,
  backoff) are left exactly as they were: the backup wait neither spends nor refills the budget.
- Poll the backup at a fixed interval, `no_progress_backoff_seconds` (30 s by default). The
  console shows the backup's session id, percent and elapsed time through the existing progress
  path.
- The wait lasts as long as a data backup of the database is running. Decided by Rudi: no
  ceiling. A second data backup queued behind the first is still a data backup of the database,
  and the wait continues. `percent_complete` is displayed, never used to end the wait: it can
  stay flat for a long time on a healthy backup (slow destination, third-party VDI backups).
- The wait polls the graceful-stop flag as well as the context. `r.wait`'s production `sleep`
  watches only the context today, so the backup wait uses a stop-aware wait (the same check
  `waitForRelief` makes), and a graceful stop ends it within one poll at most.
- When no data backup is running any more, retry the chunk at once at the same step.
- Two reactions only: one when the wait starts (`pause`, naming the backup session and when it
  started) and one when it ends, with its duration. Not one per poll: every `pause` reaction is
  a capture point in the engine sink and can fire the webhook, and a three-hour backup polled
  every 30 s would send hundreds.

The check runs only after a chunk that gained nothing and was not a yield. It does not run
before every chunk: a backup starting mid-chunk is harmless to the backup, and the cost of
discovering one is one failed chunk.

### One patience budget

`stall` keeps its shape (count, cumulative wait, backoff doubling) but the defaults change so
that the time budget is the bound that normally trips:

| Key | Old default | New default | Role |
|---|---:|---:|---|
| `shrink.self_wait_timeout_minutes` | 5 | 30 | the patience budget: cumulative backoff waited in the current stall |
| `shrink.max_no_progress` | 3 | 15 | backstop only |
| `shrink.no_progress_backoff_seconds` | 30 | 30 | unchanged |
| `shrink.no_progress_backoff_max_seconds` | 300 | 300 | unchanged |

Decided by Rudi: one budget for every stall without an identified cause, 30 minutes, his
current field workaround. `stall` checks the budget before sleeping, so with 30 s doubling to
300 s the time bound trips at the tenth stall, after 1950 s (32 min 30 s) of backoff; the count
reaches 10, below 15. The give-up reason prints the time actually waited, not the configured
value. The tempdb path's `no_progress_before_flush` (default 2) stays below `max_no_progress`,
which `config.validate` enforces.

The shipped `config.yaml` (and its embedded twin) sets `max_no_progress: 3` and
`self_wait_timeout_minutes: 5` explicitly. Both copies change to the new values, and their
comments describing "about 90 s of waiting" are rewritten.

Migration note required in `CHANGELOG.md` and `docs/configuration.md`: an operator whose own
`config.yaml` sets `max_no_progress: 3` or `self_wait_timeout_minutes: 5` keeps the old, short
behavior and should revisit both. Lowering `max_no_progress` to 2 or less makes the config fail
to load while `no_progress_before_flush` is 2 (`no_progress_before_flush (2) must be <
max_no_progress`), so the note says to lower both together.

### The give-up reason

The reason names the cause, the bound with the time actually waited, and the fact that work is
preserved. It is built after the give-up tail walk, from its result: `captureGiveUpTail` today
runs after `result.Reason` is set and returns nothing, so it changes to return its finding, and
the same finding goes on `ShrinkResult` (and still to the sink and the `.contended.yaml`, as
now). Shapes (wording is the builder's):

- yield: `yielded 7 times (blocking login=<login>, open transaction; chunk itself blocked by
  session 60, login=<login>); step at min_step_mb (50 MB); 32m30s of waiting
  (shrink.self_wait_timeout_minutes); work preserved`
- stall with tail: `no further progress: 32m30s of waiting (shrink.self_wait_timeout_minutes);
  tail object at give-up: dbo.MEASUREMENT index_id=5, 0 pages from end; work preserved`
- stall without tail (pre-2019, or the walk found nothing): as today, with the bound named.

The tail walk can name a system object (a reviewer saw Query Store's `sys.plan_persist_plan`
on a fresh 2022 database). The reason prints it as it is: it is the truth about the tail, and
the operator needs it.

## Tests

Unit tests with the existing fakes (`ShrinkReader` fakes, fake clock), no database:

- a stopped no-gain chunk above the floor halves the step and does not increment the stall
  count; at the floor it does;
- a stopped no-gain chunk while a fake data backup is running is a yield, not a backup wait;
- an errored chunk whose file size dropped is progress;
- a no-gain chunk with a running data backup does not halve, does not count, leaves the stall
  counters unchanged, waits until the fake backup disappears, then retries at the same step;
- a second fake backup appearing as the first ends keeps the wait going;
- a graceful stop during a backup wait ends it within one poll;
- the backup wait emits exactly two reactions;
- the tempdb path never calls the backup read;
- the give-up reason carries the yielded-to sessions of the current stall only, and the fresh
  tail finding, which is also on `ShrinkResult`;
- default config: a stall gives up on the time bound, at the tenth stall, not on the count.

One e2e test under the `integration` tag, on its own throwaway database: a loop of `BACKUP
DATABASE ... COPY_ONLY` to a file, kept running for longer than one chunk plus one poll, while a
shrink runs; assert the run reports a backup wait and reaches its target. A single backup is
too short to hit reliably (3 s for 1.2 GB in the lab). The test deletes its backup files and
its `msdb` backup history.

## Documentation that moves with this change

Per `CLAUDE.md`: `CHANGELOG.md` (a new minor section, with the migration note),
`internal/version/VERSION`, `docs/specs/SHRINK.md` (§7.1 algorithm, §8.2, §8.3, §8.6, the
blocking table in §11, and a new section for the backup wait), `docs/specs/TEMPDB-SHRINK.md`
(§5, whose no-progress arithmetic changes with the defaults, and the yield halving that now
applies on the tempdb path), `docs/shrink.md`, `docs/configuration.md`, `config.yaml` and
`internal/scaffold/assets/config.yaml`, and `docs/specs/TODO.md`: move "the tail object does
not steer the decision" and "the no-progress budget is sized for a chunk" to Shipped, and the
"Shrink stall causes" iteration with them.

## Left to the builder

- Names of types, functions and the new `ShrinkReader` method.
- Exact wording of reactions and give-up reasons, within the shapes above.
- How the yield snapshot is tallied, within "current stall only".
- Splitting the work into commits and tasks.

## Calls made by the designer, to check

- The backup check runs only after a no-gain chunk that was not a yield, never before a chunk.
- Yield is classified before backup.
- The backup wait leaves the stall counters untouched and is not bounded by `percent_complete`.
- Two reactions per backup wait, not one per poll.
- `max_no_progress` default 15 as a backstop to the 30-minute budget.
- The yield snapshot is the driver's own, through `ActiveSessions`, not the engine's
  `.blocked.yaml` capture.
- A system object at the tail is reported as it is.

## Revision after review

The first version (`fff1037`) had a second lot, `unblock_tail`: an opt-in online rebuild of the
tail object found at give-up, then a resumed shrink. A panel of seven readers (agy, codex
twice, a Claude subagent, GLM 5.3, Mistral Large 4 twice) found it unsound in ways that were not
editing matters, and one finding struck its premise. Measured again by the designer on 2022:
successive `ALTER INDEX ALL ... REBUILD WITH (ONLINE = ON (WAIT_AT_LOW_PRIORITY ...))` of a
5 020-page table in a 74 752-page file placed it at pages 39 728 to 45 654, then 40 655 to
70 492 (near the end of the file), then 330 to 14 368, then 30 592 to 45 384. The allocator does
not move a rebuilt object toward the start of the file; it can move it toward the end. The field
round that gained 115.4 GB after a rebuild may owe part of that to where the allocator happened
to be. The lot is withdrawn to `docs/specs/TODO.md` with the panel's findings, to be redesigned
on another basis. It could not have fired on the field server anyway (Standard has no online
rebuild).

Lot-1 changes from the review: the yield is classified before the backup (a backup running
while the driver stops a chunk would otherwise hide the yield); a chunk's file size is read
after an error too; the yield snapshot names the session blocking the chunk, not only the ones
it blocks, and is the driver's own; the backup wait is stop-aware, emits two reactions instead
of one per poll, uses no `estimated_completion_time`, leaves the stall counters alone, and is no
longer cut off by a flat `percent_complete` (that rule was the designer's, not Rudi's "as long
as it runs"); the measured cost of a failed chunk is corrected; the budget arithmetic gives the
real trip point; the tempdb path and its spec are covered; the e2e test uses a backup loop.

## Appendix: the lab script

Run with `podman exec -i -e SQLCMDPASSWORD sql2022 /opt/mssql-tools18/bin/sqlcmd -S localhost
-U sa -C`. Setup, then a backup loop in one session and a chunk loop in another, started one
second apart.

```sql
-- setup: 1.5 GB free before 1.5 GB of data
CREATE DATABASE ShrinkBackupLab;
ALTER DATABASE ShrinkBackupLab SET RECOVERY SIMPLE;
GO
USE ShrinkBackupLab;
CREATE TABLE dbo.A (id int IDENTITY PRIMARY KEY, pad char(7000) NOT NULL DEFAULT 'a');
CREATE TABLE dbo.B (id int IDENTITY PRIMARY KEY, pad char(7000) NOT NULL DEFAULT 'b');
GO
INSERT dbo.A (pad) SELECT TOP (200000) 'a' FROM sys.all_columns c1 CROSS JOIN sys.all_columns c2;
INSERT dbo.B (pad) SELECT TOP (200000) 'b' FROM sys.all_columns c1 CROSS JOIN sys.all_columns c2;
GO
DROP TABLE dbo.A;
CHECKPOINT;
BACKUP DATABASE ShrinkBackupLab TO DISK = '/var/opt/mssql/data/sbl_full.bak' WITH INIT;  -- differential base
```

```sql
-- session 1: data backups for 70 s, alternating differential and full copy-only
SET NOCOUNT ON;
DECLARE @end datetime2 = DATEADD(second, 70, sysdatetime()), @i int = 0;
WHILE sysdatetime() < @end BEGIN
  SET @i += 1;
  IF @i % 2 = 0
    BACKUP DATABASE ShrinkBackupLab TO DISK = '/var/opt/mssql/data/sbl_diff.bak'
      WITH INIT, DIFFERENTIAL, NO_COMPRESSION, BUFFERCOUNT = 2, MAXTRANSFERSIZE = 65536;
  ELSE
    BACKUP DATABASE ShrinkBackupLab TO DISK = '/var/opt/mssql/data/sbl_copy.bak'
      WITH INIT, COPY_ONLY, NO_COMPRESSION, BUFFERCOUNT = 2, MAXTRANSFERSIZE = 65536;
END
```

```sql
-- session 2: 100 MB chunks for 65 s, one line per chunk
SET NOCOUNT ON;
DECLARE @end datetime2 = DATEADD(second, 65, sysdatetime()),
        @target int, @size int, @after int, @bk int, @t0 datetime2;
WHILE sysdatetime() < @end BEGIN
  SELECT @size = size / 128 FROM sys.database_files WHERE file_id = 1;
  SET @target = @size - 100;
  IF @target < 1600 BREAK;
  SELECT @bk = COUNT(*) FROM sys.dm_exec_requests
   WHERE command LIKE 'BACKUP%' AND database_id = DB_ID();
  SET @t0 = sysdatetime();
  DBCC SHRINKFILE (N'ShrinkBackupLab', @target) WITH NO_INFOMSGS;
  SELECT @after = size / 128 FROM sys.database_files WHERE file_id = 1;
  PRINT CONCAT(CONVERT(varchar(30), @t0, 126), ' size ', @size, ' -> ', @after,
               ' backup_running=', @bk, ' ms=', DATEDIFF(ms, @t0, sysdatetime()));
END
```

The single-chunk check that isolates the error: start one `BACKUP DATABASE ... COPY_ONLY,
BUFFERCOUNT = 1, MAXTRANSFERSIZE = 65536`, and one second later, in another session,
`SET LOCK_TIMEOUT -1; DBCC SHRINKFILE (N'ShrinkBackupLab', <size - 100>) WITH NO_INFOMSGS;`.
Observed: `Msg 3140, Level 16, State 5` after 2.04 s, the backup still running. For the log
case, `ALTER DATABASE ShrinkBackupLab SET RECOVERY FULL`, take a full backup, then loop
`BACKUP LOG` in session 1. Clean up: drop the database, delete `/var/opt/mssql/data/sbl_*.bak`,
`EXEC msdb.dbo.sp_delete_database_backuphistory 'ShrinkBackupLab'`.
