# Operations

Every value `operation:` accepts, with its fields. Field names are exact YAML keys, and an
unknown operation or field is rejected at parse time rather than silently ignored.

Six of them take no `options:` block at all, because they have no injectable option:
`reorganize_index`, `add_column`, `drop_index`, `drop_column`, `drop_constraint` and
`update_statistics`. Adding one is rejected at parse time with `unknown field "options"`.
For the other ten, see [`manifests.md`](manifests.md#per-operation-options).

## At a glance

| `operation` | Generates | Required fields |
|---|---|---|
| [`rebuild_index`](#rebuild_index) | `ALTER INDEX … REBUILD` | `schema`, `table`, `index` |
| [`reorganize_index`](#reorganize_index) | `ALTER INDEX … REORGANIZE` | `schema`, `table`, `index` |
| [`rebuild_heap`](#rebuild_heap) | `ALTER TABLE … REBUILD` | `schema`, `table` |
| [`create_index`](#create_index) | `CREATE [UNIQUE] INDEX` | `schema`, `table`, `index`, `columns` |
| [`drop_index`](#drop_index) | `DROP INDEX` | `schema`, `table`, `index` |
| [`add_column`](#add_column) | `ALTER TABLE … ADD` | `schema`, `table`, `column`, `type` |
| [`alter_column`](#alter_column) | `ALTER TABLE … ALTER COLUMN` | `schema`, `table`, `column`, `type` |
| [`drop_column`](#drop_column) | `ALTER TABLE … DROP COLUMN` | `schema`, `table`, `column` |
| [`add_constraint`](#add_constraint) | `ALTER TABLE … ADD CONSTRAINT` | `schema`, `table`, `constraint`, `kind`, `columns` |
| [`drop_constraint`](#drop_constraint) | `ALTER TABLE … DROP CONSTRAINT` | `schema`, `table`, `constraint` |
| [`update_statistics`](#update_statistics) | `UPDATE STATISTICS` | `schema`, `table` |
| [`check_db`](#check_db) | `DBCC CHECKDB` | `database` |
| [`batch_update`](#batch_update-and-batch_delete) | `UPDATE TOP (n)` in a loop | `schema`, `table`, a SET, and a predicate or `confirm_full_table` |
| [`batch_delete`](#batch_update-and-batch_delete) | `DELETE TOP (n)` in a loop | `schema`, `table`, and a predicate or `confirm_full_table` |
| [`shrink`](#shrink) | `DBCC SHRINKFILE`, chunked | `type`, `targetfreespace` |
| [`shrink_tempdb`](#shrink_tempdb) | `DBCC SHRINKFILE` per tempdb data file | `targetsizemb` |

## Indexes

### `rebuild_index`

```yaml
- operation: rebuild_index
  schema: dbo
  table: Orders
  index: IX_Orders_Date     # or ALL
  data_compression: PAGE    # NONE | ROW | PAGE
  partition: 3              # optional; rebuild one partition
  intent: compression       # optional; see manifests.md
```

The only operation that changes compression. `index: ALL` expands to one operation per
real index on the table, so each is preflighted, resolved and reported separately, and
each can carry `RESUMABLE` (which `ALTER INDEX ALL` cannot).

Rebuilding a single partition narrows the option set: that syntax accepts only
`SORT_IN_TEMPDB`, `MAXDOP`, `DATA_COMPRESSION`, `XML_COMPRESSION`, `ONLINE` and
`WAIT_AT_LOW_PRIORITY`, so `RESUMABLE` is dropped with a decision line saying why.

Columnstore, XML and spatial indexes rebuild offline and reject the whole `ONLINE`
family; when `ALL` expands, the engine knows each index's kind and resolves accordingly.

### `reorganize_index`

```yaml
- operation: reorganize_index
  schema: dbo
  table: Orders
  index: IX_Orders_Date
  partition: 3              # optional
  lob_compaction: true      # optional
```

Always online, takes no `options:`, and cannot change compression. It has no
`WAIT_AT_LOW_PRIORITY` or `RESUMABLE` to fall back on, so SqlGoPace paces it differently:
it cancels and re-issues the statement, since SQL Server persists a reorganize's progress
across a cancel.

Two advisories fire at the start. If RCSI is off on the target database, readers will
block on its page locks, and the run says so. On SQL Server 2022 and later, if the
database-scoped `ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY` setting is off, the run
recommends turning it on, and states the limit in the same breath: it covers automatic
asynchronous statistics updates only, never an explicit `UPDATE STATISTICS` run by a job
or by hand. Those still block, and clearing them is what
[`kill_amplifying_maintenance`](blocking-and-kills.md#killing-amplifying-maintenance-victims)
exists for.

### `rebuild_heap`

```yaml
- operation: rebuild_heap
  schema: dbo
  table: StagingLoad
  data_compression: PAGE    # optional
```

`ALTER TABLE … REBUILD`, which also rebuilds every nonclustered index on the table. Its
`options:` accept `ONLINE`, `MAXDOP` and `DATA_COMPRESSION` only: no `RESUMABLE`, no
`WAIT_AT_LOW_PRIORITY`.

### `create_index`

```yaml
- operation: create_index
  schema: dbo
  table: Orders
  index: IX_Orders_Customer
  columns: [CustomerID, OrderDate]
  unique: false             # optional
  data_compression: PAGE    # optional
```

### `drop_index`

```yaml
- operation: drop_index
  schema: dbo
  table: Orders
  index: IX_Orders_Old
```

## Columns and constraints

### `add_column`

```yaml
- operation: add_column
  schema: dbo
  table: Orders
  column: Processed
  type: BIT
  nullable: false
  default: 0                # scalar constant only
```

No injectable options. A `NOT NULL` column with a constant default is metadata-only on
Enterprise, which preflight reports.

### `alter_column`

```yaml
- operation: alter_column
  schema: dbo
  table: Orders
  column: Notes
  type: NVARCHAR(400)
  nullable: true            # optional
```

Type and nullability only. `ONLINE` applies from SQL Server 2016, and
`WAIT_AT_LOW_PRIORITY` is not supported with an online `ALTER COLUMN` on any version, so
the matrix never pairs them.

### `drop_column`

```yaml
- operation: drop_column
  schema: dbo
  table: Orders
  column: Obsolete
```

### `add_constraint`

```yaml
- operation: add_constraint
  schema: dbo
  table: Orders
  constraint: PK_Orders
  kind: primary_key         # primary_key | unique
  columns: [OrderID]
```

### `drop_constraint`

```yaml
- operation: drop_constraint
  schema: dbo
  table: Orders
  constraint: UQ_Orders_Ref
```

## Statistics and integrity

### `update_statistics`

```yaml
- operation: update_statistics
  schema: dbo
  table: Orders
  statistic: IX_Orders_Date   # optional; omit for every statistic on the table
  full_scan: true             # at most one of full_scan / sample_percent / resample
```

`sample_percent` takes 1 to 100. Setting more than one of the three sampling fields is
rejected.

### `check_db`

```yaml
- operation: check_db
  database: PRODDB
  physical_only: true       # optional
  data_purity: false        # optional
  options:
    maxdop: 4
```

Database-scoped: no `schema` or `table`, and the existence preflight for a table is
skipped accordingly. Needs `db_owner` or `sysadmin`.

## Batched DML

### `batch_update` and `batch_delete`

Large `UPDATE`s and `DELETE`s split into committing batches, so the transaction log never
holds the whole change and the operation can be stopped at any point without a rollback.
Each batch is an `UPDATE`/`DELETE TOP (n)`, and the batch size is recalibrated from the
waits each batch produced.

```yaml
- operation: batch_update
  schema: dbo
  table: Orders
  set:
    Archived: 1             # column -> scalar literal
  where:
    - column: OrderDate
      op: "<"
      value: "2020-01-01"
    - column: Archived      # conditions are AND-ed
      op: "="
      value: 0
  batch:
    strategy: predicate     # predicate | key_range
    initial_rows: 5000      # optional; auto-sized when omitted
  options:
    maxdop: 4               # the only T-SQL option that applies
```

| Field | Purpose |
|---|---|
| `set` | Column to scalar literal. `batch_update` only. Mutually exclusive with `set_raw`. |
| `set_raw` | A raw `SET` list, interpolated verbatim. See the warning below. |
| `where` | A list of conditions, AND-ed. Operators: `=`, `<>` (or `!=`), `<`, `<=`, `>`, `>=`, `is null`, `is not null`. The two NULL tests take no `value`; every other operator requires one. |
| `where_raw` | A raw predicate, interpolated verbatim. Mutually exclusive with `where`. |
| `batch.strategy` | `predicate` re-runs the same filter until it matches nothing. `key_range` walks a key column and persists a watermark, so a crash resumes mid-table. |
| `batch.key` | The key column, for `key_range`. |
| `batch.initial_rows` | Starting batch size. Auto-sized when omitted. |
| `confirm_full_table` | Required whenever the filter spares no row — including a filter that is written but excludes nothing. Without it the manifest is rejected. |

Three rules the parser enforces, each closing a way to lose data or hang:

- No predicate at all is refused unless `confirm_full_table: true` says you meant it. For
  an unconditional delete, `TRUNCATE TABLE` is the right tool and this is not it.
- `set_raw` without any predicate cannot self-limit under the `predicate` strategy, so it
  would loop forever. It is rejected; give it a self-limiting `where_raw`.
- The `key_range` walk persists a watermark and re-applies the boundary batch on a resume,
  so it is restricted to an idempotent literal `UPDATE`.

The parser can only see whether a filter was *written*. Preflight checks whether it
**excludes anything**, by asking the server how many rows the filter would spare (counted up
to 1000, so a selective filter is answered almost immediately and only a filter that matches
everything pays for a scan):

| Rows spared | Verdict |
|---|---|
| 0 | **Fail** — this is a whole-table operation however it is spelled. Set `confirm_full_table: true` if you mean it. |
| 1–999 | **Warn**, with the number. "This deletes all but three rows" is something only you can judge. |
| 1000 | **Pass** — the filter is doing its job. |

So `where_raw: "1=1"`, and `where: [{column: Id, op: ">=", value: 0}]` on an identity column,
are refused the same way an absent filter is. The probe is skipped entirely when
`confirm_full_table: true` is already set: you have said what you mean, and there is nothing
left for the check to establish.

A literal `set` is self-limiting: the generated `WHERE` excludes rows already holding the
target value, so the loop ends when nothing is left to change. `set: {Col: null}` is included
— it generates `Col IS NOT NULL`, not `Col <> NULL`, which would be `UNKNOWN` for every row
and would never terminate.

A `set_raw` gets no such clause: the tool cannot tell from raw SQL text whether the SET
consumes its own filter. **`set_raw: "Counter = Counter + 1"` with `where_raw: "Status = 'A'"`
matches the same rows on every pass**, and each batch commits, so it would run until someone
stopped it. A cumulative-row ceiling backstops this: twice the table's row estimate (or a
million rows when the estimate is unavailable). A terminating predicate cannot exceed the
table's row count, so crossing the ceiling means the predicate is not self-consuming, and the
operation **fails** with the committed row and batch counts in its `.log`. It is a backstop,
not a budget — write a `where_raw` your `set_raw` invalidates, and it never fires.

`set_raw` and `where_raw` are interpolated into the statement verbatim. They are validated
for shape, never for content, which makes a manifest a trusted input: see
[`../SECURITY.md`](../SECURITY.md).

A batch that stops early — log pressure, blocking, or the self-wait budget — keeps every
batch it committed and is reported **INCOMPLETE**: the manifest moves to `04.failed/` for
review rather than `03.done/`, and a `key_range` walk keeps its watermark so a re-run
continues where it left off. That is the same treatment a shrink that stalls gets. It is not
a failure of the operation, it is a refusal to call a half-finished purge complete.

Permissions are the one place batched DML differs from every other operation: it needs
`SELECT` as well as `UPDATE` or `DELETE`, because the `TOP` and the predicate both read.
`db_datawriter` alone fails.

## Space reclamation

### `shrink`

```yaml
- operation: shrink
  type: data                # data | log
  files: all                # all, or a logical file name
  targetfreespace: 10%      # "N%" of used space, or "N MB"
  identify_tail_object: true  # optional; 2019+
```

### `shrink_tempdb`

```yaml
- operation: shrink_tempdb
  targetsizemb: 20480       # common absolute target for every tempdb data file
  flushcaches: false        # optional escalation on a persistent stall
```

Both are multi-statement drivers rather than a single statement, with their own chapter:
[`shrink.md`](shrink.md).
