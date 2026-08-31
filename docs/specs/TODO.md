# TODO — iterations to design / implement

Index of iteration specs awaiting brainstorming and then implementation. All in
**DRAFT**: none is coded yet. Dated 2026-06-17.

## Iterations

- [ ] **[Resume after interruption / metadata skip](crash-resumable.md)** — a Ctrl+C / kill
  / crash does not resume where it stopped (recovery replays the manifest from the beginning).
  Proposes: a real `ALTER INDEX … RESUME` where possible, and above all the **metadata skip**
  (`sys.partitions`: rebuild only if compression ≠ target) for a cheap idempotent re-run.

- [ ] **[Manifest progress (i/N counter, elapsed time, blocked sessions)](progress-tui.md)** — no
  readable progress tracking today. Proposes: an **"operation i/N"** counter, **elapsed time**
  for the current op, **number of blocked sessions** (already in the TUI, to be surfaced on stdout too).
  Centerpiece: a **step sink** from the engine → stdout & TUI.

- [ ] **[Remote TUI (server / client)](remote-tui.md)** — follow/act on a run from another
  process. Proposes: `--serve :port` (SSE broadcast hub) + `--connect host:port` (reuses the
  TUI). The real cost = **security** of remote actions (KILL). Converges with the step sink from
  `progress-tui.md`.

- [ ] **[Graceful stop / drain](graceful-stop.md)** — a TUI command (and ideally Ctrl+C 1×) that
  stops **after the current statement** (no rollback), writes a **resume point** (cursor
  in the `State` sidecar, or a dedicated control file) and resumes at the next op. The missing link
  between `pause` (mid-statement) and `kill` (brutal).

- [ ] **[Batched DML: chunked `UPDATE`/`DELETE`](BATCH-DML.md)** — extends SqlGoPace to bulk DML
  (setting a column to a value across a whole table, purging a table) by **splitting it into a loop of
  batches** to avoid lock escalation (~5000 → table X lock) and log blowup.
  Modeled on the **shrink driver** (chunked loop, adaptive calibration, reused reactions).
  `set_raw`/`where_raw` escape hatch behind a guard; crash resume (self-limiting predicate, then
  a `key_range` cursor). **This is where the RCSI check earns its keep**: it decides how much escalation
  hurts (readers frozen if RCSI is off vs tempdb version store if it is on).

- [ ] **[tempdb guard (alert + self-attributed stop)](TEMPDB-GUARD.md)** — tempdb is shared by
  the whole instance (blast radius = every database). Proposes: **preflight no-start** if tempdb is
  already above the threshold; a **runtime alert** (TUI + log) on threshold; and above all a **stop conditioned
  on self-attribution** — we only stop (pause→cancel) if tempdb is full **AND** it is *us*
  (`sys.dm_db_session_space_usage` per SPID), otherwise alert only (stopping for someone else's fault
  frees nothing). Cross-cutting: serves `SORT_IN_TEMPDB` rebuilds, shrink, batched DML. The RCSI
  version store = an accepted blind spot (alert only).

- [ ] **[Wait observability (live TUI + log)](WAIT-OBSERVABILITY.md)** — surface **in real
  time in the TUI** (and already summarized in the `.log`) our session's waits via
  `sys.dm_exec_session_wait_stats`. **Observability, not reaction**: waits explain the
  "why", they drive nothing (blocking/log already have dedicated reads; the WRITELOG/PAGEIOLATCH
  throttle already exists per driver). Reuses the existing `SessionWaits`/`DiffWaits`/`CategorizeWaits`
  — what's new is the **live panel** (sliding delta). Converges with the step sink from
  `progress-tui.md`.

## Dependencies / suggested order

1. `progress-tui.md` first — the **step sink** it introduces is reused by `remote-tui.md`
   (same message hub).
2. `remote-tui.md` next — builds on that hub.
3. `crash-resumable.md` is independent — can be done separately; start with the **metadata skip**
   (small, big win) before the real RESUME.
4. `graceful-stop.md` shares the **operation cursor** with `crash-resumable.md` (same
   `ResumeFromOp` field in `State`) — design them together.

## Context

Specs born from the compression trial `01.to_run/030_compress_exampledb_indexes.yaml` (74 PAGE indexes
on `EXAMPLEDB`, Standard edition → offline rebuilds). See also `docs/llm-operator-guide.md` and the
`.claude/skills/sqlgopace-operator/` skill (LLM help for using the tool).
