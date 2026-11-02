# Shrink stall causes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A data shrink tells a chunk it stopped itself (yield), a chunk that met a running data backup, and a plain stall apart, answers each differently, and gives up with a reason that names the cause.

**Architecture:** The pure decisions (classify a no-gain chunk, tally who a stall yielded to, format the reason) go in a new `internal/run/shrink_stall.go`. `runChunk` returns a small struct that adds a session snapshot taken at the moment of a yield. `chunkLoop` reads the file size after every chunk, classifies, and routes to the existing `stall`, to a halving retry, or to a new stop-aware `awaitBackup`. One new read, `RunningDataBackup`, joins `ShrinkReader`. Defaults change in `config.go` and both `config.yaml` copies.

**Tech Stack:** Go, `github.com/microsoft/go-mssqldb`, SQL Server DMVs.

**Spec:** `docs/specs/superpowers/specs/2026-11-02-shrink-stall-causes-design.md` (commit `d39a75d`). Read it first; this plan does not repeat its reasoning.

## Global Constraints

- English only in code, comments, identifiers, docs; US spelling.
- No client identifiers anywhere. Placeholders only: `PRODDB`, `dbo.MEASUREMENT`.
- No `context.WithTimeout` around an executing statement; waits between statements are fine.
- SqlGoPace never issues `BACKUP`, never kills a backup session (the e2e test is the one place a backup is issued, by the test, not by the tool).
- Every task ends with, in one shell invocation: `go build ./... && go vet ./... && go test -race ./internal/... ./cmd/... && golangci-lint run ./...`. Anything the linter reports that is not `gofmt` on a CRLF checkout is a real finding.
- Commit messages: prose on why, no bold, no em-dash, no `Co-Authored-By` or any attribution trailer.
- New defaults, verbatim: `shrink.self_wait_timeout_minutes: 30`, `shrink.max_no_progress: 15`; `no_progress_backoff_seconds: 30` and `no_progress_backoff_max_seconds: 300` unchanged. The `batch:` block's own `self_wait_timeout_minutes: 5` does not change.
- Backup predicate, verbatim: `command = 'BACKUP DATABASE' AND database_id = DB_ID()`. Never read `estimated_completion_time`.
- Version: `internal/version/VERSION` goes from `0.48.0` to `0.49.0`.
- Deviation from the spec, deliberate: the spec says the give-up tail finding also goes on `ShrinkResult`. Its only consumer was the withdrawn relocation lot, so this plan puts it in the reason only. Task 7 corrects that sentence of the spec.
- The work happens on a feature branch. Tasks 1 to 6 commit code without its docs; Task 7 brings the docs, config and CHANGELOG level before the branch is merged, which is what CLAUDE.md's "docs move with the code" requires of `main`.
- Measured shape on 2022 CU26, which the docs in Task 7 must state as measured (not the Msg 3140 shape alone): with SqlGoPace's own `WAIT_AT_LOW_PRIORITY` chunks and a backup to a file, a chunk that meets the backup usually ends without error and without gain; a plain chunk can fail with Msg 3140 State 5, and can also fail with 3140 after reaching its target. The classification does not depend on the error number.

## Review Focus

1. A data backup running while the supervisor stops a chunk for pressure: classified as yield, step halved, no backup wait (Task 2 table test, Task 4 driver test).
2. A graceful stop requested during a long backup wait: the shrink returns `ErrStopped` after at most one poll (Task 5).
3. `ActiveSessions` failing at the moment of a yield: the yield is still handled and the reason simply carries no login (Task 3).
4. `FileSizeMB` failing after an errored chunk: the error is returned, as on the clean path today (Task 4).
5. The tempdb path: `RunningDataBackup` is never called (Task 4).

---

### Task 1: The running-backup read

**Files:**
- Modify: `internal/mssql/shrink.go` (new type, SQL constant, method on `*Conn`)
- Modify: `internal/run/shrink.go:17-32` (`ShrinkReader`)
- Modify: `internal/run/shrink_driver_test.go` (`fakeServer`, `tempdbFakeServer`)
- Test: `internal/mssql/shrink_integration_test.go`

