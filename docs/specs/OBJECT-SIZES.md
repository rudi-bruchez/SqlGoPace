# Object sizes before and after — and what a heap rebuild really rewrites

Status: design, 2026-09-15, revised after two adversarial reviews
([REVIEW-OBJECT-SIZES-claude.md](REVIEW-OBJECT-SIZES-claude.md),
[REVIEW-OBJECT-SIZES-agy.md](REVIEW-OBJECT-SIZES-agy.md)) and a server test of what a heap rebuild
does to disabled and columnstore indexes (§ Verified behaviour), and a resumable-pause probe
([OBJECT-SIZES-ANALYSIS.md](OBJECT-SIZES-ANALYSIS.md)). Not implemented. Target version 0.35.0.

## Why

Four things, three of them defects.

**The feature.** A `rebuild_index` (with or without `data_compression`), a `rebuild_heap` and a
`reorganize_index` all exist to make an object smaller or denser, and nothing tells the operator
whether they did. The run report records reactions, waits and duration, not the size of the object
before and after. On a compression campaign that is the number the operator is asked for — per
manifest, and summed across the campaign's manifests.

**Defect 1 — a heap rebuild is measured as the heap alone.** `ALTER TABLE … REBUILD` on a heap
rebuilds every nonclustered index of the table in the same statement:

