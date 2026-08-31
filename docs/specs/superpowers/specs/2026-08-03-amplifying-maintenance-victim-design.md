# Killing amplifying maintenance victims of a non-blocking operation

Status: design approved, pending implementation plan.
Date: 2026-08-03. Revised the same day after review (see `*-kimi.md`).

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

Prefix matching is deliberate. **Verified 2026-08-04 against a live server: the verb
is `UPDATE STATISTICS`, with the trailing `S`** — so the allow-list matches. The entry
keeps the shorter `UPDATE STATISTIC` prefix on purpose, because the SSMS grid truncates
the column and the form without the trailing `S` has been reported on other versions;
the prefix covers both and costs nothing. An integration test pins the actual value.

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
6. it is not another SqlGoPace session (§1.4);
7. it is not our own direct blocker (§1.6).

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

### 1.6 Disjointness from `BlockerKiller`

`BlockerKiller` targets the session *we* are blocked by; `VictimKiller` targets
sessions blocked *by us*. A SPID satisfying both would mean a mutual block — a
two-session cycle that SQL Server's deadlock monitor normally resolves, but which a
DMV snapshot assembled row by row can show transiently.

Rather than reconcile two killers after the fact, criterion 7 makes the sets disjoint
by construction: a session appearing as our direct blocker in the same snapshot is
never a kill candidate for `VictimKiller`. `BlockerKiller` owns it.

Two consequences follow. Episode state is never shared, because no SPID is ever in
both. And the order in which `ServerSampler.Blocking` consults the two killers does
not matter — a property worth asserting in a test so a later refactor cannot quietly
introduce a double `KILL`.

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

There is nothing to reconcile between the ignore list and the suppression rule of
§2.1, because an ignored session already does not set `Unignored` — that is what
ignoring it means. It contributes to `Any` only, exactly as it does today, and
`max_block_minutes` remains its sole backstop. Kill suppression applies to a distinct
population: victims that *would* otherwise trip the yield timer.

**`ignore_blocking: true` does not suppress the kill.** That option means "do not
yield", not "do not intervene". An operator holding the lock through blocking is
precisely the one who benefits from the amplifier being cleared.

**A failed `KILL` withdraws the suppression.** On error (permissions, session already
gone) that SPID immediately counts toward `Unignored` again and we yield on the normal
timer. The feature can never make us block *longer* than today when it is not working.

### 2.3 Grace window

After a successful kill, the victim's SPID stays suppressed for a fixed **15 seconds**,
so a rollback still visible in the snapshot does not instantly trip the yield. It then
counts normally again.

The window is time-based rather than a poll count on purpose: the blocking poll
interval is configurable, so "two polls" would silently mean anything from two seconds
to a minute, and the fake clock makes a duration far easier to test than a tick count.
Fifteen seconds is not configurable — a killed lock request clears from the wait queue
almost immediately, and the window exists only to absorb one stale snapshot.

### 2.4 Configuration

```yaml
kill_amplifying_maintenance:
  enabled: false           # opt-in
  min_blocked_behind: 1
  after_seconds: 60
  commands: []             # optional override of the built-in allow-list
```

Top-level, as a sibling of the existing `kill_blockers:` block, not nested under
`monitoring:`. `kill_blockers` is the mirror-direction feature and is already
top-level; putting its counterpart somewhere else would be gratuitously
inconsistent. `monitoring:` holds cadences and thresholds, not policy arming.

A non-empty `commands` list **replaces** the built-in allow-list of §1.2 rather than
extending it, so an operator can narrow the feature to `UPDATE STATISTIC` alone.
Entries are matched by the same case-folded prefix rule. An **absent or empty** list
means the built-in allow-list, never "match nothing" — the only way to disable the
feature is `enabled: false`.

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

The kill emits `ReactionEvent{Kind: "kill"}`, which the engine sink already records in
the `.log` run report and prints to the run output:

```
killed amplifying maintenance session SPID 79 (UPDATE STATISTICS on
[dbo].[MEASUREMENT]) — 16 sessions queued behind it; source: SQL Agent job
"IndexOptimize - USER_DATABASES" step 1
```

Two limits of that path, both verified against the code rather than assumed:

- **Webhook and email do not fire.** `engine.go` calls `e.notify` only for `pause`,
  `cancel` and `abort` — the kinds that mean we yielded. A `kill` is recorded but not
  pushed. That is pre-existing behavior and this design does not change it; it is
  stated here so the run report is not mistaken for a notification.
- **In TUI mode the run output is `io.Discard`** (`main.go:206`), so the printed line
  goes nowhere. Per-kill narration therefore needs an explicit forward to the TUI, the
  way `BlockerKiller` already forwards its own kills — see §3.4.

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
instruction. Advisory only, never read back, like its siblings.

