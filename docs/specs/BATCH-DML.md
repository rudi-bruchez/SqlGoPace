# BATCH-DML — batched `UPDATE` / `DELETE`

> Source of truth for the intended behavior of batched DML operations.
> **It1 + It2 + It3 (live TUI) implemented.** It1: `batch_update`/`batch_delete`, `predicate`
> strategy, declarative `set:`/`where:` plus the `set_raw:`/`where_raw:` escape hatch, adaptive
> calibration, reaction/monitoring reuse, preflight (permission + RCSI advisory), RCSI/SI in `ServerInfo`. It2:
> `key_range` strategy (single integer key) with a **persistent cursor** (`.op<i>.wm` sidecar in
> `02.processing/`) for crash resume, key inference via `ClusteringKeyColumns`. It3:
> **live progress** — an engine *step sink* (`WithStepSink`, see `docs/specs/progress-tui.md`) feeds
> stdout (`[i/N] cmd target — started/… in Xs`) and the TUI (op counter i/N, live timer), plus a live
> batch line (rows done/estimated, %, batch size, rows/s; `BatchDMLProgress` extended with
> `RowsPerSec`). Remaining It3 / It4 (deeper RCSI-aware calibration, escalation DMV surfacing,
> exactly-once, composite/non-integer keys): see §7.

## 1. Goal and context

Today SqlGoPace only executes **DDL** (index rebuild/reorganize, compression, shrink,
checkdb, statistics). A recurring DBA need on those same multi-TB 24/7 databases is a
**massive one-shot DML statement** — "set a column to a value across a whole table", or
"delete everything (or a large subset) from a table" — which on a busy production
database is dangerous for the reasons SqlGoPace already handles on the DDL side, **plus two
specific to DML**:

- **Lock escalation.** A large `UPDATE`/`DELETE` takes row/page **X (exclusive) data locks**;
  as soon as a single statement holds ~5000 locks on an object, SQL Server
  **escalates to a table-level X lock**, freezing all concurrent access to the entire table.
- **Log explosion.** One statement = one transaction; the log cannot be truncated
  until it commits, so a multi-hour DML grows the log without bound.

The remedy is the classic pattern: **split the statement into a loop** of small batches committed
individually, each kept under the escalation threshold, sampling the server between
batches and reacting to pressure — which is **architecturally identical to the existing
shrink driver**.

This is also where the **RCSI question finally matters**: unlike DDL (schema locks,
where RCSI is orthogonal — see the `CheckServer` discussion), a DML batch conflicts on
**data locks**, and RCSI directly changes how disruptive escalation is. This
spec therefore **surfaces and uses** the database's RCSI/SI state.

## 2. Why the shrink driver is the template (reuse map)

A shrink is already "a chunked `DBCC SHRINKFILE` loop, not a single DDL statement"
with a **second, parallel driver** (`ShrinkRunner`) that does not go through `MonitoredRunner`. A batched
DML has the same shape and reuses the same seams:

| Seam | Shrink | Batch DML |
|---------|--------|-----------|
| Operation struct | `ddl.Shrink` (`internal/ddl/manifest.go`) | new `ddl.BatchDML` |
| SQL generated at run time | `ShrinkChunkSQL` built per chunk in the loop | `BatchDMLChunkSQL` built per batch |
| Driver | `ShrinkRunner` + `ShrinkDriver` iface, `WithShrinkRunner` | `BatchDMLRunner` + `BatchDMLDriver`, `WithBatchDMLRunner` |
| Read iface | `ShrinkReader` (`internal/run/shrink.go`) | new `BatchDMLReader` |
| Step math (pure) | `shrink_calc.go` `InitialStepMB`/`AdjustStepMB`/clamp | new `batch_calc.go` `InitialBatchRows`/`AdjustBatchRows`/clamp |
| Engine dispatch | `processOne` type-asserts `ddl.Shrink` → `e.shrink.Run(...)` | type-assert `ddl.BatchDML` → `e.batchDML.Run(...)` |
| Reaction/monitoring | `pumpSamples` + `supervise` + `ReactionSink` + `Capabilities` + `IgnoreSource` | **reused as-is** |
| Cancel safety | `CancelSafe: true` (commits per chunk) | `CancelSafe: true` (commits per batch) |
| Config | `ShrinkConfig` → `ShrinkTuning` | `BatchDMLConfig` → `BatchDMLTuning` |

