# FIXES — code review of the 12 unpushed commits (`origin/main..HEAD`)

> **Status:** review complete. **ALL correctness findings (#1–#9, #11–#14) plus #5/#7 are
> IMPLEMENTED, tested, and committed** on `main` (not pushed). #10 is resolved as by-design (see
> below). Commits:
> - `f71316a` — #4 (TUI join engine goroutine before closing the connection)
> - `edc2e98` — #2, #3 (plan-fingerprint validated resume cursor; `State.PlanFingerprint`)
> - `affd51c` — #1, #5, #7 (own paused resumable by recorded identity; `State.Paused`)
> - `6a1fcfe` — simplify pass (one `updateSidecar` helper)
> - `f53aba4` — #6, #8, #11 (skip doesn't orphan own resumable; watermark kept on failure; skip metric)
> - `d986d40` — #9, #14 (drain across databases; Ctrl+C can still force-quit)
> - `7f2caf9` — #12 (history migration by existence check, not error text)
> - `4f9b09c` — #13 (recovery sweeps stray atomic-write temp files)
> - `595a88f` — cleanup: `internal/fsutil.AtomicWrite` dedup + shared `recordInterrupted`
>
> **#10 (drain cancel can't un-pause a resumable) — resolved as BY-DESIGN, no code change.** The
> cancellable-drain contract is "cancel takes effect if it lands before the next check." Once a
> sampling poll observes the drain and pauses the resumable (attention sent, server paused), the
> pause is committed; a later Cancel cannot un-pause it. The op is finalized interrupted and, thanks
> to the #1 fix, RESUMEs correctly by recorded identity on the next run — no work lost, just deferred
> to the next run for that one manifest. Continuing in-place would require re-entering the op loop
> for a partially-run op (a risky restructure) for a narrow race window; not worth it.
>
> **Altitude/structural items intentionally NOT changed** (KISS; they refactor working
> infrastructure without fixing a defect, and churn the just-fixed resumable-switch code): `Stop`
> living in `Capabilities`; the two stop seams (`Capabilities.Stop` vs driver `stop` fields);
> `prepErr` nesting in `processOne`; `blockingResumable` re-building the ABORT SQL as an index-op
> probe; `skip_if_satisfied` (since removed; superseded by the per-operation `intent` field,
> `docs/specs/OPERATION-INTENT.md`) as a hardcoded RebuildIndex+compression case (vs a generic
> `Operation.Satisfied`). Left as future refinements.
>
> Note on #3: fixed conservatively via the plan fingerprint — a re-expanded `ALTER INDEX ALL`
> hashes differently, so the run restarts clean rather than skipping the wrong op. The deeper
> op-identity keying (redo only what changed) remains a future refinement.
>
> Written 2026-07-01.

## How to regenerate the review context in a new session

```bash
# The RTK hook rewrites `git diff` into a compressed format; use rtk proxy for the raw diff:
rtk proxy git diff origin/main...HEAD           # full unified diff, ~3540 lines, 45 files
git log --oneline origin/main..HEAD             # the 12 commits under review
```

Commit range under review: `a13b8ff` (step-sink) … `334bf50` (cancellable drain), 12 commits,
all on `main`, none pushed. Scope: +2269 / −157 across 45 files. `make test` / vet / gofmt /
golangci-lint were all green — every finding below is a **logic / edge-case** bug, not a
mechanical one.

**Working constraints (from CLAUDE.md + session memory):** commit directly to `main`, **never
push** unless explicitly asked; English-only for Go code/comments/identifiers (docs/specs/ may be
French); KISS/idiomatic Go; run `go test -race ./...`, `go vet ./...`, `gofmt -l`,
`golangci-lint run`, and `go vet -tags integration ./...` before committing; run a `/simplify`
pass over the diff before committing; the target is a **production** SQL Server; US spelling in
comments (misspell lint); trailer `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`.

---

# Part 1 — Findings (ranked, most severe first)

The crash-resume state machine has one structural soft spot: **it infers "is this paused
resumable mine?" and "which op am I resuming?" from a single positional cursor.** Findings #1,
#2, #3, #5, #6 are all faces of that. `[letters]` = the finder angles that converged.

## Must-fix (correctness)

