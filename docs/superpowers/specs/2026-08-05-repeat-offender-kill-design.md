# Accelerating the kill of a repeat-offender blocker

Status: design approved, pending implementation plan.
Date: 2026-08-05. Revised the same day after review (see `*-kmim.md`).

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

**The key is computed once, in `compileKilledSessions`, and stored in `killRule`** — not
rebuilt per poll. `compileKilledSessions` (`kill.go:42`) reconstructs every `killRule`, and
is re-run on each reload (`kill.go:102`), so a compile-time key has the matcher's exact
lifetime and cannot outlive the rule text it describes. Recomputing it per poll would be
strictly more work for the same value.

**Editing an existing rule's text loses that rule's debt**, because the edited rule is a
different key. This is deliberate, not a bug: an operator who rewrites a rule mid-run has
changed which sessions it describes, and carrying the old population's debt onto the new
one would be the surprising behavior. Appending a rule leaves every other rule's debt intact.

**Two representations, deliberately distinct.** The bucket key is a canonical machine
string (`sid|app|host|login|stmt`, regexp sources verbatim, empty for unset fields); the
escalation warn renders the rule for a human (`{app=~"^SQLAgent", login=~"CORP\\svc_"}`,
omitting unset fields). They are never interchanged: the key must be stable and total, the
narration must be readable.

The victim key deliberately excludes the command verb **and the statement text**: a job
that alternates `UPDATE STATISTICS` and `ALTER INDEX` across restarts, or runs the same
verb against a different object, is one offender, not two or three.

## 2. The debt clock

A new pure file, `internal/run/recidivism.go`. No mutex of its own — both callers already
hold theirs across the call.

```go
// bucket is one offender identity's accumulated blocking debt inside the window.
type bucket struct {
    accrued    time.Duration // blocking time already served under this identity
    kills      int
    escalated  bool // the cap warn has been emitted for this bucket
    lastActive time.Time
}

type recidivism struct {
    now     func() time.Time
    buckets map[string]*bucket
}

func (r *recidivism) debt(key string) time.Duration
func (r *recidivism) accrue(key string, d time.Duration)
func (r *recidivism) kills(key string) int
func (r *recidivism) recordKill(key string) int  // reserve/confirm a kill, returns the new count
func (r *recidivism) undoKill(key string)        // withdraw a reservation that never landed
func (r *recidivism) escalate(key string) bool   // true the first time only, for the cap warn
func (r *recidivism) prune()
```

`now func() time.Time` rather than the `Clock` interface, because `BlockerKiller` already
holds a `now` func and `VictimKiller` can pass `k.clk.Now`. Neither killer changes shape.

The kill decision becomes simply:

```
debt(key) >= after
```

**Debt is banked incrementally, one poll at a time**, not folded in when an episode ends.
At every poll where the offender is present and eligible, the time since *that episode's
previous poll* is added to the identity's bucket; the first poll of an episode contributes
zero, which is exactly today's `since = clk.Now()` semantics.

Folding at episode end was the first formulation and it does not survive contact with the
code: when an episode ends the offender is gone from the snapshot, so there is nothing left
to match rules against, and the killer would have to remember which key the vanished
episode belonged to. That memory would then be wrong the moment a hot reload changed which
rule matches. Banking per poll keeps one writer, cannot double count, needs no orphaned
attribution, and has the side benefit that a run ending with the blocker still present has
already banked everything it observed.

Concretely, with `after = 60s` and a 10 s poll:

```
t=0    SPID 101 blocks, matches rule R   +0s   debt(R)=0   -> episode starts
t=10   still 101                         +10s  debt(R)=10s
...
t=60   still 101                         +10s  debt(R)=60s -> 60s >= 60s -> KILL 101
t=61   SPID 155 blocks, matches R        +0s   debt(R)=60s -> 60s >= 60s -> KILL 155
t=71   SPID 162 blocks, matches R        +0s   debt(R)=60s -> KILL 162
...
t+5m   no session matched R for 5 min          bucket pruned -> back to a full 60s dwell
```

The invariant worth keeping is that nobody is killed until 60 s of blocking has actually
been observed under that identity. The feature does not lower the price of a kill; it
stops refunding it at every new SPID.

`prune` drops any bucket idle longer than `recidivismWindow`. It runs **at the top of each
`consider`, before any debt is read**, so an offender returning after a quiet window is
judged against a pruned map and serves the full dwell. The map is bounded by the number of
distinct rules (blocker side) or distinct jobs (victim side), so a linear sweep costs nothing.

