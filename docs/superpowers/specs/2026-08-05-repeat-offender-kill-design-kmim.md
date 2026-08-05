# Review: Accelerating the kill of a repeat-offender blocker

Status: reviewed, 2026-08-05.
Reviewer: Kimi.

## Overall assessment

The design is sound, tightly scoped, and fixes a real operational failure mode. Keying the dwell on offender identity rather than SPID removes the treadmill effect without lowering the price of a kill, and the rate cap bounds the rollback exposure. With a few clarifications in the implementation plan, it is ready to build.

## Strengths

- The cumulative debt clock preserves the invariant that a kill still costs 60 seconds of observed blocking under one identity; it only stops refunding that cost at every new SPID.
- No new configuration keeps the deliberate `kill_blocking_sessions` / `kill_amplifying_victims` arming model intact.
- The rate cap is a windowed rate limit, not an absolute quota, so a long shrink is not permanently disarmed by one burst.
- Adding `ReactionSink` to `BlockerKiller` closes a real reporting gap: today the operator has no durable record of why the killer stopped.
- The capped-victim handling in `VictimKiller` correctly avoids the suppression trap that would otherwise block a run indefinitely.
- Scope is disciplined: no new `Action`, no change to the reaction hierarchy, no change to `ignore_blocked_sessions` or `max_block_minutes`.

## Concerns and clarifications for the implementation plan

1. Blocker key format and stability.
   The design says the key is "canonicalized as `sid|app|host|login|stmt`" but the escalation example uses `{app=~"^SQLAgent", login=~"CORP\\svc_"}`. Decide whether the debt key and the human-readable escalation share one representation or are separate. If separate, document both. If the same, ensure the canonical form is deterministic and stable across hot reloads.

2. Rule edit versus rule append.
   The key survives appending a rule mid-run because the existing rules' text is unchanged. If an operator edits an existing rule's text, the key changes and the accrued debt for that rule is lost. This is acceptable, but state it explicitly so it is not mistaken for a bug.

3. `BlockerKiller.resetEpisode` versus `flushEpisode`.
   The design says `resetEpisode` accrues the ended episode before clearing. Keep the naming unambiguous: either `resetEpisode` calls a separate `flushEpisode` helper, or rename it to `flushAndResetEpisode`. The call sites are `consider` when unblocked, `consider` when the SPID changes, and `SetSource`.

4. When debt is accrued for a persistent blocker.
   The current episode is only folded into `accrued` when the blocker vanishes or a different SPID replaces it. If the DDL statement finishes while the same blocker is still alive, `SetSource(nil)` at manifest end clears the buckets without flushing. That is correct because the buckets are per-manifest state, but confirm the implementation does not try to carry debt across manifests.

5. Escalation must emit exactly once per bucket.
   The design says the escalation fires "once per bucket" on transition to capped. Add a `capped` bool to the bucket or a separate capped set, and guard the sink call with it so a capped offender does not emit a warning on every poll.

6. Count only successful kills.
   `recordKill` should increment only after the KILL succeeds. For `VictimKiller` this means incrementing after `killDetached` returns nil, not when `considerLocked` marks the episode optimistically. For `BlockerKiller` it means incrementing after `k.kill` succeeds.

7. `recidivism.prune` is only driven by `consider`.
   BlockerKiller only calls `consider` when a blocker exists, and VictimKiller only when armed. An idle bucket is therefore not pruned until the next active offender wakes the killer. This is harmless for memory, but the "full dwell restored after 5 minutes" only takes effect on the next consideration.

8. VictimKiller capped identity and existing episodes.
   A capped identity must not create a new episode, but also ensure that an existing episode for the same SPID does not keep it suppressed once the grace window expires. The episode map sweep at the end of `considerLocked` should drop capped episodes whose grace has ended.

9. Victim key excludes command verb but should also exclude statement text.
   The fallback key `sess:<login>|<host>|<program>` is good, but confirm that `ActiveQuery` is not included. A job running different statements across restarts should still be one offender.

10. Test coverage for `SetSource` boundary.
    `kill_test.go` should verify that `SetSource` clears both the current episode and the buckets, not just the episode. This prevents debt leakage between manifests in unit tests.

11. Test coverage for concurrent rule reload.
    `manifestKillSource.Current` can return a different slice on each call. The key must be derived from the rule text inside `consider` while `k.mu` is held, not cached across polls.

12. Shrink driver integration.
    The shrink driver benefits most from this change because it polls only while a statement is in flight. Verify that `ShrinkRunner` routes its blocking samples through the same `ServerSampler.Blocking` path so the same `BlockerKiller` and `VictimKiller` are consulted.

## Suggested next step

Draft the implementation plan with file-by-file changes, the exact `recidivism` API, the `key()` method on `killRule`, the `SetSink` wiring in the engine, and the three test files listed in the design. Include a verification step that exercises a shrink with a recurring blocker across chunk boundaries.
