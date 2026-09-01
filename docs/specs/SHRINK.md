# SHRINK — file shrinking (data & log)

> Source of truth for the intended behavior of SqlGoPace's shrink feature.
> The raw analysis documents remain in `SQL Server Shrink - Document de référence
> technique - Perplexity.md` and `gemini-shrink.md` — both still in French, kept as
> historical source material; this document fixes the settled design.

## 1. Goal and scope

Add a `shrink` operation that reduces the physical size of a SQL Server database's files,
executed and monitored by the existing engine (queue `01.to_run` → `02.processing` →
`03.done`/`04.failed`, blocking and log monitoring, least-destructive reaction hierarchy).

Shrink is **not** a recurring maintenance operation. The tool runs it when explicitly asked
to (mass purge, table drops, scope reduction) and does everything it can to keep it
**non-blocking, measured, resumable**.

### v1 scope (this spec)

- `operation: shrink` (`type: data` | `type: log`), runnable from `01.to_run`.
- **Preflight estimation**: reclaimable space, no-op detection, automatic fallback to
  `TRUNCATEONLY` when the free space sits at the end of the file.
- **Chunked** data shrink with a calibrated, dynamically adjusted stepsize.
- Log shrink **by truncation** (no chunking), with recovery-model detection and a clean
  refusal when truncation is impossible.
- `files: all` expanded into one operation per file, run sequentially.
- Integrated monitoring + reactions (pause between chunks, self-wait, no-progress, blocking,
  log above threshold).

### Out of v1 scope (→ Phase 2, §12)

- Before/after fragmentation report and automatic generation of defragmentation manifests
  (post-shrink maintenance chaining; design settled in §12.1).
- `EMPTYFILE` mode + `ALTER DATABASE … REMOVE FILE`.
- Detection of shrinkable files on the `plan` subcommand side (planner).
- Pre-allocating/resizing the log in one block after a shrink (VLF control).

## 2. Operation model (YAML)

```yaml
- operation: shrink
  type: data            # "data" | "log"
  files: all            # "all" (every file of that type) | logical name of one file
  emptyfile: false      # reserved for Phase 2; must be false (or absent) in v1
  targetfreespace: 10%  # TARGET free space in the final file: "10%" or "100MB"
  identify_tail_object: false  # optional; run the tail-object walk at shrink start (§8.6)
  options:
    wait_at_low_priority: true   # 2022+ only (gated by the matrix); auto when absent
```

Fields:

- **`type`** (required): `data` or `log`. Determines which files are eligible and which
  algorithm applies (chunking for `data`, truncation for `log`).
- **`files`** (default `all`): `all` expands into one operation per file of the given
  `type` (see §6). Otherwise, the logical name of a file (`sys.database_files.name`).
- **`targetfreespace`**: free-space target, expressed **as a % of used space**.
  `N%` ⇒ `final_mb = ceil(used_mb × (1 + N/100))`. `N MB` ⇒ `final_mb = used_mb + N`.
  Always bounded by the `SpaceUsed` floor (§5.1).
- **`emptyfile`**: reserved for Phase 2. In v1, a `true` value is rejected at validation.
- **`options.wait_at_low_priority`**: an "auto" `*bool` (like the other options).
  `nil` ⇒ decided by the matrix (ON on 2022+). `ABORT_AFTER_WAIT` stays `SELF` unless
  `Policy.AllowAbortBlockers` is enabled globally (reuses the existing field).
- **`identify_tail_object`** (plain `bool`, default `false`): opt into a proactive
  tail-object walk at shrink start (§8.6). The reactive walk at give-up runs regardless of
  this flag. Data shrinks only; requires SQL Server 2019+.

**No `maxdop`**: `DBCC SHRINKFILE` takes no MAXDOP option. Any `maxdop` key under a shrink
is ignored (or rejected at validation).

`Validate`: `type` ∈ {data, log}; `targetfreespace` parsable (`%` or `MB`) and > 0;
`emptyfile` not `true` in v1.

## 3. Generated T-SQL grammar

Target official grammar:

```
DBCC SHRINKFILE
(
    { file_name | file_id }
    { [ , EMPTYFILE ]
    | [ [ , target_size ] [ , { NOTRUNCATE | TRUNCATEONLY } ] ]
    }
)
[ WITH
  {
      [ WAIT_AT_LOW_PRIORITY [ ( ABORT_AFTER_WAIT = { SELF | BLOCKERS } ) ] ]
      [ , NO_INFOMSGS ]
  }
]
```

Consequences for the generator (a **dedicated** generator, not the index `withClause`):

- `target_size` is an **integer in MB**.
- Shrink's `WAIT_AT_LOW_PRIORITY` **has no `MAX_DURATION`** (fixed ~1 min timeout) and is
  **not** nested inside `ONLINE = ON`. There is no `ONLINE` for DBCC.
