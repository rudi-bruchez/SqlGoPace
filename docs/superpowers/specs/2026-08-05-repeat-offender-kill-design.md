# Accelerating the kill of a repeat-offender blocker

Status: design approved, pending implementation plan.
Date: 2026-08-05.

## Motivation

Both killers grant every new session a full dwell before terminating it, because both
key their episode state on the SPID:

```go
// kill.go — BlockerKiller.consider
if blocker.SPID != k.current {
    k.current, k.since, k.killed = blocker.SPID, now, false
}
```

```go
// victim.go — VictimKiller.considerLocked
episodes map[int]*victimEpisode // keyed by SPID
```

A killed session never comes back under its own SPID. What comes back is the same
*work*: a SQL Agent job step restarting, an application connection retrying out of its
pool, or the next session of a population that all match the same profile. Each arrival
is a new SPID, so each arrival buys a fresh `after` — 60 s by default.

The result is a treadmill. A job that restarts every few seconds costs 60 s of blocking
per round, indefinitely, and the blocked ratio never drops. The dwell was meant to be a
grace period offered once to a session that might finish on its own; keyed on the SPID it
becomes a grace period offered forever to work that demonstrably will not.

The shrink driver feels this worst. Its sampler only runs while a statement is in flight
— a chunk, the `TRUNCATEONLY` pass, the log shrink — so the observation restarts cold at
every chunk boundary. A blocker that reappears once per chunk is never observed
continuously for 60 s, and is therefore never killed at all.

## Scope

- **In:** accumulating blocking time per *offender identity* rather than per SPID, so a
  returning offender inherits the time already served.
- **In:** a rate cap on repeat kills, and an operator-visible escalation when it is hit.
- **In:** a `ReactionSink` on `BlockerKiller`, which today reports only to the console.
- **Out:** new configuration. The window and the cap are commented constants, following
  the `killGraceWindow` precedent. The feature is active whenever the killer is armed.
- **Out:** msdb job attribution for `BlockerKiller`. It has no resolver, and adding one
  is a separate feature; its escalation names the raw login/host/program.
- **Out:** any change to `DecideReaction`, the reaction hierarchy, `ignore_blocked_sessions`,
  `ignore_blocking`, or `max_block_minutes`. This feature adds no `Action`.
- **Unchanged when capped:** once an identity hits the cap the killer simply goes quiet
  and the normal yield timer takes over — that is exactly today's behavior, so the
  feature cannot make a run block longer than it does now.

## 1. Offender identity

What "the same offender" means differs by killer, because what recurs differs.

| Killer | Key | Rationale |
|---|---|---|
| `BlockerKiller` | the matched `kill_blocking_sessions` rule, canonicalized as `sid\|app\|host\|login\|stmt` from the compiled regexps' source strings | the rule is the operator's explicit declaration that this *class* of session may be killed. It is the only key that covers the population case, where no single session returns but the class keeps supplying new ones |
| `VictimKiller` | `job:<hex>:<step>` when `mssql.ParseJobStepProgram` resolves the program name, else `sess:<login>\|<host>\|<program>` | there are no rules on this side. The identity that recurs is the Agent job; the connection triplet is the fallback when the program name is not a job step |

The rule key derives from the rule's text, not its index, so it survives the hot reload
in `manifestKillSource.Current` — a rule appended mid-run by the TUI does not invalidate
the debt accumulated against the others. A rule that pins `session_id` is inherently
single-session; keying on it is meaningless but harmless, since a new SPID cannot match
it anyway.

The victim key deliberately excludes the command verb: a job that alternates
`UPDATE STATISTICS` and `ALTER INDEX` across restarts is one offender, not two.

## 2. The debt clock

A new pure file, `internal/run/recidivism.go`. No mutex of its own — both callers already
hold theirs across the call.

```go
// bucket is one offender identity's accumulated blocking debt inside the window.
type bucket struct {
    accrued    time.Duration // blocking time already served under this identity
    kills      int
    lastActive time.Time
}

type recidivism struct {
    now     func() time.Time
    buckets map[string]*bucket
}

func (r *recidivism) debt(key string) time.Duration
func (r *recidivism) accrue(key string, d time.Duration)
func (r *recidivism) kills(key string) int
func (r *recidivism) recordKill(key string) int
func (r *recidivism) prune()
```

`now func() time.Time` rather than the `Clock` interface, because `BlockerKiller` already
holds a `now` func and `VictimKiller` can pass `k.clk.Now`. Neither killer changes shape.

The kill decision becomes:

```
debt(key) + elapsed(current episode) >= after
```

The current episode's elapsed time is folded into `accrued` when that episode ends —
when the blocker vanishes, or when a different SPID replaces it. The two never overlap,
so nothing is double counted. Concretely, with `after = 60s`:

```
t=0    SPID 101 blocks, matches rule R    debt(R)=0    -> episode starts
t=60   0 + 60s >= 60s                                  -> KILL 101
t=61   101 gone: episode ends             debt(R)=60s
t=61   SPID 155 blocks, matches R         debt(R)=60s  -> 60s + 0 >= 60s -> KILL 155
t=62   SPID 162 blocks, matches R                      -> KILL 162
...
t+5m   no session matched R for 5 min     bucket pruned -> back to a full 60s dwell
```

The invariant worth keeping is that nobody is killed until 60 s of blocking has actually
been suffered under that identity. The feature does not lower the price of a kill; it
stops refunding it at every new SPID.

`prune` drops any bucket idle longer than `recidivismWindow`, and runs on each `consider`.
The map is bounded by the number of distinct rules (blocker side) or distinct jobs
(victim side), so a linear sweep costs nothing.

## 3. Constants

