# SqlGoPace — LLM Operator Guide

A guide written **for a language model** (chat or agent) that helps a human operate
SqlGoPace. Goal: turn a natural-language request ("compress these indexes", "add a
NOT NULL column", "reclaim space from this data file") into a **correct manifest**,
explain usage, and warn about what to verify first.

This file is provider-agnostic: paste it into any chat, index it for RAG, or load it
as a Claude Code skill. It is the source of truth for the manifest format; when in
doubt defer to the human documentation in [`docs/`](README.md), in particular
[`manifests.md`](manifests.md), [`operations.md`](operations.md) and
[`blocking-and-kills.md`](blocking-and-kills.md).

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
- **Never invent an operation type.** Only the 16 in §3 exist. If the request needs
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

## 3. Operation catalog (the only 16 valid `operation:` values)

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
| `batch_update` | `UPDATE TOP (n)` in a committing loop | `schema`, `table`, one of `set`/`set_raw`, and a predicate | `where` (list of `column`/`op`/`value`, AND-ed; ops `= <> < <= > >= is null is not null`) or `where_raw`; `batch.strategy: predicate\|key_range` (`key_range` needs a single-column unique integer clustered key), `batch.key`, `batch.initial_rows`; `confirm_full_table` required with no predicate. Needs SELECT **and** UPDATE. |
| `batch_delete` | `DELETE TOP (n)` in a committing loop | `schema`, `table`, a predicate | Same predicate and batch fields; takes no `set`. Needs SELECT **and** DELETE. |
| `shrink` | `DBCC SHRINKFILE` (chunked) | `type` (`data`\|`log`), `targetfreespace` | `files` (`all` or a logical file name; default `all`); `targetfreespace: "10%"` or `"100MB"`. Only WALP is relevant. |
| `shrink_tempdb` | `DBCC SHRINKFILE` (chunked, per tempdb data file) | `targetsizemb` (per-file MB) | Database-scoped (no schema/table). `flushcaches` (opt-in cache-flush escalation on persistent stall). Only WALP is relevant (2022+); resolves `ABORT_AFTER_WAIT = SELF` only — tempdb never kills a blocking session. |

Field names are exact YAML keys. Unknown operation values are rejected at parse time.

---

## 4. Options & automatic injection (`options:`)

`options:` overrides per operation. Every field is a pointer: **omit it = "auto"**
(the engine resolves it from the version/edition matrix). Only set a field to force a
value.

```yaml
    options:
      online: true             # true | false | (omit = auto)
      resumable: true          # requires online; refused in tempdb (Msg 11439)
      wait_at_low_priority: true   # requires online
      sort_in_tempdb: true     # cannot be combined with resumable (Msg 11438)
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

For a **targeted** version — keep yielding to important sessions but ignore specific
unimportant ones — use the top-level `ignore_blocked_sessions:` list instead (it applies to
the whole manifest):

```yaml
ignore_blocked_sessions:
  # All string fields are regexps (matched app-side). An entry matches when EVERY field it
  # sets matches (AND); the list is OR'd. session_id is an exact match.
  - app_name: "^SQLAgent"
    login_name: "svc_reporting"   # ignore this job only under this login
  - host_name: "BATCH0[0-9]"      # OR any session from these hosts
operations:
  - operation: rebuild_index
    schema: dbo
    table: DISPATCH
    index: ALL
```

A blocked session is yielded to unless it positively matches a rule (fail-safe). Prefer
`app_name`/`login_name` over `session_id` (a SPID only identifies a connection that exists
now). As a backstop, `options.max_block_minutes: N` on an operation makes it yield after `N`
minutes of continuous blocking **even if the blocker is ignored** — protection against a
too-broad rule. When the engine reacts to blocking it writes an advisory `<manifest>.blocked.yaml` next
to the run report — ready-to-paste entries plus a full diagnostic block — so you can learn who
blocked you and decide what to add. The engine never reads that file back; copying an entry
into the manifest is a deliberate step.

### Session policy: get the direction right before you write a rule

Two manifest lists share the same matcher fields and do opposite things. Writing a login into
the wrong one fails silently — a rule only fires in its own direction, so nothing happens and
nothing is reported.

| The session… | …means | List |
|---|---|---|
| appears in `<manifest>.blocked.yaml` / the TUI blocked list | **we block it** (it waits on our lock) | `ignore_blocked_sessions` — let it wait, keep going |
| is what our own operation is waiting on | **it blocks us** | `kill_blocking_sessions` — terminate it after `after_seconds` |
| is blocked by us with sessions queued behind it | it amplifies our block | `kill_amplifying_maintenance` in `config.yaml` |

`kill_blocking_sessions` is inert unless `kill_blockers.enabled: true` in the config file the
run uses — say so whenever you write such a rule, since a manifest alone never kills anything.
The delay is served by the rule, not by the session: an offender killed and returning under a
new SPID inherits the time already served, and one rule kills at most three sessions before five
*quiet* minutes, after which the run reports it and falls back to yielding. Quiet is the operative
word — an offender returning every few minutes keeps refreshing the window, so the rule stays
capped for the rest of the manifest rather than getting three more kills every five minutes. Do
not suggest a very short `after_seconds` to "keep up" with a restarting job — that is the case the
mechanism already handles, and a short delay only makes the run kill innocent short-lived
sessions. One detail to state when you write the rule: a rule that matches on `statement:` only
accrues time on the polls where that statement is the one running, so such a rule fires *later*
than `after_seconds` of wall-clock blocking, never earlier.

**Questions to ask before writing session policy** (do not guess these):

1. Which sessions may be made to *wait* — and do they hold open transactions? A read-only
   `SELECT` with `open_transactions: 0` is cheap to block; a writer with open transactions is
   not, and belongs in neither list.
2. Which sessions may be *killed*, and after how long? Killing is destructive and the delay is
   the operator's risk call — never pick it silently.
3. Is there a `.blocked.yaml` from a previous run to read first? It is evidence; a login the
   user names from memory usually is not.
4. What is the `max_block_minutes` backstop for this operation? Any ignore rule needs one.

**You normally leave `options` empty.** The matrix (`ddl_compatibility.yaml`) decides
what is legal and injects it. Key gates for `rebuild_index` (the common case):

| Option | Min version | Editions | Requires |
|---|---|---|---|
| `online` | 2005 | Enterprise, Azure | — |
| `wait_at_low_priority` | 2014 | Enterprise, Azure | online |
| `resumable` | 2017 | Enterprise, Azure | online |
| `data_compression` | 2008 | Enterprise, Azure | — |
| `maxdop`, `sort_in_tempdb` | 2005/2008 | Enterprise, **Standard**, Azure | — |

Two restrictions are database- or combination-scoped rather than version-scoped, so they
are not in the table and the resolver applies them on top of it. `resumable` is refused
outright in `tempdb` (Msg 11439), on every version and edition, while `online` alone is
accepted there. And `sort_in_tempdb` cannot be combined with `resumable` (Msg 11438,
severity 15, so the batch fails at compile time): the resolver keeps `resumable` and drops
the sort, recording the reason in the decision trail.

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
   sqlgopace --config config.yaml --dry-run --explain 01.to_run/030_compress_mydb_indexes.yaml
   ```
   For an offline preview without a connection, add `-assume-version 15 -assume-edition standard`.
2. **Run the queue** (processes everything in `01.to_run/`, one-shot, then exits):
   ```bash
   sqlgopace --config config.yaml
   ```
   Add `--tui` for the interactive incident console (watch/kill blockers, pause/kill DDL, or press
   `i` on a blocked session to ignore it — writes an `ignore_blocked_sessions` rule into the
   running manifest, hot-reloaded so the operation holds its lock through that session).
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
- **What a re-run repeats depends on how the last one ended.** A crash, a Ctrl+C drain or
  a closing window leaves the manifest in `02.processing/` with a resume cursor, and the next
  run continues at the first unfinished operation; a paused resumable index build is
  continued with `ALTER INDEX … RESUME` rather than rebuilt. A *failure* under the default
  `on_failure: stop` is different: the manifest goes to `04.failed/` and a re-run replays it
  from operation 1. Use `on_failure: continue` to get a recovery manifest holding only the
  failed operations. See [`running.md`](running.md).
- **Heaps & special indexes.** Heap rebuilds never get RESUMABLE/WALP. Columnstore/XML/
  spatial indexes use a different model and are excluded from rowstore compression.
- **Shrink is heavy and fragments indexes.** Only suggest `shrink` to reclaim genuinely
  needed space; warn it can re-fragment.

---

## 9. Recipe: natural language → manifest

1. **Identify the intent** and map it to one of the 16 operations (§3). If it does not map,
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

- [`docs/manifests.md`](manifests.md) and [`docs/operations.md`](operations.md) — the manifest format and every operation's fields.
- [`docs/running.md`](running.md) — modes, flags, the queue, and what a re-run repeats.
- [`docs/blocking-and-kills.md`](blocking-and-kills.md) — the reaction hierarchy and the three session-policy features.
- [`docs/permissions.md`](permissions.md) — the grants each operation needs, with templates.
- [`docs/shrink.md`](shrink.md) and [`docs/maintenance-planner.md`](maintenance-planner.md) — the two multi-statement drivers and the planner.
- `ddl_compatibility.yaml` and [`docs/compatibility-matrix.md`](compatibility-matrix.md) — the version × edition option matrix.
- [`docs/specs/`](specs/) — design documents, the source of truth for intended behaviour.
