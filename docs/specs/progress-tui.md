# Functional spec — Progress of a manifest (counter, timer, blocked sessions)

> **Status: items 1, 2 and 3 implemented.** The engine *step sink* (`run.WithStepSink` /
> `run.StepEvent`, emitted at the top/bottom of the op loop) feeds **stdout** (`-- [i/N] cmd target —
> started` / `— <outcome> in Xs`, items 1+2) and the **TUI** (op counter i/N + live timer via a 1 s
> tick; item 2) — shipped with BATCH-DML It3. **Item 3**: on every reaction (pause/cancel/abort) the
> blocked-session count is folded into the narration (`… ; blocking N session(s)`, stdout +
> `.log`), the **per-op peak** is recorded in the `.log` (`peak blocked: N session(s)`) and the
> manifest peak in the history (column `runs.peak_blocked`, via an `ALTER TABLE` migration). Remaining
> (DRAFT): persisting i/N in `State` (§3.1, for a resume display — to be weighed against the metadata
> skip in `crash-resumable.md`). Created on 2026-06-17, following the compression run
> `030_compress_exampledb_indexes.yaml` (74 offline rebuilds on `EXAMPLEDB`), where we found there was
> no readable progress.

## 1. Goal

Give the operator **readable progress** while a manifest runs, both in stdout mode (background run)
**and** in the TUI:

1. **Manifest-level counter "operation i / N".**
2. **Timer for the current operation** (which op, and for how long).
3. **Number of sessions blocked** by the operation (already present in the TUI — to confirm/extend).

Motivation: on **Standard**, rebuilds are **offline** and SQL Server **does not populate**
`percent_complete` (see §3.2); the TUI's only current gauge therefore stays at 0%. Progress *at the
manifest level* (i/N + timer) does not depend on what the server chooses to report.

## 2. Current state observed

### 2.1 The engine already has the information, it just doesn't expose it

The execution loop knows everything needed (`internal/run/engine.go:286-287`):

```go
for i, step := range planned {        // i = 0-based index; len(planned) = N (after expansion/plan)
    opStart := e.clk.Now()            // start of the op: the timer is already computed
    ...
}
```

`opTarget(step.Operation)` already yields the target label (used at `engine.go:299`). But the engine
narrates **no** "op i/N started" line: it only writes the manifest events
(`skip`/`complete`/`fail`/`done`) and the reaction events (`engine.go:299`). Hence a run log that is
nearly silent between start and finish (observed: only the 2 header lines).

### 2.2 The TUI is decoupled from the engine (server polling only)

`runWithTUI` (`cmd/sqlgopace/main.go:431-456`) starts in parallel:

- `engine.ProcessAll` in a goroutine, which writes to **`io.Discard`** in TUI mode;
- `feedConsole` (`main.go:459-488`), which **polls the server** and sends the TUI: `BlockersMsg`,
  `ProgressMsg` (the SPID's `percent_complete`), `WaitsMsg`.

There is **no engine → TUI channel**. The model's `operation:` label therefore stays at `(running)`
(`tui.New("(running)", …)`), never updated per op — even though `StatusMsg.Operation` already exists
on the model side (`internal/tui/model.go:78-81, 151-154, 215-216`).

### 2.3 `percent_complete` is unusable offline

`ProgressMsg.Percent` comes from `sys.dm_exec_requests.percent_complete` (`main.go:480-501`,
`internal/mssql/dmv.go:53-80`). That field is only populated for REORGANIZE, online/resumable, DBCC,
BACKUP/RESTORE, rollback… **not** for an offline `ALTER INDEX REBUILD`. On Standard it stays at 0.

### 2.4 Item 3 is already there (TUI)

The TUI already shows `blocked sessions (%d)` + the list (SPID/login/host/wait/query):
`internal/tui/model.go:222`, fed by `feedConsole`, which filters the sessions whose
`BlockingSPID == ddlSPID` (`main.go:468-478`). **Nothing to build on the TUI side** for item 3; only
the stdout/non-TUI report lacks it.

## 3. Proposed design

### 3.0 Centerpiece: an engine *step sink* → consumers

Add a step-event channel to the engine, independent of the text narration, so that **stdout and TUI**
are fed from the same source:

```go
type StepEvent struct {
    Index, Total int           // i+1 of N (1-based for display)
    Command      string        // "rebuild_index", "shrink", …
    Target       string        // opTarget(step.Operation)
    StartedAt    time.Time     // = opStart (timer)
    Phase        StepPhase     // Started | Finished
    Duration     time.Duration // filled in on Finished
    Outcome      string        // "done" | "failed" | "skipped" (see crash-resumable §9)
}
```

Wired through an `EngineOption` (`WithStepSink(func(StepEvent))`, alongside `WithProgress`/`WithOutput`,
`engine.go:147-156`). Emitted at the top and the bottom of the loop at `engine.go:286`.

- **Non-TUI mode** (the real-world case: background run): the sink formats to `e.out` —
  `\[12/74] rebuild_index dbo.ORDERS.PK_ORDERS — started 23:45:01` then
  `\[12/74] … done in 3m20s`. **This is the biggest progress win**, since the common usage is
  non-TUI.
- **TUI mode**: `runWithTUI` maps `StepEvent` → `tui.StatusMsg` (label + counter + `StartedAt`).

### 3.1 Item 1 — "operation i / N" counter

- Data: `i+1` and `len(planned)` already available (§2.1).
- TUI: extend `StatusMsg` (or `tui.New`) with `StepIndex`/`StepTotal`; rendered at the top of `View`
  (`model.go:215`): `operation 12/74: rebuild_index dbo.ORDERS.PK_ORDERS [RUNNING]`.
- stdout: see 3.0.
- (Option) persist `i/N` in `State` (`internal/run/state.go`) so that a re-run immediately shows
  "resuming at op k/N" — to be weighed against the metadata skip mechanism (crash-resumable §9),
  which makes the state cursor less necessary.

### 3.2 Item 2 — timer for the current operation

- Data: `opStart` already exists (§2.1); push it through `StartedAt`.
- TUI: store `opStartedAt` in the model; display `elapsed = now − opStartedAt`. This requires a
  periodic refresh → add a **1 s `tea.Tick`** (the model/`program.go` has none today) that only
  re-renders. Rendering: `progress: op 12/74 — elapsed 03:20` (and keep the server `percent`/ETA
  when it is available, e.g. online/resumable/rollback).
- stdout/.log: the **per-op duration** on the `Finished` line (3.0) and in the `.log`/history.

### 3.3 Item 3 — blocked sessions

- **Already shown in the TUI** (§2.4). Proposed delta:
  - surface the **count** in non-TUI: include it when a reaction fires, or on the op line
    (`… blocked: 2`) on a poll;
  - record the **peak blocked-session count** per op in the `.log`/history (useful post-mortem:
    "this compression blocked up to 5 sessions for 4 min").

## 4. Scope & limits

- Item 2 (live timer) is mainly a **TUI** feature (1 s tick). In stdout, we limit ourselves to the
  **per-op duration** at completion (a ticking timer makes no sense in a log).
- Independent of `percent_complete`: i/N + timer work even when the server reports nothing
  (offline), which is precisely the Standard case.
- Does not depend on the crash-resumable feature, but combines well with it: the sink can emit
  `Outcome = "skipped"` when the metadata skip (crash-resumable §9) skips an op already at target.

## 5. Open questions

- **Shape of the sink**: a single `WithStepSink` (Started/Finished) vs two callbacks? Reuse the
  pattern of the existing `WithX` options (`engine.go:147-156`).
- **Attach the TUI to an already-running run?** Impossible today (the TUI is in-process). Out of
  scope, but worth noting: a non-TUI background run cannot get the TUI after the fact.
- **Shrink granularity**: a `shrink` is multi-chunk (a single `step`). Do we want a "chunk j/k"
  sub-counter on top of the op i/N? The shrink driver already knows its progress (`main.go:322`).
- **Persisting i/N in `State`**: useful for the resume display, or redundant with the metadata skip?
  (see crash-resumable §6, §9.5).

## 6. Code references (as of 2026-06-17)

| Topic | Location |
|---|---|
| Op loop with `i`, `len(planned)`, `opStart` | `internal/run/engine.go:286-287` |
| Target label of an op | `opTarget(step.Operation)` (`engine.go:299`) |
| Engine options (`WithOutput`/`WithProgress`) | `internal/run/engine.go:147-156` |
| TUI: op label + `StatusMsg` | `internal/tui/model.go:78-81, 151-154, 215-216` |
| TUI decoupled (engine→`io.Discard`, server poll) | `cmd/sqlgopace/main.go:431-488` |
| `percent_complete` (0 when offline) | `cmd/sqlgopace/main.go:480-501`, `internal/mssql/dmv.go:53-80` |
| Blocked sessions already displayed (item 3) | `internal/tui/model.go:222`, `cmd/sqlgopace/main.go:468-478` |
| Metadata skip (combinable, `Outcome=skipped`) | `docs/specs/crash-resumable.md` §9 |