Batched DML is **better than shrink in one respect**: it has a natural **resume point**
(deleted rows stay deleted / the cursor advances), so it can be made **genuinely
crash-resumable**, which shrink is not.

---

## 3. Manifest shape

Two discriminants (`batch_update`, `batch_delete`) decode into a single `BatchDML` struct — mirroring
`Shrink`, which carries `type: data|log` and whose `CommandType()` returns
`shrink_data`/`shrink_log`.

```yaml
operations:
  # Idempotent UPDATE of an entire table to a literal value — the flagship case.
  - operation: batch_update
    schema: dbo
    table: Orders
    set:                                 # column -> scalar literal (string/number/bool/null)
      Status: 'Archived'
    where:                               # optional; list of simple conditions, AND-ed
      - { column: Status, op: '=', value: 'Pending' }
    batch:
      strategy: predicate                # predicate (default) | key_range
      key: OrderID                       # key_range only; asserts the clustering key, which it defaults to
      initial_rows: 5000                 # optional; auto-calibrated otherwise
    options:
      maxdop: 1                          # the only "WITH" option applicable to DML

  # Batched DELETE of a subset (or, with confirmation, of the whole table).
  - operation: batch_delete
    schema: dbo
    table: AuditLog
    where:
      - { column: CreatedAt, op: '<', value: '2024-01-01' }
    batch: { initial_rows: 10000 }

  # Raw SQL escape hatch, guarded (DBA-signed manifests; injected verbatim).
  - operation: batch_update
    schema: dbo
    table: Invoice
    set_raw: "Status = 'Closed', ClosedAt = SYSUTCDATETIME()"   # mutually exclusive with set:
    where_raw: "Status = 'Open' AND DueDate < '2024-01-01'"     # mutually exclusive with where:
```

### Field rules (`Validate()`, fail-fast at load — like the existing op validation)

- Exactly one of (`set:` | `set_raw:`) for `batch_update`; **neither** for `batch_delete`.
- At most one of (`where:` | `where_raw:`).
- `set:` values are **scalar literals only** (no column reference) →
  **idempotence** guaranteed.
- `op` ∈ a small allow-list: `=`, `<>`, `<`, `<=`, `>`, `>=`, `IS NULL`, `IS NOT NULL`, `IN`.
- `set_raw` / `where_raw` are non-empty strings injected **verbatim** into the generated
  statement. This is an explicit, documented exception to the "never raw SQL" principle, justified
  by the fact that manifests are written by DBAs (same trust boundary as a `.sql`
  script), and it has consequences (see §4).
- **"Whole table" guard**: a `batch_delete` (or `batch_update`) **without** `where`/`where_raw`
  requires an explicit `confirm_full_table: true`, otherwise preflight **fails**. (Avoids the
  accidental purge; aligned with how destructive intent is gated elsewhere.) The message
  reminds that **`TRUNCATE TABLE`** is the right tool for an unconditional full wipe when
  FKs/triggers/access allow it — batched DELETE is for when TRUNCATE is not viable.

  **Amended (0.21.0): as written above this guard did not guard.** It was a presence test on
  a YAML key, so any filter that was *written* satisfied it however little it excluded —
  `where_raw: "1=1"`, or `where: [{column: Id, op: ">=", value: 0}]` on an identity column,
  deleted every row with no confirmation and no preview. What matters is whether the filter
  spares a row, not whether it exists, so preflight now asks the server:
  `BatchUnmatchedRowsSQL` counts the rows the filter would leave alone, capped at 1000.
  Zero fails, a handful warns with the number, the cap passes. The probe is skipped when
  `confirm_full_table` is already set — there is nothing left to establish — and the CASE
  wrapper in the generated SQL is load-bearing: a plain `NOT (pred)` drops the rows where the
  predicate is UNKNOWN, which the DML spares too.

---

## 4. Idempotence & crash resume (the core safety model)

A batch is **committed individually**, so a crash/kill loses at most the in-flight batch. Whether
that batch is safely **replayable** on resume depends on idempotence:

| Case | Idempotent? | Resume strategy |
|-----|--------------|----------------------|
| `batch_delete` (any predicate) | **Yes** (a deleted row stays deleted) | predicate loop, no resume point |
| `batch_update` with literal `set:` | **Yes** (`col = 'X'` identical on replay) | predicate (`WHERE col <> 'X'`) **or** key_range + cursor |
| `batch_update` with `set_raw` referencing the row (`Counter = Counter + 1`) | **No** | **not safely replayable** |

