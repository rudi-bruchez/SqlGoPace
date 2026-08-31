# Review: Amplifying Maintenance Victim Kill - Implementation Plan

Review of `docs/specs/superpowers/plans/2026-08-03-amplifying-maintenance-victim.md` against the design spec `docs/specs/superpowers/specs/2026-08-03-amplifying-maintenance-victim-design.md` and the current codebase.

## Executive summary

The plan is technically feasible and follows the established SqlGoPace patterns (manifest-driven, pure core, `ReactionEvent` plumbing, advisory sidecars). The task breakdown is logical and the tests are well-specified. Before implementation, four categories of issues need resolution:

1. Two concrete divergences from the approved design spec (`.amplifiers.yaml` content and reaction wording).
2. A build-order mistake in Task 5 that will prevent the TDD red step from compiling.
3. A handful of integration/operational gaps (preflight permission warning, TUI log feed, multi-database `ASYNC_STATS` scope, webhook scope).
4. Missing test coverage for the interaction between suppression and the existing yield logic.

None of these are blockers, but they should be clarified or fixed before the first commit.

## What the plan does well

- Mirrors existing abstractions rather than inventing new ones: `VictimKiller` is shaped like `BlockerKiller`, `amplifierCapture` follows `blockerCapture`, and the advisory is modelled on `reorgRCSIWarning`.
- Keeps decision logic pure and database-free: command classification, chain fan-out, eligibility, advisory, and sidecar rendering can all be unit-tested without SQL Server.
- Respects the repo conventions: no query timeouts, version bump in one place, gating on `make build`, `make vet`, and `go test -race ./...` rather than lint.
- Correctly bounds the feature with `max_block_minutes` and `ignore_blocked_sessions`, and explicitly withdraws suppression on a failed `KILL`.
- Integration tests pin the two server-dependent assumptions (`UPDATE STATISTICS` command verb and the `uniqueidentifier` conversion in msdb).

## Concrete issues to resolve

### 1. The `.amplifiers.yaml` sidecar omits `first_eligible`

The design spec example (`docs/specs/superpowers/specs/2026-08-03-amplifying-maintenance-victim-design.md`, §3.3, lines 274-301) includes a `first_eligible` timestamp for each killed session. The Task 7 implementation renders only `killed_at`, and `AmplifierKillEvent` / `capturedAmplifier` do not carry the field.

| Plan location | Spec requirement |
|---|---|
| `internal/run/amplifier_capture.go` (Task 7) | `killed_at` only |
| `docs/specs/superpowers/specs/2026-08-03-amplifying-maintenance-victim-design.md` §3.3 | `first_eligible` plus `killed_at` |

Resolution: either add `first_eligible` to the event and renderer (preferred, since the spec shows it and it is useful forensics), or explicitly decide to drop it and update the design spec so the two documents agree.

### 2. The console detail string differs from the spec example

The spec example (`docs/specs/...-design.md` §3.2) reads:

```text
killed amplifying maintenance session SPID 79 (UPDATE STATISTICS on [dbo].[MEASUREMENT]) - 16 sessions queued behind it; source: SQL Agent job "IndexOptimize - USER_DATABASES" step 1
```

The Task 5 `amplifierDetail` renders:

```text
killed amplifying maintenance session SPID %d (%s) - %d session(s) queued behind it
```

followed by the source/job clause. The table/object is available in `AmplifierKillEvent.Statement`, but the one-line console/log detail omits it. This is minor, but it makes the log line less actionable than the spec intended. Consider including the statement, or at least the object name, in the detail.

### 3. Task 5 cannot be run in the stated TDD order

Task 5 Step 1 writes `victim_test.go` using `mssql.AppNamePrefix`, but Step 4 exports that constant. Step 2 therefore fails with `undefined: mssql.AppNamePrefix`, not `undefined: NewVictimKiller` as claimed.

| Plan step | Claimed failure | Actual failure |
|---|---|---|
| Step 2 | `undefined: NewVictimKiller` | `undefined: mssql.AppNamePrefix` |

Fix: export `mssql.AppNamePrefix` before writing or running the Task 5 tests, or have the tests use the literal `"SqlGoPace"` until the constant exists.

### 4. `kill_amplifying_maintenance` lacks a preflight permission warning

`kill_blockers` arms `PreflightChecker` with `killArmed := cfg.KillBlockers.Enabled || cfg.OptionsOverride.AllowAbortBlockers` so the checker warns when the login lacks `ALTER ANY CONNECTION` (`cmd/sqlgopace/main.go:388`). The new feature also needs `ALTER ANY CONNECTION`, but Task 8 does not add `cfg.KillAmplifyingMaintenance.Enabled` to that expression.

Without this, an operator can enable the feature and only discover the permission failure when the first eligible victim appears. A KILL error then withdraws suppression and the run yields, so it is safe, but the warning is inconsistent with the existing kill path.

Recommendation: update the `killArmed` predicate to include `cfg.KillAmplifyingMaintenance.Enabled`.

### 5. Per-kill detail is invisible in TUI mode

In TUI mode `engineOut` is set to `io.Discard` (`cmd/sqlgopace/main.go:206`). The engine sink prints reaction details to `e.out`, so the `-- kill <target>: <detail>` line written by the new `VictimKiller` will be discarded in TUI mode. The sticky `ConflictingJobsMsg` line will show, but the operator will not see the individual kill narration in the TUI feed.

