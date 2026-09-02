# Getting started

From nothing to a first monitored index rebuild. Fifteen minutes, most of it spent
deciding which index to point it at.

## 1. Install

Download the archive for your platform from the
[releases page](https://github.com/rudi-bruchez/SqlGoPace/releases), unpack it, and put
`sqlgopace` somewhere on your `PATH`. Builds are published for Linux, macOS and Windows,
on Intel and ARM, with a `sha256` checksum file next to them.

With a Go toolchain, `go install` works too:

```bash
go install github.com/rudi-bruchez/SqlGoPace/cmd/sqlgopace@latest
```

Or from a clone, which is what you want if you also intend to run the tests:

```bash
git clone https://github.com/rudi-bruchez/SqlGoPace.git
cd SqlGoPace
make build          # -> bin/sqlgopace
```

Building from source requires Go 1.26 or later. [`build.md`](build.md) covers
cross-compilation and how the version is embedded.

## 2. Initialize a working directory

The tool reads its compatibility matrix and its configuration from disk at startup, and
refuses to run without them. `init` writes them, along with the queue directories, into
whatever directory you point it at:

```bash
mkdir sqlgopace && cd sqlgopace
sqlgopace init
```

That leaves you with `config.yaml`, `ddl_compatibility.yaml`, `maintenance_profile.yaml`,
a `.env.example` to copy, the four queue directories, and a disabled example manifest. The
templates are compiled into the binary, so this works offline and on a machine that has
never seen the repository.

Re-running `init` is safe: anything already present is reported and left alone. Use
`--force` when you do want the shipped template back, and `--dir` to initialize somewhere
other than the current directory.

## 3. Create the login

Every run needs `VIEW SERVER STATE` at server level, because the monitoring connection
reads server-scoped DMVs on every poll. Beyond that, grant only the tier your manifests
actually use. For index, column, constraint and statistics work, this is the whole list:

```sql
-- in master
CREATE LOGIN [sqlgopace] WITH PASSWORD = N'<strong password>';
GRANT VIEW SERVER STATE TO [sqlgopace];

-- in each target database
CREATE USER [sqlgopace] FOR LOGIN [sqlgopace];
ALTER ROLE [db_ddladmin] ADD MEMBER [sqlgopace];
```

Batched DML, shrink, `check_db` and the kill features each need more.
[`permissions.md`](permissions.md) states what and why, with ready-to-run templates in
[`permissions/`](permissions/) and a script that reports what an existing login can do.

## 4. Configure

Two files. `config.yaml` holds policy and paths; `.env` holds secrets and is gitignored.
The connection string never carries a password directly: it references `${VAR}`, expanded
from the environment or from `.env`.

```bash
cp .env.example .env
$EDITOR .env            # DB_SERVER, DB_NAME, DB_USER, DB_PASSWORD
```

`init` writes `.env.example` mode `0600` so the copy is private from the start. If you
wrote `.env` by hand, or `init` refreshed an older 0644 template with `--force`, set it
yourself — `chmod 600 .env`. Windows ignores file modes, so there the file inherits the
directory's ACL and nothing here applies.

`DB_SERVER` goes into an ODBC-style `server=` keyword, so a non-default port is a comma,
not a colon: `SQLPROD01,14433`, never `SQLPROD01:14433`. A colon is read as part of the host
name and fails with `lookup SQLPROD01:14433: no such host`. A named instance uses a
backslash, `SQLPROD01\SQL2022`.

The `config.yaml` that `init` wrote is a working starting point. The queue directories it
names are relative to where you launch the binary, which is the one thing to check if you
plan to run from elsewhere:

```yaml
directories:
  to_run:     "./01.to_run"
  processing: "./02.processing"
  done:       "./03.done"
  failed:     "./04.failed"
```

`init` created those four; the engine also creates them at startup if they are missing.

Every other section has a sensible default. [`configuration.md`](configuration.md) is the
full reference when you need it.

## 5. Write a manifest

A manifest is one logical task: an ordered list of operations run in sequence. Drop it in
`01.to_run/`. The name orders the queue, so number your files.

```yaml
# 01.to_run/010_rebuild.yaml
description: "Rebuild the orders index"
operations:
  - operation: rebuild_index
    schema: dbo
    table: Orders
    index: IX_Orders_Date
```

That is the whole file. Notice what is absent: no `ONLINE`, no `RESUMABLE`, no
`WAIT_AT_LOW_PRIORITY`, no `MAXDOP`. SqlGoPace detects the server's version and edition
and injects the options that target actually supports. You write the intent; it writes
the T-SQL.

Point it at an index you actually have, though: preflight verifies the object exists before
anything runs, and a manifest naming a table that is not there fails with
`table [dbo].[Orders] does not exist`. If you need a candidate:

```sql
SELECT TOP 5 s.name AS [schema], t.name AS [table], i.name AS [index]
FROM sys.indexes i
JOIN sys.tables t  ON t.object_id = i.object_id
JOIN sys.schemas s ON s.schema_id = t.schema_id
WHERE i.type_desc = 'NONCLUSTERED' AND i.is_disabled = 0
ORDER BY t.name;
```

[`manifests.md`](manifests.md) is the format reference and
[`operations.md`](operations.md) lists every operation with its fields.

## 6. Look before you leap

Never run a manifest you have not read as T-SQL first. The dry run renders exactly what
would execute, takes no lock, and touches nothing:

```bash
sqlgopace --config config.yaml --dry-run 01.to_run/010_rebuild.yaml
```

```sql
-- detected target: tier=enterprise major=16 adr=false recovery=FULL rcsi=false si=false
-- manifest: 01.to_run/010_rebuild.yaml — Rebuild the orders index
-- [1] rebuild_index dbo.Orders.IX_Orders_Date
ALTER INDEX [IX_Orders_Date] ON [dbo].[Orders] REBUILD WITH (ONLINE = ON (WAIT_AT_LOW_PRIORITY (MAX_DURATION = 1 MINUTES, ABORT_AFTER_WAIT = SELF)), RESUMABLE = ON);
```

Add `--explain` to see why each option was chosen or dropped. This is the flag to reach
for when the generated statement surprises you:

```bash
sqlgopace --config config.yaml --dry-run --explain 01.to_run/010_rebuild.yaml
```

```
--     online = ON  (supported by target (auto))
--     resumable = ON  (supported by target (auto))
--     wait_at_low_priority = ON  (supported by target (auto))
```

A line only appears for an option the resolver had a decision to make about. When one is
dropped, the reason comes with it, message number included:

```
--     sort_in_tempdb = OFF  (omitted: SORT_IN_TEMPDB cannot be combined with RESUMABLE (Msg 11438))
--     resumable = OFF  (omitted: RESUMABLE is not supported in tempdb (Msg 11439))
```

## 7. Run it

```bash
sqlgopace --config config.yaml
```

The run drains the queue: each manifest moves through `02.processing/` and lands in
`03.done/` or `04.failed/` with a `.log` sidecar beside it recording every decision, the
statement executed, and any reaction taken.

To watch it happen instead, with the sessions it blocks and single-key actions:

```bash
sqlgopace --config config.yaml --tui
```

[`running.md`](running.md) covers the modes, the flags, the queue lifecycle, and what a
re-run repeats after each kind of ending.

## Where to go next

| You want to | Read |
|---|---|
| Understand what happens when your DDL blocks someone | [`blocking-and-kills.md`](blocking-and-kills.md) |
| Reclaim disk space from a data or log file | [`shrink.md`](shrink.md) |
| Have SqlGoPace decide the maintenance work itself | [`maintenance-planner.md`](maintenance-planner.md) |
| Know which options your server supports | [`compatibility-matrix.md`](compatibility-matrix.md) |
| Write manifests with an AI assistant | [`llm-operator-guide.md`](llm-operator-guide.md) |
