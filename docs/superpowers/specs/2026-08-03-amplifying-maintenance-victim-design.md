# Killing amplifying maintenance victims of a non-blocking operation

Status: design approved, pending implementation plan.
Date: 2026-08-03.

## Motivation

`ALTER INDEX ... REORGANIZE` is an online operation: it holds `Sch-S` plus short-term
page locks, and does not block readers. That guarantee collapses the moment a single
`Sch-M` requester queues behind it.

Observed on the PRODDB campaign:

| SPID | command | status | blocked by | wait | elapsed |
|------|---------|--------|-----------|------|---------|
| 67 | `ALTER INDEX [PK_MEASUREMENT] ... REORGANIZE` | running | — | — | 2d 18h, 53.6% |
| 79 | `UPDATE STATISTICS [dbo].[MEASUREMENT] [PK_MEASUREMENT] WITH MAXDOP 2` | suspended | 67 | `LCK_M_SCH_M` | 5h 21m |
| 91, 54, 109, 64, 176, 110, 103, 93, 104, 150, 161, 69, 182, 147, 180, 130 | `SELECT` | suspended | 79 | `LCK_M_SCH_S` | up to 5h 10m |

SQL Server does not let a compatible lock request barge past a queued incompatible
one. So one `Sch-M` waiter turns our non-blocking reorganize into a full-table outage
for every reader that arrives afterwards. The fan-out grows monotonically: it never
recovers on its own while the reorganize runs.

The victim is not application work. It is a SQL Agent maintenance job that will run
again on its next schedule. It is both the cheapest thing to terminate and the direct
cause of the outage.

### The collision this design has to resolve

Today the engine already reacts to this: SPID 79 is a session we block, it matches no
`ignore_blocked_sessions` rule, so `BlockState.Unignored` goes true and we cancel our
own reorganize after `blocking_timeout` — seconds. That is a defensible default, but
it means a long index maintenance campaign is repeatedly evicted by a statistics job
it will collide with again on the next pass.

Any design that waits before killing the victim must therefore also stop that victim
from tripping the yield timer, or the kill can never fire. That suppression is the one
place this feature changes existing blocking semantics, and it is bounded by the
existing `max_block_minutes` safety cap.

## Scope

- **In:** detecting a directly-blocked maintenance victim with sessions queued behind
  it, killing it after a dwell, suppressing its contribution to the yield timer while
  the kill is pending, attributing it to a SQL Agent job, and reporting all of that.
- **In:** a preflight advisory about `ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY`.
- **Out:** writing to msdb. SqlGoPace never enables or disables an Agent job. It
  reports the job and emits a ready-to-paste `sp_update_job` statement; acting on it
  is a deliberate operator step.
- **Out:** a preflight scan of Agent job definitions for likely conflicts. Keyword
  matching on `sysjobsteps.command` is heuristic and false-positive prone; the
  attribution path reports the job that actually collided, which is strictly better
  information.
- **Out:** a per-manifest override. Arming and thresholds live in `config.yaml` only.
- **Unchanged:** `kill_blocking_sessions` (the mirror direction — sessions blocking
  *us*), `ignore_blocked_sessions`, `ignore_blocking`, `max_block_minutes`, and
  `DecideReaction`. This feature adds no `Action`.

## 1. Detection

All detection runs on the `ActiveSessions()` snapshot the blocking poll already reads.
No new query is issued in the hot path.

### 1.1 The blocking chain

Index the snapshot once per poll into `map[int][]mssql.Session` keyed on
`BlockingSPID`. For each direct victim `v` (`v.BlockedBy(ourSPID)`), walk that map
transitively from `v` and count the sessions reachable, excluding `v` itself. In the
table above the walk from SPID 79 yields 16.

The walk carries a `visited` set. A snapshot of the blocking graph is not guaranteed
acyclic — it is assembled row by row from a DMV under concurrency — and an unguarded
walk would not terminate.

### 1.2 Classifying the victim

A new function in `internal/mssql`, on `dm_exec_requests.command`, case-folded,
space-trimmed, **prefix**-matched:

