# Running

How a run is driven, what the queue does with your manifests, and what a re-run repeats
after each way a run can end.

## The two connections

At runtime the orchestrator holds two connections, and the split is what makes the tool
work at all:

- an execution connection, dedicated and pinned, that runs the DDL;
- a monitoring connection that polls locking, blocking, transaction-log pressure and the
  operation's progress.

The monitoring connection is why `VIEW SERVER STATE` is not optional. Without it the
sampling loop fails on every run, including one that blocks nobody.

The execution connection is pinned, not immortal. Stopping a statement — a cancel
reaction, or the abort that pauses a resumable operation — sends an attention, and an
attention the driver cannot complete leaves that connection unusable. It is checked
before the next statement and re-pinned when it is broken; the new session is hardened
and re-stamped with the run marker before anything runs on it, and a DDL operation records
`warn: execution connection re-pinned: SPID a -> b` in the `.log` — a shrink and a batched
DML are repaired the same way but do not yet say so.

Before pinning a new session the run makes sure the old one has actually stopped. The client
abandoning a connection does not mean SQL Server stopped the statement on it, so the old
session is identified by its `login_time`, killed if it is still ours and still running, and
waited on for up to two minutes. If it will not stop, the operation **fails** naming that
session — the run refuses to issue DDL beside a request that still holds its locks. A session
id that has since been given to another connection is never killed.

Two consequences worth knowing: the session id in a report is the one that was live at the
time and can change mid-run, and the `--tui` header follows it. A re-pin costs one operation
nothing — before 0.33.0 it cost every operation left in the manifest.

## The queue

```
01.to_run/  →  02.processing/  →  03.done/    (success, with a .log beside it)
                              ↘   04.failed/   (failure, with a .log)
```

Manifests are claimed in name order, so number your files. Each one moves to
`02.processing/` while it runs, then to its terminal directory with a `.log` sidecar
recording every statement, decision and reaction.

A run opens one engine per database the queue targets, sequentially, so at most one heavy
DDL runs server-wide at a time.

**One run per queue.** A run takes an exclusive lock on `02.processing/` and holds it until
it exits; a second run against the same processing directory refuses to start and names the
process holding it. This is not tidiness. Crash recovery sweeps `02.processing/` before
anything is claimed, and decides an abandoned manifest is dead by looking for a running
request on its session — which a *live* run does not have while it waits for relief, sits
between shrink chunks, or moves between operations. Without the lock, a cron tick landing in
one of those windows would requeue and re-run work that was still in flight.

The lock is an OS file lock, so a run that is killed leaves nothing to clean up: the next
run takes the lock and recovers normally. Only a queue on a filesystem that does not honour
locks (an NFSv3 share) is unprotected.

**What the lock does and does not buy.** Two runs on *different* processing directories never
**sweep** each other, which is the race that matters: neither can requeue the other's
in-flight manifest. They are not otherwise isolated. If they also share `01.to_run` they both
discover the same manifests, and each one is claimed by an atomic rename — so it runs exactly
once, on whichever run got there first. The other reports
`skip <name>: claimed by another run on this queue` and counts it as **skipped**, not failed,
so a shared queue does not produce a non-zero exit for work the peer did correctly.

Give the second schedule its own `01.to_run` as well if you want two genuinely independent
queues.

## Setting up a directory

`init` writes everything a run needs into the current directory, or into `--dir`:

```bash
sqlgopace init
```

That is `config.yaml`, `ddl_compatibility.yaml`, `maintenance_profile.yaml`, a
`.env.example`, the four queue directories, and an example manifest disabled by a leading
dot. The templates are compiled into the binary, so a downloaded executable is enough; no
clone, no network.

It never overwrites: an existing file is reported and left alone, which makes it safe to
re-run against a directory you have already configured. `--force` restores the shipped
template over what is there.

## Modes

```bash
# Drain the queue, silently, tracing everything to the .log sidecars
sqlgopace --config config.yaml

# The same run, with the interactive incident console
sqlgopace --config config.yaml --tui

# Render the T-SQL for one manifest without executing or locking anything
sqlgopace --config config.yaml --dry-run 01.to_run/010_rebuild.yaml

# ... and say why each option was injected or dropped
sqlgopace --config config.yaml --dry-run --explain 01.to_run/010_rebuild.yaml

# Offline: no connection at all, assume a version and edition
sqlgopace --dry-run --assume-version 16 --assume-edition enterprise 01.to_run/010_rebuild.yaml
```

