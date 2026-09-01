# Blocking, yielding and kills

The part of SqlGoPace that earns its name. Everything here is about one question: when
your DDL and the rest of the workload get in each other's way, who gives way?

## The reaction hierarchy

When the running DDL causes pressure, SqlGoPace escalates from gentlest to harshest and
stops at the first mechanism the target supports:

1. `WAIT_AT_LOW_PRIORITY`, which yields to existing sessions.
2. `RESUMABLE` pause and resume, which continues an index operation later rather than
   rolling it back.
3. `KILL`, a last resort, after a grace period, with bounded retries.

Which of these are even eligible is decided by
[`ddl_compatibility.yaml`](compatibility-matrix.md), keyed by major version and edition
tier. The hierarchy is why there is no query timeout in the tool: duration is governed by
what the server is telling us, not by a clock.

`reorganize_index` has neither of the first two to fall back on, so it paces by cancelling
and re-issuing the statement, which SQL Server permits without losing progress.

## The three directions

Three features look alike, share matcher fields, and do opposite things. Getting the
direction wrong is silent: a rule only ever fires in its own direction, so a login named in
the wrong list simply does nothing.

Find the session first, then read across:

| Where the session appears | What is happening | The feature you want |
|---|---|---|
| `<manifest>.blocked.yaml`, or the console's blocked list | **we** block **it** | [`ignore_blocked_sessions`](#ignoring-sessions-we-block) |
| the run log names it as our blocker, our operation is suspended | **it** blocks **us** | [`kill_blocking_sessions`](#killing-the-sessions-that-block-us) |
| blocked by us, *and* has sessions queued behind it | it amplifies our block into an outage | [`kill_amplifying_maintenance`](#killing-amplifying-maintenance-victims) |

Both directions can be right for the same login at different times: a dashboard query can
block a shrink chunk on Monday and be blocked by it on Tuesday. Naming it in two lists is
coherent, not contradictory.

Read `.blocked.yaml` before writing session policy. Every session in that file is a session
*you* block, so every entry it suggests belongs to `ignore_blocked_sessions`.

## Ignoring sessions we block

By default SqlGoPace is a good citizen: when its DDL blocks another session past
`blocking_timeout_minutes`, it yields. On a busy 24/7 database one trivial session, a report
that wakes hourly or a monitoring poll, can stop an operation ever finishing: it runs,
yields to the nuisance, restarts, yields again.

`ignore_blocked_sessions` names sessions allowed to stay blocked, so the operation holds
its lock through them and keeps going. It is the targeted form of the blanket per-operation
`options.ignore_blocking: true`. Transaction-log protection is always honoured; only the
blocking reaction is suppressed, and only for matching sessions.

```yaml
ignore_blocked_sessions:
  # An entry matches when EVERY field it sets matches (AND); the list is OR'd.
  # String fields are regular expressions, evaluated app-side. session_id is exact.
  - app_name: "^SQLAgent"                 # ignore the SQL Agent job...
    login_name: "svc_reporting"           # ...but only under this login (AND)
  - host_name: "BATCH0[0-9]"              # OR any session from these hosts
  - statement: "FROM dbo\\.AuditLog"      # OR one running this query
  - session_id: 142                       # OR exactly this SPID (volatile; prefer the above)
```

A session is reacted to unless it positively matches a rule, so a narrow or absent list
keeps the default yielding behaviour. That is fail-safe by construction. Prefer `app_name`
and `login_name` for durable rules; `session_id` only identifies a connection that exists
right now.

While an operation holds its lock through an ignored session the run log records it
(`hold: holding the lock through ignored session SPID …`), so the suppression is never
silent.

As a backstop against a rule that turns out to be too broad, set
`options.max_block_minutes: N` on an operation: after N minutes of continuous blocking it
yields anyway, whatever the ignore rules say.

### Discovering who you blocked

When the engine reacts to blocking it writes `<manifest>.blocked.yaml` next to the run
report, listing the sessions it was blocking: ready-to-paste `ignore_blocked_sessions`
entries, commented out, plus a full `observed:` diagnostic block with app, login, host,
query, waits and times seen.

SqlGoPace never reads that file back. Copying an entry into a manifest is a deliberate
step, so you never accidentally ignore real work.

### Adding a rule mid-run

The running manifest's `ignore_blocked_sessions` is re-read on every blocking poll. If an
operation stalls on a blocker you decide is safe, edit the manifest in `02.processing/` and
the operation continues without a restart: the new exclusion takes effect before the next
abort. It is folded into the recovery manifest too, so a later resumed run remembers it.

In the console, select the blocked session and press `i`, then pick the criterion. The rule
is written into the running manifest for you.

## Killing the sessions that block us

`kill_blocking_sessions` handles the opposite case: a session we are waiting on, which we
may terminate so our operation can proceed.

Arming is a deliberate two-step act. The match rules live in the manifest, hot-reloadable
and appendable from the console with `x`, but nothing is ever killed unless the config also
allows it:

```yaml
# config.yaml: the master arm. Off by default; killing a session is destructive.
kill_blockers:
  enabled: true
  default_after_seconds: 60   # applied to a rule that sets no after_seconds
```

```yaml
# the manifest: who may be killed, and after how long they have blocked us
kill_blocking_sessions:
  - login_name: "^svc_dashboard$"    # a read-only dashboard: kill it quickly
    after_seconds: 30
  - app_name: "^SQLAgent"            # a maintenance job: give it longer to finish
    after_seconds: 600
```

The delay is counted over the polls on which the rule actually matched. The first poll of a
blocking episode contributes nothing, because we cannot know how long the blocker had
already been there when we first saw it. The first matching rule decides, so order rules
from most to least specific: a broad rule with a short delay placed first shadows a narrower
one behind it. Each blocker is killed at most once per episode.

### A returning offender does not buy a fresh delay

The delay is served by the *rule*, not by the session id. A blocker killed and coming
straight back under a new SPID, an Agent job restarting its step, a connection pool
retrying, or the next session of a population that all match the same rule, inherits the
blocking time already served under that rule. It is killed on the next poll instead of
buying another full delay. Blocking time is banked per rule for five minutes of quiet,
after which the rule is forgotten and the full delay applies again.

To bound the cost, one rule kills at most three sessions before five quiet minutes. On the
fourth, SqlGoPace stops killing, writes a `warn` into the run report naming the rule and the
last offender, and falls back to the normal blocking reaction, which is the behaviour you
would get with the feature off.

Note what that means for an offender returning every few minutes: each return refreshes the
window, so the rule stays capped for the rest of the manifest rather than earning three
fresh kills every five minutes. That is deliberate. A blocker being restarted faster than it
can be cleared is an operator problem, to be solved by disabling the job or moving the run
outside its window, and an unbounded kill loop would trade a blocked run for a rollback
storm.

### One consequence of counting per rule

A rule matching on `statement:` only accrues while a matching statement runs. Time is banked
on the polls where the whole rule matched, so a blocker alternating between a matching and a
non-matching statement takes *longer* than `after_seconds` of wall-clock blocking to be
killed: the polls where it ran something else count for nothing against that rule.

That errs toward not killing, which is the safe direction, but it is not what reading
`after_seconds` as "after N seconds of blocking us" would suggest.

### Getting it backwards: a worked example

A shrink on `PRODDB` kept aborting its chunks. The manifest named the two dashboard logins
under `kill_blocking_sessions`, but `<manifest>.blocked.yaml` listed them under `observed:`.
They were waiting on `LCK_M_SCH_S` behind the shrink's page relocation: victims, not
blockers. The rules never matched anything, the dashboards tripped the yield timer every
time they refreshed, and the shrink lost each chunk it had started.

They belonged in `ignore_blocked_sessions`: read-only `SELECT`s with `open_transactions: 0`
cost nothing to keep waiting. The ingestion login in the same capture, an `INSERT` with two
open transactions, was correctly left out of both lists, and `options.max_block_minutes`
kept backstopping the run.

The console used to make this same mistake for you. Its `X` key — on the blocked list, i.e.
on victims — wrote a `kill_blocking_sessions` rule, which `BlockerKiller` only ever matches
against the session blocking us. The rule could not fire, and said nothing about it. It was
removed in 0.24.0; `x` now confirms first and names the open transaction count, and the
roster (`b`) is where a kill rule against a real blocker is armed.

## Killing amplifying maintenance victims

`ALTER INDEX … REORGANIZE` is an online operation: it takes only `Sch-S` plus short-lived
page locks and does not block readers, until a single incompatible request queues behind it.

SQL Server never lets a compatible lock request pass a queued incompatible one, so the
moment a `Sch-M` requester (a nightly `UPDATE STATISTICS`, another `ALTER INDEX`, a
`TRUNCATE TABLE`) starts waiting on your reorganize, every reader arriving afterwards queues
behind *that* request instead of passing through. The fan-out only grows while the
reorganize runs: one queued statistics job on `dbo.MEASUREMENT` turned an online index
maintenance pass into a full-table outage for every subsequent `SELECT`.

The victim is not application work. It is a SQL Agent maintenance statement that will run
again on its next schedule, which makes it both the direct cause of the outage and the
cheapest thing to terminate.

`kill_amplifying_maintenance` arms a second, independent killer for exactly this: the mirror
of `kill_blockers`, killing sessions blocked *by us*. Off by default.

```yaml
kill_amplifying_maintenance:
  enabled: false          # opt-in
  min_blocked_behind: 1   # sessions queued behind the victim before it counts as an amplifier
  after_seconds: 60       # how long it must stay eligible before the KILL
  commands: []            # empty uses the built-in allow-list; a non-empty list REPLACES it
```

A blocked session is a candidate only when all of these hold:

1. it is directly blocked by our DDL session, not merely a transitive victim further down
   the chain;
2. it is not another SqlGoPace session, matched by prefix against this connection's own
   application name, so a second manifest running concurrently is never a candidate,
   including one on a different build;
3. its `sys.dm_exec_requests.command` matches the allow-list: by default `ALTER INDEX`,
   `ALTER TABLE`, `CREATE INDEX`, `CREATE STATISTICS`, `UPDATE STATISTIC` (both spellings
   SQL Server reports), `DROP INDEX`, `DROP TABLE`, `TRUNCATE TABLE`, `DBCC`;
4. it matches no `ignore_blocked_sessions` rule;
5. it is not the session our own DDL is waiting on, which belongs to `kill_blockers`. The
   two killers are disjoint by construction, so a mutual-block snapshot still produces
   exactly one `KILL`;
6. at least `min_blocked_behind` sessions are queued transitively behind it.

A non-empty `commands:` list *replaces* the default set rather than extending it, so you can
narrow the feature to `UPDATE STATISTIC` alone. An entry that is empty or whitespace-only,
the usual result of a dangling YAML item, is rejected at startup: it would be a prefix of
every command verb and would silently widen the feature to every session the run blocks.

All six conditions must hold for `after_seconds` before the `KILL` fires. That dwell
accumulates over the polls where every condition holds, so a victim dropping out of
eligibility for a poll loses that interval but keeps the time already served.

A victim is suppressed from the yield reaction for the whole dwell, from the first poll it
is eligible and well before any `KILL`. An `after_seconds` longer than
`monitoring.blocking_timeout_minutes` therefore means the operation keeps its lock through
the amplifier for the whole dwell, with `max_block_minutes` as the only backstop. That is a
valid choice, since killing sooner is the more destructive one, so SqlGoPace only warns
about it at startup.

### Precedence, which is not symmetric

- `ignore_blocked_sessions` **beats** the kill. A session the operator explicitly named as
  allowed to stay blocked is never killed, whatever its command: explicit instruction beats
  automatic classification.
- `ignore_blocking: true` does **not**. That option only means "do not yield"; it suppresses
  our own reaction to being blocked and says nothing about intervening on the victim. An
  operator running with it will still see maintenance victims killed, and precisely because
  they are holding the lock through blocking, they benefit most from the amplifier being
  cleared.

A failed `KILL`, whether permission denied or the session already gone, immediately falls
back to the normal yield: the victim stops being suppressed and counts toward the blocking
timer again on the very next poll. This feature can never make a run block *longer* than it
would without it.

The same repeat-offender rule as `kill_blocking_sessions` applies here, keyed on the SQL
Agent job step when `program_name` resolves to one, and on the login, host and program
triplet when it does not. The identity is charged at most once per poll, and the charge is
measured from that identity's own previous appearance, so one job running several concurrent
sessions does not reach the dwell any faster than a single connection would.

### Attribution and what to do afterwards

A T-SQL Agent job step sets `program_name` to `SQLAgent - TSQL JobStep (Job 0x… : Step N)`;
SqlGoPace parses that and looks up the job and step name in msdb. Only T-SQL steps carry
that program name, so a CmdExec or PowerShell step, including Ola Hallengren's scripts driven
through `sqlcmd`, will not match. The kill still happens; attribution degrades to the raw
program name, login and host, which is why all three are always recorded.

SqlGoPace never writes to msdb and never disables an Agent job itself. It reports which one
collided and prints a statement for you to run by hand:

```sql
EXEC msdb.dbo.sp_update_job @job_name = N'IndexOptimize - USER_DATABASES', @enabled = 0;
```

Naming the job needs `SELECT` on `msdb.dbo.sysjobs` and `sysjobsteps`. That grant is
optional.

A run that kills at least one amplifying victim writes `<manifest>.amplifiers.yaml`: one
entry per kill, plus a trailing comment block with one deduplicated `sp_update_job` statement
per distinct job terminated. It is deliberately a separate file from `.blocked.yaml`, whose
whole purpose is "paste this into `ignore_blocked_sessions`", and an amplifier is the exact
opposite instruction.

## Permissions

Every kill path needs `ALTER ANY CONNECTION`, or `processadmin`, or `sysadmin`. That
includes `options_override.allow_abort_blockers`, which resolves
`ABORT_AFTER_WAIT = BLOCKERS`.

Without the grant, preflight warns rather than failing, because a run that cannot kill a
blocker is still a valid run, and every kill becomes a silent no-op. See
[`permissions.md`](permissions.md).
