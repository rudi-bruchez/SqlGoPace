# SqlGoPace

SqlGoPace is a high-performance, resilient DDL task runner for Microsoft SQL Server
written in Go. It is designed for demanding database migrations and schema
refactoring — heavy `ALTER COLUMN`, `ALTER INDEX`, `CREATE INDEX`, table rebuilds,
constraint and column changes — and it bridges the gap between raw T-SQL scripts and
production safety.

Instead of running a script and hoping for the best, SqlGoPace **generates** the
T-SQL itself from a declarative manifest, **picks the right options** for the target
server's version and edition, **watches** the operation's impact on locking and the
transaction log while it runs, and **reacts** to trouble with the least destructive
mechanism available — preferring pause/resume over kill/rollback.

---

## Why a dedicated tool

Demanding DDL is risky in production:

- it **blocks** other sessions or **gets blocked** (`LCK_M_SCH_S`, `LCK_M_SCH_M`,
  `LCK_M_IX` waits);
- it can **fill the transaction log**;
- a `KILL` triggers a `ROLLBACK` that may be long and expensive;
- the correct option set (`ONLINE`, `RESUMABLE`, `WAIT_AT_LOW_PRIORITY`, `MAXDOP`, …)
  depends on **both the version and the edition** of the target server.

SqlGoPace automates the decision and the monitoring, and always favours the safest
available mechanism.

## How it works

Each task is a **YAML manifest** describing one or more DDL operations. SqlGoPace does
**not** accept arbitrary `.sql` files — parsing and rewriting unknown T-SQL is fragile
and unsafe. Because it knows each operation's exact shape, it builds the `WITH (...)`
clause without duplication, runs a precise preflight on the targeted object, and
handles idempotency and resume.

At runtime the orchestrator uses **two connections**:

- an **execution** connection (dedicated, pinned) that runs the DDL;
- a **monitoring** connection that polls locking, blocking, transaction-log pressure,
  and operation progress.

A manifest flows through a set of directories as it is processed:

```
01.to_run/  →  02.processing/  →  03.done/   (success, with a .log next to it)
                              ↘   04.failed/  (failure, with a .log)
```

By default a manifest is fail-fast: the first failed operation sends the whole manifest
to `04.failed/`. Setting `on_failure: continue` (see the manifest example below) instead
**quarantines** each failed operation and runs the rest; the run ends as **`PARTIAL`** and
a re-runnable **recovery manifest** `<name>.recovery.yaml` — holding only the failed
operations — is written into `04.failed/`. Move it back into `01.to_run/` to retry just
those. Use this for independent batches (e.g. compressing many indexes) where a few
objects may be locked while the rest should still proceed.

If the process crashes mid-operation, the next run reconciles anything left in
`02.processing/` — adopting a still-running operation, resuming a paused resumable
index build, or requeuing the work.

### Reaction hierarchy

When the running DDL causes pressure, SqlGoPace escalates from gentlest to harshest:

1. **`WAIT_AT_LOW_PRIORITY`** — yield to existing sessions where supported.
2. **`RESUMABLE` pause/resume** — pause an index operation and resume it later instead
   of rolling back.
3. **`KILL`** — last resort, only after a grace period, with bounded retries.

The exact set of options that are even *eligible* is driven by
`ddl_compatibility.yaml`, keyed by SQL Server major version and edition tier.

## Installation

Requires Go 1.26+.

```bash
go build -o bin/sqlgopace ./cmd/sqlgopace
# or
make build
```

### Versioning

The version lives in [`internal/version/VERSION`](internal/version/VERSION) and is embedded
into the binary at build time. **Edit that file before building to bump the version** — no
build flags needed — then rebuild. `sqlgopace --version` prints it, and every run writes a
`-- sqlgopace <version>` banner at the top of its `.log`, so each run record states which
build produced it.

```bash
$ sqlgopace --version
sqlgopace 0.1.0
```

A release pipeline can override the version without editing the file:

```bash
go build -ldflags "-X github.com/rudi-bruchez/SqlGoPace/internal/version.override=1.2.3" ./cmd/sqlgopace
```

See [`docs/build.md`](docs/build.md) for the full build, versioning, and cross-compilation guide.

## Configuration

Two files drive a run: `config.yaml` (policy, directories, connection, monitoring) and
a `.env` holding secrets. **Secrets are never stored in plaintext in `config.yaml`** —
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
| `notifications`    | Optional webhook URL and the events that trigger it.                    |
| `history`          | Optional SQLite run history.                                            |
| `shrink`           | Tuning for the `shrink` driver (chunk sizes, batch target, no-progress/self-wait/log-reuse timeouts). Optional — every field defaults. |
| `matrix_file`      | Path to the DDL compatibility matrix (resolved relative to the config). |

