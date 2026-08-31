# Assessment: Paced Reorg Yielding + RCSI-Off Warning Design

Date: 2026-07-31
Status: Re-assessment of revised design spec `2026-07-31-reorg-pacing-rcsi-warning-design.md`

## Overall verdict

Approved. The revised design resolves the two blockers from the first assessment (`IMPLICIT_TRANSACTIONS` handling and re-issue narration) and is now ready for an implementation plan.

## What changed since the first assessment

1. `IMPLICIT_TRANSACTIONS OFF` is now included as connection hardening in `internal/mssql/conn.go`'s `harden()`, rather than as a generator prefix. This is the right call: it keeps the generator pure, applies defensively to every operation, and matches the existing hardening pattern.
2. The paced path now narrates re-issue through a `reissue` closure that emits `ReactionEvent{Kind: "resume", Detail: "pressure cleared — re-issuing REORGANIZE"}`, so the operator sees cancel → wait → re-issue instead of a silent re-run.
3. `reorgRCSIWarning` now takes the database name, so the returned message is complete.
4. The exact emission point for the RCSI warning is specified (after `emitStep`, before `e.runner.Run`).
5. Existing `runLoop` test call sites are explicitly called out as requiring the new `reissue` argument.
6. Version bump and doc updates (`docs/REORGANIZE.md`, `README.md`) are included.

## Strengths of the revised design

- The `reissue` closure is a clean abstraction: it encodes the paced discriminator (`reissue != nil`) and the re-issue narration in one injectable function, without adding a separate `paced bool`.
- Connection hardening via `harden()` is minimal (one line), defensive, and avoids polluting `plan`/`--explain` output.
- Scope remains narrow: `MonitoredRunner.runLoop`, one engine option, one call site in `main.go`, one line in `conn.go`.
- The rationale for not keying off `CancelSafe` is preserved and well explained.
- Escape hatches (graceful stop, log-drain timeout) are unchanged.
- Test coverage is comprehensive and correctly distinguishes unit-testable pure logic from the SQL-issuing hardening change.

## Feasibility checks against the existing code

I verified the relevant touch points:

- `internal/mssql/conn.go` contains `harden()` with the exact statement `SET XACT_ABORT ON; SET DEADLOCK_PRIORITY LOW;`, so adding `SET IMPLICIT_TRANSACTIONS OFF;` is a one-line change.
- `internal/run/monitored_runner.go` has `runLoop` as a pure function with the signature `(sql, runStatement, waitForRelief, resumeSQL)`; adding a fifth `reissue` parameter is straightforward.
- `MonitoredRunner.runOnce` is the right place to construct the `reissue` closure for `ddl.ReorganizeIndex` only.
- `cmd/sqlgopace/main.go` already wires `run.WithADR(info.ADREnabled)`; adding `run.WithRCSI(info.RCSIEnabled)` is symmetric.
- `engine.go` constructs the per-step `sink` at line 600 and appends every `ReactionEvent` to `report.ReactionLine` at line 624, so the RCSI warning reaches the `.log` sidecar automatically.
- `docs/REORGANIZE.md` already describes the locking mechanisms and the earlier ShrinkRunner-style driver suggestion; updating it to reflect the `runLoop` refinement is a small doc task.

## Minor points for the implementation plan

1. `reissue` error handling.
   The design's pseudo-code writes `stmt, _ = reissue()`. Since the production closure only emits an event and returns the captured SQL, it cannot fail, but the implementation should still return the error if non-nil (mirroring the `resumeSQL` error handling) to keep the contract honest.

2. `runLoop` signature change ripples.
   The plan must update `MonitoredRunner.runOnce` and the seven `runLoop` call sites in `internal/run/executor_test.go`. The design already notes this; make it a checklist item.

3. `ReactionEvent.Kind` comment update.
   Update the doc-comment from `// "pause" | "resume" | "cancel" | "kill"` to include `warn` and `info`, since both are already emitted in production.

4. `harden()` comment.
   Update the function comment to mention `IMPLICIT_TRANSACTIONS OFF` alongside `XACT_ABORT ON` and `DEADLOCK_PRIORITY LOW`.

5. `reorgRCSIWarning` location.
   The helper can live in `internal/run/engine.go` near `cancelSafe` or in a small new file; either is fine as long as it is pure and unit-testable.

6. Terminology: "engine-side mapping" for `ReorganizeIndex → reissue`.
   The actual mapping is inside `MonitoredRunner.runOnce` (the runner, not the engine). The implementation plan should place the test accordingly.

7. `MaxRetries` test.
   The design says "paced never consumes MaxRetries". A direct `runLoop` test cannot exercise `MaxRetries` (that lives in `MonitoredRunner.Run`); add a test at the `Run` level or keep the assertion at the `runLoop` level by verifying it never returns `ErrCancelled`.

## Conclusion

The design is sound, complete, and ready to move to the implementation-plan stage. No further conceptual revisions are needed.
