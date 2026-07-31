# Assessment: Paced Reorg Yielding + RCSI-Off Warning Design

Date: 2026-07-31
Status: Assessment of design spec `2026-07-31-reorg-pacing-rcsi-warning-design.md`

## Overall verdict

Promising and mostly sound, but the design should be strengthened before implementation. The paced-yielding mechanism fits cleanly into the existing `runLoop` state machine, the RCSI warning wiring is straightforward, and the scope is appropriately narrow. The main gap is that the design does not address `IMPLICIT_TRANSACTIONS`, which `docs/REORGANIZE.md` identifies as the most likely root cause of the motivating incident. Without that, pacing alone reduces but does not eliminate the blocking risk.

## Strengths

- Clear motivation tied to a real incident, with explicit reference to the background analysis in `docs/REORGANIZE.md`.
- Narrow scope: only `MonitoredRunner.runLoop`, one engine option, and one call site in `cmd/sqlgopace/main.go`. No matrix, generator, shrink driver, or resumable path changes.
- Correctly distinguishes `CancelSafe` (narration) from the paced control flow. Keying the paced path off `ddl.ReorganizeIndex` directly prevents accidentally extending uncapped pacing to `update_statistics` or `check_db`.
- Reuses `waitForRelief` and the existing `Cancel` reaction, so the physical stop/cancel/KILL path is unchanged.
- `Run`'s `MaxRetries` loop is left untouched; paced reorg never returns `ErrCancelled`, so it never consumes retries.
- Escape hatches are already present: graceful stop (`ErrStopped`) and log-drain timeout (`ErrLogDrainTimeout`) still terminate the loop.
- RCSI detection already exists in `ServerInfo.RCSIEnabled`; wiring it through `WithRCSI` mirrors the existing `WithADR` pattern.
- Comprehensive unit-test plan: table-driven `runLoop` tests, mapping tests for the paced flag, and `reorgRCSIWarning` tests.

## Feasibility against the existing code

I verified the relevant touch points:

- `runLoop` is a pure function in `internal/run/monitored_runner.go` with the exact signature the design needs to extend.
- `MonitoredRunner.Run` retries on `ErrCancelled`; returning a different error from `runLoop` for paced cancels is the right integration point.
- `DecideReaction` returns `Cancel` for non-resumable ops under pressure; the paced conversion happens one layer up in `runLoop`, so no reaction logic changes.
- `Capabilities.CancelSafe` is already set from `cancelSafe(step.Operation)` in `engine.go`; the design correctly does not add a new capability bit.
- `cmd/sqlgopace/main.go` already passes `run.WithADR(info.ADREnabled)`; adding `run.WithRCSI(info.RCSIEnabled)` is symmetric.
- `ServerInfo.RCSIEnabled` is detected in `internal/mssql/server.go` and already consumed by `BatchDMLRunner`; extending it to the engine is trivial.
- `engine.go` already records every `ReactionEvent` (including `warn`) into `report.ReactionLine`, so the RCSI warning will reach the `.log` sidecar automatically.

## Concerns and ambiguities to resolve

1. The design does not address `IMPLICIT_TRANSACTIONS`.
   `docs/REORGANIZE.md` identifies this as the single most likely cause of the MEASUREMENT incident: when `IMPLICIT_TRANSACTIONS` is on, a REORGANIZE holds every lock until the whole operation commits, converting "short-term page locks" into a sustained blocking chain. The current design paces the reorg (cancel when blocked, wait, re-issue), but each run can still hold locks for its full duration if implicit transactions are on. Pacing alone mitigates the impact; forcing `IMPLICIT_TRANSACTIONS OFF` on the execution connection eliminates the root cause for each run. Either:
   - add "prefix REORGANIZE with `SET IMPLICIT_TRANSACTIONS OFF;`" to this design (a generator change, no new manifest/config knob), or
   - explicitly document it as a known limitation and a follow-up item.
   Recommending the first option: it is cheap, safe, and directly aligned with the incident analysis.

2. Event narration when a paced reorg re-issues.
   The Pause branch emits a `resume` event via `resumeSQL()`. The paced Cancel branch does not call `resumeSQL()`, so after the cancel event and the relief wait, the runner silently re-issues the same SQL. The operator sees a cancel, a pause, then eventual completion, with no explicit "re-issuing REORGANIZE" line. Clarify whether to emit a `resume` or `reissue` event when the paced branch loops. A small addition such as `sink(ReactionEvent{Kind: "resume", Detail: "pressure cleared — re-issuing REORGANIZE"})` before looping would make the TUI and `.log` coherent.

3. `reorgRCSIWarning` needs the database name.
   The illustrative output names the database (`RCSI is OFF on PRODDB`), but the proposed helper signature is `reorgRCSIWarning(op ddl.Operation, rcsi bool) (string, bool)` and cannot know the database. Either pass the database name to the helper, or have the helper return a format string/template and let the caller interpolate. Prefer passing the database name so the helper remains a pure, testable decision function that returns the exact message.

4. Exact insertion point for the RCSI warning in `processOne`.
   The design says "before the reorg executes". The warning should be emitted after `emitStep` (so the operator sees which step is starting) and before `e.runner.Run(...)`. It must use the same `sink` that feeds the report. A precise location would avoid off-by-one-step errors in the log.

5. `runLoop` signature change ripples to tests.
   `internal/run/executor_test.go` has several `runLoop(...)` calls that will need a `paced` argument. The implementation plan should include updating those existing tests, not only adding new ones.

6. Whether "warn" kind is surfaced distinctly in the TUI.
   The design says the warning reaches the TUI. The existing `ReactionEvent.Kind` comment lists `pause | resume | cancel | kill`, but `warn` and `info` are already used elsewhere (shrink driver). Confirm the TUI renderer handles `warn` distinctly; if not, it may need a small update.

7. Version bump and docs.
   The design does not mention a version bump or doc updates. The implementation plan should include bumping `internal/version/VERSION` and adding a note to `docs/REORGANIZE.md` (or `README.md`) describing the paced behavior and the RCSI warning.

## Recommendations

- Approve the design after adding an explicit decision on `IMPLICIT_TRANSACTIONS`. My recommendation is to include it now: prefix every `reorganize_index` statement with `SET IMPLICIT_TRANSACTIONS OFF;` in `generateReorganizeIndex`. This is a generator change but not a manifest/config change, so it still respects the "no new knobs" constraint.
- Add a `reissue` or `resume` event in the paced Cancel branch for clear operator narration.
- Pass the database name into `reorgRCSIWarning` so the returned message is complete.
- Keep the `runLoop` `paced bool` parameter; it is the cleanest way to localize the behavior.

## Conclusion

The core mechanism is sound and the scope is right. The design should be approved once it resolves the `IMPLICIT_TRANSACTIONS` omission and the re-issue narration gap. After those two changes, it is ready for an implementation plan.