The `shrink:` block is **entirely optional** — omit it and every field takes the default
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

## Manifest format

A manifest is one logical task: an ordered list of operations executed **sequentially**.
Options left empty are **injected automatically** based on the detected version, edition,
and the compatibility matrix; per-operation `options:` blocks override them.

```yaml
# 01.to_run/010_rebuild_dispatch.yaml
description: "Recompress DISPATCH indexes and add a tracking column"
database: MYDB          # optional; defaults to the connection's database
on_failure: stop        # optional: stop (default, fail-fast) | continue (quarantine + recovery manifest)

# Optional: sessions allowed to STAY blocked by these operations (the op holds its
# lock through them instead of yielding). All string fields are regular expressions,
# matched app-side. An entry matches when every field it sets matches (AND); the list
# is OR'd. Unlike options.ignore_blocking (which ignores ALL blocking), this is
# targeted. Transaction-log protection still applies. See "Ignoring unimportant
# blocked sessions" below.
ignore_blocked_sessions:
  - app_name: "^SQLAgent"          # e.g. a nightly job that may wait
  - login_name: "svc_(reporting|etl)"
operations:
  - operation: rebuild_index
    schema: dbo
    table: DISPATCH
    index: IX_DISPATCH        # or "ALL" to rebuild every index on the table
    data_compression: PAGE
    # online / resumable / wait_at_low_priority / maxdop / sort_in_tempdb:
    # left empty → injected automatically per version/edition + matrix + config
    options:
      maxdop: 4              # explicit override for THIS operation
      # ignore_blocking: true  # reaction policy: hold the lock through blocking,
      #                        # leaving other sessions blocked (force this index through)

  - operation: add_column
    schema: dbo
    table: DISPATCH
    column: PROCESSED
    type: BIT
    nullable: false
    default: 0               # constant → metadata-only on Enterprise
```

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

### Ignoring unimportant blocked sessions

By default SqlGoPace is a *good citizen*: when its DDL blocks another session past
`blocking_timeout`, it yields (pause/resume or cancel). On a busy 24/7 database one
trivial session — a report that wakes once an hour, a monitoring poll — can keep an
operation from ever finishing: it runs, yields to the nuisance, restarts, yields
again.

`ignore_blocked_sessions:` (top-level, applies to every operation in the manifest)
lets you name sessions that are allowed to **stay blocked**, so the operation holds
its lock through them and keeps going. It is the *targeted* form of the blanket
per-operation `options.ignore_blocking: true`. **Transaction-log protection is always
honored** — only the *blocking* reaction is suppressed, and only for matching sessions.

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
right now.

**Discovering who blocked you.** When the engine reacts to blocking, it writes an
advisory `<manifest>.blocked.yaml` next to the run report listing the sessions it was
blocking — both ready-to-paste `ignore_blocked_sessions:` entries (commented) and a
full `observed:` diagnostic block (app/login/host/query, waits, times seen). SqlGoPace
never reads this file back: copying an entry into the manifest is a deliberate step,
so you never accidentally ignore real work.

**Adding a rule mid-run.** SqlGoPace re-reads the running manifest's `ignore_blocked_sessions`
on every blocking poll. If an operation stalls on a blocker you decide is safe, edit the manifest
in `02.processing/` to add the rule and the operation **continues without a restart** — the new
exclusion takes effect before the next abort. It is also folded into the recovery manifest, so a
later resumed run remembers it.

### Shrinking files: `operation: shrink`

`shrink` reclaims space from a database's **data** or **log** files with `DBCC SHRINKFILE`,
driven file by file. Unlike the other operations it is not one statement: the driver reads the
file's space at run time, runs a free `TRUNCATEONLY` pass first, then moves pages in
**calibrated chunks**, adjusting the chunk size from the I/O and log waits each chunk produced.
Because every internal batch commits, a shrink can be stopped at any time with no rollback and
is **re-entrant** — re-running toward the same target resumes where it left off.

