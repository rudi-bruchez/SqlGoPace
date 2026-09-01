# Permissions

Which grants each operation needs, and why. Every line here was measured against
SQL Server 2022 (RTM-CU26) Developer edition with purpose-built restricted logins,
not inferred from documentation. Ready-to-run templates are in
[`docs/permissions/`](permissions/), and
[`docs/permissions/99-verify.sql`](permissions/99-verify.sql) reports what a login
can do today.

SqlGoPace fails preflight, with the missing grant named, rather than letting a
statement fail after it has claimed a manifest and written a sidecar. The one
exception is the kill capability, which warns instead: a run that cannot kill a
blocker is still a valid run.

## The one grant every run needs

`VIEW SERVER STATE`, at server level.

The monitoring connection reads server-scoped DMVs on every poll: active sessions
and their waits, who is blocking whom, operation progress, transaction-log usage.
Without it the sampling loop fails, and it fails on a quiet rebuild that blocks
nobody, because the loop runs regardless. There is no mode of SqlGoPace that does
not need it.

### One optional grant, for `require_data_free_space`

`VIEW DEFINITION` on the database, or membership that implies it.

The data-free-space check sizes each rebuild from `sys.dm_db_partition_stats`, which
Microsoft documents as requiring **`VIEW DATABASE STATE` *and* `VIEW DEFINITION`** (on
SQL Server 2022 and later, `VIEW DATABASE PERFORMANCE STATE` and `VIEW SECURITY
DEFINITION`). `VIEW SERVER STATE` implies the state half and **not** the definition
half, so a login that satisfies every other requirement on this page can still be
refused this one read.

It is deliberately optional. Without the grant the check reports the object's size as
unknown and passes, and the autogrowth advisory reports that it could not read the
settings — neither fails the run. Nothing else in the tool needs it.

## By operation

| Operation | In the target database | At server level |
|---|---|---|
| `rebuild_index`, `reorganize_index` | `db_ddladmin` | `VIEW SERVER STATE` |
| `create_index`, `drop_index` | `db_ddladmin` | `VIEW SERVER STATE` |
| `rebuild_heap` | `db_ddladmin` | `VIEW SERVER STATE` |
| `add_column`, `alter_column`, `drop_column` | `db_ddladmin` | `VIEW SERVER STATE` |
| `add_constraint`, `drop_constraint` | `db_ddladmin` | `VIEW SERVER STATE` |
| `update_statistics` | `db_ddladmin` | `VIEW SERVER STATE` |
| `batch_update`, `batch_delete` | `db_datareader` + `db_datawriter`, or `SELECT` + `UPDATE`/`DELETE` on the table | `VIEW SERVER STATE` |
| `shrink` (data and log) | `db_owner` | `VIEW SERVER STATE` |
| `check_db` | `db_owner` | `VIEW SERVER STATE` |
| `shrink_tempdb` | not applicable | `sysadmin` |
| `plan` subcommand | `db_ddladmin` | `VIEW SERVER STATE` |
| killing blockers or victims | not applicable | `ALTER ANY CONNECTION` |

## Four things worth knowing before you grant

`db_datareader` is not needed for the DDL tier. Not for a rebuild, not for a
compression change, and not for `update_statistics` even `WITH FULLSCAN`. Granting it
anyway is a common reflex and it widens the login for nothing. The `plan` subcommand,
which reads fragmentation, compression state and heap shape, also runs without it.

`db_datawriter` alone is not enough for batched DML. Every batch is an `UPDATE` or
`DELETE TOP (n)`, so SQL Server wants `SELECT` on the table: for the `TOP` itself, for
any column a predicate filters on, and for the `key_range` strategy's own `SELECT MAX`
walk. A login with `db_datawriter` and no read right fails with "The SELECT permission
was denied on the object", with a `where` clause and without one. Preflight now checks
both permissions, so this is caught before the run rather than mid-batch.

`shrink_tempdb` asks for `sysadmin`, and it is the only operation that does.
`DBCC SHRINKFILE` for tempdb runs in tempdb, not in whatever database the connection
sits in, so `db_owner` of a user database does not carry: it passes a naive check and
then fails with Msg 7983, "User 'guest' does not have permission to run DBCC shrinkfile
for database 'tempdb'". `db_owner` in tempdb would work, but tempdb is recreated from
model at every restart and a membership granted there does not survive one. If handing
`sysadmin` to an unattended queue runner is not acceptable, and it often is not, run
`shrink_tempdb` manifests under a separate operator-triggered login.

Killing is opt-in twice, and unprivileged by default. `kill_blocking_sessions` and
`kill_amplifying_maintenance` need `ALTER ANY CONNECTION` (or `processadmin`, or
`sysadmin`), and so does `allow_abort_blockers`, which is what resolves
`ABORT_AFTER_WAIT = BLOCKERS`. Without the grant, preflight warns and every kill
becomes a silent no-op, so the run behaves as if the feature were off. It is
destructive: grant it when you mean it.

## A least-privilege starting point

For an unattended queue that rebuilds and recompresses indexes, which is the common
case, this is the whole list:

```sql
-- master. The password must not contain the login name: with CHECK_POLICY on,
-- SQL Server rejects that with Msg 33064, which reads like a plain complexity failure.
CREATE LOGIN [sqlgopace] WITH PASSWORD = N'<strong password>';
GRANT VIEW SERVER STATE TO [sqlgopace];

-- each target database
CREATE USER [sqlgopace] FOR LOGIN [sqlgopace];
ALTER ROLE [db_ddladmin] ADD MEMBER [sqlgopace];
```

Add a tier only when a manifest needs it. The files in
[`docs/permissions/`](permissions/) are that list, split per tier, with the reasoning
kept next to each grant.

## Checking a login you inherited

Run [`docs/permissions/99-verify.sql`](permissions/99-verify.sql) as the SqlGoPace
login, connected to the database it will target. It prints one row per capability with
a yes or no and what that capability buys, using the same probes preflight uses. Run it
once per target database: the database-scoped answers change with the connection, the
server-scoped ones do not.
