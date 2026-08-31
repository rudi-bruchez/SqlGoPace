# SHRINK — implementation plan

> Companion to `SHRINK.md` (design v1). **Linear, single-developer** development, designed
> to save tokens: self-contained steps, ordered pure-core first, each one verifiable with
> `make test && make vet` **without a database**; integration/e2e (real DB) only shows up
> at steps 5 and 8.
>
> Working convention: one step = one commit (or a small PR). Do not start step N+1 until
> step N passes `make test && make vet && make lint`.

## Architecture principle (recap)

Shrink **does not fit** the "one operation = one statement" model. It is driven by a
**dedicated driver** (`internal/run/shrink.go`) that reads DMVs at run time, builds the SQL
per chunk via `ddl` helpers, and runs its own loop. The engine **routes** `Shrink` operations
to that driver instead of `MonitoredRunner`.

All decision logic (target computation, initial step size, adjustment, no-progress) is
written as **pure functions**, testable without a DB, following the `runLoop`/`supervise`/
`DecideReaction` model (pure core + injected I/O).

---

## Step 0 — Matrix + command types (foundation, tiny)

**Goal**: declare shrink option eligibility by version/edition.

- `internal/ddl/ddl_compatibility.yaml`: add the `shrink_data` and `shrink_log` command
  types. Under `shrink_data`, `wait_at_low_priority` is eligible on **SQL Server 2022 (16.x)+**
  only. Nothing is eligible under `shrink_log` (no WALP).
- Check that `matrix.go` loads these entries with no code change (data only).

**Tests**: one case in `matrix_test.go`: `Applicable(16, tier, "shrink_data", "wait_at_low_priority")`
true on 2022, false on 2019.

**Checkpoint**: `make test`.

---

## Step 1 — `shrink` operation type in `ddl` (parse / validate / target)

**Goal**: parse and validate the operation's YAML.

