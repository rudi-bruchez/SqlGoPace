# Paced Reorg Yielding + RCSI-Off Warning Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make an `ALTER INDEX ... REORGANIZE` pace itself (cancel on blocking, wait for relief, re-issue — uncapped — until done) instead of failing after `MaxRetries`, and warn the operator when a reorg starts against a database with `READ_COMMITTED_SNAPSHOT` off.

**Architecture:** Reorg already runs through `MonitoredRunner` and is `CancelSafe`. The change is a refinement of the pure `runLoop` state machine: a new `reissue` closure (non-nil only for `reorganize_index`) turns a `Cancel` decision into "wait for relief, re-issue the same REORGANIZE," looping without consuming retries. The RCSI warning is a pure decision helper emitted through the engine's existing per-step reaction `sink`. A one-line connection-hardening change sets `IMPLICIT_TRANSACTIONS OFF`. No new manifest fields, config knobs, matrix/generator changes, or new drivers.

**Tech Stack:** Go, `go test -race` (no database for unit tests), golangci-lint v2. SQL Server via `github.com/microsoft/go-mssqldb`.

## Global Constraints

- **Idiomatic Go, KISS.** Match surrounding code; no new layers/interfaces/abstractions.
- **English only** in all code, comments, identifiers, docs.
- **No new manifest fields and no new config knobs.** Behavior is automatic.
- **The matrix (`reorganize_index: {}` stays optionless), the generator, the shrink driver, and the resumable pause/resume path are unchanged.**
- **The paced path is wired for `reorganize_index` ONLY** — a type switch, not the `CancelSafe` flag. `check_db` and `update_statistics` keep today's bounded-retry behavior.
- **Version** lives in `internal/version/VERSION` (embedded); bump the file, no build flags. Current: `0.11.0`.
- **Tests:** `go test -race ./internal/run/...` runs with no database. `make vet` / `go build ./...` must stay green.
- **Repo is CRLF.** Do not reformat existing files; edit surgically. Gate on build/vet/test, not `gofmt`-the-repo.
- **Commit trailer:** end every commit message with `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`.
- Reference spec: `docs/superpowers/specs/2026-07-31-reorg-pacing-rcsi-warning-design.md`. Background: `docs/REORGANIZE.md`.

---

## File Structure

- `internal/run/monitored_runner.go` — **modify.** `runLoop` gains a `reissue` param and a paced `Cancel` branch; add `reissueFor`; wire it in `runOnce`.
- `internal/run/executor_test.go` — **modify.** Update the 7 existing `runLoop(...)` calls; add paced `runLoop` tests and `reissueFor` tests.
- `internal/run/reorg_rcsi.go` — **create.** The pure `reorgRCSIWarning` decision helper.
- `internal/run/reorg_rcsi_test.go` — **create.** Unit tests for the helper.
- `internal/run/engine.go` — **modify.** Add `rcsi bool` field + `WithRCSI` option; emit the warning through the per-step `sink` in `processOne` (only when the op will run — inside the `prepErr == nil` branch).
- `internal/run/reaction.go` / `internal/report/report.go` — **modify.** Refresh the stale `ReactionEvent.Kind` and `ReactionLine` doc-comments to include `warn`/`info`/`abort`.
- `internal/run/engine_test.go` — **modify.** Add an engine-level test asserting the warning reaches the `.log` for a reorg with RCSI off (and not with RCSI on).
- `cmd/sqlgopace/main.go` — **modify.** Pass `run.WithRCSI(info.RCSIEnabled)` next to `run.WithADR(...)`.
- `internal/mssql/conn.go` — **modify.** Add `SET IMPLICIT_TRANSACTIONS OFF;` to `harden()`.
- `internal/version/VERSION` — **modify.** `0.11.0` → `0.12.0`.
- `docs/REORGANIZE.md` — **modify.** Correct the "follow-up driver" note; document paced behavior, the RCSI warning, and the hardening.

---

## Task 1: Paced re-issue in `runLoop`

