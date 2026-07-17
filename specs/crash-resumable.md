# Functional spec — Recovery after interruption ("crash-resumable")

> **Status: §9 (metadata-based skip) IMPLEMENTED; the rest remains DRAFT.** The manifest flag
> `skip_if_satisfied` (default off) makes a `rebuild_index` whose **every partition** already
> carries the target `data_compression` **skipped** at run time (outcome `skipped`, one log
> line `— skipped in 0s (already PAGE)` / `.log` `skipped: already PAGE`, history counter
> `runs.skipped`). Narrow read `mssql.IndexCompression(schema,table,index)` per partition;
> pure comparison `compressionSatisfied` (partition-aware); gated on the engine side by
> `WithCompressionReader`. Makes re-running an interrupted compression manifest **cheap**
> (ops already done are not redone) — see §9. **The operation cursor `State.ResumeFromOp`
> (§3.4/§6) is now written incrementally, so it is crash-safe**: `advanceCursor` moves it
> to `i+1` **after each completed operation** (success or skip) and persists it (`WriteState`
> made **atomic** via temp+rename, so a crash mid-write never leaves a truncated sidecar).
> It freezes on a gap left by `on_failure: continue` (the cursor only advances if `*cursor == i`),
> so the re-run replays the failed op and the idempotent ones that follow rather than skipping an
> effect that was never produced. Recovery preserves it on requeue (already in place), and the
> re-run skips ops `i < cursor`. A *crash* (≠ drain) therefore now populates the cursor: recovery
> restarts from the next op, without depending on the metadata skip (§9), which remains
> complementary (cheap compression prefix). The **graceful stop on Ctrl+C (§3.1) is done**
> (`graceful-stop.md`: 1× drain / 2× hard).
> **The true `ALTER INDEX … RESUME` (§4.2) is IMPLEMENTED**: on the re-run of a **resumed**
> manifest (a sidecar existed at claim time → `resumed` flag returned by `writeSidecar`), if the
> **op at the cursor boundary** (`i == resumeFrom`) carries a **PAUSED** resumable on the server
> (`PausedResumable`), the engine emits `ALTER INDEX … RESUME` (`resumeStatement` →
> `ddl.ResumableControlSQL(op,"RESUME")`) instead of the REBUILD — which SQL Server would reject
> as long as a resumable is paused — reusing the whole `MonitoredRunner` (which already knows the
> pause/resume loop). Guardrail against a "foreign resumable": never RESUME on a **fresh** manifest
> nor on an op that **never started** (options may differ). Recovery **preserves the sidecar** on
> requeue when `action == Resume` (paused resumable) or cursor > 0, so the re-run recognizes itself
> as resumed. Graceful degradation (S2: Standard/heap, no resumable): the op restarts, but the
> cursor avoids redoing the previous ones.
> **The `abort-resumable` orchestration (§3.6) is IMPLEMENTED, opt-in**: a *foreign/stale* paused
> resumable that would block a fresh REBUILD (Msg 10637) is, when the manifest carries
> `abort_blocking_resumable: true`, purged by `ALTER INDEX … ABORT` (`clearOrRejectBlockingResumable`
> → `ResumableAborter.ExecDDL`) before the rebuild; **without** the flag, the op fails early with an
> actionable message ("run `sqlgopace abort-resumable` or set abort_blocking_resumable: true").
> Opt-in by default because ABORT **destroys server-side progress** — a deliberate choice on a
> shared/production database. Detection is precise to the index (`blockingResumable` = `PausedResumable`
> on the target), at execution time (symmetric with RESUME), not a sweep in the `Recoverer` (which
> does not have the in-flight index and would destroy a resumable that a queued manifest would resume).
> The pre-run rejection is a clean **failure** (excluded from the "interrupted" reclassification,
> otherwise an infinite loop). Nothing blocking remains open on this spec.
>
> Created on 2026-06-17, following a mass compression attempt (manifest
> `01.to_run/030_compress_exampledb_indexes.yaml`, 74 EXAMPLEDB indexes).

## 1. Functional goal