```go
// recidivismWindow is how long an offender identity's accumulated blocking debt
// survives without a new block before it is forgotten. Not configurable, like
// killGraceWindow: it separates "the same episode of trouble, seen through a
// succession of sessions" from "this came back an hour later", and the boundary
// between those is not something an operator needs to tune per run.
const recidivismWindow = 5 * time.Minute

// maxRepeatKills bounds how many times one identity may be killed inside the window
// before the run stops killing it and escalates to the operator. Every KILL buys a
// rollback, and a rollback of a large maintenance statement can cost more than the
// block it ends; a killer that never gives up would trade a blocked run for an
// unbounded rollback storm.
const maxRepeatKills = 3
```

The kill counter lives in the bucket, so it is forgotten with the debt after
`recidivismWindow` of quiet. The cap is therefore a **rate limit — 3 kills per identity
per 5 minutes — not a per-manifest quota**. An absolute quota would permanently disarm a
six-hour shrink because of a burst in its second minute.

## 4. `BlockerKiller`

`consider` gains the debt lookup and the cap; the shape is otherwise unchanged.

```go
if blocker.SPID != k.current {
    k.flushEpisode(now)                                   // accrue the ended episode
    k.current, k.since, k.killed = blocker.SPID, now, false
}
...
key := r.match.key()
if k.rec.debt(key)+now.Sub(k.since) < r.after {
    return                                                // not yet owed
}
if k.rec.kills(key) >= maxRepeatKills {
    k.escalate(key, blocker)                              // once per bucket
    return
}
```

`resetEpisode` — reached when we are no longer blocked, and from `SetSource` — accrues
the ended episode before clearing, since that is where most debt is actually recorded.

`SetSource` keeps clearing the episode *and* now the buckets: they are per-manifest
state, exactly like the rules they are keyed on. Within a manifest they survive operation
boundaries and shrink chunk boundaries, which is the whole point on the shrink path.

### Reporting

`BlockerKiller` reports only through `onKill func(KillEvent)` to the console; nothing it
does reaches the `.log` run report. The escalation must be in the report — it is the line
that tells an operator *why* a run went back to yielding. So `BlockerKiller` gains
`SetSink(ReactionSink)`, set and cleared per operation by the engine, mirroring
`VictimKiller.SetSink` — including the reason that pattern exists: a late event must not
be attributed to the next operation's report.

The escalation is a `warn` naming the identity of the last offender and the count:

```
stopped killing blockers matching rule {app=~"^SQLAgent", login=~"CORP\\svc_"}:
3 sessions killed in the last 5 minutes and they keep returning (last: SPID 155,
login=CORP\svc_sqlagent host=SQLPROD01 program=SQLAgent - TSQL JobStep …) —
the blocker is being restarted faster than it can be cleared; consider disabling
the job or scheduling this run outside its window
```

Emitted once per bucket, on the transition to capped. `killed` (the console callback)
is unchanged.

## 5. `VictimKiller`

`considerLocked` replaces `k.clk.Since(ep.since) < k.policy.After` with the same
debt-plus-episode comparison, keyed by `victimKey(v)`. Episodes stay per-SPID: they still
track *this* session's kill state, grace window and optimistic marking. Only the dwell
comparison moves to the bucket.

One trap has to be handled explicitly. `Suppressed` returns true for any live episode,
meaning "a kill is pending, do not count this victim toward the yield timer":

```go
default:
    return true
```

A capped victim that kept an episode would be suppressed forever while no kill would ever
come — the run would block indefinitely, the exact inverse of the intent. **A capped
identity therefore creates no episode at all**: it counts normally toward
`BlockState.Unignored` and the yield timer resumes. This preserves the property the
original design was careful about — the feature can never make us block longer than we
would without it.

`Arm` clears the buckets alongside the episodes; `Disarm` likewise.

## 6. Testing

TDD, three files:

**`recidivism_test.go`** (pure, fake clock)
- debt accrues across episodes and is readable per key
- a bucket idle beyond `recidivismWindow` is pruned; one seen at `window - 1s` is not
- `recordKill` returns the new count; the count is lost with the bucket
- keys are independent

**`kill_test.go`**
- same rule, new SPID after a kill → killed on the next poll, not after a fresh `after`
- a blocker matching a *different* rule still serves the full dwell
- the 4th kill inside the window is refused and the warn is emitted exactly once
- after `recidivismWindow` of quiet the full dwell is restored
- `SetSource` clears the debt (no leakage between manifests)

**`victim_test.go`**
- two SPIDs resolving to the same `job:<hex>:<step>` → the second is killed immediately
- two SPIDs with unparseable program names but the same login/host/program → same
- **a capped victim is not `Suppressed`** — the regression test for §5's trap
- an ignored victim still never accrues debt (eligibility is checked first, unchanged)

## 7. Rejected alternatives

- **Decaying dwell (60s → 30s → 15s → floor).** Smoother, but it needs a floor and a
  decay factor, i.e. two more tunables, and it still kills sessions that have not served
  the full price once. The cumulative clock reaches the same place with one rule.
- **Kill on sight after the first kill.** Simplest and fastest, but it withdraws the
  grace period entirely: a session that would have finished on its own in 20 s is killed
  at the next poll purely because a predecessor misbehaved.
- **Fingerprinting the session (login+host+program) on the blocker side.** More precise
  than the rule for the Agent-job and pool-retry cases, but blind to the population case,
  where the profile varies and only the rule stays constant.
- **Configuration knobs.** `kill_blockers.enabled` is already the deliberate, destructive
  arming step. An operator who set it wants the blocker gone, not returned forty times;
  the window and the cap are safety properties of the mechanism, not policy.