Rules:

- **Predicate strategy (default).** `… TOP(@n) … WHERE <predicate>` looping until
  `@@ROWCOUNT = 0`. For DELETE and literal UPDATE, the predicate is **self-limiting** (the rows
  it just changed no longer match), so a resume simply continues — **no resume
  point to manage, safe by construction.** Cost: every batch re-evaluates the predicate, so the
  predicate column must be indexed for large tables.
- **key_range strategy (opt-in).** Walks a unique/orderable `key` in ascending ranges
  (`WHERE key > @cursor ORDER BY key`), persisting the processed `MAX(key)` as a resume point.
  Predictable at multi-TB scale and re-scan free, **but** the cursor lives in the processing sidecar (a
  file), not in the same transaction as the SQL → resume is **at-least-once** on the
  boundary batch. So key_range is only offered **for an idempotent SET** (literal `set:`), where
  re-applying the boundary batch is a no-op.

  **The "unique" in that first line is load-bearing, and was unenforced until v0.26.0.**
  `BatchKeyRangeUpdateSQL` emits no `TOP`: the range `(cursor, next]` holds `batch_rows`
  rows only when one key means one row. `sys.indexes.index_id = 1` selects the clustered
  index, which SQL Server does not require to be unique, so a clustered index on a
  repeating column let one batch cover an unbounded number of rows — a single UPDATE
  escalating to a table X lock, which is the outcome this whole design exists to avoid.
  `resolveKeyColumn` now reads `i.is_unique` and refuses the table. The alternative
  considered and rejected was bounding the statement with `UPDATE TOP (n)`: alone it
  silently skips rows the watermark then advances past, and with a re-run-until-zero loop
  it fails to terminate whenever the filter is not self-limiting (`confirm_full_table`
  with no `where`) — the v0.20.0 non-termination class, reintroduced. Uniqueness bounds
  the range exactly, with neither failure mode.
- **Non-idempotent `set_raw`** is allowed but **marked non-resumable**: the op runs without a
  resume point, and a crash mid-run leaves the table partially updated for **manual
  reconciliation** (recorded in the `.log`). The MVP does **not** attempt exactly-once for this
  case (it would need a database-side control table updated within the batch's transaction —
  deferred to a later iteration if asked for).

**Superseded (0.20.0): the preflight `WARN` above was never implemented**, and the
"self-limiting by construction" claim was not true in two ways. Both are now closed, but the
reasoning is worth keeping, because it is the same reasoning that made the gap invisible.

1. **A NULL target inverted the self-limit.** `set: {Col: null}` produced
   `WHERE (Col IS NULL OR Col <> NULL)`. `Col <> NULL` is `UNKNOWN` for every row, so the
   clause reduced to `Col IS NULL` — true for exactly the rows the batch had just set. Every
   completed row re-entered the match set, so the loop's only exit ("the last batch affected
   zero rows") was unreachable. The clause written to guarantee termination guaranteed
   non-termination. The generator now emits `Col IS NOT NULL` for a NULL target
   (`internal/ddl/batch.go`).
2. **A `set_raw` that does not consume its own filter has no self-limit at all.**
   `set_raw: "Counter = Counter + 1"` with `where_raw: "Status = 'A'"` passes validation —
   the check at load is that *a* predicate exists, never that it is self-consuming, which is
   not decidable from the text. `selfLimitClause` returns `""` for any raw SET by design. So
   the guarantee this section claims never covered the `set_raw` case.

The backstop is a **cumulative-row ceiling** on the predicate loop
(`predicateRowCeiling`, `internal/run/batch_calc.go`): twice the table's row estimate, or
1,000,000 when the estimate is unavailable. A terminating predicate affects each row at most
once, so crossing it proves the predicate is not self-consuming. The operation **fails**
(`ErrRowCeiling`) rather than stopping cleanly — the committed batches are real and a human
has to decide about them. The `key_range` walk needs no ceiling: its watermark is strictly
ascending, so it terminates by construction.

A preflight `WARN` naming a non-idempotent `set_raw` is still worth having — it would catch
case 2 before any row is written rather than after the ceiling's worth — but it is a heuristic
over raw SQL text, whereas the ceiling is a proof. Order them that way if both are built.

Default strategy per verb: `batch_delete` → `predicate`; `batch_update` → `predicate`
(switch to `key_range` when the table is huge and the predicate is not selectively indexed).