**Interfaces:**
- Produces: `type BackupRequest struct { SessionID int; PercentComplete float64; StartTime time.Time }` in package `mssql`; `func (c *Conn) RunningDataBackup(ctx context.Context) (BackupRequest, bool, error)`; the same method on `run.ShrinkReader`. `fakeServer` gains a field `backups []mssql.BackupRequest`: each `RunningDataBackup` call pops the front element (found) until the slice is empty, then returns not found; and a counter `backupCalls int`.

- [ ] **Step 1: Write the failing integration test** `TestIntegrationRunningDataBackupNone` in `internal/mssql/shrink_integration_test.go`: on `openTestConn(t)`, with nothing running, `RunningDataBackup` returns `found == false` and `err == nil`. It proves the SQL parses and runs on the target version.
- [ ] **Step 2: Run** `go vet -tags=integration ./internal/mssql/` and expect a compile failure naming `RunningDataBackup`.
- [ ] **Step 3: Implement.** SQL constant `runningDataBackupSQL`: `SELECT TOP (1) session_id, percent_complete, start_time FROM sys.dm_exec_requests WHERE command = 'BACKUP DATABASE' AND database_id = DB_ID() ORDER BY start_time;`. `sql.ErrNoRows` means not found. Wrap other errors as `read running backup: %w`. Add the method to `ShrinkReader` with a doc comment saying log backups are excluded on purpose (measured harmless), and add the fakes. `tempdbFakeServer.RunningDataBackup` returns not found and increments its own `backupCalls` field, so Task 5 can assert it stays zero.
- [ ] **Step 4: Run** `go vet -tags=integration ./internal/mssql/ && go test -race ./internal/run/` and expect success; with `SQLGOPACE_TEST_DSN` set, `go test -tags=integration -run '^TestIntegrationRunningDataBackupNone$' ./internal/mssql/` reports exactly 1 test passing.
- [ ] **Step 5: Commit** `feat(mssql): read the data backup running on the shrunk database`.

### Task 2: Pure stall decisions

**Files:**
- Create: `internal/run/shrink_stall.go`
- Test: `internal/run/shrink_stall_test.go`

