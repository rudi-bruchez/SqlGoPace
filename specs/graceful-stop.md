# Feature spec — Graceful stop (drain) after the current statement

> **Status: v1 implemented, including pausing an in-flight resumable.** The engine exposes `WithDrainSignal(<-chan
> struct{})`; once the channel is **closed** (latched signal), it stops **before the next op**
> (`finalizeDrained`: manifest left in `02.processing/`, counted as `Interrupted`) and does not start any of the
> remaining manifests. **New (§3.2): an IN-FLIGHT resumable operation is paused immediately**
> instead of waiting for it to finish — the drain signal is passed to the `MonitoredRunner` via `Capabilities.Stop`;
> `supervise` returns a **`Stop`** action for a resumable op when the channel closes; `runLoop`
> translates `Stop` into the **`ErrStopped`** sentinel (the existing pause gesture = canceling the execution
> context — *note*: that cancellation is what pauses the resumable — after which we **do not resume**); the engine classifies `ErrStopped`
> as a clean interruption (`finalizeInterrupted`, manifest+sidecar left in place), and the next run **RESUMEs**
> (see `crash-resumable.md` §4.2). A **non-resumable** op ignores the stop (it finishes; the drain stops at
> the next op boundary). Shrink/batch: out of scope (separate drivers). Triggers: **Ctrl+C 1× =
> drain / 2× = hard stop** (`os/signal` handler in `cmd/sqlgopace/main.go`, `sync.Once` to close the
> channel once, 2nd signal → `cancelRun()`), and the TUI action **`d`** (`ActionDrain` → status `DRAINING`).
> Resume: the **operation cursor** `State.ResumeFromOp` (§3.3.1) is written by `finalizeDrained`
> (= number of ops completed); recovery **keeps** the sidecar when requeuing if the cursor is > 0
> (`requeue(..., keepCursor)` + `queue.InToRun` to tolerate a manifest that has already been requeued); on the re-run
> `writeSidecar` **preserves** the cursor and returns it, and the loop **skips** ops `i < cursor`
> (outcome `skipped`, reason "already done in a previous run") — via the shared helper
> `recordSkipped` (shared with the intent-based compression skip, `specs/OPERATION-INTENT.md`).
> The cursor is now **also written
> incrementally per operation** (`advanceCursor`, `crash-resumable.md` §6), not only on drain:
> a *crash* therefore populates the cursor just like a drain, and `WriteState` is made **atomic**
> (temp+rename) since the sidecar is rewritten after every op. The **metadata skip** (`crash-resumable.md`
> §9) remains complementary (it makes the compression prefix cheap). **Per-chunk stop (§3.2) is
> implemented for shrink and batch-DML too**: these chunked drivers receive the signal via
> `WithShrinkStop`/`WithBatchDMLStop` (wired in `buildEngine`) and check `stopRequested(r.stop)`
> **between chunks/batches** (each one is committed); they finish the current chunk then return
> `ErrStopped` with partial results — the engine finalizes as interrupted (left in processing), and the
> next run picks up where it left off (shrink is idempotent by free space; the predicate is self-limiting; key_range comes from
> the watermark, which the engine **does not purge** on `ErrStopped`). **Cancelling a drain (§6) is
> implemented**: the latched signal (closed channel) is replaced by a `DrainFlag` (atomic bool,
> `Request`/`Cancel`/`Draining`); every stop point becomes a **`func() bool`** predicate
> (`Capabilities.Stop`, `engine.drain`, the drivers' `stop` fields) checked **at boundaries** (op, chunk) and
> on `supervise`'s **sampling tick** — so a `Cancel` before the next check withdraws the
> request. The TUI **`d`** key **toggles** (drain ⇄ cancel, via `dispatchActions` on the shared flag);
> Ctrl+C stays **one-way** (2× = hard stop), cancellation being a TUI feature. Trade-off: the
> mid-statement pause of a resumable is now **poll-delayed** (instead of waking immediately) so that it
> can be cancelled. Created on 2026-06-17, out of the need to stop a run **without aborting the operation in
> progress**. This spec is now **fully implemented**.

## 1. Goal

Provide a command (TUI, and ideally Ctrl+C) that **stops processing at the end of the
current statement/operation**, rather than interrupting it abruptly:

- the **current operation runs to completion** (no rollback, no work lost);
- the engine **does not start the next operation**;
- it **records the resume point** so that the next run **continues at the next op**,
  not from the beginning.

This is the missing link between the two existing levers: `pause` (suspends *in the middle* of a
resumable statement) and `kill` (aborts the statement, rollback). The **drain** sits *between*
two operations.

## 2. Current state observed

- **Execution loop**: `for i, step := range planned` (`internal/run/engine.go:286`); the engine
  knows `i` and `len(planned)` and starts each op with no negotiable stop point in between.
- **Existing TUI levers**: `kill DDL`, `kill blocker`, `pause`, `extend`
  (`internal/tui/model.go` ~95-113; on-screen help at `model.go:244-245`). **No drain.**
- **No signal handler**: Ctrl+C kills the process outright (see `crash-resumable.md §3.1`).
- **No operation cursor** in the persistent state (`internal/run/state.go:12-20`).
- **Recovery = replay from the beginning**: a manifest left in `02.processing/` is requeued
  and restarted at op 1 (`internal/run/recovery.go:174-184`).
- **Counter already present**: `Summary.Interrupted` + the message "interrupted manifest(s) left in
  processing; the next run will resume them" (`cmd/sqlgopace/main.go:283`) — a drained manifest
  fits there naturally.

So today: stopping means aborting the current op (offline rollback) **and** redoing everything on the next
run. The drain removes both losses.

## 3. Proposed design

### 3.1 Triggers

- **TUI `drain` action**: a new key (e.g. `d`) that sets a "stop after current
  op" flag. The engine checks it **at the top of the loop**, before starting the next op
  (`engine.go:286`). Display: status `DRAINING — will stop after op 31/74`.
- **Ctrl+C = drain (recommended)**: finally install a signal handler (see `crash-resumable.md
  §3.1`) with the semantics **1× = drain** (clean stop after the current op), **2× = hard stop**
  (immediate cancellation, the current behavior). Standard UX, and safe by default.

### 3.2 Granularity

- Normal case (`rebuild_index`, etc.): we stop **after the current operation** (one step).
- **Shrink**: one step = a multi-chunk loop; the driver already samples between chunks
  (`ShrinkRunner`). The drain translates there to "stop **after the current chunk**" — the replay
  restarts from the current free space (already idempotent).

### 3.3 Where to record the resume point

Two options; **do not mutate the manifest** (it is the user's declarative input; rewriting the
YAML is risky and lossy):

1. **Extend the `State` sidecar** (recommended) — it already exists, lives next to the manifest in
   `02.processing/`, and is **already read by recovery**. Add a **cursor**:
   `ResumeFromOp int` (next op to execute) + `CompletedOps int` + `Reason` ("drain requested at
   op 31/74 on ..."). This is exactly the "operation cursor" mentioned in `crash-resumable.md
   §6`, here with an **intentional** trigger (not a crash).
2. **A dedicated flight-control file** (the alternative you suggested): a
   `<manifest>.flight.json` alongside. More explicit/readable on its own, but it **duplicates** the role of the
   `State` sidecar and adds a file to manage in the queue lifecycle. Worth keeping only if we
   want a resume format independent of `State`.

→ Recommendation: **reuse `State`** (less surface, already wired into recovery), unless there is an
explicit need for a separate artifact.

### 3.4 Resume

Recovery (`recovery.go`) honors the cursor: if `ResumeFromOp` is set, **continue at that
op** instead of replaying from the beginning. It combines with the **metadata skip**
(`crash-resumable.md §9`): even if the cursor were missing, the ops already done would be skipped.

### 3.5 New terminal state

A drained manifest **stays in `02.processing/`** (neither `done` nor `failed`) and counts as
**`Interrupted`** (reusing the existing counter, `main.go:283`), with a clear message:
"drained at op 31/74 — will resume on the next run".

## 4. Difference from the existing levers

| Lever | Effect on the current statement | Resume | Work lost |
|---|---|---|---|
| `pause` (existing) | suspends *mid-statement* (resumable) | same run, via `RESUME` | none (online/resumable) |
| `kill DDL` (existing) | **aborts** (offline rollback) | next run, **from the beginning** | the current op |
| **`drain` (proposed)** | **lets it finish** | next run, **at the next op** | **none** |

## 5. Links with the other iterations

- **`progress-tui.md`**: the drain needs `i`/`N` (already exposed by the proposed *step sink*) to
  display "will stop after op i/N" and to write the cursor.
- **`crash-resumable.md`**: the drain **materializes** the "operation cursor" (§6) with a
  deliberate trigger, and benefits from the **metadata skip** (§9) on resume. Design the two
  together (same `ResumeFromOp` field in `State`).
- **`remote-tui.md`**: `drain` becomes a broadcastable `Action` — a remote client can request a
  clean stop (with the same safety guards as `kill`).

## 6. Open questions

- **Ctrl+C semantics**: 1× drain / 2× hard — or reserve the drain for the TUI and keep Ctrl+C = hard?
- **Cancelling a drain**: allow backing out ("actually, keep going") before the current op
  finishes?
- **`State` vs a dedicated file** (§3.3): decide based on whether we want a standalone resume artifact.
- **Drain during a shrink**: is stopping after the current chunk enough, or do we need a persistent
  chunk sub-cursor?
- **Multi-database** (§17): does a drain stop only the current manifest, or the whole queue?

## 7. Effort estimate

**Small-to-medium.** The engine already has the loop, the index `i`, and a natural check point between
ops; `State` and recovery exist. To write: the `drain` action/flag, the signal handler
(optional but desirable), the `State` extension (cursor) + making recovery honor it,
and the display. The bulk is shared with `crash-resumable.md` (the cursor).

## 8. Code references (as of 2026-06-17)

| Topic | Location |
|---|---|
| Op loop (drain flag check point) | `internal/run/engine.go:286` |
| TUI actions/keys (where to add `drain`) | `internal/tui/model.go` ~95-113; `model.go:244-245` |
| Persistent state to extend (resume cursor) | `internal/run/state.go:12-20` |
| Recovery to make "cursor-aware" | `internal/run/recovery.go:174-184` |
| Reusable `Interrupted` counter | `cmd/sqlgopace/main.go:283` |
| No signal handler (to be added for Ctrl+C) | `specs/crash-resumable.md §3.1` |
| Operation cursor (sibling idea) + metadata skip | `specs/crash-resumable.md §6, §9` |