```
ALTER INDEX · ALTER TABLE · CREATE INDEX · CREATE STATISTICS
UPDATE STATISTIC · DROP INDEX · DROP TABLE · TRUNCATE TABLE · DBCC
```

Prefix matching is deliberate: the verb SQL Server reports for `UPDATE STATISTICS`
must be confirmed against a live server (the SSMS grid truncates it, and it is
rendered without the trailing `S` in some versions). `UPDATE STATISTIC` as a prefix
covers both spellings. An integration test pins the actual value.

This is a **new** function, not an extension of `mssql.IsMaintenanceCommand`. That
existing function answers a different question — "is this blocker of the shrink a
self-clearing transient?" — and widening its allow-list would silently change the
shrink driver's tail-object attribution.

### 1.3 Eligibility

A victim qualifies for the kill when all of the following hold:

1. it is **directly** blocked by our DDL session (`Session.BlockedBy`, not a
   transitive victim);
2. its command matches the allow-list in §1.2;
3. at least `min_blocked_behind` sessions are queued behind it (default **1** — one
   queued reader means the amplification has already begun, and it only grows);
4. (1)–(3) have held continuously for `after_seconds` (default 60);
5. it matches no `ignore_blocked_sessions` rule (see §2.2);
6. it is not another SqlGoPace session (§1.4).

### 1.4 Self-exclusion

A second SqlGoPace instance running a `REBUILD` reports `command = ALTER INDEX` and
would classify as a killable amplifier. Sessions whose `program_name` matches our own
connection's application name are never considered. This is not hypothetical: the
PRODDB campaign runs size-split manifests that can overlap in time.

### 1.5 Episode state

Keyed by victim SPID — unlike `BlockerKiller`, which tracks the single session
blocking us, several victims can be eligible at once. Per SPID: the timestamp it first
became eligible, and a killed flag. Each victim is killed at most once per episode;
its entry is dropped when it stops being blocked by us.

## 2. Reaction wiring

### 2.1 `VictimKiller`

New `internal/run/victim.go`, shaped like `internal/run/kill.go`: a kill func, an
event callback, a clock, and episode state under a mutex. The engine arms it per
manifest and disarms it between manifests. It is only constructed when the feature is
enabled in config, so it carries no separate enabled flag — the same convention
`BlockerKiller` uses.

`ServerSampler.Blocking` gains one branch inside the loop it already runs, and
consults the killer at the end using the same snapshot, mirroring today's
`s.killer.consider(...)` call:

- a victim that is **kill-eligible and pending** contributes to `st.Any` but **not**
  to `st.Unignored`;
- everything else is unchanged.

`max_block_minutes` keys off `Any`, so it continues to backstop the entire feature: a
victim we never manage to kill still forces a yield at the cap.

### 2.2 Precedence

**`ignore_blocked_sessions` wins over the kill.** A session the operator explicitly
named as allowed to stay blocked is never killed, whatever its command. Explicit
instruction beats automatic classification.

**`ignore_blocking: true` does not suppress the kill.** That option means "do not
yield", not "do not intervene". An operator holding the lock through blocking is
precisely the one who benefits from the amplifier being cleared.

**A failed `KILL` withdraws the suppression.** On error (permissions, session already
gone) that SPID immediately counts toward `Unignored` again and we yield on the normal
timer. The feature can never make us block *longer* than today when it is not working.

### 2.3 Grace window

After a successful kill, the victim's SPID stays suppressed for two further **blocking
polls**, so a rollback still visible in the snapshot does not instantly trip the
yield. It then counts normally again.

### 2.4 Configuration

```yaml
monitoring:
  kill_amplifying_maintenance:
    enabled: false           # opt-in
    min_blocked_behind: 1
    after_seconds: 60
    commands: []             # optional override of the built-in allow-list
```

A non-empty `commands` list **replaces** the built-in allow-list of §1.2 rather than
extending it, so an operator can narrow the feature to `UPDATE STATISTIC` alone.
Entries are matched by the same case-folded prefix rule.

`KILL` requires `ALTER ANY CONNECTION`, which `kill_blocking_sessions` already
requires — no new grant. `SELECT` on `msdb.dbo.sysjobs` / `sysjobsteps` is new,
optional, and degrades gracefully (§3.1).