An offline dry run has no database context, so restrictions that depend on one cannot be
evaluated: `RESUMABLE` is refused in `tempdb`, and an offline plan cannot know it is
heading there. The output says so. A manifest that names its own `database:` still gets
those restrictions applied.

A dry run, and the run's `.log`, also flag the operations whose cancel rolls back all their
work; see [blocking-and-kills.md](blocking-and-kills.md#rollback-on-cancel-operations).

## Flags

| Flag | Effect |
|---|---|
| *(none)* | Silent run; everything is traced to a `.log` beside each processed manifest. |
| `--config <path>` | Config file. Required to run. |
| `--tui` | Interactive incident console (see below). |
| `--dry-run` | Render the final T-SQL without executing or taking a lock. |
| `--explain` | With `--dry-run`, show why each option was chosen, and list any `ignore_blocked_sessions` rules. |
| `--assume-version <n>` | Offline dry-run target major version, for example `16` for SQL Server 2022. |
| `--assume-edition <t>` | Offline target tier: `enterprise`, `standard`, `express`, `azure`. |
| `--matrix <path>` | Override the compatibility matrix path. |
| `--auto` | Analyse and run generated maintenance unattended. See [`maintenance-planner.md`](maintenance-planner.md). |
| `--database`, `--databases`, `--all-databases`, `--categories`, `--profile` | Scope for `--auto`. Same meanings as on `plan`. |
| `--version` | Print the version and exit. |

## The incident console

`--tui` replaces the silent run with a live console: the running operation and its
progress, the sessions it is blocking, the sessions blocking it, and the reaction feed.

The operations panel is titled with the manifest being run. The `op i/N` counter restarts at 1
for each manifest, so the counter says where the run is inside one and the title says which.

A session this run blocks appears in the blocked-sessions panel on the poll that sees it
(`blocking_poll_seconds`), with how long it has waited on the row. That wait is the evidence to
judge it by: nothing is filtered out for being too recent, so a block that clears on its own is
visible while it lasts. A shrink's page-reclaim latch is the common one — it appears, waits a
few seconds and goes. Killing a blocker with `x` frees nothing (it is waiting on us, not the
other way round) and costs that session its transaction, so a short wait on the row is a reason
to leave it alone.

The header's right-hand box carries a third line, once the first poll has landed:

```
data 812.4 GB, 9.2% free   log 64.0 GB, 37% free, reuse=LOG_BACKUP
```

Data is summed across every ROWS file of the connected database; the log size is the total
of its log files. Sizes switch from MB to GB at 1024 MB. At 90% log space used, the log part
switches to the alert style, a sticky console alert names the percent and
`log_reuse_wait_desc`, and a `warn` is written to the running manifest's `.log`. Neither
repeats while the log stays high: both re-arm only once it has dropped back under 85%. The
console alert is tracked per database (a multi-database `--tui` run re-arms it for each
one), the `.log` warning once per manifest. Two things
worth knowing about the 90% mark:

- with the shipped `log_max_percent: 80`, the reaction hierarchy has already tried to
  relieve pressure before the alarm fires — seeing it means the log kept filling *despite*
  the reaction, not that nothing reacted;
- the percent is of the log's *current* file size (`used_log_space_in_percent`), which
  autogrowth can still extend — 90% full is not 90% of the eventual ceiling if the file
  keeps growing.

A fourth line says how busy the server is, once the first poll has landed:

```
cpu 45% (sql 32, other 13)   runnable 24/16   18 active requests
```

`cpu` is the whole machine, not this instance: `sql` is the SQL Server process's share of it and
`other` is everything else on the box — another instance, a backup agent, an antivirus sweep.
`runnable 24/16` is tasks waiting for a CPU over online schedulers, and the request count is the
requests running right now (sessions idle with a transaction open are blockers, not load, and do
not count). SqlGoPace is in that count: the console's own read is a running request while it
runs, and so is the operation, so an otherwise quiet server reads `1 active request` or
`2 active requests` rather than none.

Read it as ambiance. Nothing in the reaction hierarchy looks at it, and no operation is paced by
it; it is there so an operator can tell a slow rebuild on a saturated server from a slow rebuild
on a quiet one. Two things about the numbers:

- the CPU percentages come from the scheduler-monitor ring buffer, which emits **one record a
  minute**, and the console asks for that record at most once a minute — it is the expensive
  part of the line, and a faster poll would only re-fetch the same record. It can only ask on a
  poll, so a `progress_poll_seconds` that does not divide 60 stretches the gap: at 45 s the
  record is re-read every 90 s. Read the percentages as *a minute or so old*, not as now.
  `runnable` and the request count are live, on `progress_poll_seconds` and
  `blocking_poll_seconds` respectively;
- at one runnable task per scheduler the segment switches to the alert style and the console
  narrates `CPU pressure: 24 tasks runnable across 16 schedulers` once, re-arming only below 0.5
  per scheduler. Sustained runnable tasks mean the server is short of CPU: the operation will be
  slower, and so will everything else running on it.

Where the ring buffer cannot be read — Azure SQL Database on Basic/S0/S1 or in an elastic pool —
the `cpu` segment is left out and the rest of the line still renders. On Azure the figure would
in any case describe the machine hosting the database, not the database's own limit.

### What a run leaves on disk

Beside each manifest, in `03.done/` or `04.failed/`:

| File | What it carries | Mode |
|---|---|---|
| `<manifest>.log` | the run report: operations, reactions, timings, and the names of the databases, tables and indexes touched | `0600` |
| `<manifest>.blocked.yaml` | the sessions this run blocked — **their SQL text**, login, host and program, ready to paste back as `ignore_blocked_sessions` rules | `0600` |
| `<manifest>.contended.yaml`, `<manifest>.amplifiers.yaml` | the same shape for contention and for maintenance statements terminated | `0600` |
| `sqlgopace_history.db` | one row per run, with the object names, kept across runs | created by SQLite, `0644` on Unix |

The capture sidecars are the ones to be careful with: `active_query` and `parent_query` are
verbatim statements from someone else's application, literals included. They are written
owner-only, and they stay sensitive when you copy them into a ticket, an email or a
repository. The queue directories themselves are `0755`, so their *file names* — which
usually carry a database name — are readable by any local account.

| Key | Action |
|---|---|
| `i` | Ignore the selected session: writes an `ignore_blocked_sessions` rule into the running manifest, hot-reloaded. |
| `x` | Kill the selected session, after a confirmation. |
| `b` | The blocker roster: the sessions that have blocked *us*, and where a kill rule is armed, after a confirmation. |
| `k` | Kill the running DDL, after a confirmation. |
| `d` | Drain: finish the current operation, then stop cleanly. |

Pressing `i` asks which criterion to match on, so the rule it writes is durable
(`app_name`, `login_name`, `host_name`) rather than tied to a session id that will not
exist tomorrow.

**Arming a rule from the roster is the most destructive gesture in the console**, which is
why it asks first. `x` and `k` each end one thing you can see and name; arming ends every
session that later blocks the run and matches an `app_name`, `login_name` or `host_name` —
an unbounded set, chosen by attribute, until the run ends. It was the *last* of the three to
be gated (0.32.0), having fired on a single keystroke while its two less harmful neighbours
had asked since 0.24.0 and 0.28.0. Disarming is not gated: it can only reduce what the run
will terminate.

**`k` is the most destructive key on the main screen**, which is why it asks first. It terminates the
operation *this run* is executing. The prompt states what it would cost, from what the
console already knows: how long the operation has run and how far it got — that is what the
rollback discards — whether the edition allows the operation to be resumable (on Enterprise
or Azure a killed resumable rebuild pauses with its server-side progress kept; on Standard
or Express it cannot be resumable, so the rollback is total and cannot be interrupted), and
whether Accelerated Database Recovery is on, which makes the rollback itself cheap. Only `y`
confirms; any other key cancels. Prefer `d` (drain) whenever you can wait — it finishes the
current operation instead of undoing it.

**Read the direction before pressing `x`.** The selectable list is the sessions *waiting on*
the DDL — the ones it is holding up. Killing one of them frees nothing the operation is
waiting for; it only discards that session's work, and rolls back whatever it had open. The
confirmation names the login, the application and the open transaction count for exactly that
reason. `i` is usually the right key: it lets the operation hold its lock through a session
you have decided can wait.

To act on a session that is blocking *you*, use `b` — the roster — which arms a
`kill_blocking_sessions` rule that `BlockerKiller` can actually match. There used to be an
`X` key on the blocked list that wrote into that same list from the wrong side; the rule it
produced could never fire, and it has been removed.

## Sizes, before and after

Every `rebuild_index`, `rebuild_heap` and `reorganize_index` is measured: the used size of
each structure it rewrites is read as the statement starts, and read again when it succeeds.
One structure gets one line in the `.log`; a heap rebuild gets a block, because the statement
rewrites the whole table:

```
      size: IX_MEASUREMENT_TS 2.0 GB -> 1.4 GB (-30.0%)

      size (heap and 2 nonclustered index(es)):
        heap                               5.0 GB -> 3.1 GB (-38.0%)
        IX_MEASUREMENT_TS                  2.0 GB -> 1.4 GB (-30.0%)
        IX_MEASUREMENT_OLD (was disabled)    0 KB -> 452.0 MB
        total                              7.0 GB -> 4.9 GB (-30.0%)
```

The manifest ends with its own total, over each distinct structure it touched, and the run's
two figures are stored in the SQLite history (`runs.size_before_kb`, `runs.size_after_kb`), so
a campaign is one `SUM` over its runs. Two manifests that rebuild the same index both count it,
which a campaign total does not detect — the de-duplication is per manifest.

**Read the figure for what it is.** It is the net change in used pages between two reads, not
the gain of the operation alone: an `ONLINE` rebuild or a reorganize runs while the workload
writes, and on a busy table those writes are in the difference.

Five cases print no percentage, deliberately:

| Case | What you see |
| --- | --- |
| A rebuild that failed or was canceled | `-> unknown`: it rolled back, so there is no new size |
| A reorganize the tool canceled | the real, partial result, marked `partial` — REORGANIZE keeps committed work |
| A reorganize stopped by Ctrl+C | `-> unknown`: the compaction it committed is kept, but reading its new size needs the connection you just told the tool to stop using |
| An index the rebuild re-enabled | `0 KB -> 452.0 MB`: it had no pages to start from |
| A login without `VIEW DEFINITION` | one `sizes not measured: …` line for the manifest, instead of `unknown` everywhere |

**Before anything runs**, a manifest holding a `rebuild_heap` says what that statement really
covers — how many nonclustered indexes it rebuilds with the heap and how much that adds up to —
in the `.log`, on stdout, and on the operation's own row in the console, next to `cancel only`
where both apply. The row keeps its note for the whole run, and is replaced by the size result
when the operation finishes. In `--tui` the whole set also appears in its own block above the
dashboard — replaced at each manifest, and capped at five lines with a `+N more` tail, so a
campaign full of heaps cannot push a manifest failure off the screen.

When the structures cannot be read — most often a login without `VIEW DEFINITION`, which
returns no rows rather than an error — the line says the scope is **unknown** instead of saying
nothing. Silence there would have read as "this rebuild touches only the heap", which is the one
conclusion the tool cannot support.

## Stopping a run

The first Ctrl+C drains: a running resumable operation is paused with its work preserved,
a non-resumable one finishes, and the run stops before the next operation. A second Ctrl+C
cancels the run context for an immediate hard stop. In the console, `d` does the same as
the first Ctrl+C.

A drained manifest stays in `02.processing/` with its resume cursor, so the next run
continues rather than replaying.

## What a re-run repeats

Operations are individually addressable on a re-run, but how depends on how the previous
run ended. There are three paths and they behave differently.

| Previous run ended by | Left where | A re-run repeats |
|---|---|---|
| An operation failed, `on_failure: stop` (default) | `04.failed/` | Everything, from operation 1 |
| Operations failed, `on_failure: continue` | `04.failed/` plus `<name>.recovery.yaml` | Only the failed operations |
| Crash, Ctrl+C drain, or window close | stays in `02.processing/` | Resumes at the first unfinished operation |

**Fail-fast, the default.** The first failed operation sends the whole manifest to
`04.failed/` untouched: no recovery manifest, and the resume cursor is discarded.
Re-running it replays the operations that already succeeded. On a long batch that is the
expensive path; reach for `on_failure: continue`, or mark the operations
`intent: compression` so a replay skips those already at target.

**`on_failure: continue`.** Each failed operation is quarantined and the rest still run.
The run ends as `PARTIAL` and a re-runnable `<name>.recovery.yaml`, holding only the failed
operations, is written into `04.failed/`. Move it back into `01.to_run/` to retry just
those. This is the mode for independent batches, such as compressing many indexes, where a
few objects may be locked while the rest should proceed.

**Crash, drain, or window close.** The manifest stays in `02.processing/` with a resume
cursor in a `<name>.state.json` sidecar, and the next run continues where it stopped. A
crash also reconciles what was in flight: adopting a still-running operation, resuming a
paused resumable index build, or requeuing the work. No recovery manifest is written here,
deliberately, because the manifest itself is resumed and a recovery manifest would run the
same operations a second time.

The cursor is a watermark, not a set: it marks how many *leading* operations are done. In
`continue` mode it therefore freezes at the first quarantined operation, so a resumed run
retries that operation and re-runs the successful ones after it. Those retries are what
make the quarantine safe without a recovery manifest, but on a long batch they cost real
work: pair a windowed `continue` manifest with a manifest-level `intent: compression` so
the already-done operations after the gap collapse to a catalog read.

An interrupted run writes its report to `<name>.log` next to the manifest in
`02.processing/`, so a campaign that only ever drains or runs out of window is still
reviewable. It is superseded by the final report when the manifest eventually finishes.

## Paused resumable operations

A paused resumable index operation keeps consuming data space and blocks a concurrent
rebuild of the same index (error 10637) until it is finished or aborted.

During a run this is handled automatically. If the paused operation is this manifest's own
interrupted work, the run resumes it with `ALTER INDEX … RESUME`, reusing the server-side
progress rather than restarting. Ownership is matched by identity, the operation index plus
the target object, never by cursor position.

A stale or foreign paused resumable that would block a fresh rebuild fails the operation
with a message pointing at the subcommand below, unless the manifest opts in:

```yaml
abort_blocking_resumable: true
operations:
  - operation: rebuild_index
    schema: dbo
    table: Orders
    index: IX_Orders
```

That flag is off by default because aborting discards the paused operation's server-side
progress, which is a deliberate choice to make on a shared database.

### `abort-resumable`

**An aborted index operation cannot be resumed.** SQL Server discards its progress, and this
command cannot tell which paused operations are yours — `sys.index_resumable_operations` is
database-wide and records no owner. So it requires a target, and requires you to say so out
loud when you decline to give one.

```bash
# Preview what matches, changing nothing. Start here.
sqlgopace abort-resumable --config config.yaml --all --dry-run

# One table, or one index by name
sqlgopace abort-resumable --config config.yaml --table dbo.MEASUREMENT
sqlgopace abort-resumable --config config.yaml --index PK_MEASUREMENT

# Every paused resumable in the database, including other people's
sqlgopace abort-resumable --config config.yaml --all --yes

# Also abort RUNNING ones, killing the sessions building them
sqlgopace abort-resumable --config config.yaml --table dbo.MEASUREMENT --include-running --yes
```

| Flag | Meaning |
|---|---|
| `--table` | `schema.table`, or a bare table name to match it in any schema. |
| `--index` | An index name. Combined with `--table` both must match. |
| `--all` | No target: every matching operation in the database. Needs `--yes`. |
| `--include-running` | Also abort `RUNNING` operations, which kills the sessions building them. Needs `--yes`. |
| `--yes` | Confirms a destructive, unresumable abort. |
| `--dry-run` | Lists what would be aborted and changes nothing. Never needs `--yes`. |

Running it with neither a target nor `--all` is an error rather than a no-op, so a truncated
command line cannot become a database-wide abort. `--dry-run` is deliberately free of
ceremony: it is the review path, and making it awkward would only push you toward the
destructive form. The header prints the scope it resolved before the first `ABORT` is issued.

Only `PAUSED` operations are aborted by default. The exit code is non-zero if any abort
fails.

## Exit codes

A non-zero exit means the run did not complete cleanly: a manifest failed, recovery could
not reconcile the queue, or the process could not start. Keep a watchdog on it, since two
kinds of exit cannot notify you: a killed process, and a `config.yaml` that cannot be read.
