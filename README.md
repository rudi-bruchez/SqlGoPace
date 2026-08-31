# SqlGoPace

[![CI](https://github.com/rudi-bruchez/SqlGoPace/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/rudi-bruchez/SqlGoPace/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

SqlGoPace is a high-performance, resilient DDL task runner for Microsoft SQL Server
written in Go. It is designed for demanding database migrations and schema
refactoring — heavy `ALTER COLUMN`, `ALTER INDEX`, `CREATE INDEX`, table rebuilds,
constraint and column changes — and it bridges the gap between raw T-SQL scripts and
production safety.

Instead of running a script and hoping for the best, SqlGoPace generates the
T-SQL itself from a declarative manifest, picks the right options for the target
server's version and edition, watches the operation's impact on locking and the
transaction log while it runs, and reacts to trouble with the least destructive
mechanism available — preferring pause/resume over kill/rollback.

It runs either unattended or interactively. Left to itself — from cron, the Windows Task
Scheduler, or a SQL Agent job — it drains a queue of manifests silently and traces every
decision to a `.log` sidecar next to each one, so an overnight maintenance window needs no
operator. Started with `--tui`, it opens an incident console instead: live progress of the
running DDL, the sessions it is blocking or blocked by, and single-key actions to kill a
blocker, ignore it, or pause the operation without leaving the terminal.

---

## Why a dedicated tool

Demanding DDL is risky in production:

- it blocks other sessions or gets blocked (`LCK_M_SCH_S`, `LCK_M_SCH_M`,
  `LCK_M_IX` waits);
- it can fill the transaction log;
- a `KILL` triggers a `ROLLBACK` that may be long and expensive;
- the correct option set (`ONLINE`, `RESUMABLE`, `WAIT_AT_LOW_PRIORITY`, `MAXDOP`, …)
  depends on both the version and the edition of the target server.

SqlGoPace automates the decision and the monitoring, and always favours the safest
available mechanism.

## How it works

Each task is a YAML manifest describing one or more DDL operations. SqlGoPace does
not accept arbitrary `.sql` files — parsing and rewriting unknown T-SQL is fragile
and unsafe. Because it knows each operation's exact shape, it builds the `WITH (...)`
clause without duplication, runs a precise preflight on the targeted object, and
handles idempotency and resume.

At runtime the orchestrator uses two connections:

- an execution connection (dedicated, pinned) that runs the DDL;
- a monitoring connection that polls locking, blocking, transaction-log pressure,
  and operation progress.

A manifest flows through a set of directories as it is processed:

```
01.to_run/  →  02.processing/  →  03.done/   (success, with a .log next to it)
                              ↘   04.failed/  (failure, with a .log)
```

### What a re-run repeats

A manifest's operations are individually addressable on a re-run, but *how* depends on how
the previous run ended. There are three paths, and they behave differently:

| Previous run ended by | Left where | A re-run repeats |
|-----------------------|------------|------------------|
| An operation failed, `on_failure: stop` (default) | `04.failed/` | Everything, from operation 1 |
| Operations failed, `on_failure: continue` | `04.failed/` + `<name>.recovery.yaml` | Only the failed operations |
| Crash, `Ctrl+C` drain, or window close | stays in `02.processing/` | Resumes at the first unfinished operation |

**Fail-fast (default).** The first failed operation sends the whole manifest to `04.failed/`
untouched — no recovery manifest, and the resume cursor is discarded. Re-running it replays
the operations that already succeeded. For a long batch this is the expensive path; reach for
`on_failure: continue`, or mark the operations `intent: compression` (see the manifest format)
so a replay skips those already at target.

**`on_failure: continue`.** Each failed operation is quarantined and the rest still run. The
run ends as `PARTIAL`, and a re-runnable recovery manifest `<name>.recovery.yaml` — holding
only the failed operations — is written into `04.failed/`. Move it back into `01.to_run/` to
retry just those. This is the mode for independent batches (e.g. compressing many indexes)
where a few objects may be locked while the rest should still proceed.

**Crash, drain, or window close.** The manifest stays in `02.processing/` with a resume
cursor in a `<name>.state.json` sidecar, and the next run continues where it stopped rather
than replaying. A crash also reconciles what was in flight — adopting a still-running
operation, resuming a paused resumable index build, or requeuing the work. No recovery
manifest is written here, deliberately: the manifest itself is resumed, so a recovery
manifest would run the same operations a second time.

The cursor is a watermark, not a set: it marks how many *leading* operations are done. In
`on_failure: continue` mode it therefore freezes at the first quarantined operation, so a
resumed run retries that operation — and re-runs the successful ones after it. Those retries
are what make the quarantine safe without a recovery manifest, but on a long batch they cost
real work: pair a windowed `continue` manifest with a manifest-level `intent: compression` so
the already-done operations after the gap collapse to a catalog read.

An interrupted run writes its report to `<name>.log` next to the manifest in
`02.processing/`, so a campaign that only ever drains or runs out of window is still
reviewable; it is superseded by the final report in `03.done/` or `04.failed/` when the
manifest finishes.

### Reaction hierarchy

When the running DDL causes pressure, SqlGoPace escalates from gentlest to harshest:

1. `WAIT_AT_LOW_PRIORITY` — yield to existing sessions where supported.
2. `RESUMABLE` pause/resume — pause an index operation and resume it later instead
   of rolling back.
3. `KILL` — last resort, only after a grace period, with bounded retries.

The exact set of options that are even *eligible* is driven by
`ddl_compatibility.yaml`, keyed by SQL Server major version and edition tier.

`reorganize_index` has no `WAIT_AT_LOW_PRIORITY`/`RESUMABLE` to fall back on, so it
paces instead by cancelling and re-issuing the statement (SQL Server persists its
progress across the cancel) and warns at start if RCSI is off on the target database,
since readers will then block on its page locks.

On SQL Server 2022+ it also checks the database-scoped
`ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY` setting and, when it is off, recommends turning it
on: doing so lets *automatic, asynchronous* statistics updates queue politely at low priority
instead of blocking the reorganize. The advisory states its own limit in the same breath —
enabling the setting does **not** cover an explicit `UPDATE STATISTICS` run by a job or by hand;
those still block and can still queue readers behind them, which is exactly the collision the
`kill_amplifying_maintenance` feature (below) exists to clear. When the setting is already on,
the advisory still repeats that limitation, so an operator who has already enabled it is not
left assuming explicit `UPDATE STATISTICS` is handled too.

## Installation

Requires Go 1.26+.

```bash
go install github.com/rudi-bruchez/SqlGoPace/cmd/sqlgopace@latest
```

Or from a clone:

```bash
go build -o bin/sqlgopace ./cmd/sqlgopace
# or
make build
```

Running it needs more than the binary: a `config.yaml`, the compatibility matrix
`ddl_compatibility.yaml`, and a login with the right grants. See Configuration and
Permissions below.

### Versioning

The version lives in [`internal/version/VERSION`](internal/version/VERSION) and is embedded
into the binary at build time. Edit that file before building to bump the version — no
build flags needed — then rebuild. `sqlgopace --version` prints it, and every run writes a
`-- sqlgopace <version>` banner at the top of its `.log`, so each run record states which
build produced it.

```bash
$ sqlgopace --version
sqlgopace 0.16.0
```

A release pipeline can override the version without editing the file:

```bash
go build -ldflags "-X github.com/rudi-bruchez/SqlGoPace/internal/version.override=1.2.3" ./cmd/sqlgopace
```

See [`docs/build.md`](docs/build.md) for the full build, versioning, and cross-compilation guide.

## Permissions

Every run needs `VIEW SERVER STATE` at server level: the monitoring connection reads
server-scoped DMVs on every poll, and without them the sampling loop fails even on a
rebuild that blocks nobody. Beyond that, the grants are per tier, and most queues need
only the first one.

| You run | You need, in the target database | Plus, at server level |
|---|---|---|
| Index, column, constraint and statistics operations | `db_ddladmin` | — |
| `batch_update`, `batch_delete` | `db_datareader` + `db_datawriter` | — |
| `shrink`, `check_db` | `db_owner` | — |
| `shrink_tempdb` | — | `sysadmin` |
| Killing blockers or amplifying victims | — | `ALTER ANY CONNECTION` |

SqlGoPace fails preflight with the missing grant named, rather than letting a statement
fail after the manifest has been claimed. The kill capability is the exception: it warns,
because a run that cannot kill a blocker is still a valid run.

`db_datareader` is not needed for the DDL tier, `update_statistics WITH FULLSCAN`
included, so granting it there widens the login for nothing. Batched DML does need it:
every batch is an `UPDATE`/`DELETE TOP (n)`, and SQL Server wants `SELECT` for the `TOP`
and for any predicate column.

See [`docs/permissions.md`](docs/permissions.md) for the operation-by-operation
reference, [`docs/permissions/`](docs/permissions/) for ready-to-run T-SQL templates per
tier, and [`docs/permissions/99-verify.sql`](docs/permissions/99-verify.sql) to report
what an existing login can actually run.

## Configuration

Two files drive a run: `config.yaml` (policy, directories, connection, monitoring) and
a `.env` holding secrets. Secrets are never stored in plaintext in `config.yaml` —
the connection string references environment variables with `${VAR}`, injected from
`.env` (which is gitignored). See `.env.example`.

Key `config.yaml` sections:

| Section            | Purpose                                                                 |
|--------------------|-------------------------------------------------------------------------|
| `database`         | Connection string (`${VAR}` references), login timeout. No query timeout — duration is governed by monitoring, never a fixed timer. |
| `directories`      | The `to_run` / `processing` / `done` / `failed` queue paths.            |
| `monitoring`       | Poll intervals, log size/percent ceilings, blocking timeout, kill grace, retries. |
| `preflight`        | Data free-space, tempdb, and Availability Group send-queue checks.      |
| `options_override` | Force `online` / `resumable` / `wait_at_low_priority` / `maxdop` / `sort_in_tempdb` on/off/auto, globally. |
| `notifications`    | Optional webhook URL and email channel, and the events that trigger them. |
| `history`          | Optional SQLite run history.                                            |
| `shrink`           | Tuning for the `shrink` driver (chunk sizes, batch target, no-progress/self-wait/log-reuse timeouts). Optional — every field defaults. |
| `matrix_file`      | Path to the DDL compatibility matrix (resolved relative to the config). |

The `shrink:` block is entirely optional — omit it and every field takes the default
below. The values are global only (they depend on the instance's storage and SLA, not on a
manifest) and are starting points and bounds that the driver's dynamic calibration varies; an
operator usually only touches them for atypical storage.

```yaml
shrink:
  initial_step_small_mb:  100   # initial chunk when reclaiming < 5 GB
  initial_step_medium_mb: 250   # 5–50 GB
  initial_step_large_mb:  500   # > 50 GB
  min_step_mb:             50    # chunk floor (below this, per-loop overhead dominates)
  max_step_mb:           1024    # chunk ceiling (don't saturate I/O in one move)
  target_batch_seconds:     5    # aim each chunk at a few seconds → vivid reactions
  max_no_progress:          3    # consecutive no-gain chunks before stopping cleanly
  no_progress_backoff_seconds:      30   # wait before retrying a stalled chunk (doubles each time)
  no_progress_backoff_max_seconds: 300   # backoff ceiling
  self_wait_timeout_minutes: 5   # max total wait while blocked (Sch-M / snapshot) before clean stop
  log_reuse_wait_timeout_minutes: 30  # max wait for a scheduled BACKUP LOG to free a FULL-recovery log
```

### Notifications

`notifications` fires a webhook and/or an email for the events in `on_events`: `fail` (hard
error), `incomplete` (a shrink stopped short of target, work preserved), `interrupted`
(Ctrl+C/drain), `run_failure` (the run itself stopped, see below), and the reaction events
`pause` / `cancel` / `abort`.

`run_failure` reports what no manifest can: the run stopped for a reason outside any manifest
— the server is unreachable, the target edition is unsupported, crash recovery could not
reconcile the queue, an engine failed to start. Without it those exits show up only on stderr
and in the exit code, so an unattended run fails silently. It never doubles up with the
per-manifest events: an operator interruption arrives as `interrupted`, and a manifest that
failed as `fail`. Two exits stay silent by construction: a crashed or killed **process** (nothing
is left running to send anything) and a `config.yaml` that cannot be read (the channel settings
live in it). Keep an external watchdog — service manager, scheduled task — on the exit code.

The `email` sub-block adds an SMTP channel that shares the same `on_events` filter as the
webhook. It is disabled when `host` is empty; `port` defaults to `25`; `username` empty means
an anonymous relay (no auth); `password` is only used when `username` is set and comes from
`${VAR}` like every other secret.

```yaml
notifications:
  webhook_url: ""
  on_events: [fail, incomplete, interrupted, run_failure]
  email:
    host: "smtp.internal.example"   # empty → email disabled
    port: 25                         # default 25
    from: "sqlgopace@example.com"
    to: ["dba-team@example.com"]
    username: ""                     # empty → anonymous relay (no auth)
    password: "${SMTP_PASS}"         # from .env; only used when username is set
    starttls: false                  # opportunistic STARTTLS before auth
```

## Manifest format

A manifest is one logical task: an ordered list of operations executed sequentially.
Options left empty are injected automatically based on the detected version, edition,
and the compatibility matrix; per-operation `options:` blocks override them.

```yaml
# 01.to_run/010_rebuild_dispatch.yaml
description: "Recompress DISPATCH indexes and add a tracking column"
database: MYDB          # optional; defaults to the connection's database
on_failure: stop        # optional: stop (default, fail-fast) | continue (quarantine + recovery manifest)
intent: compression     # optional manifest-level default for rebuild_index operations below
                         # (compression | fragmentation); see "intent" below

# Optional: sessions allowed to STAY blocked by these operations (the op holds its
# lock through them instead of yielding). All string fields are regular expressions,
# matched app-side. An entry matches when every field it sets matches (AND); the list
# is OR'd. Unlike options.ignore_blocking (which ignores ALL blocking), this is
# targeted. Transaction-log protection still applies. See "Ignoring unimportant
# blocked sessions" below.
ignore_blocked_sessions:
  - app_name: "^SQLAgent"          # e.g. a nightly job that may wait
  - login_name: "svc_(reporting|etl)"

# Optional, and the OPPOSITE direction: sessions that may be KILLED when they block
# these operations. Same matcher fields, plus after_seconds. Inert unless
# kill_blockers.enabled is true in config.yaml. See "Killing the sessions that block
# you" below — the two lists are not interchangeable.
kill_blocking_sessions:
  - login_name: "svc_(reporting|etl)"
    after_seconds: 120
operations:
  - operation: rebuild_index
    schema: dbo
    table: DISPATCH
    index: IX_DISPATCH        # or "ALL" to rebuild every index on the table
    data_compression: PAGE
    # intent: fragmentation  # would override the manifest-level default above for
    #                        # just this operation; see "intent" below
    # online / resumable / wait_at_low_priority / maxdop / sort_in_tempdb:
    # left empty → injected automatically per version/edition + matrix + config
    options:
      maxdop: 4              # explicit override for THIS operation
      # ignore_blocking: true   # reaction policy: hold the lock through blocking,
      #                         # leaving other sessions blocked (force this index through)
      # max_block_minutes: 30   # safety cap: yield after 30 min of blocking even if the
      #                         # blocker is ignored (backstop a too-broad ignore rule)

  - operation: add_column
    schema: dbo
    table: DISPATCH
    column: PROCESSED
    type: BIT
    nullable: false
    default: 0               # constant → metadata-only on Enterprise
```

### `intent` (optional, `rebuild_index` only)

A `rebuild_index` operation does two unrelated things: it applies a `data_compression`
target (a state — idempotent, nothing to do if already there) and it rebuilds the index
(an act — defragments, rebuilds statistics, reclaims pages; never idempotent). `intent`
tells the engine which one motivated this operation, so a re-run knows whether skipping it
is safe:

- `intent: compression` — the goal is the compression state. If the index already carries
  the target `data_compression` on every partition, the operation is **skipped** on a
  re-run (a cheap catalog read, reported as `skipped: already PAGE`).
- `intent: fragmentation` — the goal is the rebuild itself. The operation **always runs**,
  even if its `data_compression` already matches — the defrag still needs doing.
- Unset (default) — same as `fragmentation`: the operation always runs. This is the safe
  default, because a wrongly-skipped rebuild is silent (reported as success) while a
  wrongly-repeated one is only wasted time.

`intent` can be set per operation, or once at the manifest level as a default that each
`rebuild_index` operation inherits unless it sets its own (operation value wins; then
manifest default; then unset). This replaces the old `skip_if_satisfied` manifest flag,
which applied to every operation uniformly and could not tell a defrag rebuild from a
compression rebuild; a manifest still carrying `skip_if_satisfied:` now fails to load.
See `docs/specs/OPERATION-INTENT.md` for the full design.

### `window` (optional)

Restrict a manifest's operations to a recurring window, evaluated against the SQL
Server's local clock (`SYSDATETIME()`):

```yaml
window:
  start: "01:00"      # HH:MM, 24h, server local time
  end:   "05:00"      # HH:MM
  days:  [Sat, Sun]   # optional; Mon..Sun; default = every day
```

- `end < start` is an overnight window that crosses midnight (e.g. `22:00`–`05:00`).
  `days` selects the day the window opens.
- Outside the window, the manifest is deferred (left in `01.to_run`, not run) —
  schedule the run (cron / Task Scheduler) to launch during the window.
- If the window closes while the manifest is running, the current operation
  finishes, then the run stops and the manifest stays in `02.processing` with its
  resume cursor, continuing in the next window.
- `start == end` is rejected. Offline `--dry-run` cannot evaluate the window (no
  connection) and annotates it instead.

### Supported operations

| `operation`       | Generated T-SQL                            |
|-------------------|--------------------------------------------|
| `rebuild_index`   | `ALTER INDEX … REBUILD WITH (…)`           |
| `create_index`    | `CREATE [UNIQUE] INDEX … WITH (…)`         |
| `drop_index`      | `DROP INDEX …`                             |
| `add_column`      | `ALTER TABLE … ADD …`                      |
| `alter_column`    | `ALTER TABLE … ALTER COLUMN … WITH (…)`    |
| `drop_column`     | `ALTER TABLE … DROP COLUMN …`              |
| `add_constraint`  | `ALTER TABLE … ADD CONSTRAINT … WITH (…)`  |
| `drop_constraint` | `ALTER TABLE … DROP CONSTRAINT …`          |
| `shrink`          | `DBCC SHRINKFILE (…) WITH (…)`             |
| `shrink_tempdb`   | `DBCC SHRINKFILE (…) WITH (…)` per tempdb data file |

### Ignoring unimportant blocked sessions

By default SqlGoPace is a *good citizen*: when its DDL blocks another session past
`blocking_timeout`, it yields (pause/resume or cancel). On a busy 24/7 database one
trivial session — a report that wakes once an hour, a monitoring poll — can keep an
operation from ever finishing: it runs, yields to the nuisance, restarts, yields
again.

`ignore_blocked_sessions:` (top-level, applies to every operation in the manifest)
lets you name sessions that are allowed to stay blocked, so the operation holds
its lock through them and keeps going. It is the *targeted* form of the blanket
per-operation `options.ignore_blocking: true`. Transaction-log protection is always
honored — only the *blocking* reaction is suppressed, and only for matching sessions.

```yaml
ignore_blocked_sessions:
  # An entry matches when EVERY field it sets matches (AND); the list is OR'd.
  # All string fields are regular expressions, evaluated app-side (SQL Server has
  # no regex before 2025). session_id is an exact match.
  - app_name: "^SQLAgent"                 # ignore the SQL Agent job…
    login_name: "svc_reporting"           # …but only under this login (AND)
  - host_name: "BATCH0[0-9]"              # OR any session from these hosts
  - statement: "FROM dbo\\.AuditLog"      # OR one running this query
  - session_id: 142                       # OR exactly this SPID (volatile — prefer the above)
```

A session is reacted to unless it positively matches a rule, so an overly narrow or
absent list keeps the default "yield" behavior — fail-safe. Reach for `app_name` /
`login_name` for durable rules; `session_id` only identifies a connection that exists
right now. While an operation holds its lock through an ignored session, the run log
records it (`hold: holding the lock through ignored session SPID …`), so the
suppression is never silent.

As a backstop against a too-broad rule, set `options.max_block_minutes: N` on an
operation: after `N` minutes of continuous blocking it yields even if the blocker is
ignored (`ignore_blocked_sessions` or `ignore_blocking`). Transaction-log protection
is unaffected.

**Discovering who blocked you.** When the engine reacts to blocking, it writes an
advisory `<manifest>.blocked.yaml` next to the run report listing the sessions it was
blocking — both ready-to-paste `ignore_blocked_sessions:` entries (commented) and a
full `observed:` diagnostic block (app/login/host/query, waits, times seen). SqlGoPace
never reads this file back: copying an entry into the manifest is a deliberate step,
so you never accidentally ignore real work.

**Adding a rule mid-run.** SqlGoPace re-reads the running manifest's `ignore_blocked_sessions`
on every blocking poll. If an operation stalls on a blocker you decide is safe, edit the manifest
in `02.processing/` to add the rule and the operation continues without a restart — the new
exclusion takes effect before the next abort. It is also folded into the recovery manifest, so a
later resumed run remembers it. In the interactive console (`--tui`), select a blocked session and
press `i`, then pick the criterion (`s` session_id / `a` app_name / `l` login_name / `h`
host_name) — the rule is written into the running manifest for you and hot-reloaded.

### Killing the sessions that block you: `kill_blocking_sessions`

`ignore_blocked_sessions` handles sessions **we** hold up. `kill_blocking_sessions` handles the
opposite case: a session **we are waiting on**, which we may terminate so our operation can
proceed. The two lists share the same matcher fields, and that similarity is exactly what makes
them easy to confuse — putting a login in the wrong one silently does nothing, because a rule
only ever fires in its own direction.

**Which list do I need?** Find the session in the run's advisory sidecar, then read across:

| Where the session shows up | What is happening | The list to use |
|---|---|---|
| `<manifest>.blocked.yaml`, or the TUI's blocked list | **we** block **it** — it waits on our lock (`LCK_M_SCH_S`, `LCK_M_IX`, …) | `ignore_blocked_sessions` — let it stay blocked instead of aborting our operation |
| our operation is suspended and the run log names it as our blocker | **it** blocks **us** | `kill_blocking_sessions` — terminate it after a delay |
| it is blocked by us *and* has sessions queued behind it | it amplifies our block into an outage | `kill_amplifying_maintenance` (next section) |

Both directions can be right for the same login at the same time: a dashboard query can block a
shrink chunk on Monday and be blocked by it on Tuesday. Naming it in both lists is coherent, not
contradictory.

Arming is a two-step, deliberate act — the match rules live in the manifest (hot-reloadable,
appendable from the TUI with `[x]`), but nothing is ever killed unless the config also allows it:

```yaml
# config.yaml — the master arm. Off by default; killing a session is destructive.
kill_blockers:
  enabled: true              # without this, every kill_blocking_sessions rule is inert
  default_after_seconds: 60  # delay applied to a rule that sets no after_seconds
```

```yaml
# the manifest — who may be killed, and after how long they have blocked us
kill_blocking_sessions:
  - login_name: "^svc_dashboard$"    # a read-only dashboard: kill it quickly
    after_seconds: 30
  - app_name: "^SQLAgent"            # a maintenance job: give it longer to finish
    after_seconds: 600
```

The delay is counted over the polls on which the rule actually matched — the first poll of a
blocking episode contributes nothing, because we cannot know how long the blocker had already
been there when we first saw it — and the first matching rule decides: order your rules from
most to least specific, since a broad rule with a short delay placed first will shadow a
narrower one behind it. Each blocker is killed at most once per episode.

**A returning offender does not buy a fresh delay.** The delay is served by the *rule*, not
by the session id. A blocker that is killed and comes straight back under a new SPID — an
Agent job restarting its step, a connection pool retrying, or the next session of a
population that all match the same rule — inherits the blocking time already served under
that rule, so it is killed on the next poll instead of buying another full delay. Blocking
time is banked per rule for five minutes of quiet, after which the rule is forgotten and the
full delay applies again.

To bound the cost of that, one rule kills at most three sessions before **five quiet minutes**
— the same quiet window that forgets the debt, and for the same reason. On the fourth,
SqlGoPace stops killing, writes a `warn` into the run report naming the rule and the last
offender, and falls back to the normal blocking reaction — the behavior you would get with the
feature off. Note what that means for an offender that keeps coming back every few minutes:
each return refreshes the window, so the rule stays capped for the rest of the manifest rather
than earning three fresh kills every five minutes. That is deliberate — a blocker being
restarted faster than it can be cleared is an operator problem (disable the job, or move the
run outside its window), and an unbounded kill loop would trade a blocked run for a rollback
storm.

One consequence of counting per rule rather than per session is worth spelling out before you
pick an `after_seconds`: **a rule that matches on `statement:` only accrues while a matching
statement runs.** Time is banked on the polls where the whole rule matched, so a blocker that
alternates between a matching statement and a non-matching one takes *longer* than
`after_seconds` of wall-clock blocking to be killed: the polls where it ran something else
count for nothing against that rule (they are banked against whichever other rule matched, or
nowhere at all). That errs toward not killing, which is the safe direction, but it is not what
reading `after_seconds` as "after N seconds of blocking us" would suggest.

`KILL` requires `ALTER ANY CONNECTION`. A failed kill is reported and changes nothing else: the
operation falls back to the normal reaction hierarchy, exactly as if the feature were off.

**A worked example of getting it backwards.** A shrink on `PRODDB` kept aborting its chunks. The
manifest named the two dashboard logins under `kill_blocking_sessions`, but `<manifest>.blocked.yaml`
listed them under `observed:` — they were waiting on `LCK_M_SCH_S` behind the shrink's page
relocation, i.e. *victims*, not blockers. The rules never matched anything, the dashboards tripped
the yield timer every time they refreshed, and the shrink lost each chunk it had started. They
belonged in `ignore_blocked_sessions`: read-only `SELECT`s with `open_transactions: 0` cost nothing
to keep waiting. The ingestion login in the same capture — an `INSERT` with two open transactions —
was correctly left out of both lists, and `options.max_block_minutes` kept backstopping the run.
The lesson generalizes: **read `.blocked.yaml` before writing session policy.** Every session in
that file is a session you block, so every entry it suggests belongs to `ignore_blocked_sessions`.

### Killing amplifying maintenance victims: `kill_amplifying_maintenance`

`ALTER INDEX ... REORGANIZE` is an online operation: it takes only `Sch-S` plus short-lived
page locks and does not block readers — until a single incompatible request queues behind it.
SQL Server never lets a compatible lock request barge past a queued incompatible one, so the
moment a `Sch-M` requester (a nightly `UPDATE STATISTICS`, another `ALTER INDEX`, a `TRUNCATE
TABLE`, ...) starts waiting on your reorganize, every reader that arrives afterwards queues
behind *that* request instead of passing through. The fan-out only grows while the reorganize
runs: one queued statistics job on `dbo.MEASUREMENT` on `PRODDB` turned an online index
maintenance pass into a full-table outage for every subsequent `SELECT`. The victim is not
application work, though — it is a SQL Agent maintenance statement that will simply run again on
its next schedule, which makes it the cheapest thing to terminate and the direct cause of the
outage.

`kill_amplifying_maintenance:` (top-level in `config.yaml`, a sibling of `kill_blockers:`) arms a
second, independent killer for exactly this situation — the mirror direction of `kill_blockers`,
which kills sessions blocking *us*; this one kills sessions blocked *by us*. Off by default:

```yaml
kill_amplifying_maintenance:
  enabled: false          # opt-in
  min_blocked_behind: 1   # sessions queued behind the victim before it counts as an amplifier
  after_seconds: 60       # how long it must stay eligible before the KILL
  commands: []            # empty = the built-in allow-list; a non-empty list REPLACES it
```

A blocked session is a kill candidate only when all of the following hold:

1. it is directly blocked by our DDL session, not merely a transitive victim further down the
   chain;
2. it is not another SqlGoPace session (matched by prefix against *this connection's own*
   application name — whatever `app name=` your `connection_string` sets, or `SqlGoPace` when it
   sets none — so a second size-split manifest running concurrently is never a candidate,
   including one on a different build);
3. its `sys.dm_exec_requests.command` matches the allow-list — by default `ALTER INDEX`, `ALTER
   TABLE`, `CREATE INDEX`, `CREATE STATISTICS`, `UPDATE STATISTIC` (both spellings SQL Server
   reports), `DROP INDEX`, `DROP TABLE`, `TRUNCATE TABLE`, `DBCC`; a non-empty `commands:` list
   *replaces* this set rather than extending it, so you can narrow the feature to, say,
   `UPDATE STATISTIC` alone. An entry that is empty or whitespace-only — the usual result of a
   dangling YAML item — is rejected at startup: it would be a prefix of every command verb and
   would silently widen the feature to every session the run blocks;
4. it matches no `ignore_blocked_sessions` rule;
5. it is not the session our own DDL is directly waiting on (that one belongs to
   `kill_blockers`, never to this feature — the two killers are disjoint by construction, so a
   mutual-block snapshot still produces exactly one `KILL`);
6. at least `min_blocked_behind` sessions are queued transitively behind it (default 1: one
   queued reader means the amplification has already begun, and it only grows from there).

Conditions 1–6 must hold for `after_seconds` (default 60) before the `KILL` fires. That dwell is
accumulated over the polls on which every condition holds, and it is banked against the offender
identity rather than the session id (see the repeat-offender note below), so a victim that drops
out of eligibility for a poll loses that interval but keeps the time it has already served.
A victim is suppressed from the yield reaction for that whole dwell — from the first poll it is
eligible, well before any `KILL` — so an `after_seconds` longer than
`monitoring.blocking_timeout_minutes` means the operation keeps its lock through the amplifier for
the whole dwell, with the manifest's `max_block_minutes` as the only backstop. That is a valid
choice (killing sooner is the more destructive one), so SqlGoPace only **warns** about it at
startup.

**Precedence — read this twice, the two options behave oppositely:**

- **`ignore_blocked_sessions` beats the kill.** A session the operator explicitly named as
  allowed to stay blocked is never killed, whatever its command — explicit instruction beats
  automatic classification.
- **`ignore_blocking: true` does *not*.** That option only means "do not yield" — it suppresses
  this run's own pause/cancel reaction to being blocked. It says nothing about intervening on the
  victim, so an operator running with `ignore_blocking: true` will still see maintenance victims
  killed. Precisely because that operator is holding the lock through blocking, they are the one
  who most benefits from the amplifier being cleared.

A failed `KILL` (permission denied, session already gone) immediately falls back to the normal
yield: the victim stops being suppressed and counts toward the blocking timer again on the very
next poll, so this feature can never make a run block *longer* than it would without it.
`max_block_minutes` is unaffected either way and continues to backstop the whole run: a victim
this feature never manages to kill still forces a yield once the cap is reached.

**A returning offender does not buy a fresh dwell here either.** The same repeat-offender rule as
`kill_blocking_sessions` applies, keyed on the SQL Agent job step when `program_name` resolves to
one — and on the login/host/program triplet when it does not — rather than on a match rule. A job
whose step restarts under a new SPID after being killed inherits the dwell it already served, so
it is killed as soon as it qualifies again instead of buying another full `after_seconds`. The
same cap applies: three kills per identity before **five quiet minutes**, after which SqlGoPace
stops killing that offender, writes a `warn` into the run report naming its program (or
`login@host`) and the last session, and lets the victim count toward the yield timer again — the
behavior you would get with the feature off. Five quiet minutes forget the identity and the full
`after_seconds` applies again; an offender that keeps returning sooner than that refreshes the
window on every appearance and so stays capped for the rest of the manifest. The identity is
charged at most once per poll — and the charge is measured from that identity's own previous
appearance, not from any one of its sessions — so one job running several concurrent sessions, or
an application whose pooled connections all share one login/host/program, does not reach the
dwell any faster or slower than a single connection would.

**Permissions.** `KILL` requires `ALTER ANY CONNECTION` — the same grant `kill_blockers` already
needs, so enabling this feature alone adds no new grant. Naming the SQL Agent job that owned the
killed statement additionally needs `SELECT` on `msdb.dbo.sysjobs` (and `sysjobsteps`); that grant
is optional and attribution degrades gracefully without it (see below). Enabling either kill
feature makes preflight **warn** (never fail) if the connected login lacks `ALTER ANY CONNECTION`.
SqlGoPace never writes to msdb and never disables an Agent job itself — it only reports which one
collided and prints a ready-to-paste statement for you to run by hand:

```
EXEC msdb.dbo.sp_update_job @job_name = N'IndexOptimize - USER_DATABASES', @enabled = 0;
```

**Attribution.** A T-SQL Agent job step sets `program_name` to `SQLAgent - TSQL JobStep (Job
0x... : Step N)`; SqlGoPace parses that and looks up the job/step name in msdb. Only **T-SQL** job
steps carry this program name — a CmdExec or PowerShell step (including Ola Hallengren's scripts
driven through `sqlcmd`) will not match. The kill still happens; attribution then degrades to the
raw program name, login, and host, which is why those three fields are always recorded, whether or
not the job resolved.

**`.amplifiers.yaml` sidecar.** Alongside `.blocked.yaml` and `.contended.yaml`, a run that kills
at least one amplifying victim writes `<manifest>.amplifiers.yaml` next to the run report: one
entry per kill (session, statement, database, login/host/app, how many sessions were queued
behind it, and the resolved Agent job when attribution succeeded), plus a trailing comment block
with one deduplicated `sp_update_job` statement per distinct job terminated. Advisory only, like
its siblings — SqlGoPace never reads it back. It is deliberately a separate file from
`.blocked.yaml`: that file's whole purpose is "paste this into `ignore_blocked_sessions`", and an
amplifier is the exact opposite instruction.

**Where a kill is (and is not) reported.** Every kill reaches the run `.log`, the
`.amplifiers.yaml` sidecar, and stdout. In `--tui` mode it also reaches the incident feed (as a
narration line) and a sticky alert line listing the distinct SQL Agent jobs this run has
terminated, cleared when the manifest ends. It does **not** reach webhook or email notifications:
`notifications.on_events` only ever fires for `pause`, `cancel`, and `abort` — the reaction kinds
that mean the run itself yielded — and a `kill` is a different kind of event. Extending
notifications to cover it would mean widening `on_events` and the notify branch in `engine.go`,
which this feature deliberately does not do; do not assume a configured webhook will tell you
about a killed maintenance job.

### Shrinking files: `operation: shrink`

`shrink` reclaims space from a database's data or log files with `DBCC SHRINKFILE`,
driven file by file. Unlike the other operations it is not one statement: the driver reads the
file's space at run time, runs a free `TRUNCATEONLY` pass first, then moves pages in
calibrated chunks, adjusting the chunk size from the I/O and log waits each chunk produced.
Because every internal batch commits, a shrink can be stopped at any time with no rollback and
is re-entrant — re-running toward the same target resumes where it left off.

```yaml
# Reclaim space from all data files, leaving ~10% free above what's used:
- operation: shrink
  type: data            # "data" | "log"
  files: all            # "all" (every file of the type) | a logical file name
  targetfreespace: 10%  # free space wanted in the final file: "N%" or "N MB"
  identify_tail_object: true   # optional; name the tail object up front (2019+)
  options:
    wait_at_low_priority: true   # 2022+ only (matrix-gated); auto if omitted

# Reclaim a specific log file down to ~50 MB of free space:
- operation: shrink
  type: log
  files: MyDb_Log
  targetfreespace: 50MB
```

- **`type`** (required): `data` or `log` — selects the eligible files and the algorithm
  (chunked page-move for data; truncation for log).
- **`files`** (default `all`): a logical file name (`sys.database_files.name`), or `all` to
  shrink every file of the type, one at a time (never two of a filegroup in parallel).
- **`targetfreespace`**: the free space wanted in the final file, as a percent of used
  space (`N%` ⇒ final ≈ `used × (1 + N/100)`) or an absolute `N MB` (final ≈ `used + N`).
  Always clamped to the floor a file can actually reach (its used space, or the active VLFs
  for a log).
- **`emptyfile`**: reserved for a future release; `true` is rejected in this version.
- **`options.wait_at_low_priority`**: auto by default. On SQL Server 2022+ it is injected for
  data shrinks so the schema-modify lock waits at low priority instead of blocking queries.
  It does not apply to log files. `DBCC SHRINKFILE` takes no `MAXDOP`.
- **`identify_tail_object`** (default off): when `true`, run the tail-object walk once at the
  start of each data-file shrink — after the `TRUNCATEONLY` pass — to name the object owning
  the file's last allocated page, the one `DBCC SHRINKFILE` must relocate past. The object is
  logged for early visibility, but only recorded as a confirmed blocker (see below) if that
  shrink then fails to reach target. Requires SQL Server 2019+ (`sys.dm_db_page_info`); on older
  versions it logs a warning and is skipped. Ignored for log shrinks.

Behaviour worth knowing:

- **Automatic `TRUNCATEONLY`** is always tried first — if the free space is already at the end
  of the file, it is reclaimed instantly with no page movement (and no fragmentation).
- **No-op** when there is nothing to reclaim (no free space, or the target is not below the
  current size): reported as a successful "nothing to reclaim".
- **Log files**: in `SIMPLE` recovery a `CHECKPOINT` is issued, then the log is shrunk. In
  `FULL`/`BULK_LOGGED`, if the log cannot yet be truncated (e.g. awaiting a log backup),
  SqlGoPace waits — bounded by `log_reuse_wait_timeout_minutes` — for the environment's
  scheduled backup to free the log, then shrinks; it never issues `BACKUP LOG` itself and
  abandons cleanly (work preserved) if the wait times out.
- **Fragmentation**: a data-file shrink fragments indexes by design; rebuild/reorganize
  afterwards if needed. (Automatic before/after fragmentation reporting is a future feature.)
- Reactions reuse the engine's monitoring: under blocking or log pressure the driver pauses
  between chunks (free — committed work is kept) and shrinks the next chunk smaller.

**Recording confirmed blockers.** A shrink records the objects it couldn't get past into
`<manifest>.contended.yaml` next to the run report, by two complementary means, each tagged with
a `confirmed_by`:

- `confirmed_by: lock` — whenever the shrink blocks other sessions while relocating an object
  (regardless of the run's final outcome), the object it held a `Sch-M` lock on is recorded: an
  empirically confirmed tail blocker.
- `confirmed_by: tail_position` — the **tail-object walk** names the object owning the file's
  last allocated page directly, without needing to block anyone. It runs automatically when a
  data shrink gives up short of target ("no further progress"), and — with `identify_tail_object:
  true` — once at the start of each data shrink (logged for visibility, but only *recorded* as a
  blocker if that shrink then fails to reach target: a tail object a successful shrink relocated
  was never a blocker). Requires SQL Server 2019+; below that it is skipped — silently for the
  automatic give-up walk, with a one-line warning only when you explicitly set
  `identify_tail_object`. Never runs for log or tempdb shrinks. This closes the common case of a
  shrink that stalls with no blocking victim (data pinned at the file end, a
  `WAIT_AT_LOW_PRIORITY` timeout).
- `confirmed_by: transient_maintenance` — a shrink blocked by a concurrent `ALTER INDEX`
  rebuild/reorganize or `DBCC` operation is reported as transient (a clear `.log`/TUI warning
  naming the operation and blocking session) rather than as a structural blocker, and — on a
  give-up — recorded under this tag instead of `tail_position`; `plan --confirmed` ignores it.

The sidecar is machine-readable, relocated to `03.done`/`04.failed` with the manifest on
finalize, and the run report's `.log` gets a one-line pointer (`contended objects: N — see
<file>`). Feed it into the next planning pass with `sqlgopace plan --confirmed <path>` (see
below); tail-position blockers are promoted ahead of lock-confirmed ones (they are the
definitive constraint on how far the file can shrink).

### Shrinking tempdb: `operation: shrink_tempdb`

`shrink_tempdb` is a dedicated operation for tempdb's data files — there is no `database:` field
because the operation *is* tempdb. It shrinks every data file down to a common absolute target
size, using the same chunked `DBCC SHRINKFILE` driver as `shrink` (a `TRUNCATEONLY` pass on every
file first, then calibrated chunk moves), with a clean, re-entrant give-up when the target can't
be reached live.

```yaml
- operation: shrink_tempdb
  targetsizemb: 20480    # every tempdb data file is shrunk to 20 GB
  flushcaches: false      # opt-in escalation on a persistent stall (see below)
```

- **`targetsizemb`** (required, > 0): the common absolute target, in MB, applied to **every**
  tempdb data file. A file whose used space already exceeds the target stops at that used floor
  (clamped) rather than failing.
- **`flushcaches`** (optional, default `false`): opt-in for a targeted cache-flush escalation used
  only when a file's shrink stalls persistently (see below). Off by default because it has a
  real, if narrow, performance cost.

**Non-goals** (deliberate):

- **Not a monitor.** This is a maintenance operation, not tempdb surveillance — there is no
  continuous tracking of what fills tempdb and no query-plan capture. The run report lists the
  blockers observed *while shrinking* (a by-product of choosing the reaction), which is incident
  reporting on the operation itself, not general tempdb monitoring.
- **Not a guaranteed shrink.** Internal objects held by live queries (work tables, sort/hash
  spills, the version store) can pin pages at the end of a file and refuse to move. The operation
  does its best and stops cleanly when it can't go further, reporting so plainly — bringing a
  400 GB tempdb down to 20 GB live is often impossible without a restart, and that is expected.
- **Data files only.** The tempdb log is out of scope.

**Never kills a blocker.** Live sessions blocking the shrink are always waited out, never killed
— they are legitimate application queries. Where available (SQL Server **2022+ only**), the
driver adds `WAIT_AT_LOW_PRIORITY (ABORT_AFTER_WAIT = SELF)`: this only makes *our* chunk yield
and retry, it never aborts the blocker. On SQL Server 2019 the matrix disables this option for
`shrink_tempdb`, so the reaction degrades to a plain bounded wait followed by a clean give-up.

**The `flushcaches` trade-off.** When a file's shrink shows no progress repeatedly (a no-gain
chunk, `Msg 5240` "work table page could not be moved", or error 845 buffer-latch time-out), the
driver first backs off and retries — these conditions often clear on their own as transient
tempdb objects age out. If the stall persists past a threshold *and* `flushcaches: true`, it
issues one targeted escalation, at most once per run across all files:

```sql
CHECKPOINT;
DBCC FREESYSTEMCACHE ('Temporary Tables & Table Variables');
```

This frees only the temp-object cachestore (`CACHESTORE_TEMPTABLES`) — cached temp tables/table
variables that can pin tempdb pages — after stabilizing state with a `CHECKPOINT`. It deliberately
does **not** reach for the broader sledgehammers a naive "soft restart" recipe uses:

- `DBCC FREESYSTEMCACHE ('ALL')` / `DBCC FREEPROCCACHE` empty the **whole** plan cache
  instance-wide, triggering a recompilation storm and CPU spike that can time out application
  connections — too costly to fire automatically.
- `DBCC DROPCLEANBUFFERS` empties the buffer pool for zero tempdb gain.

Widening the flush to `('ALL')` behind an `aggressive` flag is a possible future escalation but is
**deferred out of v1** — only the targeted `'Temporary Tables & Table Variables'` flush exists
today, and only when `flushcaches: true`.

**The `Unbalanced tempdb files` warning.** If the data files do not all end at the same size
(some clamped to their used floor, some stalled above target on pinned pages), the report emits
this warning. Uneven tempdb files defeat SQL Server's proportional-fill allocation: a file that
later frees its pinned pages ends up with far more free space than the others, so new allocations
skew toward it and concentrate `PAGELATCH` contention on a single file. The warning is a signal to
follow up (a re-run, or manual intervention) — SqlGoPace does not force a common floor by
under-shrinking every file to match the worst one.

**Side benefit.** `DBCC SHRINKFILE` below a file's *created* size also corrects that file's boot
size in `sys.master_files` — so besides reclaiming disk now, a successful shrink undoes a manual
`ALTER DATABASE ... MODIFY FILE (SIZE = ...)` bump made during an incident, and tempdb comes back
at the right size on the next restart.

## Usage

```bash
# Execute every queued manifest with monitoring and reaction:
sqlgopace --config config.yaml

# Same, with the interactive incident console:
sqlgopace --config config.yaml --tui

# Render the final T-SQL (injected options included) without executing anything,
# against the live server detected from the config:
sqlgopace --config config.yaml --dry-run 01.to_run/010_rebuild_dispatch.yaml

# Explain why each option was injected or removed:
sqlgopace --config config.yaml --dry-run --explain 01.to_run/010_rebuild_dispatch.yaml

# Offline dry-run — no connection; assume a target version/edition:
sqlgopace --dry-run --assume-version 16 --assume-edition enterprise \
  01.to_run/010_rebuild_dispatch.yaml
```

### Modes and flags

| Flag                | Effect                                                                          |
|---------------------|---------------------------------------------------------------------------------|
| *(none)*            | Silent run; everything is traced to a `.log` next to each processed manifest.   |
| `--config <path>`   | Config file (connection, directories, policy, matrix path). Required to run.    |
| `--tui`             | Interactive incident console: live progress and blockers; `[x]` kill a blocker, `[i]` ignore one (writes a rule into the running manifest), `[k]` kill the DDL, `[p]` pause. |
| `--auto`            | Analyse the database and run generated maintenance unattended (no review): writes the manifests into the queue, then processes it. Pairs with `--profile`/`--categories`/`--database`, or `--all-databases`/`--databases` for a server-wide run. See `plan`. |
| `--dry-run`         | Render the final DDL without executing or taking any lock.                       |
| `--explain`         | With `--dry-run`, show why each option was chosen (version/edition + matrix + config), and list any `ignore_blocked_sessions` rules. |
| `--assume-version`  | Offline dry-run target major version (e.g. `16` for SQL Server 2022).            |
| `--assume-edition`  | Target edition tier: `enterprise`, `standard`, `express`, `azure`.               |
| `--matrix <path>`   | Override the compatibility matrix path (otherwise from config).                  |
| `--version`         | Print version and exit.                                                          |

### Maintenance: `abort-resumable`

A paused resumable index operation keeps consuming data space and blocks a concurrent rebuild of
the same index (SQL Server error 10637) until it is finished or aborted. The `abort-resumable`
subcommand inventories the connected database's resumable operations and cancels them with
`ALTER INDEX … ABORT`:

```bash
# Preview what would be aborted (no change):
sqlgopace abort-resumable --config config.yaml --dry-run

# Abort every PAUSED resumable operation in the connected database:
sqlgopace abort-resumable --config config.yaml

# Also abort RUNNING operations (use with care — may disrupt an active run):
sqlgopace abort-resumable --config config.yaml --include-running
```

By default only `PAUSED` operations are aborted. The exit code is non-zero if any abort fails.

During a run, a paused resumable on an index a manifest wants to rebuild is handled automatically:
if it is this manifest's own interrupted operation, the run resumes it (`ALTER INDEX … RESUME`,
reusing the server-side progress) instead of restarting. A stale or foreign paused resumable that
would block a fresh rebuild fails the operation with a message pointing here — unless the manifest opts
in with `abort_blocking_resumable: true`, which lets the engine clear it with `ALTER INDEX … ABORT`
before rebuilding. The flag is off by default because aborting discards the paused operation's
server-side progress — a deliberate choice on a shared database.

```yaml
abort_blocking_resumable: true    # clear a blocking foreign paused resumable before a fresh rebuild
operations:
  - operation: rebuild_index
    schema: dbo
    table: Orders
    index: IX_Orders
```

### Maintenance: `plan`

The `plan` subcommand turns SqlGoPace into a maintenance planner: it inspects the connected database
and generates the maintenance work itself — fragmentation-driven `REORGANIZE`/`REBUILD`, data
compression (`ROW`/`PAGE`, chosen on measured gain and write-intensity), heap rebuilds (forwarded
records), `UPDATE STATISTICS`, and `DBCC CHECKDB` — instead of you hand-writing the manifests. The
rules live in `maintenance_profile.yaml` (thresholds, per-object overrides). See
[`docs/specs/MAINTENANCE.md`](docs/specs/MAINTENANCE.md) for the full design.

It runs cheap-first: one metadata sweep selects candidates, and the expensive reads
(`sp_estimate_data_compression_savings`, sampled `dm_db_index_physical_stats`) run only over the
survivors. The output is reviewable manifests written into the queue — nothing is executed until
you run them through the normal engine.

**Scope: the connected database.** Index, compression, heap, and statistics maintenance analyse and act
on the single database the connection string points to (the analysis DMVs and generated DDL are
database-scoped). Only `DBCC CHECKDB` can span several databases, via `checkdb.databases` in the
profile. Point the connection string at the database you want to maintain.

A server-wide multi-database mode maintains several databases in one go. `plan --all-databases` (or
`--databases a,b,c`) materialises a per-database block of manifests, scoped by a `scope:` selector in
the profile; the run then processes the queue one connection per database, sequentially. `--auto`
accepts the same flags for an unattended server-wide run. Ineligible databases (AG secondary,
read-only, offline, no access) are skipped with a logged reason.

```bash
# Plan maintenance for every eligible user database (review the per-database manifests):
sqlgopace plan --config config.yaml --all-databases

# Or a chosen set:
sqlgopace plan --config config.yaml --databases SALES,INVENTORY

# Unattended, server-wide, in one command:
sqlgopace --config config.yaml --auto --all-databases
```

Crash recovery is database-aware: each in-flight operation records the database it ran in, and a later
run reconciles it against that database (an orphan whose database is unreachable — e.g. now an AG
secondary — is left for a future run). See [`docs/specs/MAINTENANCE.md`](docs/specs/MAINTENANCE.md) §17.

```bash
# Analyse and print the manifests it would write (no files, no locks):
sqlgopace plan --config config.yaml --dry-run

# Same, with the reasoning behind every decision:
sqlgopace plan --config config.yaml --dry-run --explain

# Materialise reviewable manifests into the queue (the config's to_run directory):
sqlgopace plan --config config.yaml

# Restrict to some categories, and override the check_db target database:
sqlgopace plan --config config.yaml --categories index,compression,heaps --database MYDB

# Then review the generated 01.to_run/*.yaml and run them as usual:
sqlgopace --config config.yaml
```

| Flag           | Effect                                                                       |
|----------------|------------------------------------------------------------------------------|
| `--config`     | Config file (connection + default output directory). Required.               |
| `--profile`    | Maintenance profile path (default `maintenance_profile.yaml`).               |
| `--categories` | Comma-separated subset of `index,compression,heaps,statistics,checkdb` (default: all). |
| `--database`   | Single-database mode: the database to plan (default: the connected database). |
| `--all-databases` | Multi-database mode: plan every eligible user database (spec §17).         |
| `--databases`  | Multi-database mode: comma-separated list of databases to plan.              |
| `--out`        | Directory to write manifests into (default: the config's `to_run`).          |
| `--dry-run`    | Print the manifests instead of writing them.                                 |
| `--explain`    | Show the reasoning behind each decision.                                     |
| `--confirmed`  | Path to a `.contended.yaml` written by a prior shrink run (objects it blocked on, or the tail object it couldn't get past); prioritizes those confirmed blocker objects in the pre-shrink reorganize pass — tail-position blockers first — and marks matching heap advisories `CONFIRMED`. Requires `shrink.enabled` in the profile. |

### `shrink:` (pre-shrink reorganize + reclaim)

Optional. When enabled, `sqlgopace plan` emits an extra manifest for the connected
database that reorganizes the low-density rowstore indexes (the tables large deletes
left half-empty), then shrinks the data file. It also prints and writes a `.heaps.yaml`
advisory listing the heaps a shrink cannot benefit from (reorganize cannot compact a
heap's in-row data — rebuild them in a window). Applies to the connected database only.

```yaml
shrink:
  enabled: true          # off/absent = no shrink manifest (default)
  type: data             # data | log  (log skips reorganize + the advisory)
  files: all             # all | a logical file name
  targetfreespace: 10%   # percent or absolute MB (e.g. 100MB)
  pre_reorganize: true   # false = emit the shrink op alone (default true)
  reorganize_below_density_percent: 65  # reorganize rowstore indexes below this SAMPLED page density
  max_block_minutes: 10  # optional; carried into the shrink op's options
  identify_tail_object: true  # optional; sets identify_tail_object on the generated shrink op (2019+)
```

Notes:
- The index size floor reuses `index.page_count_floor`.
- Session policy (`ignore_blocked_sessions` / `kill_blocking_sessions`) is not generated —
  add it by editing the generated manifest.
- The reorganize selection runs a SAMPLED `sys.dm_db_index_physical_stats` scan of the
  database's indexes at plan time (heavier than the maintenance pass's LIMITED scan).
- When `pre_reorganize: false`, the heap advisory is also skipped (it requires the SAMPLED page-density scan that the pre-reorganize pass performs).

## Compatibility matrix

`ddl_compatibility.yaml` declares, per operation, which options are eligible by minimum
major version and edition tier (with `requires` dependencies, e.g. `resumable` requires
`online`). It encodes the real SQL Server rules — for example `ONLINE` index builds from
2005 (Enterprise), `RESUMABLE` rebuild from 2017 and create from 2019,
`WAIT_AT_LOW_PRIORITY` on index ops from 2014/2022, `ONLINE ALTER COLUMN` from 2016, and
that `WAIT_AT_LOW_PRIORITY` is not supported with online `ALTER COLUMN` on any
version. Azure SQL Database / Managed Instance are treated as an evergreen pseudo-version.

## Testing

The pure core (DDL generation, option resolution, reaction logic, queue, recovery) is
unit-tested without a database. The parts that talk to SQL Server are covered by
integration and end-to-end tests guarded by the `integration` build tag.

```bash
make test          # unit tests (-race), no database needed
make e2e           # spin up SQL Server in Docker, run integration + e2e, tear down
```

The container runtime is configurable, so Podman works too:

```bash
podman machine start
make e2e CONTAINER=podman COMPOSE="podman compose"
```

See [`docs/e2e.md`](docs/e2e.md) for details.

## Contributing

Pull requests are welcome. [`CONTRIBUTING.md`](CONTRIBUTING.md) covers the build and
test commands, how to run the suite against a real server, and the conventions that
are not obvious from the code: manifest-driven rather than raw SQL, no query timeout,
measured claims, and never committing an identifier from a real engagement.

For a security problem, see [`SECURITY.md`](SECURITY.md) rather than opening an issue.
It also describes what privileges the tool holds and where its trust boundary sits,
which is worth reading before you point it at a production server.

[`CHANGELOG.md`](CHANGELOG.md) records notable changes.

## License

MIT. See [`LICENSE`](LICENSE).

`docs/ShrinkDriver.ps1` is not part of SqlGoPace: it is Microsoft's own
`Invoke-ShrinkDriver` sample, kept here as the reference the shrink driver was
designed against, under its own MIT licence and with its authorship header intact.