## 3. Attribution and reporting

### 3.1 SQL Agent job attribution

A T-SQL Agent job step sets `program_name` to
`SQLAgent - TSQL JobStep (Job 0x<hex job_id> : Step N)`.

A pure `ParseJobStepProgram(program string) (hex string, step int, ok bool)` in
`internal/mssql` extracts the pair. A new `internal/mssql/agent.go` resolves it:

```sql
SELECT j.name, s.step_name
FROM msdb.dbo.sysjobs j
LEFT JOIN msdb.dbo.sysjobsteps s ON s.job_id = j.job_id AND s.step_id = @step
WHERE j.job_id = CONVERT(uniqueidentifier, CONVERT(varbinary(16), @hex, 1));
```

The GUID conversion is done in T-SQL on purpose: the binary layout of a
`uniqueidentifier` is mixed-endian, and reimplementing it in Go is a needless source
of bugs. Results are cached per `(job, step)` for the run.

Limitation, stated rather than hidden: only **T-SQL** job steps carry this program
name. A CmdExec or PowerShell step, or Ola Hallengren's scripts driven through
`sqlcmd`, will not match. Attribution then degrades to the raw program/login/host and
the kill still happens.

### 3.2 Reaction events

The kill emits `ReactionEvent{Kind: "kill"}`, which already flows to console, the
`.log` run report, the TUI incident feed, and webhook/email notification — no new
plumbing:

```
killed amplifying maintenance session SPID 79 (UPDATE STATISTICS on
[dbo].[MEASUREMENT]) — 16 sessions queued behind it; source: SQL Agent job
"IndexOptimize - USER_DATABASES" step 1
```

Alongside it, one warning per **distinct job** per run, carrying the remediation:

```
WARN: SQL Agent job "IndexOptimize - USER_DATABASES" (step 1) conflicts with this
run's index maintenance. To disable:
EXEC msdb.dbo.sp_update_job @job_name = N'IndexOptimize - USER_DATABASES', @enabled = 0;
```

### 3.3 `.amplifiers.yaml` sidecar

A third advisory sidecar alongside `.blocked.yaml` and `.contended.yaml`;
`relocateSidecar` already generalizes over the suffix, and `relocateCaptures` gains
one line.

It is deliberately **not** folded into `.blocked.yaml`: that file's stated purpose is
"paste this into `ignore_blocked_sessions`", and an amplifier is the exact opposite
instruction. The new file lists each killed amplifier with its command, chain size,
kill timestamp, and job attribution, plus a deduplicated block of ready-to-paste
`sp_update_job` statements. Advisory only, never read back, like its siblings.

### 3.4 TUI

Reaction events already render in the incident feed, but a job warning that scrolls
away is a warning nobody acts on. The distinct conflicting job names get a sticky line
in the alert area, reusing the existing manifest-alert mechanism.

## 4. `ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY` preflight advisory

SQL Server 2022 added a database-scoped configuration that makes statistics updates
queue politely instead of blocking:

```sql
ALTER DATABASE SCOPED CONFIGURATION SET ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY = ON;
```

It is worth recommending, **and** its limit is worth stating in the same breath: it
covers only *asynchronous automatic* statistics updates. The PRODDB collision is an
explicit `UPDATE STATISTICS ... WITH MAXDOP 2` issued by an Agent job, which the
setting does **not** cover. An operator who enables it and assumes the problem is
solved will be surprised.

New `internal/run/async_stats_advisory.go`, a pure decision function in the shape of
`reorgRCSIWarning` in `reorg_rcsi.go`, emitted at the same point (before a
`reorganize_index` operation) and through the same path, so it reaches the log and the
TUI without new plumbing.

Inputs: the operation, the database name, the server major version, and the current
setting value read from `sys.database_scoped_configurations` where
`name = 'ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY'`.

Emission rules:

- the setting is absent from `sys.database_scoped_configurations` (server older than
  SQL Server 2022, major version < 16) → no advisory; nothing is actionable.
- the setting is present and **off** → emit the recommendation plus the limitation.
- setting **on** → emit the limitation alone, so an operator who has already enabled
  it is not left believing explicit `UPDATE STATISTICS` is covered.

