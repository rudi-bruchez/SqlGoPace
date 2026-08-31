# WAIT-OBSERVABILITY — tracking our operation's waits (live TUI + log)

> **DRAFT** — source of truth for the intended behavior of observability over the waits caused by
> the running operation, via `sys.dm_exec_session_wait_stats`. Nothing new is coded yet; **a large
> part already exists** (see §3) — this document mainly frames the **live TUI panel**.

## 1. Goal and context

A demanding operation (rebuild, compression, shrink, batched DML) **waits**: on locks, data I/O, log
flush, parallelism, memory, tempdb… Exposing **what is slowing our session down**, live in the TUI
and as a summary in the `.log`, helps the operator **understand** a run and decide *by hand* (extend
the wait, pause, kill).

Scoping decision (see §2): this is **observability**, **not** a new reaction signal.

## 2. Stance: information, not alerting or automatic reaction

Waits are **diagnostic** (the "why"), not **prescriptive** (the "what to do"):

- The signals that genuinely warrant a reaction — **blocking** and **log pressure** — already have
  **dedicated, precise** reads (blocking sampler on `BlockingSPID`, log-space sampler). Reacting off
  **aggregated** wait stats would be less precise and **redundant**.
- The one legitimate "wait → action" use, the **adaptive throttle** (WRITELOG / PAGEIOLATCH_EX →
  shrink the step size, shrink the DML batch), **already exists** per driver (`shrink_calc.go`;
  `batch_calc.go` in `BATCH-DML.md`). We **do not generalize it** into an alerting engine.
- So: the wait panel **feeds the human decision** in the TUI; it triggers **nothing** on its own.

> If a wait were ever to drive a reaction, it would be through a **dedicated read** of that precise
> signal, not through this aggregated panel.

## 3. What already exists vs what is missing

**Already in place (reuse as is):**

- `internal/mssql/waits.go`: `SessionWaits(ctx, spid)` reads `sys.dm_exec_session_wait_stats` (2016+;
  best-effort, "no data" on an older server).
- `CategorizeWaits`: a **curated, ordered** set of useful categories (Locking, Data I/O, Transaction
  log, Parallelism, Memory, CPU & scheduling, Page latch (tempdb), Sort & spill, AG, Backup), with
  the noise filtered out; sorted by descending time + total.
- `DiffWaits(before, after)`: delta per wait type (the DMV is cumulative for the session).
- `engine.go` `snapshotWaits`/`operationWaits`: before/after capture, and **already writes the
  per-operation wait summary to the `.log`** (`report.WaitLine`).

**What is missing (the subject of this spec):**

- A **live TUI panel**: the wait categories of **our executing SPID**, as a **rolling delta since the
  start of the op**, refreshed on the sampling cadence — instead of only the end-of-op before/after.
- (Optional) an ***advisory* highlight** of a handful of "notable" waits (info, never a stop).

## 4. The feature

### 4.1 Live TUI panel

- On entering an operation, capture the wait snapshot (reuses `snapshotWaits`) as the **baseline**. On
  each sampling tick, re-read `SessionWaits(spid)`, compute `DiffWaits(base, now)` then
  `CategorizeWaits(delta)`, and **push** the result to the TUI.
- The TUI shows the **top categories** (name, wait time accumulated since the start of the op, task
  count), sorted descending — exactly what `CategorizeWaits` already returns. Updated in place.
- It is **context information** alongside the existing panels (progress, blocked sessions); it helps
  the operator choose a TUI action (`extend` / `pause` / `kill`).

### 4.2 Log summary (already present, to confirm)

- The `.log` keeps carrying the **per-operation wait summary** (categories + total) via
  `operationWaits`. No change required; at most, make sure the **max** observed during the run (peak)
  is kept if deemed useful.

### 4.3 (Optional, It2) advisory highlight

A small curated set of waits deserves a visual **highlight** (color / "⚠ info" badge), without ever
triggering an action:

- `RESOURCE_SEMAPHORE` / `RESOURCE_SEMAPHORE_QUERY_COMPILE` — memory grant starvation;
- `THREADPOOL` — worker starvation (an **instance** problem, not just ours);
- `PAGELATCH_*` (the "Page latch (tempdb)" category) — tempdb allocation contention → **ties in with
  `TEMPDB-GUARD.md`**.

These are markers **for the human**, not alerts that act.

## 5. Integration / wiring

- **No new DMV read**: everything goes through the existing `SessionWaits`/`DiffWaits`/
  `CategorizeWaits`. We do not touch `internal/mssql`.
- **Engine → TUI flow**: push the categorized delta over the **same channel** as the other TUI
  updates. Converges with the **step sink** introduced by `progress-tui.md` (`docs/specs/TODO.md`) — to be
  designed together so we don't multiply channels. With no TUI (`--tui` off), the panel is simply
  inactive; the `.log` is enough.
- **Cadence**: reuse the run's sampling cadence (the panel does not need to be more frequent than the
  other samples). Best-effort: a failed read leaves the last state displayed.

## 6. Version floor

`sys.dm_exec_session_wait_stats` exists from **SQL Server 2016** onward. On an older server,
`SessionWaits` returns "no data" (already handled) → the panel stays **empty/hidden**, with no error.
Same behavior as the `.log` today.

## 7. Phasing

- **It1.** Live TUI panel (categorized rolling delta for our SPID); confirmation of the log summary.
  No reaction.
- **It2 (option).** Advisory highlight of notable waits; possible peak kept in the `.log`.

## 8. Tests (no database; `-race`)

- **run (pure):** from two simulated snapshots, the categorized rolling delta pushed to the TUI is
  correct (reuses `DiffWaits`/`CategorizeWaits`, already tested — here we test the **streaming** and
  the choice of baseline = start of op).
- **tui:** the model shows the top categories and updates on message; empty when there is no data
  (server < 2016).
- **No reaction test**: by design, this panel triggers none.

## 9. Limits (deliberate)

1. **Diagnostic, not prescriptive**: don't expect the panel to "decide" — it informs the human.
2. **Scope = our executing SPID**: these are *our* waits, not a global server view (it is not a
   replacement for an instance monitoring tool).
3. **Cumulative → delta**: the DMV is cumulative; the whole point is the **delta since the start of
   the op** (otherwise we mix in the connection's history).
4. **2016+**: invisible on an older server (best-effort), like the current log summary.
