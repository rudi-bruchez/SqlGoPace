# Shrinking files

`shrink` and `shrink_tempdb` are the two operations that are not a single statement. They
are drivers: they read the file's space at run time, run a free `TRUNCATEONLY` pass, then
move pages in calibrated chunks, adjusting the chunk size from the I/O and log waits each
chunk produced.

Because every internal batch commits, a shrink can be stopped at any moment with no
rollback, and it is re-entrant: re-running toward the same target resumes where it left off.

## `shrink`

```yaml
# Reclaim space from all data files, leaving about 10% free above what is used
- operation: shrink
  type: data                   # data | log
  files: all                   # all, or a logical file name
  targetfreespace: 10%         # "N%" or "N MB"
  identify_tail_object: true   # optional; 2019+
  options:
    wait_at_low_priority: true # 2022+ only, matrix-gated; auto if omitted

# Reclaim a specific log file down to about 50 MB of free space
- operation: shrink
  type: log
  files: MyDb_Log
  targetfreespace: 50MB
```

| Field | Default | Meaning |
|---|---|---|
| `type` | required | `data` or `log`. Selects both the eligible files and the algorithm: chunked page move for data, truncation for log. |
| `files` | `all` | A logical file name from `sys.database_files.name`, or `all` to shrink every file of that type one at a time, never two of a filegroup in parallel. |
| `targetfreespace` | required | Free space wanted in the final file: a percent of used space (`N%` gives final ≈ used × (1 + N/100)) or an absolute `N MB` (final ≈ used + N). Always clamped to the floor the file can actually reach: its used space, or the active VLFs for a log. |
| `identify_tail_object` | false | Name the object owning the file's last allocated page up front. See below. |
| `emptyfile` | — | Reserved for a future release; `true` is rejected. |

`options.wait_at_low_priority` is auto by default. On SQL Server 2022 and later it is
injected for data shrinks so the schema-modify lock waits at low priority instead of
blocking queries. It does not apply to log files, and `DBCC SHRINKFILE` takes no `MAXDOP`.

### What it does that a bare DBCC does not

- **`TRUNCATEONLY` first, always.** If the free space is already at the end of the file it
  is reclaimed instantly, with no page movement and therefore no fragmentation.
- **A clean no-op.** Nothing to reclaim, or a target not below the current size, is
  reported as a successful "nothing to reclaim" rather than an error.
- **Log files wait for your backup, they do not take one.** In `SIMPLE` recovery a
  `CHECKPOINT` is issued and the log is shrunk. In `FULL` or `BULK_LOGGED`, if the log
  cannot yet be truncated because it awaits a log backup, SqlGoPace waits, bounded by
  `log_reuse_wait_timeout_minutes`, for the environment's scheduled backup to free it. It
  never issues `BACKUP LOG` itself, and abandons cleanly with work preserved if the wait
  times out.
- **Reactions between chunks.** Under blocking or log pressure the driver pauses between
  chunks, which is free because committed work is kept, and shrinks the next chunk smaller.

A data-file shrink fragments indexes by design. Rebuild or reorganize afterwards if needed;
[`maintenance-planner.md`](maintenance-planner.md) can generate the pre-shrink reorganize
pass for you.

### Recording what it could not get past

A shrink records the objects it could not move past into `<manifest>.contended.yaml`, by
two complementary means, each tagged with how it was established:

| `confirmed_by` | Established by |
|---|---|
| `lock` | The shrink blocked other sessions while relocating an object, whatever the run's final outcome. An empirically confirmed tail blocker. |
| `tail_position` | The tail-object walk named the object owning the file's last allocated page, without needing to block anyone. |
| `transient_maintenance` | The blocker was a concurrent `ALTER INDEX` or `DBCC`, reported as transient rather than structural. `plan --confirmed` ignores these. |

The tail-object walk runs automatically when a data shrink gives up short of target, and,
with `identify_tail_object: true`, once at the start of each data shrink. In that second
case it is logged for visibility but only *recorded* as a blocker if the shrink then fails
to reach target: a tail object a successful shrink relocated was never a blocker.

It requires SQL Server 2019 or later (`sys.dm_db_page_info`). Below that it is skipped,
silently for the automatic give-up walk and with a one-line warning when you asked for it
explicitly. It never runs for log or tempdb shrinks.

This closes the common case of a shrink stalling with no blocking victim at all: data
pinned at the file end, a `WAIT_AT_LOW_PRIORITY` timeout, and nothing in the blocked list
to explain it.

