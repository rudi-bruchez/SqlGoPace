# Contended-object capture + `plan --confirmed`

Status: design approved, pending implementation plan.
Date: 2026-07-28.

## Motivation

A data shrink of a live database can stall short of its target: `DBCC SHRINKFILE`
holds a `Sch-M` lock on the object whose pages it is relocating, contends with the
workload's `Sch-S` locks, times out under `WAIT_AT_LOW_PRIORITY`, and the engine
finalizes the manifest `INCOMPLETE` with no forward progress. The PRODDB run of
`020_shrink_proddb_data.yaml` did exactly this: 28 minutes, 0 MB gained, dominated
by `Locking` waits (547s), with the blocked-session capture showing eight readers
waiting `LCK_M_SCH_S` on `MEASUREMENT` / `DISPATCHCASESOLUTION`.

When the shrink blocks other sessions, SQL Server knows precisely which object it is
holding up others on — the object it holds a `Sch-M` lock on, i.e. the one it is
trying to relocate and cannot get past. That object is an **empirically confirmed tail
blocker**. Today we do not record it: the `.blocked.yaml` capture records the *victims*
(their identity and query text), from which the contended table can only be *inferred*.

This design records the confirmed object directly and feeds it into the next planning
iteration, so the pre-shrink reorganize pass can prioritise the objects the shrink
actually fought over.

### Relationship to the pre-shrink reorganize design

The companion design `2026-07-28-pre-shrink-reorganize-design.md` **guesses** candidates
by page density / size **before** the run and states its own limit (§3):

> **Candidates, not confirmed blockers.** The advisory identifies heaps by *property*
> (size/density), not by *position* in the file. Only a heap in the file's tail extents
> actually impedes the shrink... Confirming tail position needs
> `sys.dm_db_database_page_allocations`, which is heavy at database scope.

This design supplies the missing empirical signal: an object that appears in a run's
contended-object capture **is** confirmed to have blocked the shrink — closing the
"candidates, not confirmed" gap for heaps without ever running `page_allocations`. The
two are complementary: the density scan proposes, the capture confirms and prioritises.

## Scope