---

## 5. RCSI / snapshot isolation integration (the through line)

A DML batch holds **data X locks**; RCSI/SI changes who suffers:

- **RCSI OFF (READ COMMITTED takes S locks):** escalation to a **table X lock blocks all
  readers** → the application freezes on that table. Keeping each batch **under the escalation
  threshold (~5000 locks)** and honoring the "good citizen" reaction to blocking is **critical**.
- **RCSI / SI ON (readers use row versions):** readers are **not blocked** by the batch's
  row/page X locks, so escalation is far less disruptive to *reads* (it still
  blocks *writers*). The dominant cost shifts to the **tempdb version store**:
  every modified/deleted row generates a version retained until the batch commits → small
  batches + commit-per-batch bound version store/tempdb growth.

Concretely, this spec adds RCSI/SI awareness in three places:

1. **`mssql.ServerInfo`** gains `RCSIEnabled bool` and `SnapshotIsolation bool`, read in
   `DetectServer` from `sys.databases` (`is_read_committed_snapshot_on`,
   `snapshot_isolation_state_desc`). `CheckServer` shows them on the server facts line, next to
   the existing `adr=`/`recovery=`.
2. **Preflight advisory** (`WARN`, non-blocking) for a `batch_*` op: if **RCSI OFF**, warn that
   escalation will block readers and recommend a conservative `initial_rows` (< escalation threshold);
   if **RCSI/SI ON**, flag tempdb version store growth and to watch tempdb.
3. **Default batch calibration** keyed on RCSI: RCSI **off** → the initial auto batch is capped under
   the escalation threshold (e.g. ≤ 4000 rows); RCSI **on** → the cap can relax (the log
   and version store, not reader blocking, become the governing signals). Default
   only — an explicit `initial_rows` always wins.

Escalation is otherwise handled **reactively**, like everything else: the adaptive calibrator shrinks the
batch as soon as `LCK_M_*` pressure / blocking appears between batches (no need to pre-read the
escalation DMVs). We do **not** emit `ALTER TABLE … SET (LOCK_ESCALATION = DISABLE)` automatically — that
alters the table and is the operator's call; calibrating under the threshold is the non-intrusive lever.

**Version store / tempdb** pressure under RCSI is handled by the cross-cutting
`TEMPDB-GUARD.md` guardrail (preflight no-start + runtime alert; stop only if tempdb is full **and**
our session is the material contributor). Since the version store is not cheaply attributable per
session, this case stays **alert-only** on the DML side.

---

## 6. Driver design — `internal/run/batch_dml.go` (modeled on `shrink.go`)

- **`BatchDMLReader`** (narrow interface, satisfied by `*mssql.Conn`):
  - `ClusteringKeyColumns(ctx, schema, table) ([]mssql.KeyColumn, error)` — **new** read of the
    clustering key/PK columns in key order (extend `internal/mssql/indexes.go`; via
    `sys.index_columns`). Needed for `key_range` and for the `batch.key` default.
  - `EstimateRows(ctx, schema, table) (int64, error)` — reuse `ObjectInventory`
    (`analysis.go`) for a row-count estimate (progress %, initial calibration).
  - `SessionWaits(ctx, spid) ([]mssql.SessionWait, error)` — reused, for calibration deltas.
  - (log-reuse reads + active log floor reused from the shrink read set.)
- **`BatchDMLRunner`** struct + `NewBatchDMLRunner(exec, reader, sampler, clk, cfg, opts...)`,
  `WithBatchDMLProgress(...)` — same construction pattern as `ShrinkRunner`.
- **`Run(ctx, op ddl.BatchDML, res ddl.ResolvedOptions, ignore IgnoreSource, sink ReactionSink)
  ([]BatchDMLResult, error)`** — same signature family as `ShrinkDriver.Run`. The loop:
  1. estimate rows, pick the initial batch size (`InitialBatchRows`, RCSI-capped).
  2. loop: build the batch SQL (`BatchDMLChunkSQL`), sample waits before, execute the
     batch under `supervise` (execution + sampling goroutines, cancel-safe), sample after,
     read `@@ROWCOUNT`.
  3. stop when `@@ROWCOUNT == 0` (predicate) or the cursor passes the end (key_range).
  4. between batches: `awaitRelief` on blocking/log pressure (reused), advance/clamp via
     `AdjustBatchRows`, persist the cursor for key_range, honor the no-progress backoff +
     `SelfWaitTimeout`.