**Files:**
- Modify: `internal/run/monitored_runner.go` (`runLoop` ~line 110, `runOnce` ~line 82; add `reissueFor`)
- Test: `internal/run/executor_test.go` (`stmtRun` harness ~line 236; 7 existing `runLoop` calls at lines 260, 271, 278, 289, 300, 317, 328; add new tests)

**Interfaces:**
- Consumes: existing `runStatement`, `waitForRelief`, `resumeSQL` closures; `ddl.ReorganizeIndex`; `ReactionEvent`, `ReactionSink`; `ErrCancelled`, `ErrStopped`.
- Produces:
  - `runLoop(sql string, runStatement func(string) (Action, error), waitForRelief func() error, resumeSQL func() (string, error), reissue func() (string, error)) error` — new 5th param; `reissue == nil` means the ordinary bounded-retry path.
  - `reissueFor(op ddl.Operation, sql string, sink ReactionSink) func() (string, error)` — returns nil unless `op` is `ddl.ReorganizeIndex`; otherwise a closure that emits `ReactionEvent{Kind: "resume", Detail: "pressure cleared — re-issuing REORGANIZE"}` and returns `sql`.

- [ ] **Step 1: Extend the `stmtRun` test harness for re-issue**

In `internal/run/executor_test.go`, add a re-issue counter and closure to the `stmtRun` struct (currently ends at the `resumeSQL` method ~line 256):

```go
// reissued counts paced re-issues; reissueSQL is the paced closure passed to runLoop
// (returns the original statement, mirroring how a reorg resumes from persisted progress).
func (s *stmtRun) reissueSQL() (string, error) { s.reissued++; return "REORGANIZE", nil }
```

Add the field to the struct definition (after `reliefErr error`):

```go
	reissued  int
```

- [ ] **Step 2: Write the failing paced-completion test**

Add to `internal/run/executor_test.go`:

```go
func TestRunLoopPacedReissuesUntilComplete(t *testing.T) {
	// Cancel under pressure, then complete: the paced branch waits for relief and
	// re-issues the SAME statement (not a RESUME).
	s := &stmtRun{actions: []Action{Cancel, Continue}, errs: []error{nil, nil}}
	if err := runLoop("REORGANIZE", s.runStatement, s.waitForRelief, s.resumeSQL, s.reissueSQL); err != nil {
		t.Fatalf("runLoop() = %v, want nil", err)
	}
	if len(s.ran) != 2 || s.ran[0] != "REORGANIZE" || s.ran[1] != "REORGANIZE" {
		t.Errorf("ran = %v, want [REORGANIZE REORGANIZE] (re-issue the original, not a RESUME)", s.ran)
	}
	if s.relief != 1 {
		t.Errorf("waitForRelief called %d times, want 1", s.relief)
	}
	if s.reissued != 1 {
		t.Errorf("reissue called %d times, want 1", s.reissued)
	}
}
```

- [ ] **Step 3: Run the new test to verify it fails**

Run: `go test ./internal/run/ -run TestRunLoopPacedReissuesUntilComplete`
Expected: FAIL — compile error, `runLoop` takes 4 args not 5 (`too many arguments in call to runLoop`).

- [ ] **Step 4: Add the `reissue` parameter and paced `Cancel` branch to `runLoop`**

In `internal/run/monitored_runner.go`, replace the `runLoop` signature and `Cancel` case. The full new function:

```go
// runLoop drives the pause/resume state machine for one operation. It runs the
// current statement; on Pause it waits for relief and resumes (running the RESUME
// statement next); on Cancel it either paces (when reissue != nil: wait for relief,
// re-issue the same statement — used for reorganize_index, which resumes from
// persisted progress) or reports ErrCancelled (bounded-retry path). The function
// arguments isolate the loop logic from live execution and timing so it can be
// tested deterministically.
func runLoop(sql string, runStatement func(string) (Action, error), waitForRelief func() error, resumeSQL func() (string, error), reissue func() (string, error)) error {
	stmt := sql
	for {
		action, err := runStatement(stmt)
		switch action {
		case Cancel:
			if reissue == nil {
				return ErrCancelled
			}
			// Paced (reorganize_index): the cancel is a clean incremental stop, so wait for
			// relief and re-issue the same REORGANIZE (SQL Server continues from persisted
			// progress). Uncapped — this never returns ErrCancelled, so Run's MaxRetries
			// never applies; the loop is bounded only by graceful stop and log-drain timeout.
			if err := waitForRelief(); err != nil {
				return err
			}
			next, err := reissue()
			if err != nil {
				return err
			}
			stmt = next
		case Stop:
			// Graceful stop: the statement was paused (runStatement stopped it, preserving
			// the resumable's work). Return without resuming; the next run continues it.
			return ErrStopped
		case Pause:
			if err := waitForRelief(); err != nil {
				return err
			}
			next, err := resumeSQL()
			if err != nil {
				return err
			}
			stmt = next
		default: // Continue: the statement finished (success or real failure)
			return err
		}
	}
}
```

- [ ] **Step 5: Update the 7 existing `runLoop` call sites to pass `nil`**

In `internal/run/executor_test.go`, append `, nil` to the `runLoop(...)` call in each of these tests (they exercise the non-paced path): `TestRunLoopCompletes`, `TestRunLoopReturnsStatementError`, `TestRunLoopCancelIsRetryable`, `TestRunLoopStopReturnsErrStopped`, `TestRunLoopPauseThenResumeToCompletion`, `TestRunLoopPausesRepeatedly`, `TestRunLoopReliefErrorPropagates`. Example — `TestRunLoopCompletes` becomes:

```go
	if err := runLoop("REBUILD", s.runStatement, s.waitForRelief, s.resumeSQL, nil); err != nil {
```

- [ ] **Step 6: Run the paced-completion test and the existing tests to verify they pass**

Run: `go test -race ./internal/run/ -run TestRunLoop`
Expected: PASS (the new test and all 7 existing `runLoop` tests).

- [ ] **Step 7: Add `reissueFor` and wire it into `runOnce`**

In `internal/run/monitored_runner.go`, add the helper (place it just above `runOnce`):

```go
// reissueFor returns the paced re-issue closure for op, or nil when op is not paced.
// Only reorganize_index is paced: its re-issue resumes from SQL Server's persisted
// progress, so the uncapped loop converges. The closure narrates the re-issue and
// returns the original SQL, mirroring how resumeSQL narrates a Pause resume. check_db
// and update_statistics — the other cancel-safe ops — are intentionally not paced:
// their re-issue restarts from scratch, so bounded retry (fail-fast) is correct.
func reissueFor(op ddl.Operation, sql string, sink ReactionSink) func() (string, error) {
	if _, ok := op.(ddl.ReorganizeIndex); !ok {
		return nil
	}
	return func() (string, error) {
		sink(ReactionEvent{Kind: "resume", Detail: "pressure cleared — re-issuing REORGANIZE"})
		return sql, nil
	}
}
```

Then pass it as the new `runLoop` argument in `runOnce` (currently ends its `runLoop(...)` call at ~line 91):

```go
	return runLoop(
		sql,
		func(stmt string) (Action, error) { return r.runStatement(ctx, stmt, caps, sink) },
		func() error { return r.awaitRelief(ctx, caps.Ignore, sink) },
		func() (string, error) {
			sink(ReactionEvent{Kind: "resume", Detail: "pressure cleared"})
			return ddl.ResumableControlSQL(op, "RESUME")
		},
		reissueFor(op, sql, sink),
	)
```

- [ ] **Step 8: Write tests for the remaining paced behaviors and `reissueFor`**

Add to `internal/run/executor_test.go`:

```go
func TestRunLoopPacedLoopsUnbounded(t *testing.T) {
	// Many cancels then complete: the paced loop keeps re-issuing and never returns
	// ErrCancelled. Run only retries (and counts toward MaxRetries) when runOnce returns
	// ErrCancelled, so this runLoop-level property is exactly what makes a paced reorg
	// immune to MaxRetries.
	s := &stmtRun{
		actions: []Action{Cancel, Cancel, Cancel, Continue},
		errs:    []error{nil, nil, nil, nil},
	}
	if err := runLoop("REORGANIZE", s.runStatement, s.waitForRelief, s.resumeSQL, s.reissueSQL); err != nil {
		t.Fatalf("runLoop() = %v, want nil (unbounded re-issue, never ErrCancelled)", err)
	}
	if s.relief != 3 || s.reissued != 3 {
		t.Errorf("relief=%d reissued=%d, want 3 and 3", s.relief, s.reissued)
	}
}

func TestRunLoopPacedReliefErrorPropagates(t *testing.T) {
	// If relief cannot be reached (e.g. log-drain timeout), the paced branch returns
	// that error instead of looping forever.
	boom := errors.New("log did not drain")
	s := &stmtRun{actions: []Action{Cancel}, errs: []error{nil}, reliefErr: boom}
	if err := runLoop("REORGANIZE", s.runStatement, s.waitForRelief, s.resumeSQL, s.reissueSQL); !errors.Is(err, boom) {
		t.Errorf("runLoop() = %v, want %v", err, boom)
	}
	if s.reissued != 0 {
		t.Errorf("reissue called %d times, want 0 (relief failed first)", s.reissued)
	}
}

func TestRunLoopPacedStopWins(t *testing.T) {
	// A graceful stop after a paced re-issue ends the loop with ErrStopped.
	s := &stmtRun{actions: []Action{Cancel, Stop}, errs: []error{nil, nil}}
	if err := runLoop("REORGANIZE", s.runStatement, s.waitForRelief, s.resumeSQL, s.reissueSQL); !errors.Is(err, ErrStopped) {
		t.Errorf("runLoop() = %v, want ErrStopped", err)
	}
	if s.reissued != 1 {
		t.Errorf("reissue called %d times, want 1", s.reissued)
	}
}

func TestReissueForOnlyReorganize(t *testing.T) {
	var events []ReactionEvent
	sink := func(ev ReactionEvent) { events = append(events, ev) }

	// reorganize_index → non-nil closure that narrates and returns the original SQL.
	f := reissueFor(ddl.ReorganizeIndex{Schema: "dbo", Table: "T", Index: "IX"}, "REORG SQL", sink)
	if f == nil {
		t.Fatal("reissueFor(ReorganizeIndex) = nil, want non-nil")
	}
	got, err := f()
	if err != nil || got != "REORG SQL" {
		t.Errorf("reissue() = (%q, %v), want (\"REORG SQL\", nil)", got, err)
	}
	if len(events) != 1 || events[0].Kind != "resume" || !strings.Contains(events[0].Detail, "re-issuing REORGANIZE") {
		t.Errorf("emitted %+v, want one resume event mentioning re-issuing REORGANIZE", events)
	}

	// Other cancel-safe ops and a non-cancel-safe op → nil (not paced).
	for _, op := range []ddl.Operation{
		ddl.CheckDB{Database: "DB"},
		ddl.UpdateStatistics{Schema: "dbo", Table: "T"},
		ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "IX"},
	} {
		if reissueFor(op, "SQL", sink) != nil {
			t.Errorf("reissueFor(%T) != nil, want nil (only reorganize_index is paced)", op)
		}
	}
}
```

Note: `executor_test.go` already imports `errors`, `strings`, and the `ddl` package — no import changes needed.

- [ ] **Step 9 (optional): Run-level MaxRetries contract test**

`TestRunLoopPacedLoopsUnbounded` proves the paced path never returns `ErrCancelled` at
the `runLoop` layer, which is what makes it immune to `Run`'s `MaxRetries`. A direct
`MonitoredRunner.Run` test would lock that contract one layer up, but there is **no
existing `Executor`/`Sampler` fake for the OpRunner path** (the `runLoop` tests use the
pure `stmtRun` harness precisely to avoid it), so this requires new fakes: an `Executor`
whose `ExecDDL` blocks until its context is canceled, and a `Sampler` that reports
sustained `BlockingOthers` so `supervise` decides `Cancel` repeatedly. Add it only if you
judge the extra coverage worth the fake infrastructure; otherwise the `runLoop`-level
proof plus the architectural guarantee is sufficient. Skip with no loss of correctness.