By contrast, `BlockerKiller` is wired with an explicit `onKill` callback that sends `tui.LogMsg` and `tui.KilledMsg` (`cmd/sqlgopace/main.go:395-401`). The plan explicitly says "No console/TUI callback is wired here" (Task 8 Step 10), but the justification assumes the engine sink reaches the TUI, which it does not.

Recommendation: either forward kill events to the TUI explicitly (a small callback analogous to `BlockerKiller`'s `onKill`), or document that per-kill narration is stdout/log only.

### 6. Webhook/email notification does not cover kill/warn events

The plan/spec states that reaction events flow to webhook/email notification. The engine only calls `e.notify` for `pause`, `cancel`, and `abort` (`internal/run/engine.go:617-636`). `Kind: "kill"` and `Kind: "warn"` events are recorded in the run report but are not pushed to notifiers. This is a pre-existing limitation, but the design doc should not imply it is new plumbing.

Recommendation: clarify in Task 9 documentation that amplifier kills appear in the `.log`/`.amplifiers.yaml`/TUI sticky line, but not in webhook/email unless the notification `on_events` list and `notify` branch are extended.

### 7. `ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY` must be read per target database

The advisory is database-scoped. In multi-database mode, the engine is instantiated once per database with a connection already scoped to that database (`buildEngine` receives the per-database `conn`). The CLI wiring in Task 8 Step 10 is placed next to the `kill_blockers` block, which is inside `buildEngine`, so the read is per-database as long as it stays there.

Risk: the plan says "once after `conn.Detect`" (`cmd/sqlgopace/main.go` line 161 is in `runEngine`, before per-database connections are opened). If the implementer places the read in `runEngine` instead of `buildEngine`, it will read the connected database context at startup, not each target database.

Recommendation: explicitly place the `AsyncStatsWaitAtLowPriority` read inside `buildEngine`, not `runEngine`.

### 8. The pump goroutine now performs blocking I/O

`ServerSampler.Blocking` runs on the pump goroutine. Calling `VictimKiller.consider` there means a kill (and the optional msdb job lookup) blocks the pump for the duration of those queries. The `KILL` is on the monitoring pool and the msdb lookup is small, but on a heavily loaded server this can delay the next blocking sample by more than one poll interval.

This is acceptable for an opt-in feature, but worth a code comment noting that `consider` is on the pump path.

## Testing gaps

The plan covers the obvious unit cases well, but two load-bearing interactions are not tested:

1. Suppression plus another unignored blocker. When a victim is suppressed (`Unignored=false`) but a second, non-ignored application session is blocked by the DDL, `Blocking` must return `{Any:true, Unignored:true}`. The rewritten loop in Task 6 drops the `break`, so this is the intended behavior, but no test asserts it. Add a case to `executor_test.go`.

2. Mutual block invariance. The spec §1.6 says the order in which the two killers are consulted must not matter and must produce exactly one `KILL`. The Task 5 tests cover "skip our own blocker" but do not assert that `BlockerKiller` and `VictimKiller` together emit exactly one kill when the snapshot shows a cycle. Add a sampler-level test with both killers armed.

## Code-level notes

- `VictimKiller` caches Agent jobs in `k.jobs` but `Arm` resets only `k.episodes`. Since job IDs are globally unique this is fine, but `Disarm` also leaves the cache. A comment explaining why the cache survives manifest boundaries would avoid future confusion.
- `VictimKiller.consider` drops episodes for victims that are no longer in the snapshot. If a victim disappears for one poll and reappears, the dwell restarts. This matches the spec §1.5 and is correct.
- `amplifierCapture.add` holds its mutex; `renderAmplifiers` also locks the same mutex. The engine sink calls `add`, then `flushAmplifiers`, then `amplifierSink` sequentially, so there is no re-entrant lock. Good.
- Task 8 Step 9's engine modification references line numbers (`engine.go:606`, `638`, `690`) that will shift after earlier tasks. The implementer should locate the RCSI warning and sink by context, not by exact line.
- The commit list for Task 5 includes `internal/run/export_test.go`, but no step modifies it. `NewManualClock`, `ManualClock`, and `CompileIgnoredSessions` are already exported, so this file is unnecessary. Remove it from the commit or explain why it is needed.

## Documentation and operational notes

- Task 9 correctly scopes documentation to `README.md`, `docs/specs/SPECS.md`, and the version bump. The `config.yaml` sample block in Task 8 Step 11 is clear and opt-in.
- The README should state explicitly that `kill_amplifying_maintenance` is independent of `ignore_blocking`: an operator using `ignore_blocking: true` will still see maintenance victims killed, because `ignore_blocking` only suppresses the yield reaction.
- The README should also note that the `.amplifiers.yaml` sidecar is advisory and that SqlGoPace never writes to msdb, matching the design spec.

## Verdict

The plan is ready to implement after the following fixes:

1. Resolve the `first_eligible` field discrepancy between the spec and Task 7.
2. Reorder Task 5 so `mssql.AppNamePrefix` is exported before the test is compiled.
3. Include `cfg.KillAmplifyingMaintenance.Enabled` in the preflight `killArmed` predicate.
4. Decide whether to forward per-kill detail to the TUI; if not, document the limitation.
5. Add tests for suppression coexisting with another unignored blocker, and for mutual-block one-kill invariance.
6. Place the `ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY` read inside `buildEngine` for per-database correctness.

With those changes, the feature should land cleanly and preserve the existing reaction hierarchy semantics.