Text when the setting is off:

```
<schema>.<table>: ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY is OFF on <db> — enabling it
(ALTER DATABASE SCOPED CONFIGURATION SET ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY = ON)
lets automatic statistics updates queue at low priority instead of blocking this
REORGANIZE. It does NOT cover an explicit UPDATE STATISTICS run by a job or by hand;
those still block and can queue readers behind them.
```

## 5. Error handling

| Condition | Behavior |
|---|---|
| `ActiveSessions` read fails | Poll skipped, as today. |
| msdb unreadable / permission denied | Attribution degrades to the raw program name; no run impact, one debug line. |
| `KILL` returns an error | Warn event; suppression withdrawn for that SPID (§2.2); yield on the normal timer. |
| Victim disappears between classification and kill | The `KILL` errors harmlessly; the episode entry is dropped. |
| Cyclic or inconsistent blocking graph | The `visited` set terminates the walk (§1.1). |
| Sidecar write fails | Reported to the run output; the run continues, matching `flushCapture`. |

## 6. Testing

Everything decision-shaped is pure and needs no database:

- fan-out counting: transitive depth, multiple simultaneous victims, cyclic graph,
  victim with nothing behind it;
- command classification, including both `UPDATE STATISTIC` spellings and a
  non-maintenance verb;
- self-exclusion by application name;
- eligibility timing against the fake clock (`internal/run/clock.go`);
- `ignore_blocked_sessions` precedence (an ignored maintenance victim is never
  killed);
- kill-failure withdrawal and the two-poll grace window;
- `ParseJobStepProgram` table test, including a CmdExec program name that must not
  match;
- the advisory decision function across the three emission rules in §4.

Sampler-level tests assert the load-bearing invariant directly: an eligible victim
produces `BlockState{Any: true, Unignored: false}`, and `Unignored: true` once its
kill has failed.

Behind the `integration` build tag, because they are facts about the server rather
than about our logic:

- the exact `command` verb reported for a running `UPDATE STATISTICS`;
- the msdb job/step lookup round-trip, including the `uniqueidentifier` conversion.

**No e2e test.** Staging a genuine three-level amplification deterministically in the
compose container is disproportionate to the risk, and the unit/integration split
covers what can actually break. Recorded here as a deliberate omission rather than
left silent.

## 7. Files touched

| File | Change |
|---|---|
| `internal/run/victim.go` | **New.** `VictimKiller`, eligibility, episode state, chain walk. |
| `internal/run/async_stats_advisory.go` | **New.** Pure advisory decision function (§4). |
| `internal/mssql/agent.go` | **New.** `ParseJobStepProgram` + msdb job/step lookup. |
| `internal/run/executor.go` | `ServerSampler.Blocking` suppression branch; `SetVictimKiller`. |
| `internal/run/engine.go` | Arm/disarm per manifest; emit the advisory; sidecar flush. |
| `internal/run/capture.go` | `.amplifiers.yaml` render + one line in `relocateCaptures`. |
| `internal/mssql/maintenance.go` | Maintenance-command classifier for victims, beside the existing shrink-facing one (§1.2). |
| `internal/mssql/server.go` | Read `ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY`. |
| `internal/config` | `kill_amplifying_maintenance` block. |
| `internal/tui` | Sticky conflicting-job alert line. |
| `README.md`, `specs/SPECS.md` | Document the option, the sidecar, and the advisories. |

## References

- [Why is Index Reorganize and Update Statistics causing SQL Server blocking](https://www.mssqltips.com/sqlservertip/5880/why-is-index-reorganize-and-update-statistics-causing-sql-server-blocking/)
- [Does updating statistics cause blocking?](https://blog.sqlgrease.com/updating-statistics-cause-blocking/)
- [Async stats update causing blocking](https://straightforwardsql.com/posts/async-stats-update-causing-blocking/)
- [Locking in Microsoft SQL Server (Part 13 – Schema locks)](https://aboutsqlserver.com/2012/04/05/locking-in-microsoft-sql-server-part-13-schema-locks/)
- [The Sch-M lock is Evil](https://michaeljswart.com/2013/04/the-sch-m-lock-is-evil/)