When a long-running operation (typically an **index rebuild with compression** on a very
large table) is **interrupted** — `Ctrl+C`, stop/kill of the SqlGoPace process, machine
crash, network outage — the user wants the tool to **pick up where it left off** on the next
run rather than redoing everything from scratch.

The cost of a restart from zero on this kind of operation is high (hours of rebuild,
transaction-log pressure, maintenance window). This is exactly the scenario that mass
compression makes frequent.

## 2. Context: compression = REBUILD

Changing the compression of an existing index **is** an `ALTER INDEX … REBUILD WITH
(DATA_COMPRESSION = …)`. There is no separate SQL Server "compress" operation; a REORGANIZE
cannot change compression (`internal/ddl/manifest.go:401`:
"cannot change data compression — that requires a REBUILD").

Consequence: everything below about resuming rebuilds applies directly to compression
operations. An interrupted compression operation is an interrupted rebuild.

## 3. Current observed state (technical findings)

### 3.1 No signal handling

There is **no signal handler** in the code (`signal.Notify` / `os.Interrupt`: 0 occurrences).
A `Ctrl+C` (SIGINT) therefore kills the process **immediately**: the connection drops and
SQL Server aborts the running statement. There is no "graceful" stop that would pause the
operation in a controlled way before exiting.

### 3.2 Resume *within the process*: a true resume (already implemented)

The monitoring loop knows how to do a **true** SQL pause/resume, but only **while the process
is alive** and **in reaction to pressure** (log / locks), not on user interruption:

- `MonitoredRunner.runStatement` abandons the statement by cancelling the context (an
  *attention* that **pauses** a resumable on the server while keeping the pinned connection
  alive), with KILL as a fallback if the statement does not stop in time
  (`internal/run/monitored_runner.go:132-159`).
- `runLoop` waits for relief, then re-issues the resume statement
  (`internal/run/monitored_runner.go:105-130`), which is a true `ALTER INDEX … RESUME`
  (`internal/run/monitored_runner.go:89`, `ddl.ResumableControlSQL(op, "RESUME")`).

This is the "WAIT_AT_LOW_PRIORITY → RESUMABLE pause/resume → KILL" mechanism from the product
docs. **It does not cover external interruption** (Ctrl+C / kill / crash).

### 3.3 Resume *after a crash*: currently a restart, not a resume

After an interruption, the manifest stays in `02.processing/`. On the next run, the
`Recoverer` reconciles it:

- The recovery actions are `Adopt` / `Resume` / `Restart`
  (`internal/run/recovery.go:16-61`). `DecideRecovery` picks `Adopt` if an orphan is still
  alive, otherwise `Resume` if a resumable is known, otherwise `Restart`.
- **BUT** in `Recover()`, the `Resume` **and** `Restart` branches do exactly the same thing:
  `requeue` → "*re-enqueued for an idempotent re-run*"
  (`internal/run/recovery.go:174-184`).
- The comment on the `Resume` action is explicit: "*continues a resumable operation;
  re-enqueued for an idempotent re-run in this version (**true RESUME is a refinement**)*"
  (`internal/run/recovery.go:22-24`).

In other words: **true resume after a crash is not implemented.** The user-facing message
confirms it: "*interrupted manifest(s) left in processing; the next run will resume them*"
(`cmd/sqlgopace/main.go:283`) — but "resume" here means "replay the manifest", not "pick the
index back up where it stopped".

### 3.4 No per-operation resume point

The persistent state (`State`, `internal/run/state.go:12-20`) does **not** store an operation
cursor: it keeps `manifest`, `database`, `spid`, `login_time`, `marker`, `command`,
`started_at`. There is no trace of "I was at operation N of 74".

Consequence for a multi-operation manifest (e.g. our 74 indexes): on replay, **the whole
manifest restarts at operation 1**. Indexes already compressed are **rebuilt again**
(idempotent — correct result — but wasted work).

### 3.5 RESUMABLE eligibility conditions

`RESUMABLE` can only be injected under conditions (`ddl_compatibility.yaml:27/34/51`):

- SQL Server **2017+** (major 14) for `rebuild_index`;
- **Enterprise / Azure** editions only;
- **requires `ONLINE`** (`requires: [online]`).