- **`batch_calc.go`** (pure, tested without a database): `InitialBatchRows(estRows, rcsi, t)`,
  `AdjustBatchRows(size, elapsed, WaitDeltas, t)` (shrinks on WRITELOG/PAGEIOLATCH/`LCK_*`/blocking;
  grows when quiet and under the target batch duration), `clampBatchRows(min,max)`.

### DML reaction set (via the existing `reaction.go`)

The DDL-specific options (`ONLINE`/`RESUMABLE`/`WAIT_AT_LOW_PRIORITY`) **do not apply** — a
DML batch is not resumable in the SQL sense. Useful reactions between/during batches:

- **continue** / **shrink the batch** (adaptive calibrator) / **wait for relief** (`awaitRelief`) /
  **clean stop + resume point** (work done is committed; resume later).
- A **kill** mid-batch only rolls back the small current batch → cheap.
  `Capabilities.CancelSafe = true`; `Resumable = false`; `Ignore`/`MaxBlock` reused (so
  `ignore_blocked_sessions` and the per-op `max_block_minutes` cap work for DML too,
  for free).

---

## 7. Pipeline wiring (what each layer touches)

- **`internal/ddl/manifest.go`** — add the `BatchDML` struct (+ `Set map[string]any`, `SetRaw`,
  `Where []Condition`, `WhereRaw`, `Batch BatchSpec`, `ConfirmFullTable bool`, internal `Verb`);
  implement `CommandType()` (`batch_update`/`batch_delete`), `Target()` (`schema.table`, empty
  `Name`), `Validate()`; add `case "batch_update"` / `case "batch_delete"` to `decodeOperation`
  (sets `Verb`).
- **`internal/ddl/resolve.go`** — add `case BatchDML: return o.Options` to `overridesOf`; like
  `Shrink`, exempt it from index-style option resolution (only `maxdop` is relevant).
- **`internal/ddl/generate.go`** — `generateBatchDML` (indicative statement for the plan/report) +
  the runtime `BatchDMLChunkSQL(op, batchSize, watermark, res)`; reuse `quoteIdent`/`qualified`/
  `nLiteral`; render the declarative `set:`/`where:` into T-SQL (quoted literals, predicate from the
  `op` allow-list) or insert `set_raw`/`where_raw` verbatim.
- **`internal/ddl/plan.go` / `expand.go`** — generic, **pass-through** (no change;
  `expand` already forwards non-`RebuildIndex` ops, but **add a regression test** that
  `BatchDML` survives expansion — the bug class already hit with `OnFailure`).
- **`internal/ddl/matrix.go` / `ddl_compatibility.yaml`** — add `batch_update`/`batch_delete`
  with `maxdop` only (no online/resumable/walp).
- **`internal/mssql`** — extend `ServerInfo`/`DetectServer` (RCSI/SI); add `ClusteringKeyColumns`
  (`indexes.go`); add an `UPDATE`/`DELETE` permission probe on the table (see preflight).
- **`internal/preflight/preflight.go`** — `CheckBatchDML`: table exists (reused), `set` columns
  exist + types compatible (reuse `ColumnExists`, add a type check),
  `batch.key` exists & is unique for `key_range` (**moved**: this landed in the driver's
  `resolveKeyColumn`, not in preflight — it needs the clustered-key read the walk already does,
  and enforcing it in one place beats two), **UPDATE/DELETE permission** on the table, whole-table
  guard, **WARN** on FK reference / DELETE trigger, and the RCSI advisory. (The database-/file-scoped
  skip logic is unchanged; `batch_*` ops are schema.table-scoped so they take the normal
  existence path.)
- **`internal/run/engine.go`** — `BatchDMLDriver` interface + `WithBatchDMLRunner`; branch in
  `processOne` `if b, ok := step.Operation.(ddl.BatchDML); ok && e.batchDML != nil { … }`; map
  `[]BatchDMLResult` → a new `report.BatchDMLReport` (rows affected, batches, final batch
  size, stop reason, cursor); persist/restore the cursor in `finalizePartial`/recovery for
  `key_range`.
- **`internal/config/config.go`** — `BatchDMLConfig` → `BatchDMLTuning` (initial/min/max rows, target
  batch duration, no-progress backoff, self-wait timeout, log-reuse-wait timeout — same shape as
  `ShrinkConfig`).