- "If the table is a heap, all nonclustered indexes are rebuilt."
  ([ALTER TABLE, example A](https://learn.microsoft.com/sql/t-sql/statements/alter-table-transact-sql#examples))
- "Whenever an index is created, rebuilt, or dropped, disk space for both the old (source) and new
  (target) structures is required … The old structure isn't deallocated until the index creation
  transaction commits."
  ([Index disk space example](https://learn.microsoft.com/sql/relational-databases/indexes/index-disk-space-example))

This was specified and never built. `MAINTENANCE.md` §9 requires "`rebuild_heap`: needs free space
for a copy of the heap **plus** its nonclustered indexes (the rebuild re-creates them all)", and its
risk table names `min/max_size_mb` as the guard against a heap rebuild being "silently expensive".
`COMPRESSION-SCOPE.md` §4.5 records that the planner judges a heap "by partition 1". The code does
neither:

1. **Preflight.** `rebuiltObject` (`internal/preflight/preflight.go:194`) returns the heap with an
   empty index name, so `IndexSizeMB` reads `index_id = 0` only. A 5 GB heap carrying 20 GB of
   nonclustered indexes needs roughly 25 GB and is checked against 5.
2. **Planner.** `heapMeasurement` (`internal/plan/plan.go:182`) compares `head.SizeMB` to
   `heap.max_size_mb`; `head` is the heap's first inventory row, so this is its **first partition**,
   with no nonclustered index counted. `internal/plan/shrink.go:93` sums the partitions; `plan.go`
   does not.

The planner does know the rebuild covers the indexes — `Decide` suppresses index operations on a
table whose heap is rebuilt (`internal/maint/decide.go:153`) — it just never adds their size. And
the operator is told none of it: the manifest names one table, the statement rewrites the table and
all its indexes, holds `Sch-M` for all of it when offline, and in FULL recovery logs all of it.

**Defect 2 — a heap rebuild silently undoes a disabled index, and drops its compression.** Tested
(§ Verified behaviour): `ALTER TABLE … REBUILD` on a heap re-enables a disabled nonclustered index
and rebuilds it **uncompressed**. An operator who disabled an index on purpose — before a bulk load,
or pending a decision to drop it — gets it back live, maintained by every write, and without the
`PAGE` compression it was created with. Nothing in SqlGoPace today reads `sys.indexes.is_disabled`
for a heap; the planner cannot see such an index at all, because its inventory joins
`sys.dm_db_partition_stats`, where a disabled index has no row ("Disabling … a nonclustered index
physically deletes the index data").

**Defect 3 — the console's notice slot does not hold a notice.** The TUI has one notice line:
`case LogMsg: m.notice = msg.Line` (`internal/tui/model.go:578`). Thirteen senders in
`cmd/sqlgopace/main.go` write to it (a blocker kill, an amplifier kill, a failed kill, every
`ignore`/auto-kill answer), and each overwrites the last. The manifest-start rollback-on-cancel
notice shipped in 0.34.0 for harm finding H2 goes through that slot, so the first kill of a run
erases it. This spec needs a persistent per-operation place on the console anyway (§4.3), and fixes
H2's delivery with it.

## Verified behaviour

Tested 2026-09-15 on a throwaway database: a heap of 200,000 rows with `IX_probe_active` (`PAGE`),
`IX_probe_disabled` (created `PAGE`, then disabled) and a nonclustered columnstore `NCCI_probe`,
plus 1,000 rows inserted after the columnstore was built (an `OPEN` delta rowgroup). After
`ALTER TABLE dbo.sqlgopace_heap_probe REBUILD`:

| Index | After the heap rebuild | Conclusion |
| --- | --- | --- |
| `IX_probe_disabled` | `is_disabled = 0`, `data_compression_desc = NONE`, 452 used pages | re-enabled, rebuilt from nothing, compression lost |
| `NCCI_probe` | one `COMPRESSED` rowgroup of 201,000 rows | rebuilt: the `OPEN` delta rows were compressed, which the tuple mover does not do to an open rowgroup |
| `IX_probe_active` | `PAGE` | rebuilt with its compression |

The documentation agrees where it speaks and is silent on the rest: "compression settings metadata
is lost when nonclustered indexes are disabled"
([Enable indexes and constraints](https://learn.microsoft.com/sql/relational-databases/indexes/enable-indexes-and-constraints)),
but that page's table of what re-enables a disabled nonclustered index lists clustered-index actions
only, and no page names columnstore in "all nonclustered indexes are rebuilt". The test settles both.

## Scope

In: `rebuild_index`, `rebuild_heap`, `reorganize_index`, as run by the engine, checked by
preflight, rendered by `--dry-run`, decided by the `plan` subcommand, and recorded in the SQLite
history; the disabled-index guard on `rebuild_heap`; the console delivery of the rollback-on-cancel
notice.

Out:

- `create_index`, `alter_column`, `add_constraint`, `shrink`: no before/after size requested.
- Reserved space and data-file free space after the run: the header already shows data-file free
  space (0.34.0). This spec reports **used** pages, which is what compaction and compression change.
- A dry-run size line for `rebuild_index`/`reorganize_index`: only the heap rewrites more than its
  manifest names.
- A `rebuild_index` naming a disabled index re-enables it too, but the manifest names that index:
  it is what was asked. `index: ALL` is not affected — its expansion already skips disabled indexes
  (`internal/mssql/indexes.go:24`).
- History rows for heaps the planner skips (agy review, finding 2): `recordPlanHistory` records
  only decisions that emit an operation (`cmd/sqlgopace/plan.go:333`, `if d.Op == nil { continue }`),
  so a skip is not recorded whichever layer decides it.

## Design

### 1. One read: every structure of a table — `internal/mssql`

```go
// StructureSize is one heap or index of a table, its used pages summed over partitions.
type StructureSize struct {
	IndexID  int    // 0 = heap
	Name     string // empty for the heap
	TypeDesc string // sys.indexes.type_desc
	Disabled bool   // sys.indexes.is_disabled; a disabled nonclustered index has no pages
	UsedKB   int64
}

func (c *Conn) TableStructureSizes(ctx context.Context, schema, table string, partition *int) ([]StructureSize, error)
```

```sql
SELECT i.index_id, i.name, i.type_desc, i.is_disabled,
       COALESCE(SUM(ps.used_page_count), 0) * 8 AS used_kb
FROM sys.indexes i
LEFT JOIN sys.dm_db_partition_stats ps
  ON ps.object_id = i.object_id AND ps.index_id = i.index_id
 AND (@partition = 0 OR ps.partition_number = @partition)
WHERE i.object_id = OBJECT_ID(QUOTENAME(@schema) + '.' + QUOTENAME(@table))
  AND i.is_hypothetical = 0
GROUP BY i.index_id, i.name, i.type_desc, i.is_disabled
ORDER BY i.index_id;
```

`LEFT JOIN`, with the partition filter in the join, so a disabled index is listed with 0 KB instead
of disappearing. Kilobytes, not megabytes: `IndexSizeMB` rounds up to the MB, so a small index would
read "1 MB -> 1 MB" whatever happened to it.

It replaces `IndexSizeMB`, whose only production caller is the preflight check; the integration
test in `internal/mssql/indexes_integration_test.go` moves to the new read and keeps its four cases
(heap, named index, one partition, missing object). A missing object returns no rows, read as
"size unknown".

The selection of what an operation rewrites is pure and lives in `internal/preflight`, where it
replaces `rebuiltObject`; the engine and the dry run call it from there:

```go
// Rewritten returns the structures op rewrites: the named index for rebuild_index and
// reorganize_index; for rebuild_heap the heap and every other structure of the table,
// disabled ones included (the rebuild re-enables them).
func Rewritten(op ddl.Operation, sizes []mssql.StructureSize) []mssql.StructureSize
```

### 2. Manifest — the opt-in

`ddl.RebuildHeap` gains one field, modelled on `BatchDML.ConfirmFullTable` (a per-operation key that
a preflight `FAIL` names as its remedy):

```yaml
- operation: rebuild_heap
  schema: dbo
  table: MEASUREMENT
  allow_reenable_disabled_indexes: true
```

Valid on `rebuild_heap` only (strict decoding already rejects it elsewhere). The planner never sets
it: re-enabling an index is an operator decision, not a maintenance one. The renderer writes it when
set, so a hand-edited planner manifest round-trips.

### 3. Preflight

For a `rebuild_index` and a `rebuild_heap`, the data-free-space check needs the sum of `Rewritten`,
rounded up to the MB. Unchanged for a `rebuild_index`; for a `rebuild_heap` it now includes every
nonclustered index, as `MAINTENANCE.md` §9 always required.

**Disabled-index guard.** A `rebuild_heap` whose table has a disabled nonclustered index `FAIL`s
unless the operation sets `allow_reenable_disabled_indexes: true`:

```text
heap rebuild re-enables disabled index(es): dbo.MEASUREMENT.IX_MEASUREMENT_OLD — ALTER TABLE REBUILD rebuilds it live and without its compression (compression metadata is dropped when an index is disabled); drop the index, or set allow_reenable_disabled_indexes: true on this operation
```

With the key set, the check is a `Warn` with the same facts. The space check cannot size a disabled
index — it has no pages — so a table with one says so in the space check's detail
(`+ 1 disabled index of unknown size, rebuilt from its table`) instead of passing on an
understated figure.

**Scope record.** A heap with at least one nonclustered index also gets a `Warn` check in the `.log`,
`heap rebuild scope`:

```text
dbo.MEASUREMENT (heap): ALTER TABLE REBUILD also rebuilds 2 nonclustered index(es): IX_MEASUREMENT_TS 2.0 GB, IX_MEASUREMENT_SITE 1.1 GB; 8.1 GB rewritten in one transaction
```

The `.log` is the record, not the warning: preflight checks reach only `rep.Preflight`, and only
`FAIL` lines reach the console (`failLines`, `internal/run/engine.go:495`). The operator sees the
scope before the rewrite through §5's manifest-start line and operation row, and through §4's dry
run; the disabled-index guard reaches the console because it is a `FAIL`.

An unreadable size or index list keeps today's behaviour for the space check ("size unknown, not
checked"), and the disabled-index guard `Warn`s that it could not tell (`index state could not be
read: <error>`) rather than failing the run on a permission.

### 4. `--dry-run` — the heap line

Connected dry run (a `config.yaml` gives a connection, as the `index: ALL` expansion already uses),
under a `rebuild_heap` that has nonclustered indexes:

```text
--     also rebuilds 2 nonclustered index(es): IX_MEASUREMENT_TS 2.0 GB, IX_MEASUREMENT_SITE 1.1 GB (heap 5.0 GB; 8.1 GB rewritten)
--     re-enables disabled index IX_MEASUREMENT_OLD without its compression — preflight refuses this unless allow_reenable_disabled_indexes: true
```

The second line appears only when a disabled index exists; its tail reads `(allowed by
allow_reenable_disabled_indexes)` when the key is set.

Offline dry run, or a read that fails, under every `rebuild_heap`:

```text
--     also rebuilds every nonclustered index on the table, and re-enables any disabled one (not listed offline)
```

`renderPlan` stays free of I/O: `dryRunManifest` reads the sizes and passes them in, the way it
passes the expander.

### 5. Engine — scope at manifest start, size before and after

The engine is wired with a narrow size reader (`WithSizeReader`, `*mssql.Conn` in production). A
`nil` reader means no size work at all: no scope line, no size lines, no row detail.

#### 5.1 Manifest start

For every planned `rebuild_heap` from the resume cursor on (the same range as the rollback-on-cancel
notice), read `TableStructureSizes` once. For each heap with nonclustered indexes:

- write one line to `e.out` and append it to a new `RunReport.HeapScopeNotices []string`, rendered
  after `CancelOnlyNotice`:

  ```text
  operation 3 rebuild_heap dbo.MEASUREMENT also rebuilds 2 nonclustered index(es) (IX_MEASUREMENT_TS, IX_MEASUREMENT_SITE): 8.1 GB rewritten in one transaction
  ```

  naming any disabled index it will re-enable (only reachable with the opt-in, since preflight
  refuses otherwise);
- set that operation's row detail (§5.3).

The scope read is not reused as the operation's "before" size: operations earlier in the manifest may
change the table before the heap rebuild runs.

#### 5.2 Before and after

**Before.** Read just before the statement executes, after the resumable `switch` in `runStep`
(`engine.go:874-883`), for a planned `REBUILD` and a `RESUME` alike. A paused resumable rebuild's
partial target lives under internal index ids that neither `sys.indexes` nor
`sys.dm_db_partition_stats` shows, so the read returns the source index alone, which is the true
old size ([OBJECT-SIZES-ANALYSIS.md](OBJECT-SIZES-ANALYSIS.md): identical reads before the rebuild
and while paused at 95.67%). No exception, and no `resumed` state in the report.

**After.**

- `rebuild_index`, `rebuild_heap`: read only on success. A failed or canceled rebuild rolls back to
  the old structure; a paused resumable leaves a partial target. Neither has a meaningful "after".
- `reorganize_index`: read on **every** outcome that ran the statement. REORGANIZE commits
  incrementally and is not rolled back (ALTER INDEX docs; the engine's own cancel narration says
  "committed work preserved, no rollback", `internal/run/reaction.go:186`). A canceled or failed
  reorganize has really compacted part of the index; its size line is labeled `(partial)`.

A re-enabled index has a "before" of 0 KB because it had no pages. Its line reads
`IX_MEASUREMENT_OLD (was disabled) 0 KB -> 452 MB` with no percentage, and it is counted in the
"after" total only, so the total shows the growth it caused.

A read error means "unknown" for that side and never changes the operation's outcome.

**Report.** `report.OperationReport` gains:

```go
Sizes []SizeLine `json:"sizes,omitempty"`

type SizeLine struct {
	Name        string `json:"name"`      // "heap" for index_id 0
	Type        string `json:"type"`
	WasDisabled bool   `json:"was_disabled,omitempty"`
	BeforeKB    int64  `json:"before_kb"` // -1 = not measured
	AfterKB     int64  `json:"after_kb"`  // -1 = not measured
}
```

plus `SizesPartial bool` (a reorganize that did not succeed).

#### 5.3 Console — on the operation's row

`run.OpInfo` and `tui.OperationRow` gain `Detail string`, rendered after the status in `opRow`
(`internal/tui/view.go:218`). The row is the only per-operation line the console keeps for the
whole run, so nothing overwrites it. Two sources set it:

- **At manifest start**, through the existing `WithOpListSink` list (`engine.go:616`):
  - a rollback-on-cancel operation (`RollbackOnCancel`, 0.34.0): `cancel only`;
  - a heap with nonclustered indexes: `+2 nonclustered, 8.1 GB rewritten`, and
    `, re-enables 1 disabled` when that applies;
  - both, joined with ` · `.
- **When the operation finishes**, through `StepEvent`/`tui.StepDoneMsg`, which gain `Detail`: the
  size result replaces the start detail, e.g. `8.1 GB -> 5.4 GB (-33.3%)`, or
  `2.0 GB -> 1.7 GB (-15.0%, partial)`. A failed rebuild keeps its start detail.

```text
2 - rebuild_heap dbo.MEASUREMENT   TO RUN   cancel only · +2 nonclustered, 8.1 GB rewritten
3 - rebuild_index dbo.MEASUREMENT.PK_MEASUREMENT   DONE   12.4 GB -> 7.1 GB (-42.7%)
```

This also fixes defect 3: `cancel only` now sits on every affected row for the whole run. The
manifest-start `LogMsg` from 0.34.0 stays as a first-screen line; it is no longer the only console
trace. Size lines are **not** sent through `WithNoticeSink`: that slot is overwritten by the next
kill or `ignore` answer, and a per-operation line would erase the manifest-start notice after the
first operation.

#### 5.4 `.log` rendering

One structure (`rebuild_index`, `reorganize_index`):

```text
      size: IX_MEASUREMENT_TS 2.0 GB -> 1.4 GB (-30.0%)
```

A heap, one line per structure and a total:

```text
      size (heap and 3 nonclustered index(es)):
        heap                              5.0 GB -> 3.1 GB  (-38.0%)
        IX_MEASUREMENT_TS                 2.0 GB -> 1.4 GB  (-30.0%)
        IX_MEASUREMENT_SITE               1.1 GB -> 0.9 GB  (-18.2%)
        IX_MEASUREMENT_OLD (was disabled)   0 KB -> 452 MB
        total                             8.1 GB -> 5.8 GB  (-28.4%)
```

A missing side renders `unknown`; a percentage needs both sides and a non-zero "before". When
**both** sides are unknown for an operation, no size line is written for it; the manifest instead
gets one line, once, `sizes not measured: <first read error>`. A login without `VIEW DEFINITION`
would otherwise print `unknown -> unknown` under every operation of a campaign.

Sizes escalate KB -> MB -> GB -> TB at 1024, one decimal from MB up (two for TB, matching
`tui.HumanizeMB`), through a `HumanizeKB` in `internal/report`. It is not `tui.HumanizeMB` reused:
`report` has no internal dependency today and must not take on Bubble Tea for one formatter.

#### 5.5 Manifest totals, without double counting

A hand-written manifest can touch one structure twice — `rebuild_heap dbo.MEASUREMENT` then
`rebuild_index … IX_MEASUREMENT_TS`, or a reorganize followed by a rebuild of the same index. The
planner suppresses the first case; a manifest author does not. Totals are therefore kept **per
distinct structure** (schema, table, index id): the **first** measured "before" and the **last**
measured "after". A structure missing either is left out of the totals. `RunReport` gains
`SizeBeforeKB`, `SizeAfterKB` and `SizeStructures`, rendered after the operations:

```text
size: 25.3 GB -> 17.9 GB (-29.2%) over 14 structure(s)
```

#### 5.6 History — the campaign figure

`runs` gains `size_before_kb` and `size_after_kb`, added through `columnMigrations`
(`internal/report/history.go:72`) like `peak_blocked` and `skipped`; `RunRecord` gains the two
fields, filled from §5.5's totals, `NULL` when no structure was fully measured. A campaign is then
one `SUM` over its runs. Two manifests of the same campaign rebuilding the same index count it
twice; the per-manifest de-duplication of §5.5 does not reach across runs, and the operator page
says so.

#### 5.7 What the number means

It is the net change in used pages between two reads, not the gain of the operation alone. An
online rebuild or a reorganize runs while the workload writes; on a busy table the difference
includes those writes. The operator page says so.

### 6. Planner — `heap.max_size_mb` counts the rewrite, and disabled indexes stop a heap rebuild

`plan.go` has the whole inventory before it decides (`groupInventory` groups it by object and index,
ordered by object). For each heap object it computes, from the inventory alone:

- the heap's size, **summed over its partitions** (fixing the first-partition read);
- each nonclustered index's size, summed over its partitions;
- the rewrite total, heap plus indexes.

The two bounds then compare different things, and the profile comments say which:

- `heap.min_size_mb` compares the **heap alone**. It is the "worth it" gate: forwarded records and
  page density are properties of the heap, and a small heap does not become worth rebuilding
  because it carries large indexes.
- `heap.max_size_mb` compares the **rewrite total**. It is the cost gate, and the cost is the whole
  statement.

**Disabled indexes.** The inventory cannot see them (they have no `sys.dm_db_partition_stats` row).
`ObjectInventory`'s query does not change — it feeds every category — and the planner instead reads,
for each heap candidate that survives the size bounds, the disabled nonclustered indexes of that
table through a new `Reader` method (`DisabledIndexes(ctx, objectID) ([]string, error)`, one
`sys.indexes` query). A heap with any is skipped: the planner never emits a manifest that preflight
would refuse, and never sets the opt-in. A read error skips the heap too, with the error in the
line: an unverifiable heap is not planned.

A heap skipped for any of these reasons is no longer dropped silently. `buildInput` writes:

```text
-- skip heap dbo.MEASUREMENT: rebuild rewrites 25000 MB (heap 5000 MB + 2 nonclustered 20000 MB), above heap.max_size_mb 10000
-- skip heap dbo.MEASUREMENT: heap 4 MB below heap.min_size_mb 10
-- skip heap dbo.MEASUREMENT: rebuild would re-enable disabled index(es) IX_MEASUREMENT_OLD; rebuild it by hand with allow_reenable_disabled_indexes, or drop the index
```

**This spec owns that skip line.** `COMPRESSION-SCOPE.md` §4 prescribes a different one
("size 42000 MB outside [10, 10000] MB") and its §7 tests "skipped with the size in the reason".
COMPRESSION-SCOPE is not implemented (`raise_to_target` appears nowhere in `internal/`), and both
designs touch `buildInput`'s heap skip and `decideHeap`. Its §4 and §7 are amended to point here,
as an amendment to an unbuilt design, not a supersession of shipped behaviour.

A heap decided for rebuild carries its indexes in the `Reason`, which the plan output prints
(`cmd/sqlgopace/plan.go:360`) and the history records (`plan.go:338`):

```text
…; also rebuilds 2 nonclustered index(es) (3100 MB; 8100 MB rewritten)
```

`HeapMeasurement` gains `NonclusteredMB int64`, `NonclusteredCount int` and `RewriteMB int64`.
`decideHeap`'s own `min/max` check (`decide.go:282-287`, dead in production because `plan.go`
filters first) uses the same fields, so the unit tests exercise the rule the planner applies.

**History meaning.** `Metrics.SizeMB`, stored as `maintenance_analysis.size_mb`, stays the **heap
alone**, now summed over partitions. On a partitioned heap that value changes from partition 1 to
the sum; the CHANGELOG says so, because rows written before and after 0.35.0 differ with nothing in
the row to tell them apart. The rewrite total goes in the `Reason`, not in `size_mb`.

**Migration.** Two behaviour changes for an existing profile, both intended fixes. A profile whose
`heap.max_size_mb` was sized against heap sizes will now skip every heap whose heap-plus-indexes total
exceeds it. A heap with a disabled nonclustered index is no longer planned at all. The CHANGELOG
tells the operator to revisit `heap.max_size_mb` in `maintenance_profile.yaml` if heaps they expect
disappear from the plan; the skip line names the reason. A hand-written `rebuild_heap` on a table
with a disabled index, which ran in 0.34.0, now fails preflight until the key is set; the CHANGELOG
names the key.

## Error handling, in one place

| Read fails or returns nothing | Effect |
| --- | --- |
| Preflight, space | "size unknown, not checked" (today's wording), no scope check, run continues |
| Preflight, disabled-index guard | `Warn: index state could not be read`, run continues |
| Dry run (connected) | the offline wording for the heap line |
| Engine, manifest start | no scope line and no heap detail for that operation; `cancel only` still shown |
| Engine, before | `before` unknown; the after read still happens; no percentage |
| Engine, after | `after` unknown; no percentage; outcome unchanged |
| Engine, both sides of an operation | no size line; one manifest-level `sizes not measured: <error>` |
| Planner, disabled-index read | heap skipped, error in the skip line |

The reason every size read degrades rather than fails: `sys.dm_db_partition_stats` wants `VIEW
DATABASE STATE` and `VIEW DEFINITION` (`docs/permissions.md`), and `VIEW SERVER STATE` implies only
the first, so a legitimate login can be refused it. Today `permissions.md` ties `VIEW DEFINITION`
to `require_data_free_space` alone and says "Nothing else in the tool needs it"; after this change
the engine's size lines, the connected dry run and the disabled-index guard need it too, whatever
`require_data_free_space` says, and the page is updated. The guard degrades to `Warn` on a read
error rather than `FAIL` for the same reason; a login that cannot read `sys.indexes` metadata is told
the guard did not run.

## Settled before implementation

1. **A before read at RESUME** — settled 2026-09-15 by
   [OBJECT-SIZES-resume-probe.sql](OBJECT-SIZES-resume-probe.sql), results in
   [OBJECT-SIZES-ANALYSIS.md](OBJECT-SIZES-ANALYSIS.md). While a resumable rebuild was paused at
   95.67%, `TableStructureSizes`, `sys.indexes` and `sys.dm_db_partition_stats` returned exactly
   what they returned before the rebuild. The partial target (index id 896001, 127,856 pages) and
   what is most likely the online mapping index (896254) are visible only through `sys.partitions`
   joined to `sys.allocation_units`. §5.2 therefore reads "before" at a `RESUME` like at a
   `REBUILD`.

**The size read is not the disk footprint.** The same probe showed the data file's used space at
about twice the table while paused (2,146,240 KB, down to 1,076,992 KB after `ABORT` and `DROP`): a
paused resumable rebuild holds roughly one extra copy of the index for as long as it stays paused,
and `TableStructureSizes` does not report it. Nothing in this design uses the size read to estimate
free space, and nothing may: the size lines describe the object, the header's data-file line (0.34.0)
describes the file. Whether the existing data-free-space and shrink checks account for a paused
resumable's hidden target is a separate question, recorded in `docs/specs/TODO.md`.

## Docs to update when it ships

- `README.md` manifest reference and the operator skill's manifest pages: `allow_reenable_disabled_indexes`
  on `rebuild_heap`.
- `docs/specs/MAINTENANCE.md`: §5.3 step 1 (the two bounds, and the disabled-index skip), the profile
  block, and §9's `rebuild_heap` line (now implemented, plus the disabled-index guard; point here).
- `docs/specs/COMPRESSION-SCOPE.md`: §4 (heap skip line owned by OBJECT-SIZES), §4.5 (the heap is no
  longer judged by partition 1), §7 (test wording).
- `docs/specs/CANCEL-ONLY.md` §2: the console now also carries `cancel only` on each row; note the
  0.34.0 notice slot is overwritten by kills.
- `maintenance_profile.yaml` and its twin `internal/scaffold/assets/maintenance_profile.yaml`: the
  comments on `heap.min_size_mb` / `heap.max_size_mb`.
- `docs/permissions.md`: `VIEW DEFINITION` now also drives the size lines, the connected dry run and
  the disabled-index guard.
- `docs/running.md`: the row detail, the size lines, the totals, what the number means under a busy
  workload, and the cross-run double count in history.
- `docs/configuration.md` if its preflight section lists checks.
- `CHANGELOG.md` 0.35.0: the feature; the heap preflight and planner size fixes; the disabled-index
  guard with the key named; the `heap.max_size_mb` migration note; the `maintenance_analysis.size_mb`
  meaning change for partitioned heaps; the H2 console fix.
- `docs/specs/TODO.md`: a follow-up on whether the data-free-space and shrink checks account for a
  paused resumable rebuild's hidden target, which holds about one extra copy of the index in the
  data file and is invisible to `sys.dm_db_partition_stats` (§ Settled before implementation).

## Testing

- `internal/mssql`: integration test for `TableStructureSizes` replacing
  `TestIndexSizeMBIntegration`, plus a heap with two nonclustered indexes, one of them disabled
  (listed, `Disabled`, 0 KB), and a nonclustered columnstore (listed and sized).
- `internal/ddl`: `allow_reenable_disabled_indexes` decodes on `rebuild_heap`, is rejected on any
  other operation, and round-trips through the renderer.
- `internal/preflight`: `Rewritten` table test (rebuild_index one row, reorganize_index one row,
  rebuild_heap all rows including disabled, unknown index name none); a heap with nonclustered
  indexes is checked against the total and gets the scope `Warn`; no warning without indexes; a
  disabled index `FAIL`s without the key and `Warn`s with it, and the space detail says it is
  unsized; an unreadable size keeps "size unknown, not checked"; an unreadable index list `Warn`s.
- `cmd/sqlgopace`: dry-run heap lines, connected (fake reader, with and without a disabled index,
  with and without the key) and offline; `tuiForwarder.ops` and `step` carry `Detail` into the rows.
- `internal/tui`: `opRow` renders the detail; a `StepDoneMsg` detail replaces the start detail; a
  `LogMsg` does not touch any row.
- `internal/run`:
  - manifest start: scope line in `e.out` and the report for a heap with indexes, none without;
    `OpInfo.Detail` carries `cancel only`, the heap scope, or both;
  - before/after recorded on a successful rebuild; no after on a failed rebuild;
  - a canceled reorganize records an after labeled partial;
  - a re-enabled index renders `(was disabled)`, no percentage, after-total only;
  - a `RESUME` statement gets a before read like a `REBUILD` does;
  - a reader error does not change the outcome; both sides unknown gives no line and one
    manifest-level line; `nil` reader gives nothing;
  - totals: a heap rebuild followed by a rebuild of one of its indexes counts that index once,
    first before and last after.
- `internal/report`: rendering of the one-structure line, the heap block, `unknown`, partial,
  was-disabled, the totals; `HumanizeKB` KB/MB/GB/TB boundaries; the two `runs` columns
  migrate on an existing database and are written.
- `internal/plan` / `internal/maint`: a multi-partition heap is summed; `max_size_mb` compares heap
  plus nonclustered; `min_size_mb` compares the heap alone; a heap with a disabled index is skipped,
  and so is one whose disabled-index read fails; all skip lines are written; the `Reason` names the
  indexes; `Metrics.SizeMB` is the heap alone summed over partitions.

## Review outcome

Claude review: findings 1-13 accepted and folded in above; finding 11 (disabled indexes) settled by
the server test in the direction it warned of, and upgraded from a filter to a guard. agy review:
finding 1 (reorganize is not rolled back) accepted, same as Claude's finding 4; finding 2 (history
for skipped heaps) rejected — the history records only decisions that emit an operation, so moving
the skip into `Decide` would not record it either (see Scope). agy's claim that `internal/tui`
imports `internal/report` is false: neither package imports another internal package outside its
own tests. A Perplexity analysis of the disabled-index question reached the right answer by
inference; the server test, not the analysis, is the evidence.