The sidecar is machine-readable, moves with the manifest on finalize, and the `.log` gets a
one-line pointer to it. Feed it into the next planning pass with
`sqlgopace plan --confirmed <path>`; tail-position blockers are promoted ahead of
lock-confirmed ones, being the definitive constraint on how far the file can shrink.

## `shrink_tempdb`

A dedicated operation for tempdb's data files. There is no `database:` field because the
operation *is* tempdb.

```yaml
- operation: shrink_tempdb
  targetsizemb: 20480    # every tempdb data file is shrunk to 20 GB
  flushcaches: false     # opt-in escalation on a persistent stall
```

| Field | Default | Meaning |
|---|---|---|
| `targetsizemb` | required, > 0 | The common absolute target in MB, applied to every tempdb data file. A file whose used space already exceeds the target stops at that floor rather than failing. |
| `flushcaches` | false | Opt-in cache-flush escalation, used only when a file's shrink stalls persistently. Off by default because it has a real, if narrow, performance cost. |

It needs `sysadmin`, and it is the only operation that does: `DBCC SHRINKFILE` for tempdb
runs in tempdb, so `db_owner` of a user database does not carry, and tempdb is recreated
from model at every restart so a membership granted there does not survive one.

### What it deliberately is not

- **Not a monitor.** This is a maintenance operation, not tempdb surveillance. There is no
  continuous tracking of what fills tempdb and no query-plan capture. The run report lists
  the blockers observed *while shrinking*, which is incident reporting on the operation
  itself.
- **Not a guaranteed shrink.** Internal objects held by live queries, work tables, sort and
  hash spills, the version store, can pin pages at the end of a file and refuse to move. The
  operation does its best and stops cleanly when it cannot go further, reporting so plainly.
  Bringing a 400 GB tempdb down to 20 GB live is often impossible without a restart, and
  that is expected.
- **Data files only.** The tempdb log is out of scope.

### It never kills a blocker

Live sessions blocking a tempdb shrink are always waited out, never killed: they are
legitimate application queries. Where available, on SQL Server 2022 and later only, the
driver adds `WAIT_AT_LOW_PRIORITY (ABORT_AFTER_WAIT = SELF)`, which makes *our* chunk yield
and retry and never aborts the blocker. On 2019 the matrix disables that option for
`shrink_tempdb`, so the reaction degrades to a plain bounded wait and a clean give-up.

### The `flushcaches` trade-off

When a file's shrink shows no progress repeatedly, a no-gain chunk, `Msg 5240` "work table
page could not be moved", or error 845 buffer-latch timeout, the driver first backs off and
retries: these conditions often clear on their own as transient tempdb objects age out.

If the stall persists past `no_progress_before_flush` *and* `flushcaches: true`, it issues
one targeted escalation, at most once per run across all files:

```sql
CHECKPOINT;
DBCC FREESYSTEMCACHE ('Temporary Tables & Table Variables');
```

That frees only the temp-object cachestore, cached temp tables and table variables that can
pin tempdb pages, after stabilising state with a `CHECKPOINT`. It deliberately does not
reach for the sledgehammers a naive "soft restart" recipe uses:

- `DBCC FREESYSTEMCACHE ('ALL')` and `DBCC FREEPROCCACHE` empty the whole plan cache
  instance-wide, triggering a recompilation storm and a CPU spike that can time out
  application connections. Too costly to fire automatically.
- `DBCC DROPCLEANBUFFERS` empties the buffer pool for zero tempdb gain.

Widening the flush behind an `aggressive` flag is a possible future escalation and is
deliberately not in this version.

### The unbalanced-files warning

If the data files do not all end at the same size, some clamped to their used floor and
some stalled above target on pinned pages, the report warns.

Uneven tempdb files defeat SQL Server's proportional-fill allocation: a file that later
frees its pinned pages ends up with far more free space than the others, so new allocations
skew toward it and concentrate `PAGELATCH` contention on a single file. The warning is a
signal to follow up with a re-run or manual intervention. SqlGoPace does not force a common
floor by under-shrinking every file to match the worst one.

### A side benefit worth knowing

`DBCC SHRINKFILE` below a file's *created* size also corrects that file's boot size in
`sys.master_files`. Besides reclaiming disk now, a successful shrink undoes a manual
`ALTER DATABASE … MODIFY FILE (SIZE = …)` bump made during an incident, so tempdb comes back
at the right size on the next restart.

## Tuning

Chunk sizing, backoff and timeouts are global, in the `shrink:` block of `config.yaml`,
because they depend on the instance's storage and SLA rather than on any manifest. Every
key defaults; see [`configuration.md`](configuration.md#shrink).