- **`cmd/sqlgopace/main.go`** — wire `WithBatchDMLRunner(NewBatchDMLRunner(...))` into
  `buildEngine`; `--explain`/plan shows the strategy, the key, the row estimate, the
  idempotence/resume note, and the RCSI advisory.

---

## 8. Drawbacks / limits (deliberate, documented)

1. **Non-idempotent `set_raw` is not exactly-once** on crash (at-least-once boundary)
   — marked non-resumable; manual reconciliation. Exactly-once (database-side control table)
   is deferred.
2. **Cascading DELETEs / DELETE triggers** can make a batch touch far more than `@n` rows
   (`ON DELETE CASCADE` FKs, trigger side effects) → preflight warns; the calibrator reacts
   anyway, but the operator must account for cascade fan-out when setting `initial_rows`.
3. **`set_raw`/`where_raw` injected verbatim** — no parsing/validation. Trusted-manifest
   model only (written by DBAs), never end-user input.
4. **The predicate strategy may re-scan** on every batch if the predicate column is not
   indexed → slow on large tables; choose `key_range` there.
5. **Rows modified concurrently by the application**: predicate naturally re-includes any row
   that still matches; key_range processes each key once (a row re-introduced behind the
   cursor with the old value is missed) — acceptable for "converge a column to a
   value"; documented.
6. **No `TRUNCATE` substitution** — batched DELETE is deliberately logged row by
   row; for an authorized unconditional wipe, the operator should use `TRUNCATE` (preflight
   says so).

---

## 9. Phasing

- **It1 (MVP).** `batch_update` + `batch_delete`; declarative literal `set:`/simple `where:` **and**
  the guarded `set_raw:`/`where_raw:` escape hatch; **predicate** strategy only (safe by
  construction for DELETE + literal UPDATE; `set_raw` runs non-resumable with a WARN);
  adaptive calibration (`batch_calc.go`); reaction/monitoring reuse; log-reuse wait;
  preflight (existence, permission, whole-table guard, RCSI advisory); `ServerInfo` RCSI/SI +
  `CheckServer` line. Report + `--explain`.
- **It2.** `key_range` strategy with a persistent **cursor** for crash resume (idempotent
  updates); `ClusteringKeyColumns`; default key inference; recovery wiring; composite key
  support.
- **It3.** RCSI-aware default calibration; live TUI integration (rows/s, current batch
  size, cursor/progress %, stop reason); optional escalation DMV surfacing.
- **It4 (refinements).** Exactly-once for non-idempotent updates via a database-side control
  table; cascade/trigger-aware calibration; a CLI dry-run estimating batches/log per
  batch.

## 10. Tests (no database; `-race`)

- **ddl:** parse/validate `batch_update`/`batch_delete` (set vs set_raw exclusivity; delete rejects
  set; where allow-list; empty raw rejected; whole-table requires confirm); `generate` round-trip
  of both strategies and of raw insertion; `expand` pass-through regression; matrix keeps
  only `maxdop`.
- **run (pure):** `InitialBatchRows`/`AdjustBatchRows`/clamp table-driven (RCSI cap; shrinks
  on WRITELOG/`LCK_*`/blocking; grows when quiet); driver loop over a fake `BatchDMLReader` +
  fake `Sampler` — the predicate loop stops at `@@ROWCOUNT=0`, key_range advances the cursor, a stop
  under pressure is clean and work done is committed, no-progress backoff + self-wait timeout.
- **preflight:** missing column/permission fails; whole-table without confirm fails; RCSI-off
  emits the advisory; FK/trigger WARN.
- **mssql:** `ClusteringKeyColumns` and the RCSI/SI detection queries behind the
  `integration` tag.

## 11. Verification (end-to-end, throwaway database)

1. `make test` green.
2. Hand-write a `batch_delete` against a table seeded with several million rows;
   `--explain` shows the strategy, the batch estimate, and (on a non-RCSI database) the lock
   escalation advisory.
3. Run it; from a 2nd session, confirm that reads are/are not blocked depending on the
   RCSI state, that the log stays bounded between batches, and that a `KILL` mid-run leaves a clean
   partial (a predicate run resumes by re-launching with no double effect; DELETE simply continues).
4. Redo a literal `batch_update` with `strategy: key_range`; interrupt it; confirm that the
   recovery manifest carries the cursor and that the resumed run continues mid-table.