- `internal/ddl/manifest.go`:
  - `case "shrink": return decodeInto[Shrink](node)` in `decodeOperation`.
  - `Shrink` struct:
    ```go
    type Shrink struct {
        Type            string          // "data" | "log"
        Files           string          // "all" | logical name; defaults to "all"
        EmptyFile       bool            // reserved for Phase 2; must be false in v1
        TargetFreeSpace string          // raw "10%" | "100MB"; parsed by TargetSpec
        Options         OptionOverrides // only WaitAtLowPriority is relevant
    }
    ```
  - `CommandType()`: `"shrink_data"` or `"shrink_log"` depending on `Type` (feeds the matrix).
  - `Target()`: targets a file/a database, **not** `schema.table` (cf. the
    *check_db target shape* memory — don't abuse `ObjectRef.table`). Return
    `ObjectRef{Name: o.Files}` (or a dedicated field if clearer).
  - `Validate()`: `Type ∈ {data, log}`; `EmptyFile == false` (otherwise error "reserved for
    Phase 2"); `TargetFreeSpace` parsable and > 0.
- New `internal/ddl/shrink.go` (pure functions, no DB):
  - `type TargetSpec struct { Percent *int; AbsoluteMB *int }`
  - `ParseTargetFreeSpace(s string) (TargetSpec, error)`: `"10%"` → Percent; `"100MB"`/`"100 MB"`
    → AbsoluteMB; rejects empty / negative / unknown unit.
  - `FinalTargetMB(usedMB int, spec TargetSpec) int`: `Percent` ⇒
    `ceil(used × (1 + N/100))`; `AbsoluteMB` ⇒ `used + N`. (The clamp to the `used` floor
    happens here too: never < usedMB.)

**Tests** (`manifest_test.go`, `shrink_test.go`): YAML decoding for data/log; rejections
(invalid `type`, `emptyfile: true`, unreadable targetfreespace); case tables for
`ParseTargetFreeSpace` and `FinalTargetMB` (rounding, clamp).

**Checkpoint**: `make test`.

---

## Step 2 — Option resolution + per-chunk SQL generation

**Goal**: decide WALP (without forcing ONLINE) and produce the `DBCC SHRINKFILE` statements.

- `internal/ddl/resolve.go`:
  - Dedicated branch for the shrink command types: resolve **only** `wait_at_low_priority`
    (via the matrix); **do not** touch online/resumable/sort_in_tempdb, and do not apply the
    "WALP requires ONLINE" rule. `ABORT_AFTER_WAIT = BLOCKERS` only if
    `Policy.AllowAbortBlockers`, otherwise `SELF`. No `MAX_DURATION` (field ignored for shrink).
  - `overridesOf`: add the `case Shrink: return o.Options`.
- `internal/ddl/generate.go` (**dedicated** generator, not `withClause`):
  - `ShrinkChunkSQL(file string, targetMB int, res ResolvedOptions) string` →
    `DBCC SHRINKFILE (N'file', targetMB) WITH WAIT_AT_LOW_PRIORITY (ABORT_AFTER_WAIT = SELF), NO_INFOMSGS;`
    (WALP clause only if `res.WaitAtLowPriority`, otherwise just `WITH NO_INFOMSGS`).
  - `ShrinkTruncateOnlySQL(file string) string` →
    `DBCC SHRINKFILE (N'file', TRUNCATEONLY) WITH NO_INFOMSGS;`
  - `Generate()` for a `Shrink`: return a **representative** string (e.g. the SQL of the
    first chunk, or a comment), since the real SQL is multi-statement and built at run time
    by the driver. Document that a shrink's `PlannedOperation.SQL` is indicative only.

**Tests** (`resolve_test.go`, `generate_test.go`): WALP resolved ON/OFF according to matrix
and override; no `ONLINE`/`RESUMABLE` ever injected for shrink; file name quoting
(`N'...'`, doubling of `'`); exact shape of both helpers.

**Checkpoint**: `make test`.

---

## Step 3 — Pure core of the chunking (step size, adjustment, decisions)

**Goal**: all the calibration logic as testable pure functions.

- `internal/ddl/shrink.go` or `internal/run/shrink_calc.go` (either; keep it pure):
  - `InitialStepMB(reclaimMB int, cfg ShrinkConfig) int`: bands < 5 GB / 5–50 GB / > 50 GB.
  - `AdjustStepMB(step int, elapsed time.Duration, w WaitDeltas, cfg ShrinkConfig) int`:
    `/2` if `WRITELOG>10ms` or `PAGEIOLATCH_EX>20ms` or blocking>30s; `*2` if I/O<5ms,
    no wait, `elapsed<targetBatch`; bounded to `[min,max]`.
  - `NextTargetMB(current, step, final int) int`: `max(current-step, final)`.
  - `WaitDeltas` type (WRITELOG, PAGEIOLATCH_EX avg ms, blockingSeconds) consumed by
    `AdjustStepMB`.

**Tests**: case tables for each function (reduction, increase, bounds, last chunk landing
exactly on `final`).

**Checkpoint**: `make test`.

---

## Step 4 — `shrink:` configuration (defaults)

**Goal**: expose the defaults from §7.3 of `SHRINK.md`, all optional.

- `internal/config/config.go`: `ShrinkConfig` struct (fields `initial_step_small_mb`, …,
  `self_wait_timeout_minutes`, `log_reuse_wait_timeout_minutes`) + `time.Duration` accessors
  like `MonitoringConfig`. Defaults applied when a field is zero (block absent ⇒ all
  defaults).
- Wire `ShrinkConfig` into the root `Config` structure.

**Tests** (`config_test.go`): block absent ⇒ defaults; partial override ⇒ only the supplied
fields change; negative values rejected or clamped.

**Checkpoint**: `make test`.

---

## Step 5 — DMV reads in `internal/mssql` (adapter, real DB)

**Goal**: the run-time reads the driver needs. Code behind interfaces, tests
`integration`-tagged (skipped without `SQLGOPACE_TEST_DSN`).

- `internal/mssql` (new `shrink.go`, or extend `databases.go`/`recovery.go`):
  - `FileSpace(ctx, fileType) ([]FileSpace, error)`: `name, type_desc, size_mb, used_mb,
    free_mb` from `sys.database_files` + `FILEPROPERTY`. (`fileType` = ROWS|LOG.)
  - `FileSizeMB(ctx, file string) (int, error)`: current size (for the loop / progress).
  - `LogReuse(ctx) (recoveryModel, reuseWaitDesc string, error)` from `sys.databases`.
  - `ActiveLogFloorMB(ctx) (int, error)`: sum of active VLFs (`sys.dm_db_log_info`,
    `vlf_active=1`) → log truncation floor.
  - Reuse the existing `SessionWaits(ctx, spid)` for self-wait detection (step 6).
- Declare the narrow interfaces on the `run` side (like `sampleProbe`) for these reads, so
  fakes can be supplied in unit tests.

**Tests**: `*_integration_test.go` (`integration` tag) against the e2e database. No pure unit
test here (this is the SQL adapter).

**Checkpoint**: `make test` (integration tests are skipped), then, if a DB is available,
`make integration`.

---

## Step 6 — Shrink driver (`internal/run/shrink.go`)

**Goal**: orchestrate estimation → truncate-only → chunk loop, with reactions.

- `ShrinkRunner` (or `ShrinkDriver`) built with injected I/O:
  execution (`Executor`: `ExecDDL`/`SPID`/`Kill` — **inherits KILL through the pool**, §8.5 of
  the design), reads (`FileSpace`/`FileSizeMB`/`SessionWaits`), sampler (`ServerSampler`),
  clock (`Clock`), `ShrinkConfig`, `ReactionSink`.
- `Run(ctx, op ddl.Shrink, res ddl.ResolvedOptions, caps Capabilities, sink ReactionSink) error`:
  1. **Estimation/gating** (reuses the reads):
     - data: compute `final` via `FinalTargetMB`; no-op if `free≈0`/`final≥size` → success, "nothing to do".
     - log: `LogReuse` → SIMPLE = `CHECKPOINT` allowed, then shrink; FULL/BULK_LOGGED with
       `reuseWait≠NOTHING` → **bounded wait** (`log_reuse_wait_timeout`) for a scheduled log
       backup to free the log: re-read `reuseWait`+VLF floor on the poll cadence, emit one
       `pause` per cycle with the reason, shrink as soon as `reuseWait=NOTHING`, clean give-up
       at timeout. **Never** issue a BACKUP LOG. Floor = `ActiveLogFloorMB`.
  2. **Phase A — TRUNCATEONLY**: `ExecDDL(ShrinkTruncateOnlySQL)`; re-read size; if ≤ final → done.
  3. **Phase B — chunk loop** (data): `InitialStepMB` → loop over
     `NextTargetMB`/`ShrinkChunkSQL`, measure `elapsed`+deltas, `AdjustStepMB`, re-read the size.
     - **Free pause** under pressure: don't launch the next chunk, wait for relief
       (reuse `awaitRelief`/`waitForRelief`, `logDrainTimeout`).
     - **Self-wait**: `SessionWaits` shows a prolonged `LCK_M_SCH_M`/snapshot → wait up to
       `self_wait_timeout`, then stop cleanly.
     - **No-progress**: size unchanged after a chunk (49516, data at the end of the file) →
       increasing backoff + retry, clean stop beyond `max_no_progress`.
     - A chunk that must be stopped goes through `context` cancellation (attention), then `Kill`
       via the pool.
  4. Emit `ReactionEvent` (`pause`/`resume`/`abort`) via `sink`, plus the **deterministic
     progress** `(start-current)/(start-final)`.
  - The log is shrunk (Phase A/truncation) without chunking: one (or two) `DBCC SHRINKFILE`.
- Keep the loop structured like `runLoop`: decision mechanics in pure functions (step 3) +
  injected I/O, for a deterministic unit test with fakes (no DB).

**Tests** (`shrink_test.go`, no DB): no-op; truncate-only alone is enough; loop converging
to `final`; step adjustment under simulated pressure; no-progress → stop after
`max_no_progress`; log FULL `reuseWait=LOG_BACKUP` → wait, then shrink when it flips back to
`NOTHING`, and clean give-up if the timeout expires; log SIMPLE success after CHECKPOINT.

**Checkpoint**: `make test && make vet`.

---

## Step 7 — Engine + TUI wiring

**Goal**: route shrink operations and report on them.

- `internal/run/engine.go` (`processOne`): if `step.Operation` is a `ddl.Shrink`, call the
  `ShrinkRunner` instead of `r.runner.Run`. Build `caps` (shrink = cancel-safe, resumable).
  Populate `OperationReport` (initial/final size, space reclaimed, chunk count, reactions,
  waits, duration). `notify` on `pause`/`abort` like the existing path.
- Progress: for a shrink, feed the TUI `Model` with per-chunk progress (not
  `operationPercent`). Extend the feed (`feedConsole`/TUI messages) if needed, or expose the
  driver's progress via a channel/`sink`.
- Recovery (`Recoverer`): check that an interrupted shrink is simply requeued/re-runnable
  (no resumable-specific logic; re-running toward the same target picks up where it left off).

**Tests**: `engine_test.go` — a shrink manifest is routed to the driver (fake driver), the
report contains the expected fields; data + log cases.

**Checkpoint**: `make test && make vet && make lint`.

---

## Step 8 — e2e + documentation

**Goal**: validate against a real instance and document the user-facing surface.

- e2e (`make e2e`, SQL Server 2022): create a throwaway database, inflate a file (insert +
  massive delete to create free space), run a shrink data manifest, check the reduction and
  the report; SIMPLE log case (CHECKPOINT + shrink) and FULL refused.
- `README.md` (canonical reference): document `operation: shrink` (fields, data and log
  examples), the `shrink:` block of `config.yaml`, and the behavior (automatic TRUNCATEONLY,
  FULL log refusal, per-chunk progress).
- `docs/testing.md`: note the required permissions/context if different (shrink requires rights on
  the database; `VIEW SERVER STATE` already required for monitoring).
- Calibrate the defaults empirically (§14 of the design) and adjust if needed.

**Checkpoint**: `make e2e` green; re-read the full diff.

---

## Dependency / ordering recap

```
0 matrix ─▶ 1 type ─▶ 2 resolve+generate ─▶ 3 pure chunking core ─▶ 6 driver ─▶ 7 engine ─▶ 8 e2e+doc
                                          ╲                        ╱
                                4 config ──┴── 5 mssql (DB) ──────┘
```

- Steps 0–4: 100% pure core, testable without a DB — the bulk of the logic is frozen there.
- Step 5: the only SQL adapter; `integration`-tagged tests.
- Steps 6–7: assembly, unit tests with fakes (no DB).
- Step 8: real DB + user documentation.

## Watch points (already settled in `SHRINK.md`, don't reinvent)

- **KILL of one's own session**: always via `Executor.Kill` (pool), never on the executing
  connection (design §8.5).
- **FULL log**: clean refusal, **never** an automatic `BACKUP LOG` (design §5.2).
- **No MAXDOP** on `DBCC SHRINKFILE`; **no `MAX_DURATION`** on the shrink's WALP.
- **`targetfreespace`** = % of the **used** space (`final = ceil(used × (1 + N/100))`).
- **`files: all`** expanded sequentially (never two files of a filegroup in parallel).
- Decision core as **pure functions** (the `runLoop`/`supervise` pattern) to stay testable
  without a DB.