### #1 — `internal/run/engine.go:493-499` — a manifest's OWN paused resumable is misclassified as foreign. `[A,B,C,E,altitude]`
The RESUME case is gated `resumed && i == resumeFrom && caps.Resumable`. That inference breaks:
- **Frozen cursor (`on_failure: continue`):** op0 fails-continue → `advanceCursor` never fires
  for it → cursor freezes at 0. A later resumable op (say op5) is interrupted and left PAUSED →
  `finalizeInterrupted` persists `ResumeFromOp=0`. Re-run: at i=5, `resumed && i==resumeFrom` is
  `5==0` = false, so RESUME is skipped; op5's own paused rebuild is caught by
  `blockingResumable`. abort **off** → op **fails** ("a paused resumable blocks this rebuild"),
  orphaning server-side progress. abort **on** → `clearOrRejectBlockingResumable` issues
  `ALTER INDEX … ABORT`, **destroying** the very progress crash-resume exists to preserve.
- **`caps.Resumable` flips false between runs** (edition failover to a non-resumable tier, policy
  or manifest edit): same fall-through, same wrongful rejection of its own work.

Root: the engine cannot distinguish its own paused resumable from a foreign one; cursor position
+ a bare `PausedResumable` probe is insufficient.

### #2 — `internal/run/engine.go:424,957` — unvalidated resume cursor → silent false SUCCESS. `[B; A/E triggers]`
`writeSidecar` inherits `st.ResumeFromOp` verbatim (no clamp, no identity check). If the resumed
plan is **shorter** than the stored cursor — manifest edited to fewer ops, `ALTER INDEX ALL` set
shrank, or a stale same-name sidecar left by a prior manifest — then `if i < resumeFrom` (line
424) skips **every** operation, `finalizeAll` sees no failed ops, and the manifest is reported
**SUCCESS and moved to `03.done/` having executed nothing.** Silent no-op success on prod is the
worst outcome in the review.

### #3 — `internal/run/engine.go:424` — positional cursor over a per-run-regenerated plan skips the WRONG op. `[A,E]`
The cursor and the `.wm` watermark store are keyed by expanded-plan **index**, but `ExpandAll`
regenerates the op list each run. If a table's index set changes between an interrupted run and
its resume, `resumeFrom=3` now points at a different op: an un-rebuilt index is silently skipped
as "already done," and `watermarkStore(name, i)` applies a stale `.wm` to the wrong op.

### #4 — `cmd/sqlgopace/main.go:556` (`runWithTUI`) — use-after-close race on the DB connection. `[D, verified inline]`
```go
go func() { summary, err := engine.ProcessAll(ctx); done <- result{summary, err}; program.Quit() }()
if err := program.Run(); err != nil { return run.Summary{}, err }   // <-- early return, no <-done
r := <-done
```
On `program.Run()` error it returns **without** `<-done`, leaving `engine.ProcessAll(ctx)`
running; the caller then hits `_ = dbConn.Close()` (main.go:311) under an in-flight statement →
use-after-close / protocol race on `*mssql.Conn`. The drain feature widens the window (the engine
keeps running after the 1st interrupt).

## Should-fix

### #5 — `internal/run/engine.go:493` — the opposite hole: a FOREIGN paused resumable is ADOPTED. `[altitude]`
When a manifest drains at an op boundary (the op never ran) and a foreign paused resumable happens
to sit on the next op's index, `resumed && i==resumeFrom && caps.Resumable` matches and issues
RESUME on it — completing a rebuild with **unknown options** and marking success. Same root as #1;
**the #1 fix (record identity of our own paused op) fixes this too.**

### #6 — `internal/run/engine.go:431` — the compression skip runs before the resumable switch, orphaning a paused resumable. `[A]`
If compression already reads satisfied while a PAUSED resumable still sits on the index,
`skipSatisfied` returns true, the op is recorded skipped and `advanceCursor` moves past it — the
paused resumable is neither RESUMEd nor ABORTed → a later rebuild fails with Msg 10637.

> The manifest-level flag this was originally defined against (`skip_if_satisfied`) has since
> been removed; the skip is now gated per-operation by `intent: compression`
> (`docs/specs/OPERATION-INTENT.md`). The orphaning hazard described above is unchanged — it lives in
> the ordering of `skipSatisfied` relative to the resumable switch, not in what triggers the skip.

