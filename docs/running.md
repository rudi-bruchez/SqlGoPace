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
locks (an NFSv3 share) is unprotected. Two runs on *different* processing directories never
interfere, whether or not they target the same database.

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

| Key | Action |
|---|---|
| `x` | Kill the selected blocker. |
| `i` | Ignore the selected session: writes an `ignore_blocked_sessions` rule into the running manifest, hot-reloaded. |
| `k` | Kill the running DDL. |
| `p` | Pause the operation. |
| `d` | Drain: finish the current operation, then stop cleanly. |

Pressing `i` asks which criterion to match on, so the rule it writes is durable
(`app_name`, `login_name`, `host_name`) rather than tied to a session id that will not
exist tomorrow.

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

```bash
# Preview what would be aborted, changing nothing
sqlgopace abort-resumable --config config.yaml --dry-run

# Abort every PAUSED resumable operation in the connected database
sqlgopace abort-resumable --config config.yaml

# Also abort RUNNING ones, which may disrupt an active run
sqlgopace abort-resumable --config config.yaml --include-running
```

Only `PAUSED` operations are aborted by default. The exit code is non-zero if any abort
fails.

## Exit codes

A non-zero exit means the run did not complete cleanly: a manifest failed, recovery could
not reconcile the queue, or the process could not start. Keep a watchdog on it, since two
kinds of exit cannot notify you: a killed process, and a `config.yaml` that cannot be read.
