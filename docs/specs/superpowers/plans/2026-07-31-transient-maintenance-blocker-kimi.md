# Assessment: Transient-Maintenance-Blocker Recognition Implementation Plan

Date: 2026-07-31
Status: Assessment of implementation plan `2026-07-31-transient-maintenance-blocker.md`

## Overall verdict

Ready to execute. The plan is concrete, test-first, correctly ordered, and stays within the design constraints. It addresses all six concerns raised in the design assessment and includes a clear self-review mapping each spec section to a task. Minor wording and robustness tweaks are noted below.

## Strengths

- Test-first steps for every task with explicit failing-test commands.
- Task decomposition matches the natural dependency graph: pure classification first, then the recording model, then the planner filter, then runner detection, then give-up downgrade, then docs.
- Correctly corrects the design spec on `DbccFilesCompact`: it is a wait/task name, not a `sys.dm_exec_requests.command` verb, so the allow-list is exactly `ALTER INDEX` and `DBCC`.
- Uses the simplest possible planner integration: filter `transient_maintenance` entries in `confirmedSetFor` so `DecidePreShrink` needs no changes.
- Separate `tailProbe.maintWarned` guard, satisfying the design-assessment concern that the existing `warned` guard must not be reused.
- `giveUpReason` and `markTransient` helpers keep the give-up paths readable and avoid duplicating maintenance logic across the two stall branches.
- Per-file reset of `tp.maintBlock` in `chunkLoop` is correct for `files:all` runs; `maintWarned` stays once per operation as intended.
- Self-review section explicitly maps every design-spec paragraph to a task/step and confirms no placeholders remain.

## Feasibility checks against the codebase

I verified the following touch points:

- `TailFinding` lives in `internal/run/reaction.go`, exactly where the plan modifies it.
- `fakeServer.SPID()` returns `99`, matching the `blockedByMaint` test helper.
- `fakeServer` already has `noProgress`, `tail`, `tailFound`, and the other fields the new tests script.
- `wantTail` is defined in `internal/run/shrink_tailobject_test.go` (same package), so `shrink_maintblock_test.go` can reuse it.
- `newTestRunner` returns a `*ShrinkRunner` whose `major` field the tests can set to `15`.
- `tempdbFakeServer` exists and will need the `ActiveSessions` stub the plan describes.
- `internal/version/VERSION` is currently `0.9.0`, so the `0.10.0` bump is correct.
- `confirmedSetFor` in `cmd/sqlgopace/shrink_plan.go` is the only place that interprets `ConfirmedBy`; filtering there is the minimal change.

## Minor issues to address during execution

1. Test robustness in Task 5.
   `TestMaintBlockRecordsTransientTail` accesses `res[0].Reason` without first checking `len(res) == 1`. Add the same guard used in the existing `TestChunkLoopCapturesTailOnNoGainGiveUp`:
   ```go
   if len(res) != 1 || res[0].Reason == "" {
       t.Fatalf("got %+v, want a give-up result with a reason", res)
   }
   ```

2. Doc comment for new `ContendedObject` fields.
   The plan updates the `ConfirmedBy` comment but does not add a comment for `BlockedByCommand` / `BlockedBySPID`. Add a short line explaining they are set only when `ConfirmedBy == "transient_maintenance"`.

3. Warning message wording.
   The design-spec example named both database and file (`shrink of "PRODDB" file "PRODDB" …`). The plan's message names only the file (`shrink of %q blocked by …`). This is acceptable, but if the design spec's wording is intentional, align the format string with it.

4. `addTail` on a lock-captured object.
   If an object was already captured via a held Sch-M lock (`confirmed_by: lock`) and later `addTail` is called with `Transient: true`, the plan upgrades the entry to `transient_maintenance` and preserves the original `times_blocked`. This is the right behavior for the false-positive goal, but the resulting sidecar will show `times_blocked` alongside `transient_maintenance`. That is fine; just ensure the header comment explains that `transient_maintenance` is informational and not fed to the planner regardless of other fields.

5. `probeMaintBlock` placement after `stall()` returns an error.
   The plan calls `probeMaintBlock` even when `stall()` returns `werr != nil`. This is harmless because the function is best-effort and ignores read errors, but be aware that it will run once more on the context-canceled path.

6. Task 6 lacks exact insertion points.
   The plan says "add a subsection" in `docs/specs/SHRINK.md` and "one line under the shrink / contended-capture description" in `README.md` without line numbers. The implementer will need to locate the relevant sections. This is reasonable for a plan, but consider adding file-search hints (e.g., grep for `confirmed_by` or `tail_position`) if the plan is handed to a subagent.

## Open questions (non-blocking)

- Should the `warn` event include the database name in addition to the file name? The design example did; the plan does not. Pick one and keep both the `.log`/TUI message and the design spec consistent.
- Should there be an explicit test that `tailProbe.warned` (2019+ warning) and `tailProbe.maintWarned` are independent? The code structure guarantees it, but a regression test running the proactive walk below 2019 plus a maintenance block would lock the behavior in.

## Conclusion

The plan is coherent, executable, and low-risk. After the small robustness tweaks above, it can be implemented task-by-task as written. The expected test gate (`go build ./...`, `go vet ./...`, `go test -race ./...`) is appropriate, and the explicit note to skip `golangci-lint` is correct given the repo-wide CRLF/gofmt issue.