### #7 — `internal/run/engine.go:549` — audit log records the wrong statement. `[C, verified inline]`
When the boundary op runs `ALTER INDEX … RESUME`, `opRep.SQL = step.SQL` still logs the full
REBUILD text; `stmt` (the actually-executed RESUME) is discarded from the report. The `.log` /
history misrepresents what ran on prod. **Trivially fixed alongside #1** (`opRep.SQL = stmt`).

### #8 — `internal/run/engine.go:529` — `key_range` watermark cleared on any non-`ErrStopped` return. `[A,D,E]`
A 2nd-Ctrl+C hard stop (`context.Canceled`) or a transient mid-walk error clears the `.wm` even
though committed batches remain → next run re-walks from key 0. Idempotent-safe but wasteful, and
inconsistent with the "a crash preserves the watermark" design. Fix: clear only on `runErr == nil`.

## Nits (low)

- **#9 — `main.go:275`** — the multi-DB outer loop ignores the shared drain; remaining databases
  still open connections / flash TUI consoles after a stop. `[C]`
- **#10 — `executor.go:188`** — a drain Cancel can't un-pause a resumable once `supervise` returned
  Stop; that manifest is finalized interrupted despite the operator cancelling. Documented tradeoff,
  but surprising. `[B]`
- **#11 — `report/history.go:22`** — `RunRecord.Skipped` (originally documented as
  *skip_if_satisfied*, now the intent-compression skip) also counts resume-cursor "already done"
  skips, polluting the metric each resume cycle. `[altitude]` The field's population change (the
  flag is gone; the count now reflects `intent: compression` skips) is described in
  `docs/specs/OPERATION-INTENT.md` §6; this open item about resume-cursor pollution is unaffected and
  remains open.
- **#12 — `report/history.go:~90`** — additive migration detected via
  `strings.Contains(err, "duplicate column")`; a driver reword/localization breaks it and history
  fails to open on every later start. Prefer `PRAGMA user_version`. `[D, efficiency]`
- **#13 — `state.go:46`** — `atomicWriteFile` leaks a hidden `.sqlgopace-*.tmp` on a crash mid-write;
  recovery never sweeps them, amplified by the new per-op cursor writes + `.wm`. `[D]`
- **#14 — `main.go:263`** — after the 2nd Ctrl+C the signal goroutine exits; a hung run can no
  longer be force-killed by a further Ctrl+C. `[D]`

## Cleanup (no bugs — from reuse/simplification/efficiency/altitude)

- **Duplication:** `run/state.go:45` `atomicWriteFile` is a verbatim twin of `ddl/edit.go:82`
  (different packages → extract a shared helper, e.g. `internal/fsutil`). `finalizeDrained` is a
  near-copy of `finalizeInterrupted`. The ABORT SQL is built twice (`blockingResumable` discards it
  as a type-probe, then `clearOrRejectBlockingResumable` rebuilds it).
- **Efficiency:** `saveResumeCursor` does a full read+unmarshal+atomic-rewrite of the sidecar after
  *every* op (churn on `ALTER INDEX ALL`) — keep the `State` in a `processOne` local and write it
  directly. `blockingResumable` runs a `PausedResumable` DMV round-trip per index op on the hot path.
- **Altitude:** two parallel stop seams (`Capabilities.Stop` in `supervise` vs driver `stop` fields
  polled between chunks, checked at 4 copy-pasted sites); `Stop` is a live predicate smuggled into
  the otherwise-static `Capabilities`; `skip_if_satisfied` (since removed; the skip is now gated
  by the per-operation `intent` field, `docs/specs/OPERATION-INTENT.md`) was a generic flag backed by
  one hardcoded `RebuildIndex`+compression case (consider
  `Operation.Satisfied(ctx, readers) (bool, reason)`).

---

# Part 2 — Implementation plan for #1–#4

