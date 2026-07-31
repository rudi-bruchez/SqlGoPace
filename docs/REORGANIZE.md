# ALTER INDEX ... REORGANIZE: locking behavior and pacing implications

This note documents *why* an `ALTER INDEX ... REORGANIZE` can block concurrent
queries — despite being an "online" operation — and what that means for pacing it
inside SqlGoPace. It exists because a real incident (an index REORGANIZE on a live
server showed ~20 queries stuck in `LCK` waits behind it) contradicted the common
assumption that REORGANIZE is non-blocking.

## TL;DR

REORGANIZE is **online, not lock-free.** It never takes the long-term Sch-M table
lock an offline `REBUILD` takes, but it compacts and reorders leaf pages in many
small transactions, each taking **short-term page/row X locks** (plus table-level IX
intent locks). Any query touching the *same leaf pages it is actively compacting*
waits on `LCK_M_*`. Microsoft's own wording is deliberately hedged:

> Reorganizing an index is always performed online. The process holds locks **only
> for short periods of time and is unlikely to block** queries or updates.
> — [ALTER INDEX (Transact-SQL) › Online index operations](https://learn.microsoft.com/sql/t-sql/statements/alter-index-transact-sql?view=sql-server-ver17#online-index-operations)

"Unlikely" is not "never." Under a busy workload on a hot index, the brief locks are
enough to build a blocking chain.

## The four mechanisms that turn "brief" into "20 blocked queries"

1. **Implicit / explicit transactions — the big one.** Straight from the docs:
   > When `ALTER INDEX REORGANIZE` uses explicit transactions (for example, `ALTER
   > INDEX` inside a `BEGIN TRAN ... COMMIT/ROLLBACK`) instead of the default implicit
   > transaction mode, the locking behavior of REORGANIZE becomes **more restrictive,
   > potentially causing blocking.**
   > — [ALTER INDEX (Transact-SQL) › REORGANIZE a rowstore index](https://learn.microsoft.com/sql/t-sql/statements/alter-index-transact-sql?view=sql-server-ver17#arguments)

   If the connection has `SET IMPLICIT_TRANSACTIONS ON` (JDBC, and some ODBC/.NET
   configurations, enable this by default) or wraps the statement in an explicit
   `BEGIN TRAN`, the whole reorganize runs inside **one** transaction and **holds
   every lock it acquires until commit** instead of releasing them incrementally.
   That single setting converts "brief page locks" into "locks held for the entire
   run" — the most likely cause of a large, sustained pileup. **Check this first.**

2. **LOB compaction (default ON in SQL Server) — only when there *is* LOB.** For an
   index/table with LOB columns (`varchar(max)`, `nvarchar(max)`, `varbinary(max)`,
   `xml`, `text`, etc.), REORGANIZE compacts the LOB allocation unit; this phase is
   single-threaded, moves LOB pages, and holds locks noticeably longer. **But
   `LOB_COMPACTION = ON` is a no-op on a table with no LOB allocation unit** — there
   is nothing to compact, so it neither helps nor blocks. Do **not** blame LOB
   compaction for a block on an all-scalar table (see "This incident" below). The SQL
   Server default is `LOB_COMPACTION = ON`; SqlGoPace only emits the clause when the
   operation explicitly sets it (see below).

3. **Reader isolation.** Under default READ COMMITTED (locking), readers take S locks
   and **block on the reorganize's page X locks**. With RCSI / snapshot isolation on
   the database, readers version-read and do not block on those X locks — so whether
   the blocking is even *visible* depends on the database's isolation configuration.

4. **A pre-existing long-running transaction** on the same table can make the
   reorganize *wait* while holding locks it has already acquired, putting it at the
   head of a blocking chain.

## This incident: MEASUREMENT (no LOB, still blocked)

The table that blocked ~20 queries was `dbo.MEASUREMENT` in PRODDB — reorganizing
its clustered PK `PK_MEASUREMENT`. Every column is scalar (`datetime`, `numeric`,
`varchar(10..12)`); there is **no `max`/`xml`/`text` column, so no LOB allocation
unit.** That rules out mechanism #2 entirely: `LOB_COMPACTION = ON` did nothing here.

What remains is **pure rowstore leaf-page compaction** — the reorganize taking
short-term X locks on leaf pages of the clustered index as it reorders and compacts
them to fill factor. The workload contends on those same pages (the clustered key
leads with `SETTLEMENTDATE`, so recent-date reads/writes cluster near the physical
tail the reorganize is also touching). A sustained 20-query pileup on top of
"short-term" locks is the fingerprint of mechanism #1 — the reorganize's locks being
held for its whole duration because the session ran under
`IMPLICIT_TRANSACTIONS ON` / an explicit transaction — and/or #3, the database not
being under RCSI so readers block on those X locks. Confirm with the checks below.

## Diagnosing a live incident

To identify which mechanism is responsible, capture (during the block, not after):

- `sys.dm_exec_requests` / `sys.dm_os_waiting_tasks` — the blocker's `wait_resource`
  (a `PAGE`/`KEY` resource vs. a LOB allocation unit vs. an `OBJECT` lock tells #1/#2
  apart).
- The reorganize session's `implicit_transactions` setting
  (`sys.dm_exec_sessions.transaction_isolation_level` and the session options) — to
  confirm or rule out mechanism #1.
- Whether the database has `READ_COMMITTED_SNAPSHOT` on (`sys.databases`) — mechanism
  #3.

A REORGANIZE's block is only present in `sys.dm_exec_requests` **while its statement
is running** (an idle session between internal sub-transactions is invisible there),
so sample **in-flight**, not between statements — the same sampling rule the shrink
driver already follows.

## Implications for pacing REORGANIZE in SqlGoPace

Two hard constraints, and one convenient property:

- **No `WAIT_AT_LOW_PRIORITY`, no `RESUMABLE`.** Those options belong to `REBUILD ...
  ONLINE`, not to REORGANIZE. The compatibility matrix already reflects this:
  `reorganize_index: {}` in `ddl_compatibility.yaml` has **no injectable options**.
  So SqlGoPace's usual least-destructive lever (WALP → RESUMABLE pause/resume → KILL)
  collapses here to just **KILL/cancel**.

- **REORGANIZE is abort-safe — like the shrink loop.** Per the docs:
  > If you cancel a reorganize operation, or if it's otherwise interrupted, the
  > progress it made to that point is **persisted in the database.** To reorganize
  > large indexes, the operation can be started and stopped multiple times until it
  > completes.
  > — [Optimize index maintenance …](https://learn.microsoft.com/sql/relational-databases/indexes/reorganize-and-rebuild-indexes?view=sql-server-ver17#index-maintenance-methods-reorganize-and-rebuild)

  This means a paced REORGANIZE belongs to a `ShrinkRunner`-style driver, **not**
  `MonitoredRunner`: run it, sample blocking in-flight, and **cancel when it blocks —
  the completed work stays** — then re-issue later. Cancel-and-reissue *is* the pause
  mechanism, because there is no RESUMABLE to pause. This mirrors the shrink driver
  and the transient-maintenance-blocker recognition already in the codebase.

- **Force `SET IMPLICIT_TRANSACTIONS OFF`** on the connection that issues REORGANIZE.
  Cheap insurance against mechanism #1, which is otherwise driver-dependent and easy
  to miss.

## How SqlGoPace generates REORGANIZE today

`generateReorganizeIndex` (`internal/ddl/generate.go`) emits:

```sql
ALTER INDEX <index> ON <schema>.<table> REORGANIZE [WITH (LOB_COMPACTION = ON)];
```

`LOB_COMPACTION = ON` is appended **only** when the operation's `LOBCompaction`
field is set (a rule-driven switch, not a version-gated capability — see the comment
on `reorganize_index` in `ddl_compatibility.yaml`). It is not injected automatically.
Reorganize is emitted one op per index by the planner (density-selected — see
`docs/superpowers/specs/2026-07-28-pre-shrink-reorganize-design.md`), never as a
paced/monitored statement yet: turning it into a paced, cancel-and-reissue driver is
the natural follow-up, and would reuse the shrink driver's shape.

## References

- [ALTER INDEX (Transact-SQL)](https://learn.microsoft.com/sql/t-sql/statements/alter-index-transact-sql?view=sql-server-ver17)
- [Optimize index maintenance to improve query performance and reduce resource consumption](https://learn.microsoft.com/sql/relational-databases/indexes/reorganize-and-rebuild-indexes?view=sql-server-ver17)
- [Perform index operations online](https://learn.microsoft.com/sql/relational-databases/indexes/perform-index-operations-online?view=sql-server-ver17)
