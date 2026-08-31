# Getting started

From nothing to a first monitored index rebuild. Fifteen minutes, most of it spent
deciding which index to point it at.

## 1. Install

```bash
go install github.com/rudi-bruchez/SqlGoPace/cmd/sqlgopace@latest
```

Or from a clone, which is what you want if you also intend to run the tests:

```bash
git clone https://github.com/rudi-bruchez/SqlGoPace.git
cd SqlGoPace
make build          # -> bin/sqlgopace
```

Requires Go 1.26 or later. [`build.md`](build.md) covers cross-compilation and how the
version is embedded.

The binary alone is not enough to run. You also need three things that live in the
repository and that `go install` does not put on your disk:

- `ddl_compatibility.yaml`, the version and edition option matrix, which the tool reads at
  startup and refuses to run without;
- `config.yaml`, for which the repository's own file is a working starting point;
- `.env.example`, the template for your secrets.

So even on the `go install` path, fetch those three:

```bash
base=https://raw.githubusercontent.com/rudi-bruchez/SqlGoPace/main
curl -O $base/ddl_compatibility.yaml
curl -O $base/config.yaml
curl -o .env.example $base/.env.example
```

Cloning gets you all three and the test suite with them, which is why the clone is the
easier path for a first run.

## 2. Create the login

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

## 3. Configure

Two files. `config.yaml` holds policy and paths; `.env` holds secrets and is gitignored.
The connection string never carries a password directly: it references `${VAR}`, expanded
from the environment or from `.env`.

```bash
cp .env.example .env
$EDITOR .env            # DB_SERVER, DB_NAME, DB_USER, DB_PASSWORD
```

`DB_SERVER` goes into an ODBC-style `server=` keyword, so a non-default port is a **comma**,
not a colon: `SQLPROD01,14433`, never `SQLPROD01:14433`. A colon is read as part of the host
name and fails with `lookup SQLPROD01:14433: no such host`. A named instance uses a
backslash, `SQLPROD01\SQL2022`.

The `config.yaml` in the repository root is a working starting point. The only thing you
must check before a first run is the queue directories, which are relative to where you
launch the binary:

```yaml
directories:
  to_run:     "./01.to_run"
  processing: "./02.processing"
  done:       "./03.done"
  failed:     "./04.failed"
```

Create them:

```bash
mkdir -p 01.to_run 02.processing 03.done 04.failed
```

Every other section has a sensible default. [`configuration.md`](configuration.md) is the
full reference when you need it.

## 4. Write a manifest

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

[`manifests.md`](manifests.md) is the format reference and
[`operations.md`](operations.md) lists every operation with its fields.

## 5. Look before you leap

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

## 6. Run it

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
