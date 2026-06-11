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
| `matrix_file`      | Path to the DDL compatibility matrix (resolved relative to the config). |

## Manifest format

A manifest is one logical task: an ordered list of operations executed **sequentially**.
Options left empty are **injected automatically** based on the detected version, edition,
and the compatibility matrix; per-operation `options:` blocks override them.

```yaml
# 01.to_run/010_rebuild_dispatch.yaml
description: "Recompress DISPATCH indexes and add a tracking column"
database: MYDB          # optional; defaults to the connection's database
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
| `--database`   | `check_db` target database (default: the connected database).                |
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