So: no resumable on **Standard**, nor on a **heap rebuild** (note
`ddl_compatibility.yaml:70`: no RESUMABLE and no WAIT_AT_LOW_PRIORITY for a heap). A true
resume after a crash will only be **possible where RESUMABLE was active**. Elsewhere, only
restarting the operation is conceivable.

### 3.6 Risk of being blocked by a paused resumable

When the session is killed while a `RESUMABLE = ON` rebuild was running, SQL Server leaves the
operation **paused** (progress kept on the server). Launching a **new** `REBUILD` on that index
may then be **rejected** by SQL Server as long as the paused resumable is not resumed or
aborted. The tool already exposes an **`abort-resumable`** subcommand
(`cmd/sqlgopace/abort.go`) to purge a paused resumable, but the recovery flow does not
orchestrate it automatically today.

## 4. The requirement (what we want to settle)

1. **Real resume after an external interruption** (Ctrl+C / kill / crash / outage), not only
   after pressure detection during a run.
2. When the interrupted operation was a **paused RESUMABLE** rebuild, the resume must emit an
   `ALTER INDEX … RESUME` (reusing the retained progress) instead of a full REBUILD.
3. For a **multi-operation manifest**, do not redo operations already completed: restart at the
   first unfinished operation.
4. **Graceful degradation** when resuming is not possible (Standard, heap, resumable not
   injected): restart the operation, telling the user clearly.
5. Handle the **blocking paused resumable** (resolve / abort before relaunching) without
   systematic manual intervention.

## 5. Functional scenarios (to validate at brainstorming)

- **S1 — Kill during index 31/74 (Enterprise, resumable active).** Expected: on the next run,
  indexes 1–30 are untouched, index 31 **resumes** via RESUME, then 32–74 follow.
- **S2 — Kill during index 31/74 (Standard, no resumable).** Expected: index 31 restarts from
  zero (unavoidable), but 1–30 are not redone; explicit message "no resume possible, restarting
  the operation".
- **S3 — Full machine crash.** Expected: same behavior as S1/S2 when the tool restarts (state
  reconstructed from `02.processing/` + the server state).
- **S4 — Resumable left paused, then run relaunched.** Expected: no "resumable operation already
  in progress" failure; the tool resumes or aborts cleanly.

## 6. Open questions for the brainstorming

- **State granularity**: do we need an operation cursor in `State` (op N/M + status), or should
  we rely solely on idempotence + the server state (existing resumable)?
- **"Already done" detection**: how do we know an index is already at the target compression
  without the rebuild? (reading `data_compression_desc` before each op — already done on the
  planner side; port it to the run side?) That would make replay *cheap* even without a cursor.
- **True resume vs idempotent replay**: implement `Resume` = a true `ALTER INDEX RESUME`
  (reuses server-side progress) vs simply "skip what is done". Both are useful and can be
  combined.
- **Graceful stop on Ctrl+C**: install a signal handler that pauses the resumable and writes a
  "cleanly paused" state before exiting — reuse the existing `runLoop`/`ResumableControlSQL`
  machinery?
- **`abort-resumable` orchestration**: should recovery automatically resume, or abort, a paused
  resumable that is incompatible with the replay?
- **v1 scope**: limit to the `rebuild_index` RESUMABLE case (the most frequent and the only one
  that retains server-side progress), with shrink and other ops handled separately (shrink is
  already chunked, see `specs/SHRINK.md`).
- **Splitting manifests**: a pragmatic alternative with no new code — recommend/generate smaller
  manifests to bound the cost of a replay. A palliative, not a real resume.

## 7. Out of scope (for the record)

- Pause/resume **in reaction to pressure** is already in place (§3.2) and is not the subject of
  this feature.
- **Shrink** follows a distinct chunked driver (`ShrinkRunner`) that resumes naturally between
  chunks; its resume requirement is different and already partially covered.

## 8. Code references (as of 2026-06-17)