Nothing else drives `prune`: `consider` only runs while a statement is in flight and the
killer is armed, so buckets belonging to an idle run are not reclaimed until the next
offender wakes the killer. That is harmless — the map is tiny and per-manifest — and it does
not change semantics, since a forgotten bucket can only matter at the moment it is consulted.

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
    k.current, k.since, k.killed = blocker.SPID, now, false
    k.lastPoll = time.Time{}                              // first poll of the episode: banks 0
}
...                                                       // r is the first matching rule
k.rec.accrue(r.key, k.sincePoll(now))                     // 0 on an episode's first poll
k.lastPoll = now
if k.rec.debt(r.key) < r.after {
    return                                                // not yet owed
}
if k.rec.kills(r.key) >= maxRepeatKills {
    if k.rec.escalate(r.key) {                            // true the first time only
        sink(ReactionEvent{Kind: "warn", Detail: capDetail(r, blocker, maxRepeatKills)})
    }
    return
}
if err := k.kill(ctx, blocker.SPID); err == nil {
    k.killed = true
    k.rec.recordKill(r.key)                               // successful kills only
    ...
}
```

The episode state keeps its existing role — `k.killed` still means "already killed *this*
SPID", `k.since` still dates the current episode — and gains only `lastPoll`, the previous
observation of this episode. `resetEpisode` keeps its current meaning (clear the episode,
touch no bucket), so the naming ambiguity the review raised disappears rather than being
renamed around: no method both accrues and resets.

`KillEvent.Waited` becomes `debt(key)`, not `now.Sub(k.since)`. For a recidivist the
episode's own elapsed time is near zero, and a console line reading *"killed blocker SPID
155 after 0s blocking the DDL"* would misstate why the kill was justified. The debt is the
blocking actually observed under that identity, which is what the operator needs to see.

**The counter follows the kill, not the intent.** `BlockerKiller` handles one blocker at a
time and issues the `KILL` inline, under `k.mu`, so incrementing after `k.kill` returns nil
is both correct and sufficient. A failed `KILL` consumes no budget — it already leaves
`k.killed` false and falls through to the normal reaction. (`VictimKiller` cannot do this;
see §5.)

**Buckets are per-manifest state**, cleared by `SetSource` alongside the episode, exactly
like the rules they are keyed on — so debt is never carried across manifests. Because
banking is incremental, there is no unflushed tail to worry about when a statement (or the
whole manifest) ends with the blocker still present: everything observed is already banked,
and what happens to it next is simply that `SetSource(nil)` discards it.

Within a manifest, buckets survive operation boundaries and shrink chunk boundaries. Note
what that does *and does not* fix on the shrink path: the sampler runs only while a
statement is in flight, so between chunks `consider` is not called at all and `since` is
not reset — an offender that keeps the *same* SPID across a chunk gap already accumulates
correctly today. What the debt adds is continuity when the offender comes back under a
*different* SPID, which is the case the shrink actually hits against a hot table.

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

`considerLocked` banks the same per-poll increment against `victimKey(v)` and replaces
`k.clk.Since(ep.since) < k.policy.After` with `k.rec.debt(key) < k.policy.After`. Episodes
stay per-SPID: they still track *this* session's kill state, grace window, optimistic
marking, and now its `lastPoll`. Only the dwell comparison moves to the bucket, and
`AmplifierKillEvent.Waited` becomes the debt for the same reason `KillEvent.Waited` does
(§4) — `FirstEligible` keeps naming this episode's own start.

One trap has to be handled explicitly. `Suppressed` returns true for any live episode,
meaning "a kill is pending, do not count this victim toward the yield timer":

```go
default:
    return true
```

A capped victim that kept an episode would be suppressed forever while no kill would ever
come — the run would block indefinitely, the exact inverse of the intent. **A capped
identity therefore creates no episode and is not marked `seen`**, which hands it to the
existing end-of-scan sweep (`victim.go:310-315`): the episode is dropped as soon as it is
outside its post-kill grace window, `Suppressed` goes false, and the victim counts toward
`BlockState.Unignored` again. This preserves the property the original design was careful
about — the feature can never make us block longer than we would without it.

**The kill counter is reserved under the lock, not incremented on success.** This is the
one place the design departs from the review's advice, and the reason is structural:
`considerLocked` selects *several* victims in a single scan (`victim.go:288-306`), and the
`KILL`s are issued afterwards with `k.mu` released. If the counter only moved on success,
three sessions of the same Agent job would all pass the `kills >= maxRepeatKills` test in
one scan against a counter still reading zero, and the cap would be exceeded by the number
of targets in that scan. So the counter follows the same optimistic pattern the code
already uses for `ep.killed`:

- `considerLocked` calls `recordKill(key)` as it selects a target — the reservation;
- `withdrawKill` (the `KILL` failed) also calls `undoKill(key)`;
- `abandonKill` (the run was shutting down, no `KILL` attempted) also calls `undoKill(key)`.

The review's actual concern — a failed `KILL` must not consume the budget — is satisfied
by the withdrawal, and the cap is genuinely enforced rather than approximately.

`Arm` clears the buckets alongside the episodes; `Disarm` likewise.

## 6. Testing

TDD, four files:

**`recidivism_test.go`** (pure, fake clock)
- debt accrues across episodes and is readable per key
- a bucket idle beyond `recidivismWindow` is pruned; one seen at `window - 1s` is not
- `recordKill` returns the new count; `undoKill` gives the budget back; the count is lost
  with the bucket
- `escalate` returns true exactly once per bucket, and again after the bucket is pruned
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
- the same job running a *different* statement is still one identity (key excludes the verb
  and the statement)
- **a capped victim is not `Suppressed`** once its grace window ends — the regression test
  for §5's trap
- **one scan selecting more victims than the cap allows kills exactly `maxRepeatKills`** —
  the regression test for the reservation, which increment-on-success would fail
- a failed `KILL` returns the budget: the next eligible victim of that identity is still
  killable
- an ignored victim still never accrues debt (eligibility is checked first, unchanged)

**`shrink_driver_test.go`** — the integration the review asks for: a fake sampler whose
snapshots present a *different* blocker SPID matching the same rule on each chunk, driven
through `ShrinkRunner` across chunk boundaries, asserting the second chunk's blocker is
killed without serving a fresh dwell. Confirmed the path exists: `runChunk` and
`runWatchedStatement` both pump through `pumpSamples` → `ServerSampler.Blocking`, which is
where both killers are consulted (`executor.go:326`, `shrink.go:696` and `shrink.go:779`).

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
- **Folding the episode's elapsed time into the bucket when the episode ends** (the first
  formulation of §2). It reads well but cannot be implemented cleanly: the offender is gone
  from the snapshot by then, so the episode would have to carry the key it accrued under —
  a copy that a hot reload can invalidate, and a second writer to reconcile against the
  live match. Per-poll banking has one writer and no attribution to remember.