```yaml
# Amplifying maintenance victims killed during 032-compress-small.yaml
# Advisory only — SqlGoPace never reads this file back.
# Each entry is a session this run terminated because it requested a Sch-M lock
# behind our operation while other sessions queued behind it.

killed:
  - session_id: 79
    command: "UPDATE STATISTICS"
    statement: "UPDATE STATISTICS [dbo].[MEASUREMENT] [PK_MEASUREMENT] WITH MAXDOP 2"
    database: "PRODDB"
    login_name: "CORP\\svc_sqlagent"
    host_name: "SQLPROD01"
    app_name: "SQLAgent - TSQL JobStep (Job 0x9B3C... : Step 1)"
    blocked_behind: 16
    waited_ms: 19280023
    first_eligible: "2026-08-03T13:41:11Z"
    killed_at: "2026-08-03T13:42:11Z"
    agent_job:
      resolved: true
      job_id: "0x9B3C..."
      job_name: "IndexOptimize - USER_DATABASES"
      step_id: 1
      step_name: "Update statistics"

# Distinct SQL Agent jobs terminated by this run. Review before disabling:
#   EXEC msdb.dbo.sp_update_job @job_name = N'IndexOptimize - USER_DATABASES', @enabled = 0;
```

`login_name`, `host_name`, `app_name` and `statement` are recorded on **every** entry,
not only the attributed ones. When `resolved: false` — msdb unreadable, or a CmdExec /
PowerShell step that carries no job id in `program_name` — `job_name` and `step_name`
are omitted and those four fields are all the operator has to identify the source
manually. That is precisely the case where they matter most, so they are never
conditional on attribution succeeding.

Multiple kills attributed to the same job produce one `killed:` entry each (they are
distinct sessions, at distinct times) but a **single** `sp_update_job` line in the
trailing comment block, deduplicated on job id.

One concurrency note, because it differs from `blockerCapture`: that accumulator is
touched only from the engine goroutine, but a kill happens on the **pump** goroutine
inside `Sampler.Blocking`. The amplifier accumulator therefore carries its own mutex.
It is small and append-mostly, so a mutex is the right amount of machinery — no
channel funnel.

### 3.4 TUI

The TUI needs **two** things, because the run output it would otherwise read from is
`io.Discard` while the console is up.

Per-kill narration is forwarded explicitly, by a callback on the killer that sends a
`tui.LogMsg` — exactly what `BlockerKiller` already does for its own kills
(`main.go:395`). This is presentation only and lives in the CLI; the engine keeps its
own `ReactionEvent` path for the run report and the sidecar. The two exist because
they have different consumers and different lifetimes, not by accident.

The second is the sticky line: a job warning that scrolls past in the incident feed is
a warning nobody acts on, so the distinct conflicting job names are rendered in the
alert area.

It uses a **new** `tui.ConflictingJobsMsg{Jobs []string}` that *replaces* the model's
current set, rather than the existing `AlertMsg`. `AlertMsg` appends to a slice that is
never cleared — correct for a manifest failure, which should stay on screen for the
rest of the run, but wrong here. Reusing it would mean teaching it to clear and
changing the behavior of every existing alert. A replace-semantics message keeps the
two lifetimes independent and is less code.

Deduplication key: `(job_id, step_id)` when attribution resolved, falling back to the
raw `program_name` when it did not — so an unattributed CmdExec step still produces
one stable line rather than one per kill. Dedup lives in the run package's capture
accumulator (§3.3), which needs the same distinct-job set for the sidecar; the TUI
just renders what it is sent.

Lifetime: the engine sends `ConflictingJobsMsg{Jobs: nil}` when it disarms the killer
at the end of a manifest. A queue that runs for hours would otherwise accumulate jobs
from manifests the operator has already dealt with, and a stale alert is read as noise
within about two manifests.

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

The setting is **database-scoped**, so it must be read per target database. In a
multi-database run `buildEngine` is called once per database with a connection already
scoped to it; the read belongs there, not in `runEngine`, which holds only the startup
connection.

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
- kill-failure withdrawal and the 15-second grace window, on the fake clock;
- disjointness from `BlockerKiller` (§1.6): a snapshot showing a mutual block must
  produce exactly one `KILL`, and must do so regardless of the order the two killers
  are consulted in;
- `ParseJobStepProgram` table test: the canonical form, tolerance for extra internal
  whitespace and casing, a CmdExec program name that must not match, and a truncated
  or malformed program name that must fail closed rather than yield a wrong job id;
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

**No live-Agent-job test either.** Creating a job, starting it, and catching its
`program_name` mid-flight would require enabling SQL Agent in the compose container
and racing a running step — substantial machinery to test a fixed string format. The
format is pinned by the table test above using real captured samples, and the part
that genuinely depends on the server (the `uniqueidentifier` conversion in the msdb
lookup) is already covered by the integration test below.

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
| `README.md`, `docs/specs/SPECS.md` | Document the option, the sidecar, and the advisories. |

## References

- [Why is Index Reorganize and Update Statistics causing SQL Server blocking](https://www.mssqltips.com/sqlservertip/5880/why-is-index-reorganize-and-update-statistics-causing-sql-server-blocking/)
- [Does updating statistics cause blocking?](https://blog.sqlgrease.com/updating-statistics-cause-blocking/)
- [Async stats update causing blocking](https://straightforwardsql.com/posts/async-stats-update-causing-blocking/)
- [Locking in Microsoft SQL Server (Part 13 – Schema locks)](https://aboutsqlserver.com/2012/04/05/locking-in-microsoft-sql-server-part-13-schema-locks/)
- [The Sch-M lock is Evil](https://michaeljswart.com/2013/04/the-sch-m-lock-is-evil/)