```yaml
# Reclaim space from all data files, leaving ~10% free above what's used:
- operation: shrink
  type: data            # "data" | "log"
  files: all            # "all" (every file of the type) | a logical file name
  targetfreespace: 10%  # free space wanted in the final file: "N%" or "N MB"
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
- **`targetfreespace`**: the free space wanted in the final file, as a **percent of used
  space** (`N%` ⇒ final ≈ `used × (1 + N/100)`) or an absolute `N MB` (final ≈ `used + N`).
  Always clamped to the floor a file can actually reach (its used space, or the active VLFs
  for a log).
- **`emptyfile`**: reserved for a future release; `true` is rejected in this version.
- **`options.wait_at_low_priority`**: auto by default. On SQL Server 2022+ it is injected for
  **data** shrinks so the schema-modify lock waits at low priority instead of blocking queries.
  It does not apply to log files. `DBCC SHRINKFILE` takes no `MAXDOP`.

Behaviour worth knowing:

- **Automatic `TRUNCATEONLY`** is always tried first — if the free space is already at the end
  of the file, it is reclaimed instantly with no page movement (and no fragmentation).
- **No-op** when there is nothing to reclaim (no free space, or the target is not below the
  current size): reported as a successful "nothing to reclaim".
- **Log files**: in **SIMPLE** recovery a `CHECKPOINT` is issued, then the log is shrunk. In
  **FULL/BULK_LOGGED**, if the log cannot yet be truncated (e.g. awaiting a log backup),
  SqlGoPace **waits** — bounded by `log_reuse_wait_timeout_minutes` — for the environment's
  scheduled backup to free the log, then shrinks; it **never issues `BACKUP LOG` itself** and
  abandons cleanly (work preserved) if the wait times out.
- **Fragmentation**: a data-file shrink fragments indexes by design; rebuild/reorganize
  afterwards if needed. (Automatic before/after fragmentation reporting is a future feature.)
- Reactions reuse the engine's monitoring: under blocking or log pressure the driver pauses
  between chunks (free — committed work is kept) and shrinks the next chunk smaller.

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
| `--tui`             | Interactive incident console: live progress, blockers, and operator actions.    |
| `--auto`            | Analyse the database and run generated maintenance unattended (no review): writes the manifests into the queue, then processes it. Pairs with `--profile`/`--categories`/`--database`, or `--all-databases`/`--databases` for a server-wide run. See `plan`. |
| `--dry-run`         | Render the final DDL without executing or taking any lock.                       |
| `--explain`         | With `--dry-run`, show why each option was chosen (version/edition + matrix + config). |
| `--assume-version`  | Offline dry-run target major version (e.g. `16` for SQL Server 2022).            |
| `--assume-edition`  | Target edition tier: `enterprise`, `standard`, `express`, `azure`.               |
| `--matrix <path>`   | Override the compatibility matrix path (otherwise from config).                  |
| `--version`         | Print version and exit.                                                          |

### Maintenance: `abort-resumable`

A paused resumable index operation keeps consuming data space and **blocks a concurrent rebuild of
the same index** (SQL Server error 10637) until it is finished or aborted. The `abort-resumable`
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

### Maintenance: `plan`

The `plan` subcommand turns SqlGoPace into a maintenance planner: it inspects the connected database
and **generates** the maintenance work itself — fragmentation-driven `REORGANIZE`/`REBUILD`, data
compression (`ROW`/`PAGE`, chosen on measured gain and write-intensity), heap rebuilds (forwarded
records), `UPDATE STATISTICS`, and `DBCC CHECKDB` — instead of you hand-writing the manifests. The
rules live in `maintenance_profile.yaml` (thresholds, per-object overrides). See
[`specs/MAINTENANCE.md`](specs/MAINTENANCE.md) for the full design.

It runs cheap-first: one metadata sweep selects candidates, and the expensive reads
(`sp_estimate_data_compression_savings`, sampled `dm_db_index_physical_stats`) run only over the
survivors. The output is **reviewable manifests** written into the queue — nothing is executed until
you run them through the normal engine.

**Scope: the connected database.** Index, compression, heap, and statistics maintenance analyse and act
on the single database the connection string points to (the analysis DMVs and generated DDL are
database-scoped). Only `DBCC CHECKDB` can span several databases, via `checkdb.databases` in the
profile. Point the connection string at the database you want to maintain.

A server-wide **multi-database mode** maintains several databases in one go. `plan --all-databases` (or
`--databases a,b,c`) materialises a per-database block of manifests, scoped by a `scope:` selector in
the profile; the run then processes the queue **one connection per database**, sequentially. `--auto`
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
secondary — is left for a future run). See [`specs/MAINTENANCE.md`](specs/MAINTENANCE.md) §17.

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

## Compatibility matrix

`ddl_compatibility.yaml` declares, per operation, which options are eligible by minimum
major version and edition tier (with `requires` dependencies, e.g. `resumable` requires
`online`). It encodes the real SQL Server rules — for example `ONLINE` index builds from
2005 (Enterprise), `RESUMABLE` rebuild from 2017 and create from 2019,
`WAIT_AT_LOW_PRIORITY` on index ops from 2014/2022, `ONLINE ALTER COLUMN` from 2016, and
that `WAIT_AT_LOW_PRIORITY` is **not** supported with online `ALTER COLUMN` on any
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

## License

MIT.