- `TRUNCATEONLY`, `NOTRUNCATE` and `EMPTYFILE` are mutually exclusive with a move chunk;
  with `TRUNCATEONLY`, `target_size` is ignored by the engine.
- We add `NO_INFOMSGS` to cut down on noise.

Typical generated statements:

```sql
-- move chunk (one per iteration); WAIT_AT_LOW_PRIORITY only if resolved ON
DBCC SHRINKFILE (N'MyDb_Data', 8192) WITH WAIT_AT_LOW_PRIORITY (ABORT_AFTER_WAIT = SELF), NO_INFOMSGS;

-- truncate-only (phase A, §7); no page movement, no fragmentation
DBCC SHRINKFILE (N'MyDb_Data', TRUNCATEONLY) WITH NO_INFOMSGS;

-- log
DBCC SHRINKFILE (N'MyDb_Log', 512) WITH NO_INFOMSGS;
```

## 4. Version / edition detection (matrix)

Dedicated matrix entries, keyed by command type `shrink_data` / `shrink_log` × version × tier:

- `wait_at_low_priority`: eligible on **SQL Server 2022 (16.x) and later** only, for
  `shrink_data`. (Not relevant for `shrink_log`: no page movement, hence no Sch-M lock on
  IAM to wait for.)

`Resolve` produces the usual `Decision`s for `--explain`, **without** applying the
"WALP requires ONLINE" rule (which doesn't exist for DBCC). `ABORT_AFTER_WAIT = BLOCKERS`
is emitted only if `Policy.AllowAbortBlockers` is true; otherwise `SELF`.

## 5. Preflight

### 5.1 Data

Read `sys.database_files` + `FILEPROPERTY(name,'SpaceUsed')` (refresh as needed):

```sql
SELECT name, type_desc, file_id,
       size/128.0                                              AS size_mb,
       CAST(FILEPROPERTY(name,'SpaceUsed') AS INT)/128.0       AS used_mb,
       (size - CAST(FILEPROPERTY(name,'SpaceUsed') AS INT))/128.0 AS free_mb
FROM sys.database_files
WHERE type_desc = 'ROWS';
```

- **Floor**: a file cannot go below `used_mb`. The `final_mb` computed from
  `targetfreespace` (§2) is clamped to `max(final_mb, ceil(used_mb))`.
- **No-op**: if `free_mb ≈ 0` or `final_mb ≥ size_mb`, the operation is pointless → explicit
  skip (a "nothing to do" success, recorded in the report).
- **TRUNCATEONLY fallback**: we always try a truncate-only phase first (§7) — free and
  instantaneous — before moving anything.

### 5.2 Log

```sql
SELECT recovery_model_desc, log_reuse_wait_desc
FROM sys.databases WHERE name = DB_NAME();
```

Reclaimable floor = the last active VLF (`sys.dm_db_log_info`, `vlf_active = 1`): we cannot
truncate past it.

Decision by recovery model (**limited responsibility, settled decision**):

- **SIMPLE**: a `CHECKPOINT` (harmless) is allowed to free the VLFs, then shrink.
- **FULL / BULK_LOGGED**:
  - `log_reuse_wait_desc = NOTHING` ⇒ the log is already truncated (recent backup) → shrink.
  - otherwise (`LOG_BACKUP`, `ACTIVE_TRANSACTION`, …) ⇒ **bounded wait**, **not** an
    immediate refusal. SqlGoPace **never issues** a `BACKUP LOG` (it doesn't touch the backup
    chain), but it **lets the environment's scheduled log backup happen**: a wait loop
    (reusing the `awaitRelief`/`waitForRelief` pattern) re-reads `log_reuse_wait_desc` and the
    VLF floor on the log poll cadence, emitting a `pause` event with the reason on each cycle.
    - as soon as `reuse_wait` returns to `NOTHING` (backup done, trailing VLFs now inactive)
      → shrink.
    - past `log_reuse_wait_timeout` → **clean abandon** with the last observed reason.
  - Note: `LOG_BACKUP` / `ACTIVE_TRANSACTION` are transient (resolved by the backup job or by
    a transaction finishing). Structural reasons (`REPLICATION`, `AVAILABILITY_REPLICA`,
    `DATABASE_MIRRORING`) may never resolve: the same bounded wait applies, the reason
    reported on each cycle lets the operator interrupt, and the timeout guarantees we never
    hang indefinitely.

### 5.3 Permissions

`DBCC SHRINKFILE` requires `db_owner` (in the target database) or `sysadmin` —
`db_ddladmin` is not enough. Preflight **checks this proactively** (probe
`IS_ROLEMEMBER('db_owner') = 1 OR IS_SRVROLEMEMBER('sysadmin') = 1`) and emits a
`permissions` `Check`: a login without the right fails **before any DBCC**, with an
actionable message written by the tool, instead of an opaque runtime error. The same check
covers `check_db` (DBCC CHECKDB requires the same rights). The probe runs only once per
manifest, and only if a high-privilege operation is present.

## 6. `files: all` expansion

Like `index: ALL` (see `expand.go`): resolved against `sys.database_files` (filtered by
`type`: `ROWS` for data, `LOG` for log) into **one shrink operation per file**, executed
**sequentially**. Never shrink two files of the same filegroup in parallel (contention on
the system tables) — guaranteed for free by the engine's sequential execution.

## 7. Chunking driver (data shrink)

A **dedicated driver** in `internal/run` (alongside `MonitoredRunner`, not a generalization
of it), reusing `Executor` (`SPID`/`ExecDDL`/`Kill`), `ServerSampler`, `supervise`/`Pressure`,
and the **delta** waits pattern (`snapshotWaits`/`operationWaits`).

### 7.1 Algorithm

```
finalTarget := preflight.FinalTargetMB           // §5.1, clamped to the floor
startSize   := preflight.SizeMB

// Phase A — truncate-only (free, no fragmentation)
exec: DBCC SHRINKFILE(file, TRUNCATEONLY)
re-read size; if size <= finalTarget → done

// Phase B — move chunks
step := initialStep(startSize - finalTarget)      // heuristic §7.2
current := size
noProgress := 0
for current > finalTarget {
    next := max(current - step, finalTarget)
    t0 := clk.Now()
    action := runChunk(file, next, walp)          // §8: Continue | pause | abort
    if action == abort { return resumable }       // work is preserved
    elapsed := clk.Since(t0)

    newSize := readFileSizeMB(file)
    dWaits  := deltaWaits()                        // WRITELOG, PAGEIOLATCH_EX
    dLog    := deltaLogSpace()

    if newSize >= current {                        // no gain (cf. §8.3)
        noProgress++
        if noProgress >= maxNoProgress { return resumable } // or wait + retry
        wait backoff; continue
    }
    noProgress = 0
    step = adjustStep(step, elapsed, dWaits, dLog) // §7.2
    current = newSize
    logChunk(current, next, elapsed, step, dWaits, dLog)
}
```

**Phase A is monitored, but does not react.** On a large file the `TRUNCATEONLY` pass runs for
minutes, so it samples the server for its whole duration exactly as a chunk does: the progress
pump reports the server's `percent_complete` (`dm_exec_requests`) to the console, and the
blocking poll runs so the blocker and victim killers — which are consulted inside
`ServerSampler.Blocking` — can act on a session that blocks it. The samples are otherwise
discarded: **no pressure reaction applies to Phase A**. `TRUNCATEONLY` takes no
`WAIT_AT_LOW_PRIORITY` (§3), and aborting it stays the operator's call through a graceful stop,
which is clean and re-entrant (the space already released is preserved).

This gives the driver **two** monitoring postures, split on whether the statement is chunked:

| Statement | Samples (killers, progress) | Reacts to pressure |
|---|---|---|
| Phase B chunk (§7.1, §8) | yes | yes — pause between chunks, abort |
| Phase A `TRUNCATEONLY` | yes | no |
| Log shrink (§5.2) | yes | no |

The log shrink is a single unchunked statement like Phase A, so it takes the same posture and
the same code path (`runWatchedStatement`): it can be blocked (§8.2) and can block others, so
it must not run dark, but it has no chunk boundary to pause at. A graceful stop cancels either
one and the run stays re-entrant.

### 7.2 Stepsize calibration

Initial step by volume to reclaim (Perplexity §9.2), taking the low end of each band as a
prudent default (dynamic adjustment will raise it if the I/O keeps up):

| Volume to reclaim | initial step (default) |
|--------------------|-----------------------|
| < 5 GB             | 100 MB                |
| 5–50 GB            | 250 MB                |
| > 50 GB            | 500 MB                |

Adjustment between chunks, on **deltas** (never the cumulative values of
`sys.dm_os_wait_stats`):

- **Reduce** (`step/2`, bounded by `minStep`) if: `WRITELOG` avg > 10 ms, or
  `PAGEIOLATCH_EX` avg > 20 ms, or the supervisor stopped the chunk (it was blocking others
  past `max_block_minutes`, or the log was over cap).
- **Hold** if the chunk reached `targetBatchDuration`. That value is a **ceiling on growth,
  never a target**: a chunk longer than it is not corrected downward.
- **Increase** (`step * 5/4`, bounded by `maxStep`) otherwise — that is, whenever the chunk
  was not expensive. Growth is deliberately *not* also gated on near-idle latency.

The increase is additive-ish against a multiplicative decrease (AIMD) on purpose: recovering
one halving costs `log2 / log1.25 ≈ 3.1` clean chunks, so the loop trends upward while pressure
is rarer than about one chunk in three, and settles below the pressure threshold otherwise.

> **Superseded (v0.17.0).** Until then, increase required *both* `latency < 5 ms` **and**
> `elapsed < targetBatchDuration`. A multi-GB chunk takes minutes and `target_batch_seconds`
> defaulted to 5, so the second condition was unsatisfiable and every reduction was permanent:
> the step walked down to `minStep`, where the fixed per-invocation cost of `DBCC SHRINKFILE`
> dominates the work done. The gap between the 5 ms grow ceiling and the 10/20 ms reduce floors
> froze the step for any shrink living in between — which a shrink, being itself an I/O
> generator, usually does. `blocking of other sessions > 30 s` was never wired: `waitDeltas`
> never populated it. The `stopped` flag replaces it with something the supervisor measured.
>
> A residual, deliberate bound remains: a reduction is recoverable only while the resulting
> chunk finishes inside the ceiling. Above it the step holds where it landed, so the descent
> stops at "the step that takes about the ceiling" rather than at `minStep`.
>
> The full diagnosis, the alternatives weighed and rejected, and the TDD plan are recorded in
> [shrink-stepsize-aimd.md](shrink-stepsize-aimd.md).

### 7.3 Proposed defaults (`shrink:` block in `config.yaml`)

```yaml
shrink:
  initial_step_small_mb:  100   # volume to reclaim < 5 GB
  initial_step_medium_mb: 250   # 5–50 GB
  initial_step_large_mb:  500   # > 50 GB
  min_step_mb:             50    # below this, per-loop overhead dominates the gain
  max_step_mb:           1024    # cap so we don't saturate the I/O in one go
  max_chunk_seconds:      300    # ceiling on chunk duration: stops growth, never shrinks a step
  max_no_progress:          3    # consecutive chunks without gain before a clean stop
  no_progress_backoff_seconds:      30   # wait before retry, doubled on each no-progress
  no_progress_backoff_max_seconds: 300   # backoff cap (5 min)
  self_wait_timeout_minutes: 5   # max wait on Sch-M / snapshot before a clean stop (§8.2)
  log_reuse_wait_timeout_minutes: 30  # max wait for a scheduled BACKUP LOG to free the log (§5.2)
```

The knob was called `target_batch_seconds` and defaulted to 5 s while the original rationale held
— short chunks meant short reaction latency, because the driver could only react at a chunk
boundary. That is no
longer how it works: `runChunk` supervises the statement while it runs, `pumpServerProgress`
re-emits the server's own `percent_complete`, and the `max_block_minutes` cap applies *inside*
the chunk. A long chunk is therefore neither blind nor unstoppable, and shrink work is preserved
and re-entrant at any point. What is left of the knob is a growth ceiling, and 5 s was three
orders of magnitude away from the volume-based sizing `target_chunks` performs (1000 chunks × 5 s
= 83 min total, not a plausible budget for a multi-TB reclaim). Hence **300 s** from v0.17.0.

Because the meaning inverted, the key was renamed to **`max_chunk_seconds`** rather than
redefined in place: `KnownFields(true)` then rejects a config still carrying the old key, so an
operator who had pinned `target_batch_seconds: 5` learns at load time instead of silently
keeping the pre-fix behaviour (defaults only fill *absent* keys). `batch_dml` keeps its own
`target_batch_seconds`, where a duration objective is still the right one.

`log_reuse_wait_timeout_minutes` defaults to 30 min: room for one or two cycles of a common
log backup cadence (~15 min). The wait is free (the shrink hasn't started) and emits a
`pause` per cycle, so it is interruptible.

**Configurability**: a **global** block, **all fields optional** (absent ⇒ default applied,
like `MonitoringConfig`) — a `config.yaml` with no `shrink:` block works. **Global only,
never per-manifest**: these values depend on the instance's storage and SLA, not on the
operation; a manifest carries only its business `options:`. They are **starting points and
bounds** that the dynamic adjustment (§7.2) moves around; the spirit stays "self-calibrating",
and an operator only touches them for atypical storage.

Band thresholds (`< 5 GB`, `5–50 GB`) are documented constants (not exposed). The
cross-cutting reaction durations (`blocking_timeout_minutes`, `log_drain_timeout_minutes`,
`kill_grace_seconds`) stay those of `MonitoringConfig`, reused by the driver.

## 8. Reactions and pressure

Shrink is the **safest** operation to interrupt: each internal batch (~32 pages) is a clean
transaction; stopping the shrink **preserves the work already done** and it is re-entrant
(re-running toward the same target picks up where it left off). So the least destructive
reaction isn't even a cancel.

### 8.1 "Free" pause between chunks

Under pressure (blocking of other sessions past the timeout, or log above the threshold),
the driver **doesn't launch the next chunk** and waits for relief (reusing the
`awaitRelief`/`waitForRelief` logic and `logDrainTimeout`). No rollback: the work of the
previous chunks is already committed. This is gentler than a rebuild's pause/resume.

Reused `ReactionSink` events: `pause` (waiting), `resume` ("pressure cleared"), `abort`
(clean stop, work preserved). A running chunk that has to be stopped goes down the same path
as `runStatement`: soft cancellation via the execution connection's `context`, then a
**`KILL` fallback issued from the monitoring pool** (see §8.5).

### 8.2 Self-wait (a new pressure dimension)

A shrink can be **blocked by** other sessions (RCSI/SI snapshot transactions → messages
5202/5203; `LCK_M_SCH_M` wait). The current model only covers `Pressure.BlockingOthers`. Add
"we are waiting" detection via **our own** session's waits (`sys.dm_exec_requests` /
`SessionWaits` on our SPID):

