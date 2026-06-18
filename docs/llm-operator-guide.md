# SqlGoPace — LLM Operator Guide

A guide written **for a language model** (chat or agent) that helps a human operate
SqlGoPace. Goal: turn a natural-language request ("compress these indexes", "add a
NOT NULL column", "reclaim space from this data file") into a **correct manifest**,
explain usage, and warn about what to verify first.

This file is provider-agnostic: paste it into any chat, index it for RAG, or load it
as a Claude Code skill. It is the source of truth for the manifest format; when in
doubt defer to `README.md` (canonical user reference) and `specs/`.

---

## 1. What SqlGoPace is (mental model)

SqlGoPace is a **resilient DDL task runner for Microsoft SQL Server**. It does **not**
run arbitrary `.sql`. Instead it:

1. **Generates** T-SQL from a declarative **YAML manifest** (one task = an ordered list
   of operations).
2. **Injects** the right `WITH (...)` options (ONLINE / RESUMABLE / WAIT_AT_LOW_PRIORITY /
   MAXDOP / SORT_IN_TEMPDB) for the detected SQL Server **version × edition**.
3. **Monitors** locking and transaction-log pressure while the DDL runs.
4. **Reacts** with the least destructive lever available
   (`WAIT_AT_LOW_PRIORITY` → `RESUMABLE` pause/resume → `KILL`).

So your job as the assistant is to produce the **manifest** and the **guidance**, never
raw SQL and never the `WITH (...)` clause.

### Golden rules for the assistant

- **Never hand-write `WITH (...)` options** (ONLINE/RESUMABLE/WALP/MAXDOP/SORT_IN_TEMPDB).
  The engine injects them from the matrix. Only set them under `options:` when the user
  explicitly wants to **override** the automatic choice.
- **Never invent an operation type.** Only the 13 in §3 exist. If the request needs
  something else (e.g. `MERGE`, data updates, creating a table), say it is out of scope.
- **No raw SQL.** A capability that has no `operation:` type cannot be expressed.
- **Always recommend a `--dry-run --explain` first** (§7) before a real run.
- **Surface the warnings in §8** (edition, permissions, target DB, maintenance window).
- **Ask before guessing** the database, schema, or whether an operation is meant to
  defragment vs. (re)compress.

---

## 2. Manifest file — top level

```yaml
description: "Human-readable purpose of this task"   # optional but recommended
database: EXAMPLEDB        # optional; defaults to the database in the connection string
on_failure: stop           # optional; stop (default, fail-fast) | continue
operations:                # required; at least one; run SEQUENTIALLY, in order
  - operation: <type>
    ...
```

- One file = one logical task = a list of operations run **in order**.
- `database` is optional; when set it overrides the connection's default database for
  this manifest. Use it whenever the request names a database.
- `on_failure` is optional and defaults to `stop` (the first failed operation fails the
  whole manifest → `04.failed/`). Set `on_failure: continue` for a batch where each
  operation is independent (e.g. compressing many indexes): a failed operation is
  **quarantined**, the engine **continues** with the rest, and at the end writes a
  re-runnable **recovery manifest** `<name>.recovery.yaml` into `04.failed/` containing
  just the failed operations (it carries `on_failure: continue` itself, so it round-trips).
  Such a run ends as **`PARTIAL`** (counted as failed for the exit code). To retry the
  failures later, move the recovery manifest into `01.to_run/`.
- Files live in the **queue** (`01.to_run/`). See §6.

---

## 3. Operation catalog (the only valid `operation:` values)

| `operation` | T-SQL | Required fields | Optional / notes |
|---|---|---|---|
| `rebuild_index` | `ALTER INDEX … REBUILD` | `schema`, `table`, `index` | `index: ALL` rebuilds every index; `data_compression: NONE\|ROW\|PAGE`; `partition: N`; `options`. **The only way to change compression.** |
| `reorganize_index` | `ALTER INDEX … REORGANIZE` | `schema`, `table`, `index` | `partition`, `lob_compaction: true`. Always online; **no `options`**; **cannot change compression**. |
| `rebuild_heap` | `ALTER TABLE … REBUILD` | `schema`, `table` | `data_compression`; `options` (ONLINE/MAXDOP/DATA_COMPRESSION only — **no RESUMABLE/WALP**). Also rebuilds all NC indexes. |
| `create_index` | `CREATE [UNIQUE] INDEX` | `schema`, `table`, `index`, `columns` (≥1) | `unique: true`; `data_compression`; `options`. |
| `drop_index` | `DROP INDEX` | `schema`, `table`, `index` | — |
| `add_column` | `ALTER TABLE … ADD` | `schema`, `table`, `column`, `type` | `nullable: true\|false`; `default: <literal>`. No injectable options. |
| `alter_column` | `ALTER TABLE ALTER COLUMN` | `schema`, `table`, `column`, `type` | `nullable`; `options` (ONLINE 2016+). Type + nullability only. |
| `drop_column` | `ALTER TABLE DROP COLUMN` | `schema`, `table`, `column` | — |
| `add_constraint` | `ALTER TABLE ADD CONSTRAINT` | `schema`, `table`, `constraint`, `kind`, `columns` (≥1) | `kind: primary_key\|unique`; `options`. |
| `drop_constraint` | `ALTER TABLE DROP CONSTRAINT` | `schema`, `table`, `constraint` | — |
| `update_statistics` | `UPDATE STATISTICS` | `schema`, `table` | `statistic` (empty = all on table); at most one of `full_scan: true`, `sample_percent: 1..100`, `resample: true`. |
| `check_db` | `DBCC CHECKDB` | `database` | Database-scoped (no schema/table). `physical_only`, `data_purity`; `options.maxdop`. |
| `shrink` | `DBCC SHRINKFILE` (chunked) | `type` (`data`\|`log`), `targetfreespace` | `files` (`all` or a logical file name; default `all`); `targetfreespace: "10%"` or `"100MB"`. Only WALP is relevant. |

Field names are exact YAML keys. Unknown operation values are rejected at parse time.

---

## 4. Options & automatic injection (`options:`)

`options:` overrides per operation. Every field is a pointer: **omit it = "auto"**
(the engine resolves it from the version/edition matrix). Only set a field to force a
value.

```yaml
    options:
      online: true             # true | false | (omit = auto)
      resumable: true          # requires online
      wait_at_low_priority: true   # requires online
      sort_in_tempdb: true
      maxdop: 2
      ignore_blocking: true    # reaction policy, NOT a T-SQL option (see below)
```

`ignore_blocking: true` is a **reaction-policy override**, not a T-SQL `WITH` option:
when set, the engine does **not** yield this operation when it blocks other sessions — it
holds its lock to completion and **leaves the other connections blocked**. Use it to force
an important index through despite blocking (e.g. on the one index that matters in a batch).
Transaction-log protection still applies. It is per operation, so the rest of the batch keeps
the normal "yield when blocking" behavior. Pair it with `on_failure: continue` on the batch so
one forced index does not change how the others react.

**You normally leave `options` empty.** The matrix (`ddl_compatibility.yaml`) decides
what is legal and injects it. Key gates for `rebuild_index` (the common case):

| Option | Min version | Editions | Requires |
|---|---|---|---|
| `online` | 2005 | Enterprise, Azure | — |
| `wait_at_low_priority` | 2014 | Enterprise, Azure | online |
| `resumable` | 2017 | Enterprise, Azure | online |
| `data_compression` | 2008 | Enterprise, Azure | — |
| `maxdop`, `sort_in_tempdb` | 2005/2008 | Enterprise, **Standard**, Azure | — |

The most important consequence is in §8.

---

## 5. Compression is a REBUILD

There is **no "compress" operation** in SQL Server. Changing the compression of an
existing index is `ALTER INDEX … REBUILD WITH (DATA_COMPRESSION = …)`. So:

- To compress / recompress → `rebuild_index` with `data_compression: PAGE` (or `ROW`/`NONE`).
- `reorganize_index` **cannot** change compression.
- For a partitioned index, a whole-index rebuild applies the compression to **all**
  partitions; use `partition: N` to target one.

---

## 6. Queue lifecycle

```
01.to_run/  →  02.processing/  →  03.done/   (+ .log report)
                              ↘   04.failed/  (+ .log report)
```

- Drop a manifest (`.yaml`) into `01.to_run/`. The engine processes every file there,
  in filename order, **per target database**.
- A file whose name starts with `.` is **skipped** (use it to disable/park a manifest).
- A manifest moves as a **unit**: it reaches `03.done/` only when **all** its operations
  succeed; any failure sends it to `04.failed/` with a `.log` sidecar (human + JSON).
- Suggested naming: `NNN_short_description.yaml` (e.g. `030_compress_mydb_indexes.yaml`).

---

## 7. The safe workflow (always recommend this order)

1. **Dry-run + explain** (renders the final T-SQL, executes nothing; shows why each
   option was injected):
   ```bash
   sqlgopace -config config.yaml -dry-run -explain 01.to_run/030_compress_mydb_indexes.yaml
   ```
   For an offline preview without a connection, add `-assume-version 15 -assume-edition standard`.
2. **Run the queue** (processes everything in `01.to_run/`, one-shot, then exits):
   ```bash
   sqlgopace -config config.yaml
   ```
   Add `--tui` for the interactive incident console (watch/kill blockers, pause/kill DDL).
3. **Recovery / interruptions**: a crash leaves the manifest in `02.processing/`; the next
   run reconciles it. A paused RESUMABLE that blocks a retry can be cleared with the
   `abort-resumable` subcommand.
4. **Automatic maintenance** (a different path): the `plan` subcommand inspects a live
   database and *generates* reviewable manifests into `01.to_run/` from
   `maintenance_profile.yaml`. Use it for "reorganize/rebuild/compress/update stats/checkdb
   based on the DB's actual state" rather than a hand-listed manifest.

Secrets (passwords) come from a gitignored `.env` via `${VAR}` in `config.yaml` — never
put credentials in `config.yaml` or a manifest.

---

## 8. Warnings to surface to the user (checklist)

Before a real run, verify and call out:

- **Edition decides online-ness.** On **Standard** edition, index rebuilds are **OFFLINE**:
  each `rebuild_index` takes a schema-modification (Sch-M) lock — the **table is fully
  unavailable** (reads and writes) for the duration. No RESUMABLE, no WAIT_AT_LOW_PRIORITY;
  the only reaction lever is KILL. → Recommend a **maintenance window** for large tables.
  On Enterprise/Azure the same rebuilds can be ONLINE + RESUMABLE.
- **Permissions.** The connection login needs `ALTER` on the target tables (e.g. membership
  in `db_ddladmin` or `db_owner`) and `VIEW SERVER STATE` for monitoring. A login without
  rights on an object makes `OBJECT_ID(...)` return NULL, so **preflight reports
  "table does not exist"** even though it does. If you see that error, suspect permissions,
  not a missing table.
- **Right database.** Confirm `database:` (or the connection's DB) is the intended one,
  especially on multi-DB servers.
- **No resume across a kill (current limitation).** If interrupted, an offline rebuild
  rolls back fully and is re-run from scratch on the next run; a multi-op manifest re-runs
  from the start (idempotent but redoes completed work). See `specs/crash-resumable.md`.
- **Heaps & special indexes.** Heap rebuilds never get RESUMABLE/WALP. Columnstore/XML/
  spatial indexes use a different model and are excluded from rowstore compression.
- **Shrink is heavy and fragments indexes.** Only suggest `shrink` to reclaim genuinely
  needed space; warn it can re-fragment.

---

## 9. Recipe: natural language → manifest

1. **Identify the intent** and map it to one of the 13 operations (§3). If it does not map,
   say so plainly.
2. **Collect the targets**: schema, table, and index/column/constraint names. If the user
   pasted scripted DDL (e.g. `ALTER INDEX [ix] ON [dbo].[T] REBUILD WITH (DATA_COMPRESSION = PAGE)`),
   extract `schema`/`table`/`index` and the intended `data_compression`; **drop the `WITH (...)`**
   (the engine re-injects options) — keep only `data_compression` and, if the user insists,
   `maxdop`.
3. **Ask the few things you cannot infer**: which database; is a `rebuild_index` meant to
   recompress, defragment, or both; target compression tier; maintenance window.
4. **Emit the YAML manifest** (top-level §2 + operations §3). One operation per object.
   Leave `options` empty unless overriding.
5. **Add the guidance**: suggest a filename in `01.to_run/`, the dry-run command (§7), and
   the relevant warnings (§8) for this specific request.

### Example A — compress a list of indexes to PAGE

```yaml
description: "Compress reporting indexes (PAGE)"
database: EXAMPLEDB
operations:
  - operation: rebuild_index
    schema: dbo
    table: ORDERS
    index: PK_ORDERS
    data_compression: PAGE
    options:
      maxdop: 2
  - operation: rebuild_index
    schema: dbo
    table: ORDERS
    index: ORDERS_LCX
    data_compression: PAGE
    options:
      maxdop: 2
```

### Example B — add a NOT NULL column with a default

```yaml
description: "Add PROCESSED flag to DISPATCH"
database: MYDB
operations:
  - operation: add_column
    schema: dbo
    table: DISPATCH
    column: PROCESSED
    type: BIT
    nullable: false
    default: 0
```

### Example C — reclaim space from a data file

```yaml
description: "Reclaim space from MYDB data files"
database: MYDB
operations:
  - operation: shrink
    type: data
    files: all
    targetfreespace: 10%
```

### Example D — reorganize (online defrag, no compression change)

```yaml
description: "Online defrag of a hot index"
database: MYDB
operations:
  - operation: reorganize_index
    schema: dbo
    table: ORDERS
    index: IX_ORDERS_DATE
    lob_compaction: true
```

---

## 10. Deeper references

- `README.md` — canonical user reference (manifest format, flags, subcommands).
- `ddl_compatibility.yaml` — the version × edition option matrix.
- `maintenance_profile.yaml` + `specs/MAINTENANCE.md` — the `plan` (auto-maintenance) path.
- `specs/SPECS.md`, `specs/SHRINK.md`, `specs/crash-resumable.md` — design details and limits.
- `docs/e2e.md` — required login permissions for a live run.
