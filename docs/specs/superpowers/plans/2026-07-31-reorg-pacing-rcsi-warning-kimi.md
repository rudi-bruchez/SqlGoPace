# Review: Paced Reorg Yielding + RCSI-Off Warning Implementation Plan

Date: 2026-07-31
Status: Review of implementation plan `2026-07-31-reorg-pacing-rcsi-warning.md`

## Overall verdict

Ready to execute. The plan is well-structured, test-first, and faithful to the approved design spec. Task ordering is correct, call sites are enumerated, and the pure/SQL-issuing split is respected. A few minor gaps and one recommended test addition are noted below.

## Strengths

- Test-first steps for every task, with explicit failing-test commands and expected compile errors.
- Correct task ordering and dependency tracking: Task 1 and Task 2 are independent; Task 3 depends on Task 2; Tasks 4 and 5 are independent.
- The `reissue` closure design is implemented cleanly: `runLoop` gains a fifth parameter, `reissueFor` localizes the reorg-only decision, and existing tests pass `nil`.
- Comprehensive `runLoop` test coverage: paced completion, unbounded looping, relief-error propagation, graceful stop, and the mapping test for `reissueFor`.
- `reorgRCSIWarning` is pure and isolated in its own file with focused unit tests.
- Engine wiring mirrors the existing `WithADR` pattern, and the warning emission point is specified precisely (after `emitStep`, before `e.runner.Run`).
- `IMPLICIT_TRANSACTIONS OFF` is added to the existing `harden()` statement without changing the generator or adding config knobs.
- Version bump (0.11.0 → 0.12.0) and doc update in `docs/reference/reorganize-locking.md` are included.
- The CRLF/gofmt warning and the build/vet/test gate are correctly stated.

## Verification against the codebase

I checked the touch points the plan relies on:

- `internal/run/executor_test.go` is package `run` and already imports `errors`, `strings`, and `internal/ddl`, so the planned test additions need no import changes.
- `internal/run/engine_test.go` has `setupEngine(t, pf, runner, opts ...run.EngineOption)`, so passing `run.WithRCSI(false)` works.
- `report.Write` renders reactions as `reaction: <kind> at <time> (<detail>)`, so the engine test's assertion `"reaction: warn"` will match.
- `internal/mssql/conn.go` calls `harden()` once when pinning the execution connection, so `SET IMPLICIT_TRANSACTIONS OFF` will persist for all operations on that connection.
- `internal/version/VERSION` is currently `0.11.0`, so the bump to `0.12.0` is correct.

## Minor gaps and recommendations

1. Add a `MonitoredRunner.Run` level test for MaxRetries.
   The design spec requests "paced never consumes MaxRetries". The plan covers this indirectly via `TestRunLoopPacedLoopsUnbounded` (which proves `runLoop` never returns `ErrCancelled`), but a direct test at the `Run` level is stronger: construct a `MonitoredRunner` with `MaxRetries: 1`, script many cancels through a fake `runStatement`, and assert the runner succeeds instead of returning "retries exhausted". This is a small addition to `internal/run/executor_test.go` that locks the contract with `Run`.

2. Consider moving the RCSI warning emission to after resumable conflict handling.
   The plan inserts the warning immediately after the `sink` block and before `waitsBefore := e.snapshotWaits(ctx)`. That is before the `prepErr` check for blocking paused resumables. If `prepErr` prevents the reorg from running, the warning has already been emitted. This is harmless but slightly misleading. A safer insertion point is just before `runErr = e.runner.Run(...)`, after all preparation. Either is acceptable; pick one and make it explicit.

3. Update `README.md` if reorg behavior is documented there.
   The design spec §5 mentions adding a short line to `README.md` if reorg behavior is user-facing there. The plan only updates `docs/reference/reorganize-locking.md`. Check `README.md` for a reorg/maintenance section and add a one-line note if present.

4. Refresh the `ReactionLine` doc-comment in `internal/report/report.go`.
   The comment on `ReactionLine` currently says "a pause, resume, cancel, or fallback kill". Since `warn` and `info` events are now recorded here too, update the comment alongside the `ReactionEvent.Kind` comment update.

5. `Co-Authored-By` trailer.
   The plan includes `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>` in every commit message. If an executing agent is not allowed to mutate git history without explicit user approval, these commits will need to be made only after confirmation. This is fine as a plan convention, but flag it in the execution notes.

6. `TestRunLoopPacedLoopsUnbounded` comment wording.
   The comment says "so Run's MaxRetries can never fire for a reorg", but the test only exercises `runLoop`. Either rephrase the comment or add the `Run`-level test recommended in point 1.

## Nitpicks

- The plan says `make lint` may flag pre-existing CRLF/gofmt noise repo-wide. Consider running `golangci-lint` on only the modified files to reduce noise, but the stated gate (build/vet/test) is sufficient.
- The doc replacement paragraph in `docs/reference/reorganize-locking.md` says "As of 0.12.0...". Verify no other paragraph in that file still floats the ShrinkRunner-style driver idea; search for "follow-up" or "ShrinkRunner" and clean up any leftovers.

## Conclusion

The plan is sound and executable. After adding the `Run`-level MaxRetries test and deciding on the exact RCSI warning insertion point, it can be implemented as written.