- prolonged `LCK_M_SCH_M` (or `LCK_M_SCH_M_LOW_PRIORITY`), or blocking by a snapshot:
  prefer **waiting** (the shrink will resume), then a **clean stop** if the wait exceeds a
  threshold (the work is preserved).

### 8.3 No-progress / silent WALP 49516 timeout

Under `WAIT_AT_LOW_PRIORITY`, if the Sch-M lock isn't acquired within ~1 min, the shrink
**finishes with no visible error and without having done anything** (error 49516 appears only
in the SQL Server log). So the return code can't be trusted. Detection by **size comparison**:
if `newSize >= current` after a chunk (49516, or data at the end of the file that can't be
moved), increment a no-progress counter → backoff + retry, then a clean stop past
`maxNoProgress`.

### 8.4 Log during a data shrink

A data shrink **generates log itself** (every moved page is logged). The existing `Log`
sampler (`LogOverCap` threshold) applies as-is: above the threshold, pause between chunks and
reduce the stepsize for the next chunk.

### 8.5 KILL of one's own session — critical implementation detail

You cannot `KILL` your own session from the connection executing the DDL. `Conn` keeps
**two** connections: `exec` (pinned, stable `@@SPID`) and `pool` (monitoring). `Conn.Kill`
issues `KILL <spid>` **on the pool**, never on `exec`. The shrink driver **must** go through
the `Executor` interface (`ExecDDL` on `exec`, `Kill` via the pool) to inherit that
guarantee; do not open a parallel execution path that would bypass this split.