**Interfaces:**
- Produces:
  - `type noGainCause int` with constants `causeYield`, `causeBackup`, `causeStall`.
  - `func classifyNoGain(stopped, backupRunning bool) noGainCause`: `stopped` wins, then `backupRunning`, else stall.
  - `type yieldSnapshot struct { Blocked []mssql.Session; Self mssql.SelfBlock }`.
  - `type yieldTally struct` with `func (t *yieldTally) add(s yieldSnapshot)`, `func (t *yieldTally) reset()`, `func (t yieldTally) count() int`, `func (t yieldTally) describe() string`.
  - `describe()` returns `""` for zero yields; otherwise `yielded N times` followed, when known, by `(blocking login=<most frequent blocked login>, open transaction)` (the `open transaction` part only when that login's sessions had `OpenTransactions > 0` in any snapshot) and `; chunk itself blocked by session <SPID>, login=<login>` for the most recent `Self.Blocked` snapshot. Ties on frequency break by the login seen first.

- [ ] **Step 1: Write the failing tests.** `TestClassifyNoGain`, table-driven, four rows: `(true,true)→causeYield`, `(true,false)→causeYield`, `(false,true)→causeBackup`, `(false,false)→causeStall`. `TestYieldTallyDescribe`: three snapshots, logins `svc_loader` (2 times, one with `OpenTransactions: 2`) and `svc_report` (1), the last with `Self: mssql.SelfBlock{Blocked: true, SPID: 60, Login: "svc_writer"}`; assert `describe()` contains `yielded 3 times`, `login=svc_loader`, `open transaction`, `session 60`, `login=svc_writer`; after `reset()`, `describe() == ""` and `count() == 0`. `TestYieldTallyEmptySnapshot`: one `add(yieldSnapshot{})` gives `yielded 1 times` and no `login=`.
- [ ] **Step 2: Run** `go test -race -run '^(TestClassifyNoGain|TestYieldTally)' ./internal/run/` and expect a compile failure.
- [ ] **Step 3: Implement** the types and functions in `internal/run/shrink_stall.go`.
- [ ] **Step 4: Run** the same command; expect exactly 3 tests passing. Any other count means the filter or the work is wrong.
- [ ] **Step 5: Commit** `feat(run): classify a no-gain shrink chunk and tally who it yielded to`.

### Task 3: Snapshot the sessions at the moment of a yield

**Files:**
- Modify: `internal/run/shrink.go` (`runChunk`, its one caller in `chunkLoop`)
- Test: `internal/run/shrink_driver_test.go`

**Interfaces:**
- Consumes: `yieldSnapshot` (Task 2).
- Produces: `type chunkOutcome struct { stopped bool; maint *MaintBlock; yield *yieldSnapshot; err error }`; `func (r *ShrinkRunner) runChunk(ctx context.Context, file string, targetMB int, res ddl.ResolvedOptions, ignore IgnoreSource, sink ReactionSink, base ShrinkProgress, watchSelfBlock bool) chunkOutcome`. `yield` is non-nil exactly when `stopped` is true.

- [ ] **Step 1: Write the failing test** `TestShrinkSnapshotAtYieldBeforeCancel`. Fake setup: `blockShrink: true`, `r.pollIntv = 2 * time.Millisecond`, `r.sampler = &blockingSampler{clk: clk, step: 40 * time.Second}`, `ResolvedOptions{MaxBlockMinutes: 1}`, `fakeServer.sessions` holding one session with `BlockingSPID: 99` (the fake's SPID), `Login: "svc_loader"`, `OpenTransactions: 2`. Add a field `clearSessionsOnCancel bool` to `fakeServer`: in the `blockShrink` branch of `ExecDDL`, after `<-ctx.Done()`, it sets `s.sessions = nil` (under the fake's mutex if it has one), so a snapshot taken after `cancelAndAwait` sees nothing; set it in this test. Call `r.runChunk` directly once with a 10 s bounded context; assert `out.stopped`, `out.yield != nil`, `len(out.yield.Blocked) == 1`, `out.yield.Blocked[0].Login == "svc_loader"`. Second test `TestShrinkSnapshotAtYieldSurvivesReadError`: a fake whose `ActiveSessions` returns an error (add a field `sessionsErr error` to `fakeServer`); assert `out.stopped` and `out.yield != nil` with no sessions. (The names avoid the `TestShrinkYield` prefix, which Task 4's filter uses.)
- [ ] **Step 2: Run** `go test -race -run '^TestShrinkSnapshotAtYield' ./internal/run/` and expect a compile failure.
- [ ] **Step 3: Implement.** In `runChunk`, when `action != Continue`, before `cancelAndAwait`: read `r.reader.ActiveSessions(ctx)`; on success keep the sessions with `s.BlockedBy(r.exec.SPID())` and `mssql.FindSelfBlock(sessions, r.exec.SPID())`; on error keep an empty snapshot. Return `chunkOutcome`. Update `chunkLoop` to read the struct fields; behavior otherwise unchanged in this task.
- [ ] **Step 4: Run** the same filter with `-v`; expect exactly 2 tests passing. Then the full `go test -race ./internal/run/`.
- [ ] **Step 5: Break it on purpose.** Move the `ActiveSessions` read after `cancelAndAwait`; `TestShrinkSnapshotAtYieldBeforeCancel` must fail. Report "1 of 2 pass" as the success of this step, then restore.
- [ ] **Step 6: Commit** `feat(run): snapshot blocked sessions when a shrink chunk yields`.

### Task 4: Classify no-gain chunks; the yield path

**Files:**
- Modify: `internal/run/shrink.go` (`chunkLoop`)
- Test: `internal/run/shrink_driver_test.go`

**Interfaces:**
- Consumes: `chunkOutcome` (Task 3), `classifyNoGain`, `yieldTally` (Task 2), `RunningDataBackup` (Task 1).
- Produces: in `chunkLoop`, a local `tally yieldTally`; the backup branch calls `r.awaitBackup` (Task 5). Until Task 5 lands, the backup branch falls through to the stall branch, so this task stays green on its own.

Loop shape after each chunk (replaces today's two branches at `shrink.go:518-590`):
1. `ownCancellation(out.err)` returns as today.
2. Read `newSize` with `FileSizeMB` whatever `out.err` was; a read error is returned. Every chunk that gained nothing adds its elapsed time to `blocked` before classification, as today's clean no-gain branch does, so the productive rate and `ETASecondsNoBlock` stay honest across yields and backups.
3. If `out.stopped`, run `awaitRelief` exactly as today (log-drain and graceful-stop handling unchanged).
4. If `newSize < current`: progress path as today, plus `tally.reset()`. This is new for an errored chunk that shrank: it now reaches `AdjustStepMB` like any progressing chunk (the step may grow), where today the error branch halved and skipped it. That is intended (the chunk made progress, whatever else happened).
5. Else, call `r.reader.RunningDataBackup` only when `prof == nil && !out.stopped` (not on tempdb, not after a yield); a read error is a `warn` reaction and counts as not running. Then `switch classifyNoGain(out.stopped, running)`:
   - `causeYield`: `tally.add(*out.yield)`; `wasAboveFloor := step > r.tuning.MinStepMB`; `step = halveStep(step, r.tuning, maxStep)`; if `wasAboveFloor` then `continue` with no stall call; else fall into the stall call below.
   - `causeBackup`: Task 5.
   - `causeStall`: `step = halveStep(...)` only when `out.err != nil` (today's error path behavior; a clean no-gain does not halve, as today).
   Stall call: today's `r.stall(...)`, `r.applyMaintBlock(...)`, and the give-up branch, whose reason Task 6 rewrites. Keep `out.err` in the reason as today.

The yield tests need pressure that is chunk-scoped, not poll-scoped: `blockingSampler.stopAfter` counts polls, and the 2 ms pump can fire a fourth poll before sampling stops, so a poll count does not map to a chunk count. Add a `chunkGatedSampler` to the test file: it holds a pointer to a chunk counter that `fakeServer.onExec` increments on each `DBCC SHRINKFILE`, reports `BlockState{Any: true}` and advances the clock by its step while the counter is `<= blockChunks`, and reports no blocking afterward. `onExec` clears `blockShrink` once the counter exceeds `blockChunks`. Between chunks `awaitRelief` returns at once: the pump sets `BlockingOthers` from `Unignored` (`executor.go:554`), which this sampler never sets.

- [ ] **Step 1: Write the failing tests.**
  - `TestShrinkYieldHalvesWithoutCountingAboveFloor`: the Task 3 fake setup with `chunkGatedSampler{blockChunks: 3}`, tuning `testTuning()` with `InitialStepSmallMB: 400` (so the steps go 400, 200, 100, then 50 = `MinStepMB`: three yields all above the floor; with the stock 100 the second yield is already at the floor), file 4000 MB, used 400, target `10%`. Assert from `s.execLog` that the first three chunk targets are 3600, 3800, 3900, that no `pause` reaction says `made no progress`, and that the run reaches target with an empty `Reason`.
  - `TestShrinkYieldAtFloorSpendsBudget`: `testTuning()` with `MinStepMB: 100` (equal to the initial step, so the first yield is at the floor), `chunkGatedSampler{blockChunks: 1000}`; assert the run ends with a non-empty `Reason` and that `pause` reactions saying `made no progress` occurred (the budget was spent). The `yielded` wording is Task 6's and is asserted there.
  - `TestShrinkYieldWinsOverBackup`: same as the first test but `s.backups` holds 5 requests; assert the same three targets and `s.backupCalls == 0`.
  - `TestShrinkErroredChunkThatShrankIsProgress`: `chunkErrs` with one `mssqldb.Error{Number: 3140}` and an `onExec` hook that, on that chunk, sets `s.sizeMB` 100 MB lower before the error returns; assert the next chunk's target is computed from the reduced size and no `made no progress` reaction was emitted.
  - `TestShrinkSizeReadErrorAfterChunkErrorFails`: add `sizeErr error` to `fakeServer` (returned by `FileSizeMB` when set after the TRUNCATEONLY pass; use a call counter, since `Run` reads the size before the chunk loop); a chunk error then a size-read error makes `Run` return a non-nil error.
  - `TestTempdbNeverReadsBackups`: the existing tempdb fake with a persistent stall; assert its `backupCalls == 0`.
- [ ] **Step 2: Run** `go test -race -v -run '^(TestShrinkYield|TestShrinkErroredChunkThatShrankIsProgress|TestShrinkSizeReadErrorAfterChunkErrorFails|TestTempdbNeverReadsBackups)' ./internal/run/` and expect failures (or compile failures for the new fake fields).
- [ ] **Step 3: Implement** the loop shape above.
- [ ] **Step 4: Break it on purpose, twice.** Remove the `continue` of the above-floor yield; `TestShrinkYieldHalvesWithoutCountingAboveFloor` must fail; report "5 of 6 pass", then restore. Remove the `prof == nil` guard on the backup read; `TestTempdbNeverReadsBackups` must fail; report "5 of 6 pass", then restore.
- [ ] **Step 5: Run** the filter with `-v`; expect exactly 6 tests passing. Then `go test -race ./internal/run/`; every existing shrink test must still pass, in particular `TestShrinkDataNoProgressStops`, `TestShrinkAbsorbsAllocationError`, `TestTempdbFlushesOnceOnPersistentStall`.
- [ ] **Step 6: Commit** `feat(run): let a shrink chunk that yields shrink its step instead of stalling`.

### Task 5: The backup wait

**Files:**
- Modify: `internal/run/shrink.go` (new method `awaitBackup`, the `causeBackup` branch of Task 4)
- Test: `internal/run/shrink_driver_test.go`

**Interfaces:**
- Consumes: `RunningDataBackup` (Task 1), the `causeBackup` branch (Task 4).
- Produces: `func (r *ShrinkRunner) awaitBackup(ctx context.Context, file string, first mssql.BackupRequest, sink ReactionSink, prog ShrinkProgress) error`. Returns `nil` when no data backup runs any more, `ErrStopped` on graceful stop, the context error on cancellation.

Behavior: emit one `ReactionEvent{Kind: "pause"}` whose detail contains the literal text `BACKUP DATABASE`, the backup's session id and its start time (Task 8 greps the run log for that text); then loop: if `stopRequested(r.stop)` return `ErrStopped`; `r.wait(ctx, r.tuning.NoProgressBackoff)`; re-read; on not found or read error, leave the loop (a read error adds a `warn`). During the loop, `emitProgress(prog)` with `Statement` set to `-- waiting for BACKUP DATABASE (session N, P% done)`. On exit emit one `ReactionEvent{Kind: "info"}` with the wait's duration (`r.clk.Since(start)`). In `chunkLoop`'s `causeBackup` branch: no `halveStep`, no `stall`, no change to `noProgress`, `backoff`, `stallWaited`; add the wait to `blocked`; `ErrStopped` sets `result.FinalMB = current`, `result.Reason = "stopped: graceful stop during a backup wait (work preserved)"` and returns it; then `continue`.

- [ ] **Step 1: Write the failing tests.**
  - `TestShrinkWaitsForDataBackup`: `chunkErrs` with one Msg 3140, `s.backups` holding 3 requests (`SessionID: 56`); assert the run reaches target, the chunk after the wait has the same target as the failed one, exactly one `pause` and one `info` reaction mention `BACKUP DATABASE` or `backup`, and no `made no progress` reaction.
  - `TestShrinkBackupWaitLeavesBudgetAlone`: `testTuning()` with `MaxNoProgress: 3`; `chunkErrs` of three errors: two plain ones (`errors.New("plain stall")`) then one `mssqldb.Error{Number: 3140}`; the fourth chunk succeeds. `s.backups` starts empty; an `onExec` hook appends 3 requests when the third `DBCC SHRINKFILE` runs, so only that chunk is classified as a backup. Assert the run reaches target with an empty `Reason` (two counted stalls, then the backup wait, then progress). If the backup chunk were counted, it would be the third stall and the run would give up on `max_no_progress`.
  - `TestShrinkBackupWaitHonorsGracefulStop`: `chunkErrs` with one Msg 3140 on the first chunk (so it gains nothing and reaches the classification), `s.backups` with 1000 entries, a `stop` func that turns true after the first poll (count calls to `r.wait`); assert `Run` returns `ErrStopped` and `len(s.backups)` dropped by at most 2.
- [ ] **Step 2: Run** `go test -race -v -run '^(TestShrinkWaitsForDataBackup|TestShrinkBackupWait)' ./internal/run/` and expect failures.
- [ ] **Step 3: Implement** `awaitBackup` and the branch.
- [ ] **Step 4: Break it on purpose.** Route `causeBackup` to the stall call (treat it exactly as `causeStall`, give-up included); `TestShrinkBackupWaitLeavesBudgetAlone` must fail on a `max_no_progress` give-up, and `TestShrinkWaitsForDataBackup` should fail on its `made no progress` assertion; the graceful-stop test may go either way. Report which tests failed and why as the success of this step, then restore.
- [ ] **Step 5: Run** the filter with `-v`; expect exactly 3 tests passing; then `go test -race ./internal/run/`.
- [ ] **Step 6: Commit** `feat(run): wait out a running data backup instead of stalling a shrink`.

### Task 6: The give-up reason

**Files:**
- Modify: `internal/run/shrink.go` (`captureGiveUpTail`, the give-up branch of `chunkLoop`)
- Test: `internal/run/shrink_tailobject_test.go`, `internal/run/shrink_driver_test.go`

**Interfaces:**
- Consumes: `yieldTally.describe()` (Task 2).
- Produces: `func (r *ShrinkRunner) captureGiveUpTail(ctx context.Context, f mssql.FileSpace, sink ReactionSink, tp *tailProbe) *TailFinding` (same side effects as today, now returns the finding or nil).

Reason assembly at give-up, in this order, joined by `; `: `tally.describe()` when non-empty; `no further progress after <bound>` (with `: <err>` when the last chunk errored, as today); `step at min_step_mb (N MB)` only when the tally is non-empty and `step == r.tuning.MinStepMB`; `tail object at give-up: <schema>.<table> index_id=<id>, <n> pages from end` when the walk found one; `work preserved`. `giveUpReason(tp, base)` keeps its precedence: when `tp.maintBlock` is set, its transient-maintenance wording replaces the whole assembled reason, tally and tail included. That is deliberate: a tail pinned by concurrent maintenance is the cause, and the operator's action (re-run after maintenance) does not depend on the rest. Say so in a comment on `giveUpReason`.

- [ ] **Step 1: Write the tests.** `TestGiveUpReasonNamesFreshTail` (in `shrink_tailobject_test.go`): set `r.major = 15` (`walkTail` does nothing below 15) and `IdentifyTailObject: true` on the operation (so the entry walk consumes the first `tailSeq` entry, as in `TestProactiveTailRecordedOnGiveUp`); `tailSeq` with an entry object then a different give-up object (`dbo.MEASUREMENT`, `IndexID: 5`), `noProgress: true`; assert the `Reason` contains `tail object at give-up: dbo.MEASUREMENT index_id=5`. `TestGiveUpReasonNamesYieldedLogin`: the floor-yield setup of Task 4 (`MinStepMB: 100`, `chunkGatedSampler{blockChunks: 1000}`) with a blocked session `Login: "svc_loader"`; assert the `Reason` contains `yielded`, `login=svc_loader`, `min_step_mb`. `TestGiveUpReasonPrintsTimeWaited`: `testTuning()` with `MaxNoProgress: 15`, `SelfWaitTimeout: 30 * time.Minute`, `NoProgressBackoff: 30 * time.Second`, `NoProgressBackoffMax: 300 * time.Second`, `noProgress: true`; assert the `Reason` contains `32m30s of waiting (shrink.self_wait_timeout_minutes)` and not `max_no_progress`. This last one already passes on today's code (measured by the review panel); it is a guard that the new assembly keeps the bound wording, not a red test.
- [ ] **Step 2: Run** `go test -race -v -run '^TestGiveUpReason' ./internal/run/` and expect exactly 2 failures (`NamesFreshTail`, `NamesYieldedLogin`) and 1 pass (`PrintsTimeWaited`).
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the filter with `-v`; expect exactly 3 tests passing; then `go test -race ./internal/run/`. Existing tests asserting reason substrings (`TestShrinkAbsorbsAllocationError` wants `adjust`, `TestShrinkAbsorbsUnknownChunkError` wants the error text) must still pass unchanged.
- [ ] **Step 5: Commit** `feat(run): name the cause in a shrink's give-up reason`.

### Task 7: Defaults, docs, version

**Files:**
- Modify: `internal/config/config.go:473,477`; `internal/config/config_test.go:162,166` (`TestShrinkConfigDefaultsWhenAbsent` pins the old defaults 3 and 5 and must move to 15 and 30); `config.yaml:86-93`; `internal/scaffold/assets/config.yaml` (byte-identical copy of `config.yaml`); `internal/version/VERSION`; `CHANGELOG.md`; `docs/configuration.md` (shrink section around lines 312-335 only, not the batch table at 368); `docs/shrink.md`; `docs/specs/SHRINK.md` (§7.1, §7.3 defaults block at 346-349, §8.2, §8.3, §8.6, §11 table, new section "Backup wait"); `docs/specs/TEMPDB-SHRINK.md` §5; `docs/specs/TODO.md`; the spec, one sentence.

- [ ] **Step 1: Change the defaults** in `applyDefaults` (`setIf(&s.MaxNoProgress, 15)`, `setIf(&s.SelfWaitTimeoutMinutes, 30)`) and the two YAML copies; rewrite the YAML comment that says "about 90 s of waiting" to state the new behavior (the time bound trips first, after about 32 minutes of backoff).
- [ ] **Step 2: Run** `go test -race ./internal/config/ ./internal/scaffold/`. Expect exactly one failure before you touch the tests, `TestShrinkConfigDefaultsWhenAbsent` (`max_no_progress = 15, want default 3`, `self_wait_timeout_minutes = 30, want default 5`): update its two expected values. `TestShippedConfigStatesTheRealDefaults` and the twin-pinning test must pass without edits; if either fails, the YAML and `applyDefaults` disagree, so fix the YAML, not those tests.
- [ ] **Step 3: Docs.** Each file listed above says what the code now does: yield halving, backup wait (no ceiling, stop-aware, two reactions, log backups excluded, the measured Msg 3140 State 5 behavior), the new defaults, the reason shapes. `CHANGELOG.md` gets a `## [0.49.0]` section in the style of `0.16.0`: one bullet per change, the migration note verbatim in substance: an operator whose `config.yaml` sets `max_no_progress: 3` or `self_wait_timeout_minutes: 5` keeps the short behavior and should revisit both, and lowering `max_no_progress` to 2 or less fails to load while `no_progress_before_flush` is 2. `TODO.md`: move "Shrink stall causes", "The tail object is identified before the give-up but never feeds the decision" and "The no-progress budget is sized for a chunk" to Shipped, naming `internal/run/shrink_stall.go` as evidence. In the spec, replace "and the same finding goes on `ShrinkResult` (and still to the sink and the `.contended.yaml`, as now)" with "and still goes to the sink and the `.contended.yaml`, as now".
- [ ] **Step 4: Bump** `internal/version/VERSION` to `0.49.0`.
- [ ] **Step 5: Run** the full constraint command; then `grep -rn '90 s\b\|90 seconds' config.yaml internal/scaffold/assets/config.yaml README.md docs/configuration.md docs/shrink.md docs/specs/SHRINK.md docs/specs/TEMPDB-SHRINK.md` returns nothing about the old shrink give-up (today it finds line 88 of both YAML copies and `docs/configuration.md:333`, where "about" ends the previous line). Leave `docs/specs/superpowers/` and the TODO entries moved to Shipped as they are: they are historical records.
- [ ] **Step 6: Commit** `feat(shrink): wait half an hour before giving up a stalled shrink by default`.

### Task 8: e2e against a real backup

**Files:**
- Modify: `cmd/sqlgopace/e2e_integration_test.go`

**Interfaces:**
- Consumes: the whole feature, through `cli`.

- [ ] **Step 1: Write the test** `TestE2EShrinkWaitsForBackup`. With `e2eDSN(t)`, open a `database/sql` connection and create `sqlgopace_e2e_backup` (skip the test with `t.Skip` and the server's message if `CREATE DATABASE` is refused), `RECOVERY SIMPLE`, two tables of `char(7000)` rows (60 000 rows each), drop the first, `CHECKPOINT`; `t.Cleanup` sets it `SINGLE_USER WITH ROLLBACK IMMEDIATE`, drops it and runs `msdb.dbo.sp_delete_database_backuphistory`. Start a goroutine on its own connection looping `BACKUP DATABASE [sqlgopace_e2e_backup] TO DISK = '/var/opt/mssql/data/sqlgopace_e2e_backup.bak' WITH INIT, COPY_ONLY, NO_COMPRESSION, BUFFERCOUNT = 1, MAXTRANSFERSIZE = 65536`. Not `TO DISK = 'NUL'`: measured by the review panel, a backup to `NUL` of this database takes 0.6 s, every chunk waits it out and advances, and the test never sees a backup wait; to a file it takes about 6 s and 5 chunks out of 8 gained nothing. The loop is bounded in time, not by the shrink: it stops after 90 s or when a done channel closes, whichever comes first, because `awaitBackup` waits as long as a data backup runs and an unbounded loop would make the shrink wait for the test. It sends its first error, if any, on a buffered channel. Before running `cli`, poll `sys.dm_exec_requests` with the plan's predicate from a connection in the test database until a backup is seen (fail after 30 s), so the shrink starts against a live backup. `t.Cleanup`, registered before the goroutine starts, closes the done channel, waits for the goroutine to return, then drops the database as above and runs `EXEC master.sys.xp_delete_files N'/var/opt/mssql/data/sqlgopace_e2e_backup.bak'` (measured on 2022 CU26: removes the file). Write a manifest with `database: sqlgopace_e2e_backup`, one `shrink` of `type: data`, `files: all`, `targetfreespace: 10%`. Run `cli` with `writeE2EConfig`. Assert the backup goroutine reported no error, the manifest is in `03.done`, and its `.log` contains `BACKUP DATABASE`.
- [ ] **Step 2: Run** with the e2e container: `make e2e CONTAINER=podman COMPOSE="podman compose"`, or against the lab with `SQLGOPACE_TEST_DSN` set, `go test -tags=integration -run '^TestE2EShrinkWaitsForBackup$' ./cmd/sqlgopace/`; expect exactly 1 test passing. Run it three times: a pass that depends on timing must pass all three. If a run completes without a backup wait (every chunk advanced), do not tune the test until it passes: report the run log's chunk lines and stop, because that is a measurement against the brief.
- [ ] **Step 3: Break it on purpose.** Make `RunningDataBackup` return not found unconditionally; the test must fail (no `BACKUP DATABASE` in the log, or an `INCOMPLETE`). Restore.
- [ ] **Step 4: Check the server** afterward: no database named `sqlgopace_e2e_backup`, no `msdb.dbo.backupset` row for it, no `sqlgopace_e2e_backup.bak` under `/var/opt/mssql/data`, no `BACKUP` request left in `sys.dm_exec_requests`.
- [ ] **Step 5: Commit** `test(e2e): run a shrink against a live backup loop`.