| Topic | Location |
|---|---|
| Compression ⇒ REBUILD mandatory | `internal/ddl/manifest.go:401` |
| RESUMABLE matrix (2017+/Enterprise/Azure/online) | `ddl_compatibility.yaml:27,34,51` |
| No RESUMABLE for a heap | `ddl_compatibility.yaml:70` |
| True pause/resume during a run | `internal/run/monitored_runner.go:89,105-159` |
| PAUSE/RESUME/ABORT SQL generation | `internal/ddl/control.go` (`ResumableControlSQL`) |
| Recovery actions + "true RESUME is a refinement" comment | `internal/run/recovery.go:16-61` |
| `Resume` == `Restart` == requeue | `internal/run/recovery.go:174-184` |
| Persistent state without an operation cursor | `internal/run/state.go:12-20` |
| "next run will resume" message (= replay) | `cmd/sqlgopace/main.go:283` |
| Subcommand to purge a paused resumable | `cmd/sqlgopace/abort.go` (`abort-resumable`) |
| Absence of a signal handler | no `signal.Notify` in the repository |
| Compression read on the planner side (already-done) | `internal/maint/decide.go` (`decideCompression`) + `data_compression_desc` |
| Per-op existence check at preflight (model to follow) | `internal/mssql/existence.go`, `internal/preflight/preflight.go` |

## 9. Design direction: metadata-based skip (proposed on 2026-06-17)

> Came out of a real attempt: compressing 74 indexes to PAGE on `EXAMPLEDB` (Standard, so
> **offline and atomic** rebuilds). Idea: make each op **idempotent at near-zero cost** on replay.

### 9.1 Principle

Before executing each `rebuild_index` carrying a `data_compression`, **read the object's
compression metadata** and **rebuild only if it differs from the target**. Example: target
PAGE → rebuild only if the current compression ≠ PAGE.

This covers the **"skip what is done" half** of the requirement (§4.3, §6) **without** needing
the true `ALTER INDEX … RESUME`. A 74-op manifest killed at op 31: on the re-run, 1–30 are
**skipped** (a simple read), 31 is redone, 32–74 follow.

### 9.2 Why it is clean on Standard (and in general)

An offline rebuild is **atomic**: an interruption causes a **full rollback**, so the index
reverts to its previous compression. On replay there are therefore **only two states** per
index — *already at the target* (skip) or *not at the target* (redone) — **never a half-state**.
On Enterprise online/resumable, the interrupted index is left *paused* (≠ target): it will
therefore be redone by this mechanism, unless combined with a true RESUME (§6).

### 9.3 Partition granularity

`sys.partitions` carries **one row per partition** (`data_compression`/`data_compression_desc`,
2 = PAGE). So:

- an op on the whole index (`PARTITION = ALL`) → skip **only if *all* partitions** are already
  at the target;
- an op targeting `PARTITION = n` → test only that partition.

### 9.4 Decisions to settle

1. **Compression ≠ defrag.** A `rebuild_index` may also aim at a defrag; skipping "already
   compressed" would drop that defrag. → make the skip **opt-in** (manifest/CLI flag, e.g.
   `skip_if_satisfied: true`), **default off**, to preserve the "do what I say" contract of a
   direct run. For a pure *compression* manifest (the use case), we enable it.
2. **Where.** At the **moment each op executes** (at run time, the way shrink generates its SQL),
   not at global preflight: we read the freshest state, which naturally handles the progress of a
   previous interrupted run.
3. **Reuse.** The planner **already** does this comparison (`decideCompression` reads
   `data_compression_desc` and emits nothing if already at the target). Port that logic onto the
   run path via a narrow `mssql` read, e.g. `IndexCompression(schema, table, index) →
   desc per partition`. No new logic.
4. **Reporting.** Log each skip (`[k] rebuild_index dbo.X.IX — already PAGE, skipped`) and
   distinguish `skipped` / `done` in the `.log` and the history, so a re-run clearly shows what
   was reused.
5. **Scope.** Extend the idea to other ops with a metadata target (e.g. `add_column` already
   present, `alter_column` already at the right type)? To be discussed; start with compression.

### 9.5 Relation to the rest of the spec

- A **complementary** mechanism, not a substitute, to the true RESUME (§6): it makes replay
  *cheap* but does not resume an index interrupted *in the middle* of a rebuild.
- Much simpler than an operation cursor in `State` (§6): the "state" is read directly from the
  server, the source of truth, so **no progress persistence** to maintain.