### 8.6 Tail-object identification

`DBCC SHRINKFILE` relocates the highest allocated pages toward the front of the file, so the
object owning the **last allocated page** is the binding constraint: it decides how far the
file can shrink and what the move contends on. When it can't be moved cheaply (a heap, a LOB
allocation, a page pinned by another session), the shrink stalls short of target — often with
**no blocking victim** for the `Sch-M`-lock capture (§8.3) to record.

`FindTailObject` (`internal/mssql`) closes that gap: a bounded backward walk over
`sys.dm_db_page_info` from the file's last page down to the first page owning a user object
(§13). The look-back is capped at the file's free-page count plus a margin, with an absolute
backstop, so a mostly-free file can't drive an unbounded scan; free/allocation-bitmap pages
(`object_id IS NULL`) are skipped. It takes only brief page latches, never transaction locks.

Two triggers (`ShrinkRunner.walkTail` finds; `emitTail` logs and, optionally, records):

- **Reactive (always on):** `captureGiveUpTail` runs once when `chunkLoop` gives up ("no
  further progress") for a data file — the definitive "couldn't get past the tail" moment — and
  records the result. Not run on a graceful stop / `ErrStopped` (that resumes later).
- **Proactive (`identify_tail_object: true`):** runs once at `chunkLoop` entry, **after**
  `TRUNCATEONLY` (so the free-page bound reflects the post-truncate size and the last
  allocated page is the real one). Its finding is **logged, then stashed** on the `tailProbe`
  and *recorded* only if the shrink later misses target (`shrinkData` checks `FinalMB > TargetMB`
  after `chunkLoop`) — a tail object a successful shrink relocated was never a blocker, so
  recording it would plant a false `tail_position` entry that misleads `plan --confirmed`. When
  proactive has already stashed a finding, the give-up path reuses it rather than walking again.

Gating: SQL Server **2019+** only (`sys.dm_db_page_info`); below that the walk is skipped —
**silently** for the always-on reactive path (a run that never opted in is not nagged), and with
**one `warn` per operation** only for the proactive (opt-in) path. `sys.dm_db_page_info` needs
only `VIEW DATABASE STATE` (`VIEW DATABASE PERFORMANCE STATE` on 2022+), already covered by the
`VIEW SERVER STATE` the monitoring requires — **no `DBCC PAGE`** (it would need `sysadmin`).
Tempdb (`prof != nil` in the shared `chunkLoop`) and log shrinks never walk.

A recorded tail object is emitted as a `ReactionEvent{Tail: *TailFinding}`; the engine sink
records it into the same `<manifest>.contended.yaml` accumulator as the lock capture, tagged
`confirmed_by: tail_position` (with `index_id` and `page_from_end`, and `first_seen`/`last_seen`
timestamps). If the same object was also lock-captured, the entry is upgraded to `tail_position`
while its lock stats are preserved. `plan --confirmed` promotes tail-position blockers ahead of
lock-confirmed ones (and, among tail entries, the one closest to the file end first).

### 8.7 Concurrency with index maintenance

A shrink stalled behind a concurrent `ALTER INDEX` (rebuild or reorganize) or `DBCC`
(indexdefrag, another shrink, ...) is not a structural tail blocker — it's a maintenance job
that will finish and release the file's tail on its own. The driver doesn't special-case its
reaction: the same yielding it already does for any self-wait (§8.2) or no-progress stall
(§8.3) applies — bounded backoff, then a clean stop with work preserved and the manifest
re-queued. It never waits indefinitely on the blocker, and it never `KILL`s it.

What changes is recognition and reporting. The self-block is only observable **while a chunk is
executing** — that is the one moment the shrink's session appears in `sys.dm_exec_requests` with
a `blocking_session_id`; between chunks it holds no lock request and is invisible. So a
`pumpSelfBlock` sampler runs in-flight during each `runChunk` (alongside the progress pump),
takes an `ActiveSessions` snapshot and runs it through `FindSelfBlock` (our own SPID's direct
blocker) and `IsMaintenanceCommand` (an allow-list of `ALTER INDEX`/`DBCC` verbs read off
`sys.dm_exec_requests.command`), and hands the first maintenance-block observation back to the
chunk loop. Both reads need only `VIEW SERVER STATE` — already required for monitoring — so the
recognition works below SQL 2019 too, unlike the page-walk in §8.6. Once the shrink has stalled,
that stashed observation is promoted (`applyMaintBlock`): it emits one `warn` per operation
naming the verb, the blocking session, and the elapsed wait (e.g. "shrink of ... blocked by a concurrent maintenance operation — ALTER
INDEX on session 62 (waiting 2m31s). Transient; SqlGoPace is yielding, not forcing. Re-run
after maintenance completes."), through the same `ReactionSink` that feeds the `.log` and the
TUI, and a give-up while blocked states the same in its stop reason.

If the shrink then gives up while a maintenance block was seen, the tail object the give-up
walk (or an already-stashed proactive walk, §8.6) found is recorded with
`confirmed_by: transient_maintenance` instead of `tail_position`, plus `blocked_by_command` and
`blocked_by_spid` naming the operation and session that pinned it. `plan --confirmed` skips
`transient_maintenance` entries entirely (`confirmedSetFor` filters them before they ever reach
`DecidePreShrink`) — a rebuild that happened to be running is never mistaken for the kind of
structural blocker a pre-shrink reorganize should target. Below SQL 2019, or when the tail walk
itself fails, there is no sidecar entry at all — only the `.log`/TUI message; the give-up reason
still names the blocker, there's just nothing to feed back into the planner.

## 9. Progress, reporting, TUI

- **Deterministic progress** (an advantage of chunking, unlike the fluctuating
  `percent_complete` of `dm_exec_requests`): `(startSize - currentSize) / (startSize - finalTarget)`.
  To be fed into the TUI's `Model` in place of `operationPercent` for shrink.
- **Per-chunk log**: target aimed for, size obtained, duration, stepsize, waits/log deltas.
- **`.log` report + history**: `initial_size`, `final_size`, space reclaimed, total duration,
  chunk count, reactions, and the `--version` that produced the run (as everywhere else).

**Contended-object capture.** Whenever a shrink blocks other sessions while relocating an object —
regardless of the run's final outcome — the engine writes `<manifest>.contended.yaml` next to the
run report, listing the objects it held a `Sch-M` lock on while blocking others — empirically
confirmed tail blockers. The `.log` report carries a one-line pointer to it. `sqlgopace plan --confirmed <path>`
reads the sidecar back into the next pre-shrink pass, prioritizing those objects' reorganizes and
marking matching heap advisories `CONFIRMED`. Full design in
[`docs/specs/superpowers/specs/2026-07-28-contended-object-capture-design.md`](superpowers/specs/2026-07-28-contended-object-capture-design.md).

## 10. Recovery / re-entrancy

An interrupted shrink (engine crash, connection loss) is trivially resumable: re-running
`DBCC SHRINKFILE` toward the same target picks up where it stopped (committed work
persists). The existing database-aware `Recoverer` applies: an orphan whose database is
unreachable (e.g. now an AG secondary) is left for a later run. No `abort-shrink` subcommand
is needed (the operation is already safe to stop).

## 11. Errors and messages to know

| Code / signal | Type | Meaning | SqlGoPace action |
|---------------|------|---------------|------------------|
| 5202 / 5203 | Informational (SQL log) | Shrink blocked by a snapshot transaction | Self-wait (§8.2): wait, then clean stop |
| 49516 | Level 16 error (SQL log) | WALP timeout, Sch-M not acquired | No-progress (§8.3): backoff + retry |
| 9002 | Error | Transaction log full | Pause + reduce the stepsize (§8.4) |
| No reduction | Normal | No free space, or not at the end of the file | TRUNCATEONLY already tried; no-progress → clean stop |
| Log not shrinkable | Normal | Active VLFs at the end, or `log_reuse_wait ≠ NOTHING` | Bounded wait for the log to free up (§5.2), never a BACKUP LOG; clean abandon at timeout |
| Insufficient rights | Error (DBCC) | Login not `db_owner`/`sysadmin` on the database | **Caught in preflight** (§5.3): failing `permissions` `Check`, actionable message, before any DBCC |

## 12. Phase 2 (out of v1)

- **Before/after fragmentation & maintenance chaining**: `sys.dm_db_index_physical_stats`;
  compare and **generate `rebuild_index`/`reorganize_index` manifests in `01.to_run`**
  (reusing the pipeline and the planner); **post-shrink** automation. Design settled in §12.1.
- **`EMPTYFILE`**: migrate the content to the filegroup's other files, then
  `ALTER DATABASE … REMOVE FILE`.
- **Detection on the `plan` side**: spotting shrinkable files, like maintenance does.
- **Log pre-allocation** after a shrink (a single `ALTER DATABASE … MODIFY FILE`) to control
  the VLF count.

### 12.1 Post-shrink maintenance chaining (design settled for Phase 2)

**The need.** A **data** shrink fragments indexes by construction (pages are moved toward the
front of the file, which scrambles the indexes' physical order). It is therefore natural to
want to **follow it with a defragmentation**. The question raised: "add an option to the
`shrink` operation to automatically chain a `plan --auto`?".

**Layering decision — this is NOT a field of the `shrink` operation.** Three reasons settle
this choice:

1. **An `operation` is a declarative, single-object DDL unit** (parse → resolve → generate
   → plan). Chaining "do X then generate/execute Y" is **orchestration**, which lives in
   `cmd/sqlgopace` and the engine, not in an operation's definition. Putting a
   `maintain_after: true` in the shrink's YAML would mix the two levels.
2. **No `run → maint` coupling.** Today `internal/run` does not import `internal/maint`
   (pure-core / planner separation). Calling the planner from `processOne` would introduce
   that coupling into the engine's hot path. To be avoided.
3. **The seam already exists.** `--auto` (`runAuto`, `cmd/sqlgopace/plan.go`) *is* the
   "analyze the connected database → generate maintenance manifests in `to_run` → let the
   engine process them like any other manifest" mechanism. We **reuse** it, we don't
   duplicate it.

**Decisive SQL Server pitfall — `REBUILD` grows the file back.** An `ALTER INDEX … REBUILD`
needs working space ≈ the size of the largest index/partition being rebuilt; on a file we
have just shrunk, it **grows the data file back** and can therefore **undo part of the
reclaimed space**. `REORGANIZE` is in-place (no notable re-growth). Direct consequence for the
design: post-shrink chaining **biases toward `REORGANIZE` by default**, and `REBUILD` is
emitted only on the operator's **explicit opt-in**, accompanied by the warning "a rebuild
grows the file back". This is exactly the kind of trade-off that must **stay reviewed by a
human** rather than hidden behind an operation boolean.

**Chosen shape.** Orchestration at the **run** level, not the operation level, and a
**reviewed manifest, not a silent execution**:

- A run flag, e.g. `--maintain-after-shrink` (name to be confirmed at implementation).
- **Scope**: applies only after a successful **`data`** shrink (a `log` shrink doesn't
  fragment indexes → nothing to chain). Restricted to the **databases actually shrunk** in
  the run.
- **Post-shrink measurement**: fragmentation is read **after** the shrink finishes (the real
  post-page-movement state), via the existing planner (`internal/maint` +
  `dm_db_index_physical_stats`).
- **Generation**: the planner produces a defrag manifest dropped into `to_run/` (reusing
  `runAuto`/`writeManifests`), **biased toward `REORGANIZE`** (see the pitfall above).
- **Reviewed by default**: the generated manifest **waits in the queue** for review. It only
  **executes** if the run is already in `--auto` (consistent with the "`--auto` = unsupervised"
  semantics). Without `--auto`, we've dropped a reviewable manifest, nothing more.
- **`REBUILD`**: only if an explicit opt-in is given (e.g. `--post-shrink-allow-rebuild`, or a
  dedicated `reorganize-only` maintenance profile deliberately lifted). Otherwise,
  `REORGANIZE` only.

**What this reuses (nothing new on the execution side).** The planner (`internal/maint`), the
`ddl`/`render` pipeline, `writeManifests`, and the existing engine + monitoring + reactions.
No new execution path and no new dependency in the pure core: the addition is confined to the
`cmd/sqlgopace` layer (the flag, the call to the planner after the shrink, writing the
manifest).

**Rejected alternative.** A `maintain_after` field on the `shrink` operation: rejected for the
layering reasons (1–3) and because it would make auto-rebuild too easy to trigger without
review, contradicting the re-growth pitfall.

## 13. Reference queries

### Free space in data files
```sql
SELECT name, type_desc,
       size/128.0                                              AS size_mb,
       CAST(FILEPROPERTY(name,'SpaceUsed') AS INT)/128.0       AS used_mb,
       (size - CAST(FILEPROPERTY(name,'SpaceUsed') AS INT))/128.0 AS free_mb
FROM sys.database_files;
```

### Active portion of the log (reclaimable floor)
```sql
SELECT file_id, vlf_begin_offset, vlf_size_mb, vlf_sequence_number, vlf_active, vlf_status
FROM sys.dm_db_log_info(DB_ID())
WHERE vlf_active = 1;

SELECT total_log_size_in_bytes/1048576.0 AS total_log_mb,
       used_log_space_in_bytes/1048576.0 AS active_log_mb,
       used_log_space_in_percent         AS active_pct
FROM sys.dm_db_log_space_usage;
```

### Reason the log can't be truncated
```sql
SELECT name, recovery_model_desc, log_reuse_wait_desc
FROM sys.databases WHERE name = DB_NAME();
```

### Tail object of a data file (§8.6; SQL Server 2019+)
Walk backward from the last page of a data file to the first page owning a user object —
the object `DBCC SHRINKFILE` must relocate past. `FindTailObject` bounds the loop by the
file's free-page count (with an absolute backstop); the illustrative shape:
```sql
DECLARE @file_id int = 1, @page_id int;
SELECT @page_id = CAST(size AS int) - 1 FROM sys.database_files WHERE file_id = @file_id;
WHILE @page_id >= 0
BEGIN
    IF (SELECT object_id FROM sys.dm_db_page_info(DB_ID(), @file_id, @page_id, 'LIMITED')) IS NOT NULL
    BEGIN
        SELECT OBJECT_SCHEMA_NAME(object_id) AS [schema], OBJECT_NAME(object_id) AS [object],
               index_id, object_id
        FROM sys.dm_db_page_info(DB_ID(), @file_id, @page_id, 'LIMITED');
        BREAK;
    END
    SET @page_id -= 1;
END
```

### Native progress (diagnostic; we prefer per-chunk progress, §9)
```sql
SELECT session_id, command, percent_complete,
       estimated_completion_time/1000/60 AS est_min_left,
       wait_type, blocking_session_id
FROM sys.dm_exec_requests
WHERE command IN ('DbccFilesCompact', 'DbccSpaceReclaim');
```

### Critical waits (to be read as deltas)
```sql
SELECT wait_type, wait_time_ms, waiting_tasks_count
FROM sys.dm_os_wait_stats
WHERE wait_type IN ('PAGEIOLATCH_EX','PAGEIOLATCH_SH','WRITELOG',
                    'LCK_M_SCH_M','LCK_M_SCH_M_LOW_PRIORITY');
```

## 14. Still-open points

- No blocking point. Settled conventions: `targetfreespace` as a % of **used** space
  (`final = ceil(used × (1 + N/100))`, §2); driver defaults frozen (§7.3). Those defaults
  still need empirical validation during the e2e tests (real I/O calibration).
- **Post-shrink maintenance chaining**: design settled (§12.1) — orchestration at the run
  level (`--maintain-after-shrink` flag) reusing `--auto`, a reviewed manifest dropped into
  `to_run`, biased toward `REORGANIZE` (`REBUILD` grows the file back), **not** an operation
  field. Still to confirm at implementation: the exact flag name and the shape of the
  `REBUILD` opt-in.