Implement in this order (each is independently committable): **#4 (isolated, small) → #2 → #3
(shares #2's mechanism) → #1 (largest, touches the sidecar + the switch)**, then fold in **#7** with
#1 for free. Rationale: #4 is orthogonal; #2/#3 share the cursor-validation mechanism; #1 is the
deepest and benefits from #2's sidecar plumbing being in place.

Key files: `internal/run/engine.go` (processOne ~345-598, writeSidecar ~956, advanceCursor ~991,
saveResumeCursor ~1001, the resumable helpers ~843-896), `internal/run/state.go` (the `State`
struct), `cmd/sqlgopace/main.go` (`runWithTUI` ~535-561).

## Fix #4 — TUI use-after-close (smallest, do first)

**File:** `cmd/sqlgopace/main.go`, `runWithTUI` (~535-561).

**Change:** derive a cancelable context for the engine and always drain `done` before returning.
```go
func runWithTUI(ctx context.Context, ...) (run.Summary, error) {
    ...
    engineCtx, cancelEngine := context.WithCancel(ctx)
    defer cancelEngine()
    done := make(chan result, 1)
    go func() {
        summary, err := engine.ProcessAll(engineCtx)   // was ctx
        done <- result{summary, err}
        program.Quit()
    }()

    runErr := program.Run()
    if runErr != nil {
        cancelEngine()          // TUI died first: unwind ProcessAll so it stops touching the conn
    }
    r := <-done                 // ALWAYS wait for the engine goroutine before returning
    if runErr != nil {
        return run.Summary{}, runErr
    }
    return r.summary, r.err
}
```
**Why it works:** the caller closes `dbConn` right after `runWithTUI` returns (main.go:311); waiting
for `<-done` guarantees `ProcessAll` has fully returned first. On the error path, `cancelEngine()`
cancels the exec contexts inside the monitored runner so `ProcessAll` unwinds promptly.

**Caveat / gotcha:** cancelling `engineCtx` mid-DDL is a hard cancel, but this only happens when the
TUI already errored (unmonitorable run), so it's acceptable. A statement that ignores context could
still delay `<-done` — that is the existing no-query-timeout model, not new.

**Test (`cmd/sqlgopace/step_stdout_test.go` neighborhood, or a new `main_tui_test.go`):** hard to
unit-test `program.Run()`; instead add a focused test around the goroutine-join contract if the code
is refactored so the join is a testable helper. At minimum, a manual note in the PR body. Do **not**
regress: the normal path (program.Quit from the goroutine) must still return `r.summary, r.err`.

## Fix #2 + #3 — validate/bind the resume cursor (shared mechanism)

**Idea:** stop trusting a bare integer. Bind the cursor to the **content of the plan** it was
recorded against. On resume, if the current plan doesn't match, **restart clean** (cursor→0) rather
than skip-all or skip-the-wrong-op. This fixes both the "shorter plan → false success" (#2) and the
"reindexed ALL → wrong op" (#3) cases, conservatively (redo idempotent work rather than silently
skip real work).

**Step 2a — extend `State` (`internal/run/state.go`).**
```go
type State struct {
    ... existing fields ...
    ResumeFromOp int    `json:"resume_from_op,omitempty"`
    // PlanFingerprint identifies the plan the resume cursor was recorded against: a hash over the
    // ordered (CommandType, Target) of every planned operation. A resumed run whose current plan
    // hashes differently ignores the stale cursor and restarts from the first operation, so a
    // shortened/reordered/re-expanded manifest is never silently skipped.
    PlanFingerprint string `json:"plan_fingerprint,omitempty"`
}
```

**Step 2b — a pure fingerprint helper (`engine.go` or a small `resume.go`).**
```go
// planFingerprint hashes the ordered identity of the planned operations, so a resumed run can
// detect that the plan changed since the cursor was recorded (edit, re-expansion of ALTER INDEX
// ALL, or a stale same-name sidecar) and restart cleanly instead of skipping operations.
func planFingerprint(planned []ddl.PlannedOperation) string {
    h := sha256.New()
    for _, s := range planned {
        fmt.Fprintf(h, "%s\x00%s\n", s.Operation.CommandType(), opTarget(s.Operation))
    }
    return hex.EncodeToString(h.Sum(nil))
}
```
(Imports: `crypto/sha256`, `encoding/hex`. `opTarget` already exists and yields `schema.table[.name]`.)

**Step 2c — validate after planning, in `processOne`.** The cursor is read at line 360 but the plan
is only known after `ddl.Plan(...)` at ~389. Insert validation right after `planned` is computed
(after line 393), BEFORE the op loop:
```go
fp := planFingerprint(planned)
if resumeFrom > 0 {
    switch {
    case resumeFrom > len(planned):
        fallthrough
    case resumed && st.PlanFingerprint != "" && st.PlanFingerprint != fp:
        fmt.Fprintf(e.out, "-- resume cursor no longer matches the plan (had %d ops, cursor %d); restarting clean\n", len(planned), resumeFrom)
        resumeFrom = 0
        cursor = 0
        e.clearWatermarks(name)     // sweep stale name.op*.wm so a fresh walk doesn't read them
    }
}
// (re)bind the fingerprint for this run's cursor writes
e.saveResumeState(name, cursor, fp)   // helper that persists ResumeFromOp + PlanFingerprint
```
Notes:
- `cursor` is declared at line 408 (`cursor := resumeFrom`); move its declaration above this block,
  or set both `resumeFrom` and `cursor` here. Keep them consistent.
- Needs the **full `State`** (for `st.PlanFingerprint`), so change `writeSidecar` to return it — see
  Step 2d.

**Step 2d — surface the full `State` from `writeSidecar` (`engine.go:956`).** Single caller
(engine.go:360), so this is a localized signature change:
```go
func (e *Engine) writeSidecar(ctx context.Context, name string) (st State, resumeFrom int, resumed bool) {
    if s, err := ReadState(e.sidecarPath(name)); err == nil {
        st, resumeFrom, resumed = s, s.ResumeFromOp, true
    }
    ... // when writing fresh state, carry PlanFingerprint="" (set later, after planning)
    return st, resumeFrom, resumed
}
```
Caller: `st, resumeFrom, resumed := e.writeSidecar(ctx, name)`.

**Step 2e — persist the fingerprint alongside the cursor.** Generalize `saveResumeCursor` (or add
`saveResumeState`) to also write `PlanFingerprint`, and have `advanceCursor` keep using it. Cheapest:
add `PlanFingerprint` to the in-place update. (This also sets up the "keep State in a local instead
of re-reading" efficiency cleanup — optional.)

**Step 2f — `clearWatermarks` helper.** Sweep `filepath.Join(e.dirs.Processing, name+".op*.wm")`
via `filepath.Glob` + `os.Remove`. Only needed on a fingerprint-mismatch reset (a clean restart must
not read a stale key_range watermark keyed by a now-different op index).

**Edge cases / gotchas:**
- Fresh run (`!resumed`, no sidecar): `st.PlanFingerprint==""`, skip the mismatch branch, just bind
  the fingerprint. No behavior change.
- Legacy sidecar written by the current code (no fingerprint): `st.PlanFingerprint==""` → the
  mismatch branch is skipped (can't compare), but the `resumeFrom > len(planned)` clamp still
  protects against the worst case (#2's false-success). Acceptable migration behavior.
- A clean restart re-runs already-completed idempotent ops (rebuild/reorg/compression/checkdb are
  idempotent; literal batch_update/delete are idempotent; key_range walks restart from 0). This is
  the deliberate conservative trade vs. silently skipping real work.
- **Deeper future refinement (out of scope for this fix):** key the cursor and `.wm` by a **stable op
  identity** (e.g. `CommandType + Target`) instead of ordinal, so a re-expansion only redoes the ops
  that actually changed rather than restarting. Note it in the spec; don't build it now (KISS).

**Tests (`internal/run/resume_test.go` / `engine_test.go`, no DB):**
- `TestResumeCursorBeyondPlanRestartsClean`: seed a sidecar with `ResumeFromOp=5` for a 2-op
  manifest → assert both ops run, manifest `done`, not a false success.
- `TestResumeFingerprintMismatchRestarts`: seed `ResumeFromOp=1, PlanFingerprint="stale"` → assert
  op0 re-runs (not skipped).
- `TestResumeFingerprintMatchSkipsPrefix`: seed with the **correct** fingerprint + `ResumeFromOp=1`
  → assert op0 is skipped ("already done") and op1 runs (guards against over-correction / regressing
  the existing resume path — see existing `resume_test.go` cases).
- `TestFingerprintMismatchSweepsWatermarks`: create a `name.op0.wm`, force a mismatch, assert the
  `.wm` is gone after the reset.

## Fix #1 (+ #5, #7) — record the identity of the manifest's own paused resumable

**Idea:** when the engine leaves an op with a paused resumable, **record that fact** (op index +
target index identity) in the sidecar. On resume, RESUME iff the recorded identity matches the op at
hand — regardless of cursor position or the current `caps.Resumable`. A paused resumable that is NOT
recorded as ours is foreign → the existing reject/abort path. This fixes the frozen-cursor rejection
(#1), the caps-flip rejection (#1), AND the foreign-adoption hole (#5).

**Step 1a — extend `State` (`internal/run/state.go`).**
```go
// PausedResumable identifies an operation the engine left with a paused resumable rebuild on the
// server, so a resumed run continues exactly that operation via ALTER INDEX … RESUME instead of
// inferring ownership from the cursor position. Nil when nothing was left paused. Op is the plan
// index; Schema/Table/Index are the target of the paused rebuild.
type PausedResumable struct {
    Op     int    `json:"op"`
    Schema string `json:"schema"`
    Table  string `json:"table"`
    Index  string `json:"index"`
}
// in State:
    Paused *PausedResumable `json:"paused,omitempty"`
```

**Step 1b — record it when finalizing an interrupted resumable.** In `processOne`, the interrupted
branch is ~line 569:
```go
stopped := errors.Is(runErr, ErrStopped)
if stopped || (prepErr == nil && caps.Resumable && e.resumableInterruption(ctx, step.Operation)) {
    // NEW: only for a resumable INDEX op (not shrink/batch ErrStopped, which resume differently).
    if caps.Resumable {
        ref := step.Operation.Target()
        e.recordPausedResumable(name, PausedResumable{Op: i, Schema: ref.Schema, Table: ref.Table, Index: ref.Name})
    }
    ... existing finalizeInterrupted path ...
}
```
`caps.Resumable` is true only for a resumable index rebuild; it is false for shrink and batch-DML
drivers (whose `ErrStopped` also lands here) — so this correctly records ONLY index rebuilds.
`recordPausedResumable` is a read-modify-write of the sidecar (mirror `saveResumeCursor`).

Gotcha: on an **inconclusive** `resumableInterruption` (server unreachable, `!conclusive`), we may
record a Paused that isn't actually paused on the server. That's harmless: on re-run `resumeStatement`
re-probes `PausedResumable`; if nothing is paused it returns `ok=false` and the op runs a fresh
REBUILD (clean restart). Recording is safe either way.

**Step 1c — rewrite the pre-run switch (`engine.go:490-500`).**
```go
stmt := step.SQL
var prepErr error
ref := step.Operation.Target()
ownPaused := st.Paused != nil && st.Paused.Op == i &&
    strings.EqualFold(st.Paused.Schema, ref.Schema) &&
    strings.EqualFold(st.Paused.Table, ref.Table) &&
    strings.EqualFold(st.Paused.Index, ref.Name)
switch {
case ownPaused:
    // Our own paused resumable from a previous run — continue it whatever the current resolve
    // says about resumability (RESUME finishes/aborts server-side state; a fresh REBUILD is
    // rejected with Msg 10637 while it is paused).
    if resume, ok := e.resumeStatement(ctx, step.Operation); ok {
        stmt = resume
        sink(ReactionEvent{Kind: "resume", Detail: "continuing paused resumable rebuild (server-side progress kept)"})
    }
    // else: nothing actually paused now → run the planned REBUILD (clean restart).
case e.blockingResumable(ctx, step.Operation):
    // A paused resumable we did NOT record blocks a fresh REBUILD (Msg 10637): reject, or ABORT
    // when the manifest opts in.
    prepErr = e.clearOrRejectBlockingResumable(ctx, step.Operation, manifest.AbortBlockingResumable)
}
```
This **drops** the `resumed && i == resumeFrom && caps.Resumable` gate in favor of the recorded
identity match. `st` comes from the `writeSidecar` change in Fix #2 (Step 2d) — so **Fix #2 must land
first**, or add the `st` return as part of #1.

**Step 1d — clear the record on success (`engine.go:592-595`, success path).**
```go
opRep.Outcome = "success"
if st.Paused != nil && st.Paused.Op == i {
    e.clearPausedResumable(name)   // read-modify-write: set Paused=nil
    st.Paused = nil                // keep the local in sync for later iterations
}
e.emitStep(stepEv.finished("success", opDuration(opRep)))
...
```
Not strictly required (only one op is recorded per run, and `finalize` removes the sidecar on
completion), but it prevents a stale `Paused` from a resumed-and-completed op lingering if a *later*
op in the same run is interrupted as a non-resumable (shrink/batch) `ErrStopped`. Cheap insurance.

**Step 1e — Fix #7 for free.** In the `ownPaused` branch, after `stmt = resume`, the op report
still records `step.SQL`. Set `opRep.SQL = stmt` when building `opRep` (line 549) — capture the
executed `stmt`, not `step.SQL`, so the `.log` shows the RESUME that actually ran. (`opRep` is built
at 545-558; either move the `stmt` decision above it and use `SQL: stmt`, or assign `opRep.SQL = stmt`
right after the switch.)

**Interaction with Fix #6 (not in this batch but note it):** `skipSatisfied` runs at line 431,
BEFORE this switch, so a skipped op still never RESUMEs/ABORTs a paused resumable. If #6 is also
fixed later, reorder so a recorded `ownPaused` op is not silently skipped while paused. For the #1
batch, leave #6 as a documented follow-up.

**Tests (`internal/run/resume_test.go`, no DB; extend the existing fakes):**
- `TestOwnPausedResumedAfterContinueOnFailureGap` (the #1 headline): manifest `on_failure: continue`,
  op0 fails-continue, op1 resumable interrupted → sidecar has `ResumeFromOp=0` + `Paused{Op:1,...}`.
  Re-run with a fake `resumeCheck` reporting op1's index PAUSED → assert op1 issues `ALTER INDEX …
  RESUME` (assert the captured SQL contains `RESUME`, not a fresh `REBUILD`), NOT a failure.
- `TestOwnPausedResumedWhenResumableFlippedOff`: same but `caps.Resumable=false` on re-run → still
  RESUMEs (identity match ignores caps.Resumable).
- `TestForeignPausedNotAdoptedOnDrainBoundary` (#5): sidecar has NO `Paused` record (drained at
  boundary), fake reports the next op's index PAUSED → assert the op does NOT RESUME; with abort off
  it fails with the "blocks this rebuild" message (reuse/rename existing
  `TestBlockingResumableWithoutOptInFails`).
- `TestOwnPausedRecordedOnInterruption`: interrupt a resumable op, assert the sidecar now carries
  `Paused{Op:i, Schema, Table, Index}`.
- `TestResumeStatementLogsResumeSQL` (#7): on a resumed boundary op, assert `opRep.SQL` / the `.log`
  contains the RESUME statement, not the REBUILD.
- Regression: keep `TestResumeAdoptsPausedResumableAtBoundary` passing (adjust it to seed the new
  `Paused` record instead of relying on `i==resumeFrom`), and the abort-opt-in tests.

**Existing test fakes to reuse:** `sqlCapturingRunner`, `cursorProbeRunner`, `fakeAborter`,
`fakeResumeCheck` (pointer receiver, `becomesPaused`/`calls`), `readFailLog`, `abortOptInManifest`,
`writeSidecarState` (extend `run.State` seeds with `Paused`/`PlanFingerprint`).

## Cross-cutting notes for all four fixes

- **State is exported and JSON-`omitempty`** → adding `PlanFingerprint` and `Paused` is
  backward-compatible; old sidecars simply lack them (handled: empty fingerprint skips the mismatch
  branch, nil `Paused` means "no own paused resumable recorded").
- **Two sidecar read-modify-writes now exist** (`saveResumeCursor`, plus new `recordPausedResumable`
  / `clearPausedResumable`). Consider the efficiency cleanup (keep `State` in a `processOne` local,
  write directly) while here — but that's optional and can be a separate `/simplify` commit.
- **`writeSidecar` signature change** ripples only to engine.go:360 (single caller) — grep-verified.
- Run after each fix: `go test -race ./...`, `go vet ./...`, `gofmt -l .`, `golangci-lint run`,
  `go vet -tags integration ./...`. Then `/simplify` over the diff before committing.
- Suggested commit messages (one per fix, on `main`, do NOT push):
  - `fix(run): join the engine goroutine before closing the connection in TUI mode (#4)`
  - `fix(run): validate the resume cursor against the plan fingerprint — no false success (#2,#3)`
  - `fix(run): resume a manifest's own paused resumable by recorded identity, not cursor position (#1,#5,#7)`

## Update these when done
- `docs/specs/crash-resumable.md` and `docs/specs/graceful-stop.md`: note the identity-based RESUME and the
  fingerprint-validated cursor (both currently claim the positional guard is safe — it is not).
- Memory `crash-resume-alter-index.md`: correct the "boundary op (i==resumeFrom)" description to the
  recorded-identity model.