- [ ] **Step 10: Run the full run-package test suite**

Run: `go test -race ./internal/run/`
Expected: PASS. Then `go build ./...` and `make vet` — both green.

- [ ] **Step 11: Commit**

```bash
git add internal/run/monitored_runner.go internal/run/executor_test.go
git commit -m "$(cat <<'EOF'
feat(run): pace reorganize_index yielding (cancel → relief → re-issue, uncapped)

A reorganize_index now waits for relief and re-issues the same REORGANIZE
(resuming from persisted progress) instead of returning ErrCancelled and
exhausting MaxRetries. Wired via a reissueFor closure that is non-nil only
for ddl.ReorganizeIndex; check_db/update_statistics keep bounded retry.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: `reorgRCSIWarning` pure decision helper

**Files:**
- Create: `internal/run/reorg_rcsi.go`
- Test: `internal/run/reorg_rcsi_test.go`

**Interfaces:**
- Consumes: `ddl.Operation`, `ddl.ReorganizeIndex` (fields `Schema`, `Table`).
- Produces: `reorgRCSIWarning(op ddl.Operation, database string, rcsi bool) (string, bool)` — returns the complete warning text and `true` only for a `ddl.ReorganizeIndex` when `rcsi` is false; `("", false)` otherwise.

- [ ] **Step 1: Write the failing test**

Create `internal/run/reorg_rcsi_test.go`:

```go
package run