- **Shrink runs only.** The injection point (the engine's shared reaction `sink`) is
  common to all operations, but the capture is emitted only for `ddl.Shrink` — the sole
  consumer is the pre-shrink planner. Generalising to other operations is a non-goal
  here.
- **Connected database only**, like the pre-shrink pass. Multi-database is a non-goal.
- **Partial evidence, by design.** The shrink stalls on the *first* blocker it cannot
  get past and dies, so the capture only sees objects encountered up to that point. The
  planner therefore **augments** its density selection with the capture — it never
  **replaces** it (see §4).
- **No change** to the shrink driver, the `shrink` operation, or the
  `reorganize_index` / `rebuild_index` / `rebuild_heap` operations. This design only
  adds a run-side read + sidecar and a planner-side input.

## 1. Run-side read — `HeldObjectLocks`

A new read on the blocker reader in `internal/mssql` (a `*mssql.Conn` in production,
fakes in tests), mirroring the existing `ActiveSessions`:

```go
// HeldObjectLocks returns the user objects on which spid currently holds a granted
// Sch-M lock — the objects a running shrink is relocating and holding others up on.
func (c *Conn) HeldObjectLocks(ctx context.Context, spid int) ([]LockedObject, error)
```

Query:

```sql
SELECT l.resource_associated_entity_id,        -- = object_id for resource_type OBJECT
       l.resource_database_id,
       OBJECT_SCHEMA_NAME(l.resource_associated_entity_id, l.resource_database_id),
       OBJECT_NAME(l.resource_associated_entity_id, l.resource_database_id),
       l.request_mode
FROM sys.dm_tran_locks l
WHERE l.request_session_id = @spid
  AND l.resource_type   = 'OBJECT'
  AND l.request_status  = 'GRANT'          -- we HOLD it (victims WAIT — the mirror)
  AND l.request_mode LIKE 'Sch-M%';        -- the relocation lock
```

`LockedObject` carries `ObjectID int64`, `Schema string`, `Table string`, `Mode string`.

Why this is robust (verified against Microsoft's lock documentation):
- `Sch-M` / `Sch-S` are **object-level** locks; for `resource_type = OBJECT` the
  `resource_associated_entity_id` **is** the `object_id` (a bigint) — no fragile
  `wait_resource` string parsing.
- `OBJECT_SCHEMA_NAME` / `OBJECT_NAME` with the `database_id` argument resolve
  **cross-database**, so the read is correct regardless of the sampler connection's
  current database context.
- `request_status = 'GRANT'` selects the lock we *hold*; it is the exact mirror of the
  victims' `LCK_M_SCH_S` waits (`request_status = 'WAIT'`).

An object-scoped `Sch-M` covers the dominant contention (7 of the 8 victims in the
PRODDB run waited `LCK_M_SCH_S`); the one `LCK_M_IS` victim is intentionally not
represented here — we capture only what the shrink holds, which is the signal we want.

## 2. Capture accumulation and injection

`internal/run` gains a `contendedCapture` accumulator, mirroring `blockerCapture`:
distinct objects keyed by `object_id`, in first-seen order, each with `times_blocked`
(count), `first_seen`, `last_seen`.

Injection point: the reaction `sink` in `processOne` (`engine.go`), where
`captureBlockers` already runs on `pause` / `cancel` / `abort` using `e.session.SPID()`
— which is the shrink's executing SPID (it is what produced the existing
`.blocked.yaml`). At the same instant, **only when the operation is `ddl.Shrink`**, the
engine calls `HeldObjectLocks(ctx, spid)` and folds the result into the accumulator. No
new sampling cadence — it reuses the existing reaction snapshots.

Because a `Sch-M` lock is transient (held while an allocation unit is relocated), a
single snapshot may return empty even if the shrink was blocking a moment earlier. This
is accepted: capture runs across every reaction, so recurrent objects surface. Like the
existing blocked-session capture, it is best-effort.

## 3. Sidecar `.contended.yaml` (run output)

Written next to the shrink manifest, same base name + `.contended.yaml` suffix —
distinct from the human `.blocked.yaml`, which is unchanged. Flushed into
`02.processing` during the run and relocated to the terminal directory
(`03.done` / `04.failed`) on finalize, mirroring `flushCapture` / `relocateCapture`. A
commented header for humans; a machine `observed:` block the planner parses.

```yaml
# Contended-object capture for 020_shrink_proddb_data.yaml
# Objects this shrink held a Sch-M lock on while blocking other sessions —
# i.e. the objects it was relocating and could not get past. These are
# EMPIRICALLY CONFIRMED tail blockers (partial: the shrink stops at the first).
# Feed this to the planner:  sqlgopace plan --confirmed <this file>
database: PRODDB
observed:
  - object_id: 261575970
    schema: dbo
    table: MEASUREMENT
    lock_mode: Sch-M
    times_blocked: 3
    first_seen: "2026-07-28T11:10:09Z"
    last_seen:  "2026-07-28T11:19:09Z"
  - object_id: 262004013
    schema: dbo
    table: DISPATCHCASESOLUTION
    lock_mode: Sch-M
    times_blocked: 1
    first_seen: "2026-07-28T11:14:29Z"
    last_seen:  "2026-07-28T11:14:29Z"
```

Content decisions:
- **No heap-vs-index classification here.** The sidecar carries object identity only
  (`object_id` + `schema.table`). `plan` reconnects and already reads index density and
  heaps — it classifies heap-vs-index there, for free, from a single source of truth.
  This keeps the run side minimal (no catalog read during the run).
- **`object_id` is included** alongside the name: it is the reliable join key on the
  planner side (a rename between run and plan stays consistent by id; the name is for
  humans).
- **`database:`** at the top lets `plan` reject a sidecar for a database other than the
  connected one (a guard).
- Ordered by `first_seen` (encounter order ≈ tail order).
- **Empty capture → no file** (like `flushCapture` when nothing was held).

### `.log` pointer line

On an `INCOMPLETE` shrink whose capture is non-empty, the run report gets a single
pointer line — not the detail — e.g.:

```
  contended objects: 2 — see 020_shrink_proddb_data.yaml.contended.yaml
```

This surfaces the evidence at the point of failure without duplicating the sidecar into
the report.

## 4. Planner integration — `plan --confirmed <path>`

`plan` gains an optional `--confirmed <path>` flag. Absent → the pre-shrink behavior is
unchanged. Present → `plan` loads the sidecar, asserts `database:` equals the connected
database (else an actionable error), and passes the set of confirmed `object_id`s (with
`times_blocked`) into the generation.

The effect is to **augment**, not replace, the DB-wide density selection (the partial
-evidence constraint from Scope). In the pure `internal/maint` selection function:

1. **Classification is free at plan time.** For each confirmed `object_id`, `plan`
   already knows from its density scan + heap read whether it is a heap or an
   index-bearing object.
2. **Confirmed index-bearing object:**
   - already selected by density → its `reorganize_index` op(s) move to the **head** of
     the manifest, annotated `# confirmed blocker (times_blocked=N)`;
   - not selected by density (dense but contended) → a `reorganize_index` is added
     anyway for its eligible indexes, annotated
     `# confirmed blocker — added despite density`. This is the safety net.
3. **Confirmed heap:** stays **advisory-only** — a `rebuild_heap` is offline/blocking on
   Standard and the pre-shrink design (§3) declines to auto-generate it. Its entry in
   the `.heaps.yaml` advisory is upgraded from *candidate* to **CONFIRMED**, with
   reinforced wording (`observed blocking the shrink at HH:MM, times_blocked=N`). A
   confirmed heap lifts the §3 "candidates, not confirmed" limit **without** running
   `page_allocations`.

Prioritisation among confirmed objects: by `times_blocked` descending (most recurrent
first), then `first_seen`.

What does not change: the density logic itself, the manifest format, `decide.go`.
`--confirmed` is a pure input modifier to the selection, fully testable without a
database. And `plan` reads only the machine `.contended.yaml`, never the human
`.blocked.yaml` — the "SqlGoPace never reads *that* file back" convention holds.

Accepted consequence: if a confirmed object is a heap in a database with no LOB (this
DB's case), the pre-shrink pass can generate nothing automatic for it — the reinforced
advisory is the only lever. The design makes this visible rather than hiding it.

## 5. Units and boundaries

| Unit | Package | Responsibility | Depends on |
|---|---|---|---|
| `HeldObjectLocks(ctx, spid)` | `internal/mssql` | read objects held in Sch-M by our SPID, resolved | `sys.dm_tran_locks` |
| `contendedCapture` | `internal/run` | dedup by `object_id`, count, first/last seen | pure |
| `renderContended` + flush/relocate | `internal/run` | write/relocate `.contended.yaml` | os; mirrors `capture.go` |
| `sink` injection | `internal/run` (`engine.go`) | call the capture on pause/cancel/abort **if `ddl.Shrink`** | the two above |
| `.log` pointer line | `internal/report` | one-line pointer at `INCOMPLETE` | capture count |
| parse `.contended.yaml` + db guard | `internal/maint` (loaded in `cmd`) | load the sidecar, validate `database:` | pure (I/O upstream) |
| augmented selection | `internal/maint` | prioritise / add / annotate per confirmed set | pure |
| `--confirmed` flag | `cmd/sqlgopace` | wire path → parse → selection | plumbing |

## 6. Testing

All decision logic lands in pure code (no database):

- **Accumulator:** same `object_id` across two snapshots → one entry, `times_blocked=2`,
  `last_seen` advanced; two distinct objects → two entries in `first_seen` order.
- **`renderContended`:** golden file of the YAML (header + `observed:`), including the
  empty case (nothing held → no file written).
- **Parse + guard:** valid sidecar; `database:` ≠ connected database → error; missing /
  malformed file → actionable error.
- **Augmented selection (the core):**
  - confirmed object already density-selected → its reorganize moves to head + annotation;
  - confirmed *dense* object (not density-selected) → reorganize added with the
    "despite density" note;
  - confirmed **heap** → **never** a generated reorganize/rebuild; advisory entry becomes
    CONFIRMED;
  - ordering by `times_blocked` desc then `first_seen`;
  - **without `--confirmed`** → output identical to the current pre-shrink (non
    -regression).
- **`HeldObjectLocks`:** covered by the `integration`-tagged tests (real DMV), like the
  other `internal/mssql` reads — not in unit tests.

## Non-goals

- Generalising the contended-object capture to non-shrink operations.
- Scoped `page_allocations` for tail-position confirmation (remains the §3 deferred
  enhancement).
- Auto-generating `rebuild_heap` (or any session-policy rules).
- `plan` reading the human `.blocked.yaml`.
- Any change to the shrink driver, the shrink operation, or the reorganize/rebuild
  operations themselves.