import (
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

func TestReorgRCSIWarning(t *testing.T) {
	reorg := ddl.ReorganizeIndex{Schema: "dbo", Table: "MEASUREMENT", Index: "PK_MEASUREMENT"}

	// Reorg + RCSI off → warn, message carries schema.table and the database name.
	msg, ok := reorgRCSIWarning(reorg, "PRODDB", false)
	if !ok {
		t.Fatal("reorgRCSIWarning(reorg, off) ok = false, want true")
	}
	for _, want := range []string{"dbo.MEASUREMENT", "PRODDB", "RCSI is OFF"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}

	// Reorg + RCSI on → silent.
	if _, ok := reorgRCSIWarning(reorg, "PRODDB", true); ok {
		t.Error("reorgRCSIWarning(reorg, on) ok = true, want false")
	}

	// Non-reorg ops → silent regardless of RCSI.
	for _, op := range []ddl.Operation{
		ddl.CheckDB{Database: "DB"},
		ddl.UpdateStatistics{Schema: "dbo", Table: "T"},
		ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "IX"},
	} {
		if _, ok := reorgRCSIWarning(op, "DB", false); ok {
			t.Errorf("reorgRCSIWarning(%T, off) ok = true, want false (only reorganize_index warns)", op)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/run/ -run TestReorgRCSIWarning`
Expected: FAIL — `undefined: reorgRCSIWarning`.

- [ ] **Step 3: Write the helper**

Create `internal/run/reorg_rcsi.go`:

```go
package run

import (
	"fmt"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

// reorgRCSIWarning returns the warning to emit before running op, and whether to emit
// it: only for a REORGANIZE against a database with RCSI (READ_COMMITTED_SNAPSHOT) off,
// where readers take shared locks and block on the operation's short-term page X locks.
// The database name is passed in so the returned message is complete and the helper
// stays a pure, testable decision function. Returns ("", false) for any other operation
// or when RCSI is on.
func reorgRCSIWarning(op ddl.Operation, database string, rcsi bool) (string, bool) {
	if rcsi {
		return "", false
	}
	reorg, ok := op.(ddl.ReorganizeIndex)
	if !ok {
		return "", false
	}
	return fmt.Sprintf(
		"%s.%s: RCSI is OFF on %s — readers may block on this REORGANIZE's page locks; the pacing loop will still yield on blocking.",
		reorg.Schema, reorg.Table, database,
	), true
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test -race ./internal/run/ -run TestReorgRCSIWarning`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/run/reorg_rcsi.go internal/run/reorg_rcsi_test.go
git commit -m "$(cat <<'EOF'
feat(run): add reorgRCSIWarning decision helper

Pure helper returning the RCSI-off advisory for a reorganize_index only.
Wired into the engine in a follow-up commit.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: Wire RCSI into the engine and emit the warning

**Files:**
- Modify: `internal/run/engine.go` (`Engine` struct field ~line 169; `WithADR` ~line 212; `processOne` per-step `sink` ~line 600–633; `ReactionEvent.Kind` doc-comment in `reaction.go`)
- Modify: `cmd/sqlgopace/main.go` (engine options list ~line 476–488)
- Test: `internal/run/engine_test.go` (uses `setupEngine`/`fakeOpRunner` ~line 139–187)

**Interfaces:**
- Consumes: `reorgRCSIWarning` (Task 2); the per-step `sink`; `manifest.Database`; `e.database`; `step.Operation`.
- Produces: `WithRCSI(rcsi bool) EngineOption`; `Engine.rcsi bool` field.

- [ ] **Step 1: Write the failing engine test**

Add to `internal/run/engine_test.go`. It writes a reorg manifest, processes it with RCSI off, and asserts the warning is in the `.log`; a second run with RCSI on asserts it is absent.

```go
const reorgManifest = `
database: TESTDB
description: reorg rcsi test
operations:
  - operation: reorganize_index
    schema: dbo
    table: MEASUREMENT
    index: PK_MEASUREMENT
`

func TestProcessAllReorgWarnsWhenRCSIOff(t *testing.T) {
	runner := &fakeOpRunner{}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner, run.WithDatabase("TESTDB"), run.WithRCSI(false))
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "011_reorg.yaml"), []byte(reorgManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	logBytes, err := os.ReadFile(filepath.Join(dirs.Done, "011_reorg.yaml.log"))
	if err != nil {
		t.Fatalf("read run log: %v", err)
	}
	log := string(logBytes)
	for _, want := range []string{"reaction: warn", "RCSI is OFF on TESTDB", "dbo.MEASUREMENT"} {
		if !strings.Contains(log, want) {
			t.Errorf("run log missing %q\n--- log ---\n%s", want, log)
		}
	}
}

func TestProcessAllReorgSilentWhenRCSIOn(t *testing.T) {
	runner := &fakeOpRunner{}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner, run.WithDatabase("TESTDB"), run.WithRCSI(true))
	if err := os.WriteFile(filepath.Join(dirs.ToRun, "011_reorg.yaml"), []byte(reorgManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	logBytes, err := os.ReadFile(filepath.Join(dirs.Done, "011_reorg.yaml.log"))
	if err != nil {
		t.Fatalf("read run log: %v", err)
	}
	if strings.Contains(string(logBytes), "RCSI is OFF") {
		t.Errorf("run log should not warn when RCSI is on\n--- log ---\n%s", logBytes)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/run/ -run TestProcessAllReorg`
Expected: FAIL — `undefined: run.WithRCSI` (compile error).

- [ ] **Step 3: Add the `rcsi` field and `WithRCSI` option**

In `internal/run/engine.go`, add the field to the `Engine` struct next to `adr bool` (line 169):

```go
	rcsi             bool
```

Add the option next to `WithADR` (after line 212):

```go
// WithRCSI sets the connected database's READ_COMMITTED_SNAPSHOT state. Used to warn
// before a reorganize_index whose page locks would block readers when RCSI is off.
func WithRCSI(rcsi bool) EngineOption { return func(e *Engine) { e.rcsi = rcsi } }
```

- [ ] **Step 4: Emit the warning in `processOne`**

In `internal/run/engine.go`, emit the warning **only when the operation will actually
run** — i.e. after resumable-conflict handling, inside the `prepErr == nil` branch, at
the top of the `else {` block just before `switch op := step.Operation.(type) {` (line
677). This avoids warning about a reorg that `prepErr` prevents from running. Insert as
the first statement of that `else` block:

```go
			} else {
				// Advisory: a reorganize_index against an RCSI-off database blocks readers on
				// its page locks. Emitted through the sink so it lands in the run's .log and TUI.
				// manifest.Database is empty for a no-database manifest, which runs on the
				// engine's connected database (e.database). The helper self-gates to reorg only.
				db := manifest.Database
				if db == "" {
					db = e.database
				}
				if msg, ok := reorgRCSIWarning(step.Operation, db, e.rcsi); ok {
					sink(ReactionEvent{Kind: "warn", Detail: msg})
				}
				switch op := step.Operation.(type) {
				// ... existing Shrink / ShrinkTempdb / BatchDML / default branches, unchanged ...
```

- [ ] **Step 5: Run the engine tests to verify they pass**

Run: `go test -race ./internal/run/ -run TestProcessAll`
Expected: PASS (both new tests plus the existing `TestProcessAll*` suite).

- [ ] **Step 6: Wire `WithRCSI` in `main.go`**

In `cmd/sqlgopace/main.go`, in the `opts := []run.EngineOption{...}` list (the `run.WithADR(info.ADREnabled)` line ~477), add:

```go
		run.WithRCSI(info.RCSIEnabled),
```

- [ ] **Step 7: Update the two stale kind doc-comments**

(a) In `internal/run/reaction.go`, the `ReactionEvent.Kind` field comment currently reads `// "pause" | "resume" | "cancel" | "kill"`. Update it:

```go
	Kind   string // "pause" | "resume" | "cancel" | "kill" | "abort" | "warn" | "info"
```

(b) In `internal/report/report.go` (line 25), the `ReactionLine` doc-comment reads `// ReactionLine records one reaction taken while an operation ran (a pause,` / `// resume, cancel, or fallback kill), so the log shows how pressure was handled.`. Since `warn`/`info` events are recorded here too, update it:

```go
// ReactionLine records one reaction taken while an operation ran (pause, resume,
// cancel, fallback kill, abort, or an advisory warn/info), so the log shows how
// pressure was handled.
```

- [ ] **Step 8: Build, vet, and run the full run-package suite**

Run: `go build ./... && go test -race ./internal/run/ && make vet`
Expected: all green.

- [ ] **Step 9: Commit**

```bash
git add internal/run/engine.go internal/run/engine_test.go internal/run/reaction.go internal/report/report.go cmd/sqlgopace/main.go
git commit -m "$(cat <<'EOF'
feat(run): warn before a reorganize_index when RCSI is off

Wire the connected database's RCSI state into the engine (WithRCSI) and emit
an advisory warning through the per-step sink so it reaches the .log and TUI.
Advisory only — never blocks or skips. Also refresh the stale ReactionEvent.Kind
comment to include warn/info/abort.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: `IMPLICIT_TRANSACTIONS OFF` connection hardening

**Files:**
- Modify: `internal/mssql/conn.go` (`harden()` ~line 85–91)

**Interfaces:**
- Consumes: nothing new. Produces: no exported change.

- [ ] **Step 1: Extend the hardening statement**

In `internal/mssql/conn.go`, change the `harden()` statement constant (line 86) and its doc-comment to add `SET IMPLICIT_TRANSACTIONS OFF;`:

```go
// harden applies the safety session settings to the execution connection:
// XACT_ABORT ON, DEADLOCK_PRIORITY LOW (so the DDL is the deadlock victim rather than
// a user query), and IMPLICIT_TRANSACTIONS OFF (so a REORGANIZE releases its locks
// incrementally instead of holding them until an implicit transaction commits —
// defensive; go-mssqldb already defaults it off, but a server-level `user options`
// default could turn it on).
func (c *Conn) harden(ctx context.Context) error {
	const stmt = "SET XACT_ABORT ON; SET DEADLOCK_PRIORITY LOW; SET IMPLICIT_TRANSACTIONS OFF;"
	if _, err := c.exec.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("harden execution session: %w", err)
	}
	return nil
}
```

- [ ] **Step 2: Build and vet**

Run: `go build ./... && make vet`
Expected: green. (No unit test — `harden()` issues SQL; the integration/e2e suite exercises the execution connection. Per project convention unit tests need no database.)

- [ ] **Step 3: Commit**

```bash
git add internal/mssql/conn.go
git commit -m "$(cat <<'EOF'
feat(mssql): set IMPLICIT_TRANSACTIONS OFF when hardening the exec connection

Defensive: ensures a REORGANIZE (and every DDL) releases locks incrementally
rather than holding them until an implicit transaction commits. go-mssqldb
already defaults it off; this guards against a server-level user options default.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: Version bump and documentation

**Files:**
- Modify: `internal/version/VERSION`
- Modify: `docs/REORGANIZE.md`

**Interfaces:** none.

- [ ] **Step 1: Bump the version**

Set `internal/version/VERSION` to:

```
0.12.0
```

- [ ] **Step 2: Correct and extend `docs/REORGANIZE.md`**

First grep the file for leftover follow-up-driver language so none survives:
`grep -niE "follow-up|shrinkrunner|shrink driver's shape" docs/REORGANIZE.md`. Then, under
"How SqlGoPace generates REORGANIZE today", replace the sentence that floats a
ShrinkRunner-style follow-up driver ("turning it into a paced, cancel-and-reissue driver
is the natural follow-up, and would reuse the shrink driver's shape") with the shipped
decision, and add the RCSI warning and hardening. Suggested replacement paragraph:

```markdown
As of 0.12.0, SqlGoPace paces a reorganize_index directly in `MonitoredRunner`'s
`runLoop` (no separate driver): under blocking pressure it cancels the statement
(committed work preserved), waits for relief, and re-issues the same REORGANIZE —
which continues from SQL Server's persisted progress — looping uncapped until the
reorg completes or a graceful stop / log-drain timeout ends it. When RCSI is off on
the target database, it prints an advisory warning at reorg start (readers will block
on the page locks). The execution connection is also hardened with
`SET IMPLICIT_TRANSACTIONS OFF` so a reorg releases its locks incrementally.
```

- [ ] **Step 3: Add a one-line README note if there is a natural home**

`README.md` documents reorganize under the `shrink:` / maintenance sections (e.g. lines
~429, ~452, ~681) and describes the reaction hierarchy elsewhere. Grep for the reaction
hierarchy / maintenance-reaction description
(`grep -niE "wait_at_low_priority|resumable|reaction|blocking" README.md`); if there is a
section describing how operations react under blocking, add one line noting that a
`reorganize_index` paces by cancel-and-reissue (it cannot use `WAIT_AT_LOW_PRIORITY`/
`RESUMABLE`) and warns when RCSI is off. If there is no natural home, skip — the spec
made this conditional; do not force a note where it does not fit.

- [ ] **Step 4: Verify the build embeds the new version**

Run: `go build ./... && go test ./internal/version/`
Expected: green.

- [ ] **Step 5: Commit**

```bash
git add internal/version/VERSION docs/REORGANIZE.md README.md
git commit -m "$(cat <<'EOF'
docs(reorganize): document paced yielding + RCSI warning; bump to 0.12.0

Correct REORGANIZE.md to describe the shipped runLoop refinement (no new
driver), the RCSI-off advisory, and the IMPLICIT_TRANSACTIONS OFF hardening.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

## Execution notes

- **Branch first.** These commits land per task; start from a feature branch (e.g. `feat/reorg-pacing`), not `main`.
- **Commit only with approval.** The per-task `git commit` steps are the intended granularity, but committing/pushing happens only when the user asks (project policy overrides the plan's convention). If commits are deferred, still complete each task's red→green cycle so the working tree stays test-green between tasks.

## Final verification

- [ ] Run the whole suite: `go test -race ./...` (unit tests, no DB) — all pass.
- [ ] `go build ./...` and `make vet` — green.
- [ ] `make lint` may flag pre-existing CRLF/gofmt noise repo-wide; gate on build/vet/test, not a repo reformat (see project memory on CRLF vs gofmt).
- [ ] Confirm the spec's Status line can move to "implemented"; optionally update `docs/superpowers/specs/2026-07-31-reorg-pacing-rcsi-warning-design.md`.
