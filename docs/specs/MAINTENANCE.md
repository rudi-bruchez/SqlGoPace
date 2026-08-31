# SqlGoPace — Maintenance mode (generated DDL from database state)

> Spec for **TASK01**. All code, comments, identifiers, and files are **English** (international
> project). This builds *on top of* `docs/specs/SPECS.md` and reuses its engine, monitoring, reaction
> hierarchy, recovery, and reporting unchanged. Read SPECS.md first.

## 1. Goal

Today SqlGoPace runs **predefined, hand-written** DDL: the operator authors a YAML manifest, the
engine generates the T-SQL, runs it, monitors it, and reacts to pressure. This task adds a second way
in: SqlGoPace **inspects the database and generates the maintenance work itself**, from live state and
a set of rules, then runs it through the very same engine.

Five maintenance categories are in scope for v1:

1. **Index maintenance** — fragmentation-driven `REORGANIZE` vs `REBUILD`.
2. **Compression analysis** *(the novel, headline feature)* — test `ROW`/`PAGE`, choose `PAGE` only on
   substantial gain, protect write-intensive objects, decide **per partition**, fall back to `NONE`
   when there is no real gain.
3. **Update statistics** — modification-counter-driven `UPDATE STATISTICS`.
4. **DBCC CHECKDB** — scheduled integrity check.
5. **Heap maintenance** — detect heaps (tables with no clustered index) and `REBUILD` the ones that
   have accumulated **forwarded records** (and/or fragmentation / free-space waste), within size
   bounds. Forwarded records force extra random I/O on every heap scan; an `ALTER TABLE … REBUILD`
   clears them and reclaims space.

The existing predefined-manifest workflow is **untouched**; everything here is additive.

### Scope: one database at a time, server-wide via M8

The object-level analysis and the DDL it generates always run in **one database's context** — the
analysis reads (`sys.indexes`, `sys.partitions`, `sys.dm_db_partition_stats`,
`sys.dm_db_index_physical_stats`, `sys.dm_db_index_operational_stats`, `sys.dm_db_stats_properties`,
`sp_estimate_data_compression_savings`) are all **database-scoped to the current context**, and the
generated DDL (`ALTER INDEX … ON [schema].[table]`, `ALTER TABLE … REBUILD`, `UPDATE STATISTICS`)
executes against the **current** database — none of it can be three-part-named across databases. The
**multi-database mode** (M8, §17) therefore does not work around this; it **iterates** eligible
databases, opening **one connection per database** so each runs in its own context.

**`check_db` is the one exception**: `DBCC CHECKDB (name)` runs from any context, so
`checkdb.databases: [...]` (spec §4.5) genuinely targets several databases from a single connection.

**Status of the manifest `database:` field.** It is now **honored end to end** (M8.3 + M8.4): the run
path groups the queue by `database:` and runs **one engine per database, each on a connection in that
database's context** (§17.6), so `plan --all-databases` → run, and `--auto --all-databases`, execute
every database correctly. Each engine processes only its database's manifests (empty `database:` =
the connected database), leaving the rest for their own engine. *(Behaviour change since the
single-database era: a manifest whose `database:` names a database other than the connection's is no
longer silently run against the connection.)* Crash recovery is multi-database too (M8.5): each
orphan's sidecar records the database it ran in, and recovery reconciles it against a connection in
that database (an orphan whose database is unreachable — e.g. now an AG secondary — is left for a
later run). M8 is complete; the only outstanding item is a multi-database end-to-end run under
`make e2e`.

## 2. Core design decision — the planner emits manifests

The reusable value of SqlGoPace is the **engine** (`internal/run`): claim → expand → preflight →
`ddl.Plan` → monitored run with the reaction hierarchy → report → crash recovery. A `Manifest` is just
an ordered list of `Operation` sum-types, and `ddl.Generate` renders each to T-SQL.

So the maintenance mode is, at heart, a **planner that produces a `ddl.Manifest` from live DB state +
rules** instead of the operator hand-writing the YAML. The generated operations then flow through the
existing engine **with no changes to the run/monitor/react/recover path**. This is the KISS win and
the safety win: one execution path, already tested and hardened.

There are **two surfaces** over that planner (confirmed: build the first now, the second later):

- **`plan` subcommand (primary, v1).** Analyze the database and **write manifest YAML files into
  `01.to_run/`** (or a chosen directory), each operation annotated with the *reasoning* behind it
  (chosen compression, measured fragmentation, estimated gain, write-intensity, the rule that fired).
  The operator **reviews, edits, reorders, or deletes** the manifests, then runs them through the
  normal engine. Auditable, git-able, diff-able, `--dry-run` friendly — no surprises, no locks taken
  during analysis beyond cheap reads.

- **`--auto` run flag.** Once the analysis is trusted, analyze and run in a single invocation
  (unattended / SQL Agent / cron). Same planner, same engine: it writes the generated manifests into
  the queue and **immediately processes the queue**, skipping only the human review pause. Writing to
  the queue (rather than running purely in memory) is deliberate — a crash mid-run leaves recoverable
  manifests + sidecars in `02.processing/`, exactly like any other run. Off by default; explicitly
  opted in.

```
            ┌─────────────┐     materialize      ┌────────────┐   review    ┌────────────────┐
 plan  ───▶ │  ANALYZER   │ ───▶ 01.to_run/*.yaml ───────────▶ │  operator  │ ──▶ existing engine
            │ (read-only) │                                     └────────────┘
            └─────────────┘
 --auto ──▶ same ANALYZER ──▶ 01.to_run/*.yaml ──(no review pause)──────────────▶ existing engine
```

The analyzer is **pure-ish at the edges**: a thin `internal/mssql` layer that reads the analysis DMVs
(impure), feeding a **pure decision core** in `internal/maint` that turns measurements + rules into
`ddl.Operation`s. The decision core has zero I/O and is exhaustively table-tested, exactly like the
existing `ddl` core.

## 3. New operation types

Maintenance needs operations the closed sum-type does not yet model. They slot into the existing
`ddl.Operation` interface (`CommandType()`, `Target()`, `Validate()`), get a matrix key, a generator
in `generate.go`, and preflight coverage — the same pattern as every existing operation.

| New `operation`      | Generated T-SQL                                                        |
|----------------------|------------------------------------------------------------------------|
| `reorganize_index`   | `ALTER INDEX … REORGANIZE [PARTITION = n] [WITH (LOB_COMPACTION = ON)]` |
| `update_statistics`  | `UPDATE STATISTICS … [stat] WITH {FULLSCAN \| SAMPLE n PERCENT}[, RESAMPLE]` |
| `check_db`           | `DBCC CHECKDB ([db]) WITH NO_INFOMSGS, ALL_ERRORMSGS[, PHYSICAL_ONLY][, DATA_PURITY][, MAXDOP = n]` |
| `rebuild_heap`       | `ALTER TABLE [schema].[table] REBUILD [WITH (…)]` (clears forwarded records, reclaims space) |

Plus a change to the **existing** index operations:

- **`partition` field** (optional `int`) on `rebuild_index` and `reorganize_index`. Empty = whole
  index. Set = `… REBUILD PARTITION = n …` / `… REORGANIZE PARTITION = n …`. This is what makes
  **per-partition compression** expressible. The expansion of `index: ALL` is unaffected; partition
  targeting is orthogonal and only emitted by the planner (or written by hand).
  - **A single-partition rebuild cannot be `RESUMABLE`.** SQL Server's single-partition rebuild
    syntax accepts only `SORT_IN_TEMPDB` / `MAXDOP` / `DATA_COMPRESSION` / `XML_COMPRESSION`, plus
    `ONLINE` / `WAIT_AT_LOW_PRIORITY` — but **not** `RESUMABLE`
    ([ALTER INDEX docs](https://learn.microsoft.com/sql/t-sql/statements/alter-index-transact-sql#arguments)).
    Option resolution therefore drops `RESUMABLE` for a partition-targeted rebuild (the same way it
    does for `ALTER INDEX ALL`), keeping `ONLINE`/`WALP`. The reaction loop loses pause/resume for
    these, falling back to wait→cancel→retry like a non-resumable rebuild.

### 3.1 Semantics that matter for generation

- **Compression changes always imply `REBUILD`.** `REORGANIZE` cannot change `DATA_COMPRESSION`; it only
  defragments in place and (optionally) compacts LOBs. So any compression decision other than
  "leave as is" produces a `rebuild_index` with the chosen `data_compression`, never a
  `reorganize_index`. The planner enforces this.
- **`check_db` targets a database, not a table.** Its `Target()` carries the database name in place of
  schema/table; preflight validates the database, not an object. It takes none of the
  ONLINE/RESUMABLE/WALP family.
- **`update_statistics`** may target a whole table (all stats) or a single named statistic. `FULLSCAN`
  vs `SAMPLE n PERCENT` comes from the rules, not option injection.
- **A freshly rebuilt index already has fullscan statistics.** The planner must **not** emit an
  `update_statistics` for a statistic backing an index it rebuilt in the same plan (it would be
  redundant work and extra log). This ordering rule lives in the pure planner and is unit-tested.
- **`rebuild_heap` is heavier than it reads, and is its own operation — not a `rebuild_index`.** A heap
  has no index name; the target is the table. Critically, `ALTER TABLE … REBUILD` **also rebuilds every
  nonclustered index on the table** (the heap's RID locators change), so it is far from free. Two
  consequences the planner enforces: (a) it never emits a separate `rebuild_index` for a nonclustered
  index on a heap it is rebuilding in the same plan (the heap rebuild already does it — same family as
  the stats-after-rebuild suppression); (b) `rebuild_heap` accepts `ONLINE`, `MAXDOP`, and
  `DATA_COMPRESSION`, but **not** `RESUMABLE` (heap rebuild is not resumable) and not
  `WAIT_AT_LOW_PRIORITY`.

### 3.2 Matrix entries (`ddl_compatibility.yaml`)

Following "data, not code", the new commands get matrix rows. Most are sparse:

```yaml
  reorganize_index: {}                  # always online, incremental; no injectable WITH options
                                        # (LOB_COMPACTION is decided by rules, not version-gated)
  update_statistics: {}                 # FULLSCAN/SAMPLE from rules; MAXDOP not injected in v1
  check_db:
    maxdop: { min_major: 12, editions: [enterprise, standard, azure] }   # DBCC … MAXDOP since 2014

  rebuild_heap:                         # ALTER TABLE … REBUILD
    online:           { min_major: 13, editions: [enterprise, azure] }    # ONLINE heap rebuild, 2016+
    maxdop:           { min_major: 9,  editions: [enterprise, standard, azure] }
    data_compression: { min_major: 10, editions: [enterprise, azure] }
    # NB: no RESUMABLE, no WAIT_AT_LOW_PRIORITY for a heap rebuild on any version.
```

`PHYSICAL_ONLY` / `DATA_PURITY` / `NO_INFOMSGS` are behavioural switches from the rules, not
version-gated capabilities, so they are not matrix options.

## 4. The analyzer — what it reads

NOTE : all ad-hoc queries here must run with `OPTION (RECOMPILE)` to prevent the plan cache to keep a them.

All analysis runs on the **monitoring pool** (never the pinned execution connection) and uses cheap,
non-`DETAILED` reads. Each category maps to well-known DMVs.

### 4.0 Cheap object inventory (the shared first pass)

Before any expensive read, a single cheap sweep over catalog views builds the candidate list that every
category filters from. It costs nothing close to `sp_estimate` or `physical_stats` — it reads only
metadata and cached page counts:

```sql
SELECT
    OBJECT_SCHEMA_NAME(i.object_id)  AS [schema],
    OBJECT_NAME(i.object_id)         AS [table],
    i.object_id, i.index_id, i.name  AS index_name,
    i.type, i.type_desc,                       -- 0=HEAP, 1=CLUSTERED, 2=NONCLUSTERED, 5/6=columnstore…
    p.partition_number,
    p.data_compression_desc,                   -- current compression: NONE | ROW | PAGE | COLUMNSTORE…
    p.rows,
    s.used_page_count * 8 / 1024.0   AS size_mb -- 8 KB pages → MB (cheap; from dm_db_partition_stats)
FROM sys.indexes i
JOIN sys.objects o            ON o.object_id = i.object_id AND o.is_ms_shipped = 0   -- skip system objects
JOIN sys.partitions p         ON p.object_id = i.object_id AND p.index_id = i.index_id
JOIN sys.dm_db_partition_stats s ON s.partition_id = p.partition_id
WHERE i.object_id = OBJECT_ID(QUOTENAME(@schema) + '.' + QUOTENAME(@table))  -- or sweep the whole DB
OPTION (RECOMPILE, MAXDOP 1);
```

This one read supplies, per index **and partition**, the three facts every downstream decision needs
without touching data: **current compression** (`data_compression_desc` — feeds `IndexMeasurement.Current`
and lets us skip already-`PAGE` objects), **size** (`size_mb` — feeds the `rebuild_max_size_mb` ceiling
and the heap size bounds), and **row count**. It also classifies heaps (`type = 0`) and
columnstore/XML/spatial indexes (which use a different compression model and are excluded from rowstore
compression analysis). The expensive reads below then run **only on the survivors** of the cheap filters
— this is what keeps `sp_estimate` (which samples into tempdb) and `SAMPLED` `physical_stats` affordable.
*(Adapted from `docs/specs/uncompressed-objects.sql`.)*

### 4.1 Index fragmentation (reorg vs rebuild)

```sql
SELECT object_id, index_id, partition_number, avg_fragmentation_in_percent, page_count
FROM sys.dm_db_index_physical_stats(DB_ID(), @object_id, @index_id, @partition_number, 'LIMITED')
OPTION (RECOMPILE);
```

`LIMITED` is the cheap mode (leaf level only); never `DETAILED` (IO-heavy, per SPECS §4). Returns one
row per partition, so partition-level decisions fall out naturally.

### 4.2 Compression estimation (the headline)

**Two-pass, like the heaps.** The cheap inventory (§4.0) already gives current compression and size, so
`sp_estimate_data_compression_savings` is called **only for candidates that can still benefit** — rowstore
indexes/partitions that are `NONE` or `ROW` (an already-`PAGE` object is skipped; there is nothing higher
to try), within any size bounds, and not excluded by an override. Each surviving candidate gets the
estimate:

```sql
EXEC sp_estimate_data_compression_savings
     @schema_name = N'…', @object_name = N'…',
     @index_id = @index_id, @partition_number = @partition_number,
     @data_compression = N'ROW';     -- and again with N'PAGE'
```

It returns `size_with_current_compression_setting` and `size_with_requested_compression_setting` (KB)
per partition; the planner runs it for `ROW` and `PAGE`, reads the current size, and computes the **gain**
of each (§5.4). Because this proc **samples real data into tempdb**, gating it behind the cheap pass is
what keeps the analysis affordable on a large database — the difference between estimating every object
and estimating only the uncompressed/under-compressed ones can be enormous.

### 4.3 Write-intensity (protect hot objects from PAGE)

```sql
SELECT leaf_insert_count, leaf_update_count, leaf_delete_count,
       range_scan_count, singleton_lookup_count
FROM sys.dm_db_index_operational_stats(DB_ID(), @object_id, @index_id, @partition_number)
OPTION (RECOMPILE);
```

`PAGE` compression costs CPU on every modification, so a write-intensive object should be capped at
`ROW` or `NONE`. **Caveat — these counters are cumulative since the last cache eviction / server
restart**, not a fixed window. v1 treats them as a heuristic (ratio of writes to total access since
restart, above a configured threshold ⇒ "write-intensive"). The estimation is recorded with its
`sys.dm_os_sys_info.sqlserver_start_time` so a future version can baseline over a sampling window. The
limitation is documented, not hidden.

### 4.4 Statistics staleness

```sql
SELECT s.name, sp.rows, sp.rows_sampled, sp.modification_counter, sp.last_updated
FROM sys.stats s
CROSS APPLY sys.dm_db_stats_properties(s.object_id, s.stats_id) sp
WHERE s.object_id = @object_id
OPTION (RECOMPILE);
```

A statistic is stale when `modification_counter` crosses a threshold (default: the Ola-style
`sqrt(1000 * rows)` dynamic threshold, or a configured percentage of `rows`).

### 4.5 CHECKDB targets

Per the configured database list (or the connection's database). `check_db` is database-level; the
analyzer simply emits one `check_db` op per in-scope database with the rule-selected switches.

### 4.6 Heaps and forwarded records

A heap is a table with no clustered index (`sys.indexes.index_id = 0`, `type_desc = 'HEAP'`). The
trigger that matters is **forwarded records**: an update that grows a row past its page leaves a
forwarding pointer, and every subsequent heap scan pays an extra random read to chase it. `ALTER TABLE
… REBUILD` clears them and reclaims free space.

**Crucial cost caveat:** `forwarded_record_count` is **only populated by `SAMPLED` or `DETAILED`** modes
of `sys.dm_db_index_physical_stats` — **not** `LIMITED`. To keep that cost bounded, heap analysis is
**two-pass**:

1. **Cheap discovery** — list heaps and their size from `sys.indexes`/`sys.partitions`/
   `sys.allocation_units` (or a `LIMITED` scan), and apply the size bounds (`min_size_mb` /
   `max_size_mb`) first.
2. **Targeted `SAMPLED` scan** — only for heaps that passed the size filter, read the forwarded-record
   and space metrics:

```sql
SELECT object_id, forwarded_record_count, record_count,
       avg_fragmentation_in_percent, avg_page_space_used_in_percent, page_count
FROM sys.dm_db_index_physical_stats(DB_ID(), @object_id, 0, NULL, 'SAMPLED')
OPTION (RECOMPILE);
```

This mirrors the reference script (`015.rebuild_heaps.sql`): it walks heaps, computes a forwarded-row
percentage and a page-space deviation, and emits `ALTER TABLE … REBUILD` within `[min, max]` size
bounds — and is AlwaysOn/mirroring-aware (act on the **primary** only). SqlGoPace inherits the
primary-only safeguard from the existing preflight's replica-state check (SPECS §4).

### 4.7 Analyzer error handling & degradation

Analysis sweeps **many** objects; one bad object must never sink the whole plan. The analyzer
degrades, it does not abort:

- **Per-object read failure** (`sp_estimate_data_compression_savings` errors on an object, a
  `physical_stats` scan errors or times out, a table is dropped mid-sweep): **skip that object**, record
  the reason in the analysis log (`decision = skip`, `reason = "read error: …"`), and continue. The
  object simply gets no operation this run.
- **Missing permission** (`VIEW DATABASE STATE`, or rights to run `sp_estimate_*`): this is not
  per-object, it is fatal to the category — **fail fast** with a clear, actionable message naming the
  missing grant (see SPECS §17), exit code 2. Better to stop than to silently produce an empty plan
  that looks like "nothing to do".
- **Missing DMV / old server** (e.g. a DMV column absent on an unsupported version): treat as "no data"
  for that signal and fall back to the coarser decision (e.g. no write-intensity reading → do not apply
  the PAGE cap, decide on gain alone), exactly as the existing `SessionWaits` reader already does.
- **Database offline / inaccessible** (one of several `checkdb.databases`): skip that database with a
  logged warning; process the rest.
- **No silent truncation:** every skip is counted and surfaced (`log()` + history), so an empty or
  partial plan is never mistaken for "the database is healthy".

## 5. Decision rules — `maintenance_profile.yaml`

The rules live in a dedicated **data file** (analogous to `ddl_compatibility.yaml`), so tuning
thresholds or carving out tables is a config change, never a recompile. Path configured in
`config.yaml` (`maintenance.profile_file`).

```yaml
# maintenance_profile.yaml
index:
  page_count_floor: 1000           # skip indexes smaller than this (Ola default)
  reorganize_from_percent: 5       # frag 5–30%  → REORGANIZE
  rebuild_from_percent: 30         # frag > 30%  → REBUILD
  # "Ban REBUILD on big tables" — rule-driven (see §5.1). Above the ceiling, downgrade or skip.
  rebuild_max_size_mb: 51200       # 50 GB; above this a REBUILD is downgraded per rebuild_over_ceiling
  rebuild_over_ceiling: reorganize # reorganize | skip
  lob_compaction: true             # WITH (LOB_COMPACTION = ON) on REORGANIZE

compression:
  enabled: true
  # PAGE is only chosen over ROW when its EXTRA saving is substantial:
  page_min_extra_gain_percent: 10  # PAGE must save ≥10% more pages than ROW, else ROW
  min_gain_percent: 5              # below this absolute saving → NONE (don't bother rebuilding)
  write_intensive_ratio: 0.30      # writes / total access above this → cap at ROW (never PAGE)
  write_intensive_compression: row # row | none — cap applied to hot objects
  activity_floor: 1000             # min total access since restart before write_ratio is trusted (§5.4)
  per_partition: true              # decide each partition independently (alive/hot vs cold/archive)
  # Restrict which objects the heuristic may compress (§5.2). Globs match a whole
  # table ("schema.table") or one index ("schema.table.index"); exclude wins. This
  # filter governs ONLY compression — fragmented objects are still reorganized/rebuilt.
  objects:
    include: []                    # allowlist: empty = all; ["dbo.BigFact", "dbo.Orders.IX_Orders_Date"]
    exclude: []                    # denylist: ["dbo.HOT_*", "dbo.Orders.PK_Orders"]

heap:
  enabled: true
  min_size_mb: 10                  # skip heaps smaller than this (rebuild not worth it)
  max_size_mb: 10000              # skip heaps larger than this (too heavy; needs a maintenance window)
  forwarded_record_percent: 10     # rebuild when forwarded_record_count / record_count ≥ this
  # secondary triggers, OR-ed with the forwarded-record trigger:
  fragmentation_percent: 15        # OR avg_fragmentation_in_percent ≥ this
  free_space_deviation_percent: 30 # OR page-space deviation (100 − avg_page_space_used) ≥ this
  online: null                     # null = auto (matrix); true/false to force ONLINE heap rebuild

statistics:
  enabled: true
  sample: fullscan                 # fullscan | { percent: 30 }
  # modification threshold: dynamic (Ola sqrt) unless a percent is given
  modification_percent: null       # e.g. 20 → update when modifications ≥ 20% of rows

checkdb:
  enabled: true
  databases: []                    # empty = the connection's database only
  physical_only: false
  data_purity: false
  maxdop: null                     # null = server default; injected only if matrix allows

# Per-object overrides — the "both modes" answer (§5.1). Glob on schema.table.
overrides:
  - match: "dbo.AUDIT_*"
    rebuild: forbid                # never REBUILD these; REORGANIZE only
  - match: "dbo.STAGING"
    skip: true                     # exclude entirely from index/compression maintenance
  - match: "dbo.DISPATCH"
    compression: page              # pin compression; bypass the gain/write-intensity heuristic
```

### 5.1 REBUILD vs REORGANIZE — rule-driven **with** overrides ("both modes")

Confirmed model: deterministic rules produce a default decision, and per-object overrides can **pin or
forbid** it. The decision for one index/partition:

1. **Skip** if `page_count < page_count_floor`, or an override marks it `skip`.
2. Else by fragmentation: `< reorganize_from_percent` → nothing; `< rebuild_from_percent` →
   `REORGANIZE`; otherwise `REBUILD`.
3. **Size ceiling:** a `REBUILD` whose object exceeds `rebuild_max_size_mb` is downgraded per
   `rebuild_over_ceiling` (`reorganize` or `skip`). This is the "ban REBUILD on big tables" rule —
   automatic but **deterministic from a measured size**, not random.
4. **Overrides win:** `rebuild: forbid` forces `REBUILD`→`REORGANIZE`; `compression: <value>` pins the
   compression and bypasses the heuristic; `skip: true` removes the object.

Because the plan is **materialized for review** (§2), "automatic" never means "unsupervised surprise":
the operator sees every decision and its reason before any lock is taken (and `--auto` is an explicit,
separate opt-in for when the rules are trusted).

### 5.2 Compression decision (per object, or per partition when `per_partition`)

For each candidate:

1. If an override pins `compression`, use it (honored even if the object is excluded by the scope filter).
2. Else, if `compression.objects` is set and the object is outside it (not in a non-empty `include`, or in
   `exclude` — exclude wins, matched at `schema.table` or `schema.table.index` granularity), make **no
   compression change**. Fragmentation-driven reorganize/rebuild (§5.1) still applies.
3. Else read `NONE`/`ROW`/`PAGE` estimated sizes (§4.2) and write-intensity (§4.3).
4. If write-intensive (`ratio ≥ write_intensive_ratio`) → cap at `write_intensive_compression`
   (`ROW` or `NONE`), regardless of PAGE gain.
5. Else choose the **highest-gain tier that clears its bar**: prefer `PAGE` if it saves
   `≥ page_min_extra_gain_percent` more than `ROW`; else `ROW` if it saves `≥ min_gain_percent` vs
   current; else `NONE` (no rebuild emitted for compression).
6. A chosen compression that differs from the current setting emits a `rebuild_index` (whole index or
   `PARTITION = n`) carrying `data_compression`. The current setting is read from
   `sys.partitions.data_compression_desc` so an unchanged decision emits nothing.

### 5.3 Heap rebuild decision

Per heap (after the cheap size pre-filter of §4.6):

1. **Skip** if size is outside `[min_size_mb, max_size_mb]`, or an override marks the table `skip`.
2. **Rebuild** when **any** trigger fires: forwarded-record ratio ≥ `forwarded_record_percent`, **or**
   `avg_fragmentation_in_percent` ≥ `fragmentation_percent`, **or** free-space deviation ≥
   `free_space_deviation_percent`. Forwarded records are the primary motivation; the other two catch
   bloated/fragmented heaps the same way the reference script does.
3. Emit a `rebuild_heap` for the table. `ONLINE`/`MAXDOP`/`DATA_COMPRESSION` resolve through the matrix
   and policy like any other operation; an override may pin compression.
4. **Suppress redundant nonclustered rebuilds** — if the same plan would also `rebuild_index` a
   nonclustered index of this table, drop it: the heap rebuild already rebuilds all NC indexes (§3.1).

> Note: the long-term fix for a chronically-forwarding heap is usually **a clustered index**, not a
> repeated rebuild. SqlGoPace deliberately stays with `ALTER TABLE … REBUILD` (matches the reference
> script and is a non-schema-changing maintenance action); recommending a clustered index is advisory
> only and out of scope for v1.

### 5.4 Exact formulas (pin the arithmetic)

These are the precise computations the pure core implements, so there is no ambiguity. All percentages
are 0–100. Every divisor is guarded (denominator 0 ⇒ the metric is 0 / "no data", never a panic).

**Compression gain** — `sp_estimate_data_compression_savings` returns, per object/partition,
`size_with_current_compression_setting` (`cur_kb`, identical across the per-setting calls) and
`size_with_requested_compression_setting` for the `@data_compression` asked (`row_kb`, `page_kb`):

```
gain_row_pct   = (cur_kb - row_kb)  / cur_kb  * 100      # saving vs the CURRENT setting
gain_page_pct  = (cur_kb - page_kb) / cur_kb  * 100
page_extra_pct = (row_kb - page_kb) / row_kb  * 100      # how much SMALLER page is than row
```

Decision (after the override / write-intensity checks of §5.2):

```
if page_extra_pct >= page_min_extra_gain_percent AND gain_page_pct >= min_gain_percent:  choose PAGE
elif gain_row_pct >= min_gain_percent:                                                   choose ROW
else:                                                                                    choose NONE
```

A chosen tier equal to the current `data_compression_desc` emits **nothing**.

**Write-intensity** — from `sys.dm_db_index_operational_stats`:

```
writes = leaf_insert_count + leaf_update_count + leaf_delete_count
reads  = range_scan_count  + singleton_lookup_count
total  = writes + reads
write_ratio = writes / total                            # 0 when total == 0
write_intensive = (total >= activity_floor) AND (write_ratio >= write_intensive_ratio)
```

`activity_floor` guards against noise: a near-idle object (tiny `total` since restart) is **not** judged
write-intensive on a meaningless ratio — its few writes mean PAGE is harmless, so the gain-based choice
stands. (`activity_floor` is a profile knob; default small, e.g. 1000.) When write-intensive, the
chosen tier is capped at `write_intensive_compression` (`row` or `none`) regardless of gain.

**Statistics staleness** — from `sys.dm_db_stats_properties`:

```
if modification_percent is set:  stale = modification_counter >= rows * modification_percent / 100
else (dynamic / Ola):            stale = modification_counter >= sqrt(1000 * rows)
```

**Heap triggers** — from the `SAMPLED` `physical_stats` row (§4.6):

```
forwarded_pct       = forwarded_record_count / record_count * 100     # 0 when record_count == 0
free_space_dev_pct  = 100 - avg_page_space_used_in_percent
rebuild = (forwarded_pct      >= forwarded_record_percent)
       OR (avg_fragmentation_in_percent >= fragmentation_percent)
       OR (free_space_dev_pct >= free_space_deviation_percent)
```

## 6. Worklist, ordering, and batching

A single analysis can surface a lot of work; running all of it at once is exactly the production
hazard SqlGoPace exists to avoid. The planner therefore supports **bounding and ordering** a plan.

- **Ordering** within a generated manifest: clustered index first (matches existing ALL-expand
  convention), then by configured key — default **smallest-first** (quick wins, least log per op) with
  a `most_fragmented_first` / `most_gain_first` alternative in the profile.
- **Batching budget:** `maintenance.max_operations` and/or `maintenance.time_budget_minutes` cap how
  much one `plan` invocation materializes. When the budget truncates the worklist, the planner
  **`log()`s exactly what was dropped** (count + first few) — silent truncation would read as
  "everything is maintained" when it is not (per the project's no-silent-caps principle).
- **Manifest splitting:** categories are emitted as **separate, filename-ordered manifests** so the
  operator controls sequencing and the engine keeps its strict one-manifest-at-a-time guarantee, e.g.:

```
01.to_run/
  010_maint_MYDB_checkdb.yaml          # integrity first (configurable; some shops run it separately)
  020_maint_MYDB_index.yaml            # reorganize/rebuild (+ compression rebuilds)
  030_maint_MYDB_heaps.yaml            # heap rebuilds (forwarded records / fragmentation)
  040_maint_MYDB_statistics.yaml       # stats for NON-rebuilt objects only (§3.1)
```

Filenames embed database + category; the numeric prefix gives the operator the sort handle SPECS §2
already relies on.

## 7. History — extend the local SQLite store

Confirmed: materialize worklists as manifests **and** record analysis snapshots in the **existing**
local SQLite history (`internal/report` history store), extended with maintenance tables. This keeps
one history destination and avoids needing write/DDL permission on the target server. (A target-server
`CommandLog`-style table is noted as a future option but is **not** v1.)

Two tables are added by an additive, idempotent migration in `report` (opening an existing run-history
DB simply creates them; existing `runs` data is untouched):

- `maintenance_plan` — one row per `plan` run: `database, generated_at, manifests, operations`.
- `maintenance_analysis` — one row per **emitted** decision, linked via `plan_id`: `category, target,
  decision, reason, page_count, size_mb, fragmentation, forwarded_percent, write_ratio,
  current_compression, chosen_compression`.

The object identity lives in `target` (`schema.table.index (+partition)` or `schema.table` or the
database); the rule that fired and the gain figures live in `reason`. v1 records **emitted operations
only** (the actionable set, bounded by what a run would execute) — recording every healthy/skipped
object is deferred to keep the table small on large databases. The structured columns
(`fragmentation`, `forwarded_percent`, `write_ratio`, `chosen_compression`…) come from the metrics
attached to each `maint.Decision`.

This enables the trend analysis the feature is about: which objects are repeatedly rebuilt
(oscillation → fill-factor problem), whether a chosen compression held, how fragmentation evolves.
Recording is **best-effort and opt-in** (`history.enabled`) — a failure is logged, never fatal, and
the existing per-run `.log` / `RunRecord` history is unchanged.

## 8. Monitoring & reaction fit (per operation)

The engine's reaction hierarchy (SPECS §9) was built around `REBUILD`/`CREATE`. The new operations
have different interruption semantics; the planner tags each with the right capability so the existing
reaction loop does the right thing without new branches in the engine where avoidable.

| Operation          | Resumable | On pressure (blocking / log)                              | KILL cost |
|--------------------|-----------|-----------------------------------------------------------|-----------|
| `rebuild_index`    | yes (rowstore, ver/edition) | preferred: pause→resume (existing path)         | rollback (long) |
| `reorganize_index` | no (but **incremental**)    | **cancel is safe** — commits incrementally, no rollback storm; retry/skip remaining | cheap |
| `update_statistics`| no                          | cancel → cheap rollback; retry                  | cheap |
| `check_db`         | no                          | duration/tempdb-driven; on pressure **KILL** and report (read-only snapshot → nothing to roll back) | cheap |
| `rebuild_heap`     | no                          | like `rebuild_index` minus pause/resume — WALP/RESUMABLE unavailable, so on pressure: wait then cancel→KILL (and retry) | rollback (rebuilds all NC indexes too) |

This is implemented as a `CancelSafe` flag on `run.Capabilities` (alongside `Resumable`, `ADR`), set by
`cancelSafe(op)` for `reorganize_index` / `check_db` / `update_statistics`. It does **not** change which
`Action` the selector picks (these are not resumable, so they still `Cancel`) — it refines the
**narration** so a REORGANIZE cancel reads as a clean stop ("incremental — committed work preserved, no
rollback") rather than a rollback-bearing KILL. CHECKDB and stats otherwise rely on the existing
cancel/KILL path. No change to the monitoring threads themselves — they already poll blocking, log, and
progress.

## 9. Preflight additions

The existing preflight (SPECS §4) runs per manifest and already covers version/edition, object
validity, recovery model, log health, data/tempdb free space, AG, ADR. Maintenance adds:

- **`check_db`**: validate the database exists; warn on very large databases (CHECKDB tempdb/IO
  footprint); `PHYSICAL_ONLY` recommended past a configurable size.
- **`reorganize_index` / `update_statistics`**: object/stat existence (reuses the existing object
  validity check); these are light and need no extra space check.
- **Compression rebuilds** inherit the ONLINE-rebuild free-space check unchanged (a rebuild builds a
  copy).
- **`rebuild_heap`**: needs free space for a copy of the heap **plus** its nonclustered indexes (the
  rebuild re-creates them all); confirm the table is genuinely a heap; act on the **primary** replica
  only (reuses the existing AG/replica-state check).

Analysis itself runs **before** any manifest exists, so it has its own light guard: confirm
`VIEW DATABASE STATE` / the ability to run `sp_estimate_data_compression_savings`, and that the target
databases are online and accessible.

- **Multi-database (M8).** Ineligibility is enforced at two points: at **enumeration** (`UserDatabases`,
  §17.3) so an AG secondary / read-only / offline database is never queued, and again as a **run-start
  re-check** so a database that became ineligible *between* `plan` and the run (e.g. a failover) is
  **left in the queue and logged**, never run against the wrong replica. The connected database is
  always runnable (a usable connection is already held).

## 10. CLI surface

```bash
# v1 — analyze and MATERIALIZE reviewable manifests into the to_run dir:
sqlgopace plan --config config.yaml [--profile maintenance_profile.yaml] \
  [--database MYDB] [--categories index,compression,heaps,statistics,checkdb] [--out 01.to_run]

# Preview the analysis and the manifests it WOULD write, without writing anything:
sqlgopace plan --config config.yaml --dry-run

# Show the reasoning for each decision (reuses the existing --explain idiom):
sqlgopace plan --config config.yaml --dry-run --explain

# Then run the reviewed manifests through the normal engine (unchanged):
sqlgopace --config config.yaml [--tui]

# Analyze and run in one shot, unattended (explicit opt-in; no review pause):
sqlgopace --config config.yaml --auto [--profile …] [--categories …] [--database …]

# Multi-database (M8, §17): plan/run every eligible user database, or a chosen set:
sqlgopace plan   --config config.yaml --all-databases
sqlgopace plan   --config config.yaml --databases SALES,INVENTORY
sqlgopace        --config config.yaml --auto --all-databases
```

`plan` follows the `abort-resumable` subcommand pattern already in `cmd/sqlgopace` (dispatched before
flag parsing, its own `FlagSet`, narrow injected interfaces for testability). `--auto` is a flag on the
normal run path that swaps the queue source for the in-memory planner output.

## 11. Package layout

- **`internal/maint`** *(new)* — the **pure decision core**: takes measurement structs (fragmentation,
  compression estimates, write-intensity, stats properties) + the parsed profile, returns
  `[]ddl.Operation` plus a `[]Decision` reasoning trail (mirroring `ddl.Resolve`'s `[]Decision`). Zero
  I/O; exhaustively table-tested.
- **`internal/mssql`** — add the analysis reads: `ObjectInventory` (the cheap §4.0 catalog +
  `dm_db_partition_stats` sweep that yields current compression, size, and rows per index/partition and
  drives candidate selection), then `IndexPhysicalStats`, `EstimateCompression`,
  `IndexOperationalStats`, and `StatsProperties` run only over the inventory's survivors. Same
  thin-typed-function style as the existing DMV layer (`dmv.go`, `indexes.go`, `waits.go`).
- **`internal/ddl`** — add the three new operation structs + generators + matrix keys + the `partition`
  field on rebuild/reorganize.
- **`internal/report`** — the two new history tables + migration.
- **`cmd/sqlgopace`** — the `plan` subcommand and the `--auto` flag wiring.

Dependency direction is preserved: `maint` → `ddl` (pure); the analyzer wiring in `cmd`/`run` pulls
`mssql` reads and feeds `maint`. Nothing pure imports `mssql`.

## 12. Implementation phases

Each phase is independently buildable/testable; the pure phases need no database.

- **M0 — operation types (pure).** Add `reorganize_index`, `update_statistics`, `check_db`,
  `rebuild_heap`, the `partition` field, generators, matrix rows. Golden-file T-SQL tests. `--dry-run`
  of a *hand-written* manifest using the new ops works offline. *No analyzer yet.*
- **M1 — profile loader (pure).** Parse + validate `maintenance_profile.yaml`; table-test threshold
  and override resolution.
- **M2 — decision core (pure).** `internal/maint`: measurements + profile → operations + reasons.
  This is the correctness heart (compression tiering, write-intensity cap, reorg/rebuild + size
  ceiling, override pin/forbid, stats-after-rebuild suppression, per-partition, heap triggers +
  NC-index suppression). Exhaustive table-driven tests, no DB.
- **M3 — analysis reads (integration).** The `mssql` DMV/`sp_estimate_*` functions, behind the
  `integration` build tag against Dockerized SQL Server. Built cheap-pass-first: the §4.0
  `ObjectInventory` sweep selects candidates, then the expensive reads run only over survivors — the
  two-pass heap read (inventory size filter → targeted `SAMPLED` `physical_stats` for forwarded records,
  §4.6) and the gated `sp_estimate` (only on uncompressed/under-compressed rowstore objects, §4.2).
- **M4 — `plan` subcommand.** Wire reads → core → manifest materialization into `01.to_run/`, with
  `--dry-run`/`--explain`. Then it's end-to-end: `plan` → review → existing engine.
- **M5 — history.** SQLite maintenance tables + recording.
- **M6 — reaction tagging.** `Incremental`/`CancelSafe` capability for `REORGANIZE`; confirm CHECKDB /
  stats cancel paths. Integration tests for reorganize-cancel and checkdb-kill.
- **M7 — `--auto`.** A flag on the run path that analyses, writes the generated manifests into the
  queue, and processes them in one invocation — no review pause; rejected together with `--dry-run`
  (use `plan --dry-run` to preview). Materialising into the queue keeps crash recovery intact.
- **M8 — multi-database mode.** Server-wide maintenance: enumerate eligible user databases, scope via
  flags + profile, analyse and run **one connection per database**, honoring the manifest `database:`
  field. Specified in §17. Built: M8.1 (connection + enumeration), M8.2 (scope resolution), M8.3
  (`plan` per-database blocks), M8.4 (run engine-per-database, `--auto` multi), M8.5 (per-database
  crash recovery via the sidecar `database` + a probe resolver), M8.6 (run-start eligibility re-check
  so a failover-since-plan database is left in the queue, + docs). **Complete.** The remaining
  live-server validation is a multi-database `make e2e` run.

## 13. Dangers & how they are contained

- **Generating too much work at once.** → ordering + `max_operations`/`time_budget_minutes` budget,
  separate filename-ordered manifests, and the strict one-at-a-time engine guarantee (SPECS §2).
- **"Automatic" feels random.** → every decision is deterministic from measured inputs + named rules,
  emitted with a reasoning trail, and **materialized for review** before any lock; `--auto` is a
  separate explicit opt-in.
- **PAGE on a hot table tanks write throughput.** → write-intensity cap (§5.2 step 3) and per-object
  `forbid`/pin overrides.
- **Wasteful rebuilds for no gain.** → `min_gain_percent` floor → `NONE`; unchanged-compression
  decisions emit nothing.
- **operational_stats counters reset on restart.** → documented heuristic, snapshot stamped with
  `sqlserver_start_time`; windowed baseline is future work, not a hidden assumption.
- **Redundant stats updates after a rebuild.** → planner suppresses stats for just-rebuilt indexes.
- **CHECKDB on huge DBs.** → preflight size warning + `physical_only` guidance.
- **Heap rebuild is silently expensive** (rebuilds all NC indexes, no pause/resume). → size bounds
  (`min/max_size_mb`), NC-index rebuild suppression in the same plan, and a forwarded-record trigger so
  it only fires when there is a real scan-cost problem — never on a quiet heap.
- **`forwarded_record_count` needs a `SAMPLED` scan.** → two-pass read (cheap size filter first, then
  `SAMPLED` only on the survivors) keeps the IO cost bounded; never `DETAILED`.

## 14. Out of scope for v1 (noted for later)

- **Multi-database / server-wide object maintenance.** The current build analyses and maintains the
  **connected database only** (see "Scope" under §1); `check_db` is the lone multi-database operation.
  The server-wide mode is **fully specified as M8 in §17** (enumeration + eligibility, scope flags +
  profile selector, per-database connection, honoring the manifest `database:` field) — specified, not
  yet built.
- Target-server `CommandLog`-style history table (v1 uses the local SQLite store).
- Windowed/baselined write-intensity (v1 uses cumulative-since-restart counters as a heuristic).
- Columnstore-specific maintenance (`REORGANIZE … COMPRESS_ALL_ROW_GROUPS`), partition merge/split,
  fill-factor tuning, index-usage-driven drop recommendations.
- `MAXDOP` injection for `UPDATE STATISTICS` (2016 SP2+); kept out of the matrix in v1.

## 15. Testing notes & known friction

Hard-won points to keep in front of mind across phases — the spec is the durable source of truth, so
re-read the relevant § when picking up a later phase rather than relying on conversation memory.

### 15.1 The matrix-vs-generator boundary (do not over-stuff the matrix)

The compatibility matrix answers exactly one question: **"is this option even legal on this
version × edition?"** — `ONLINE`, `MAXDOP`, `DATA_COMPRESSION`, `RESUMABLE`, `WAIT_AT_LOW_PRIORITY`.
Everything that is a **rule-driven behavioural switch** — `PHYSICAL_ONLY`, `DATA_PURITY`,
`NO_INFOMSGS`, `FULLSCAN`/`SAMPLE`, `LOB_COMPACTION` — belongs in the **generator + profile**, never in
`ddl_compatibility.yaml`. When adding the new operations, resist the pull to generalise these into the
matrix; they are not capabilities.

### 15.2 `check_db` breaks the `ObjectRef{schema, table, name}` shape

`check_db` targets a **database**, not an object. Do **not** smuggle a database name into the `table`
field. Decide the target shape deliberately — e.g. a database-level target where `Target()` carries
the database in a dedicated field — and make preflight validate the database, not an object. This is
the single most structural snag in the feature; handle it consciously in **M0**.

### 15.3 The `partition` field touches existing code

Adding `partition` to `rebuild_index`/`reorganize_index` touches `ddl.ExpandRebuildAll` and its golden
tests in `internal/ddl`. It is additive (empty partition = today's behaviour), but **must not regress
the existing `ALL`-expansion tests**. Run the existing `ddl` suite before and after.

### 15.4 Where the real risk lives (it inverts the obvious)

- **M2 (pure decision core) is the correctness heart and needs no database.** Compression tiering
  edge cases, the write-intensity ratio math, per-partition decisions, and the NC-index / stats
  suppression rules are where subtle bugs hide. It is the *lowest-friction* phase to run and the
  *highest-value* to test exhaustively (table-driven, golden) — weight test effort here.
- **M3 (integration) friction is narrower than it looks.** The repo already wires a Dockerized SQL
  Server via `make e2e` (Developer edition), so standing up an instance is solved. AlwaysOn AG is *not*
  required to test any of this — the primary-only safeguard is the existing preflight AG check, already
  covered. To exercise heaps, seed a heap with forwarded records (a heap + a variable-length column +
  `UPDATE`s that grow rows past the page). The genuine M3 cost is `sp_estimate_data_compression_savings`
  itself: it **samples real data into tempdb** and can be slow/heavy on large tables, and returns a
  per-index/partition rowset to parse carefully. Run integration tests on demand and feed failures back.
- **`forwarded_record_count` requires a `SAMPLED` scan** (§4.6) — verify the integration test actually
  reads a non-zero count, since a `LIMITED` scan silently returns zero and a green test would prove
  nothing.

## 16. Golden-path example (state → decisions → manifest)

One worked end-to-end run, using the default profile from §5. It doubles as the canonical
acceptance test for the pure decision core (M2) and the `plan` subcommand (M4): given these
measurements, the planner must emit exactly this manifest. Target server: SQL Server 2022, Enterprise.

**Measured state of `MYDB` (after the analyzer's reads):**

| Object                          | Kind  | Size   | Frag % | Fwd %  | sp_estimate (cur/row/page KB) | write_ratio | current comp |
|---------------------------------|-------|--------|--------|--------|-------------------------------|-------------|--------------|
| `dbo.ORDERS` clustered PK        | CI    | 8 GB   | 42     | —      | 8.0M / 5.5M / 4.0M            | 0.05        | NONE         |
| `dbo.ORDERS` `IX_ORDERS_CUST`    | NCI   | 1.2 GB | 12     | —      | 0.9M / 0.9M / 0.85M          | 0.05        | ROW          |
| `dbo.LEDGER` clustered PK        | CI    | 120 GB | 38     | —      | —                             | 0.40        | NONE         |
| `dbo.AUDIT_2024` clustered PK    | CI    | 3 GB   | 50     | —      | 3.0M / 1.1M / 0.7M            | 0.01        | NONE         |
| `dbo.STAGING` heap               | HEAP  | 4 GB   | 20     | 18     | —                             | —           | n/a          |
| `dbo.CustStats` (stat on ORDERS) | stat  | —      | —      | —      | rows=50M, mod_counter=900k    | —           | —            |

**Decisions (rules from §5, formulas from §5.4):**

1. `dbo.ORDERS` CI — frag 42 ≥ 30 → **REBUILD**. Compression: `gain_row=31%`, `gain_page=50%`,
   `page_extra=(5.5−4.0)/5.5=27% ≥ 10` and `gain_page ≥ 5` → **PAGE**. Not write-intensive (0.05).
   → `rebuild_index` with `data_compression: PAGE`.
2. `dbo.ORDERS` `IX_ORDERS_CUST` — frag 12 (5–30) → **REORGANIZE**. It is already `ROW`, and PAGE saves
   only `(0.9−0.85)/0.9 = 6% < 10%` extra, so compression wants no change — nothing promotes this to a
   rebuild, and a REORGANIZE can't change compression anyway. → `reorganize_index` (with
   `LOB_COMPACTION = ON` from the profile). *(Had a beneficial ROW/PAGE change been found, §5.2 would
   have promoted this to a `rebuild_index` — compression is a first-class rebuild motive.)*
3. `dbo.LEDGER` CI — frag 38 ≥ 30 → would REBUILD, but size 120 GB > `rebuild_max_size_mb` (50 GB) →
   downgrade per `rebuild_over_ceiling: reorganize` → **REORGANIZE**. (No `sp_estimate` was even run —
   over-ceiling objects skip compression analysis.)
4. `dbo.AUDIT_2024` CI — matches override `dbo.AUDIT_*` → `rebuild: forbid`. Frag 50 would REBUILD →
   forced to **REORGANIZE**. Compression decision suppressed (forbidden rebuild can't carry it).
5. `dbo.STAGING` heap — matches override `dbo.STAGING` → `skip: true` → **nothing**.
6. `dbo.CustStats` — dynamic threshold `sqrt(1000 × 50M) ≈ 223,607`; `mod_counter` 900k ≥ that →
   **stale**. But it backs no index rebuilt above, so it is **not** suppressed → `update_statistics`
   `WITH FULLSCAN`.

**Generated manifests** (`--out 01.to_run/`, smallest-first within each, clustered-first honoured):

```yaml
# 020_maint_MYDB_index.yaml
description: "Maintenance plan for MYDB — index reorg/rebuild + compression (generated)"
database: MYDB
operations:
  - operation: rebuild_index        # dbo.ORDERS CI: frag 42% ≥ 30 → rebuild; PAGE saves 50% (27% over ROW)
    schema: dbo
    table: ORDERS
    index: PK_ORDERS
    data_compression: PAGE
  - operation: reorganize_index     # dbo.ORDERS.IX_ORDERS_CUST: frag 12% in [5,30); already ROW → reorganize
    schema: dbo
    table: ORDERS
    index: IX_ORDERS_CUST
    lob_compaction: true
  - operation: reorganize_index     # dbo.AUDIT_2024 CI: frag 50% but override rebuild=forbid → reorganize
    schema: dbo
    table: AUDIT_2024
    index: PK_AUDIT_2024
    lob_compaction: true
  - operation: reorganize_index     # dbo.LEDGER CI: frag 38% but 120 GB > 50 GB ceiling → reorganize
    schema: dbo
    table: LEDGER
    index: PK_LEDGER
    lob_compaction: true
```

```yaml
# 040_maint_MYDB_statistics.yaml
description: "Maintenance plan for MYDB — stale statistics (generated)"
database: MYDB
operations:
  - operation: update_statistics    # CustStats: mod 900k ≥ sqrt(1000*50M)≈223,607 → stale; not index-backed here
    schema: dbo
    table: ORDERS
    statistic: CustStats
    full_scan: true
```

`dbo.STAGING` produces no operation (skipped); no heaps manifest is written because the only heap was
excluded. With `--explain`, each operation is preceded by its measurement + the rule that fired (the
trailing comments above are the shape of that reasoning). The operator reviews these two files, then
runs them through the normal engine.

## 17. Multi-database mode (M8)

> **Planning section.** This specifies the server-wide mode deferred in §1 (Scope) and §14. It is a
> real milestone (M8), not a config tweak, because the analysis DMVs and the generated DDL are
> *database-scoped to the current context* — so covering several databases means establishing **each
> database's connection context**, and finally honoring the manifest `database:` field.

### 17.1 Goal & non-goals

**Goal.** Let `plan` and `--auto` scope maintenance to **many user databases** on the connected server
(all eligible, or a selected subset) instead of only the connection's database. Each database is
analysed and maintained in its own context; `check_db`'s existing multi-database list is folded in.

**Non-goals (still deferred).** Per-database *profiles* or DB-qualified overrides (`match:
"DB1.dbo.AUDIT_*"`) — v1 of M8 applies one profile to every in-scope database, overrides matching on
`schema.table` within each. Cross-server / linked-server maintenance. Parallelism across databases
(forbidden — see §17.7).

### 17.2 The connection model (decided)

Each database is reached by a **dedicated connection in that database's context**, reusing
`mssql.Open` with the server and credentials from the base DSN but the `Database`/`Initial Catalog`
rewritten to the target database. A `USE [db]` on the existing pinned/monitoring connections is
**rejected** — it is exactly the "pool trap" (SPECS §3): the monitoring pool can hand back a
connection in a different database context. So: **one `*mssql.Conn` per database**, opened when that
database's turn comes and closed when done (no connection storm — at most one server-wide heavy
operation at a time, §17.7).

`internal/mssql` gains:
- a way to open against a specific database (e.g. `OpenDatabase(ctx, dsn, db, version)`, which parses
  the DSN once and overrides the catalog), and
- `UserDatabases(ctx)` — the eligibility sweep (§17.3), runnable from any context.

### 17.3 Database enumeration & eligibility

Run once from the initial connection (reading `sys.databases` works from any context). A database is
**eligible** when it is a user database, online, writable, and writable *here* (AG primary / mirroring
principal), and the login can access it:

```sql
SELECT d.name
FROM sys.databases d
LEFT JOIN sys.dm_hadr_database_replica_states  rs  ON rs.database_id = d.database_id AND rs.is_local = 1
LEFT JOIN sys.dm_hadr_availability_replica_states ars ON ars.replica_id = rs.replica_id
LEFT JOIN sys.database_mirroring dm ON dm.database_id = d.database_id
WHERE d.database_id > 4                 -- exclude master / tempdb / model / msdb
  AND d.state_desc = 'ONLINE'
  AND d.is_read_only = 0
  AND d.source_database_id IS NULL      -- exclude database snapshots
  AND (ars.role_desc IS NULL OR ars.role_desc = 'PRIMARY')          -- non-AG, or AG primary
  AND (dm.mirroring_role_desc IS NULL OR dm.mirroring_role_desc = 'PRINCIPAL')  -- non-mirrored, or principal
  AND HAS_DBACCESS(d.name) = 1          -- skip databases the login cannot access
ORDER BY d.name;
OPTION (RECOMPILE);
```

**Ineligible databases are skipped with a logged reason** (AG secondary / read-only / offline /
restoring / no access), never fatal — consistent with the §4.7 degradation rule. Eligibility is
re-evaluated at the start of every run (a failover between runs is handled naturally). Databases are
processed in **name order** for determinism.

### 17.4 Scope selection (decided: flags + profile selector)

The set processed is **eligible ∩ selected**:

- **CLI** (on `plan` and the run path's `--auto`):
  - `--all-databases` — every eligible database.
  - `--databases a,b,c` — an explicit list (each is intersected with eligibility; an explicitly named
    but ineligible database is skipped+logged — a future `--strict` could make that fatal, see §17.9).
  - neither — today's behaviour: the connected database only.
- **Profile** — a top-level selector applied when `--all-databases` is used (or as the default scope):

```yaml
# maintenance_profile.yaml
scope:
  databases:
    include: ["*"]            # globs on database name; default: all eligible
    exclude: ["*_archive", "tempdb_*"]
```

Resolution precedence: an explicit `--databases` list wins; else `--all-databases` ∩ profile
`scope.databases` (include minus exclude); else the connected database. Glob matching reuses the
case-insensitive matcher from `OverrideFor` (§5).

### 17.5 `plan` output — per-database manifests

`plan --all-databases` opens a connection per eligible database, runs the existing analysis there
(`buildInput` → `Decide`), and writes that database's manifests, **grouped database-by-database** in
the queue (the chosen ordering). The `database:` field is set on every manifest, and the filename
encodes the database so the engine processes whole-database blocks in order, e.g.:

```
01.to_run/
  010_maint_DB1_checkdb.yaml     020_maint_DB1_index.yaml
  030_maint_DB1_heaps.yaml       040_maint_DB1_statistics.yaml
  050_maint_DB2_checkdb.yaml     060_maint_DB2_index.yaml
  …
```

(The numeric prefix advances per database so DB1's four manifests sort before DB2's — one contiguous
block per database, categories in their §6 order within each.) History records one `maintenance_plan`
row per database (the `database` column already exists, §7).

### 17.6 Run orchestration — one engine per database (closes the `database:` gap)

The run path groups the queued manifests **by their `database:` field** and, for each database in turn,
opens a connection in that database's context and runs the engine over **that database's manifests
only**. Because the filenames form per-database blocks, this is equivalent to walking the
filename-sorted queue and opening a new connection whenever the target database changes.

This is the change that finally **honors the manifest `database:` field** (the gap noted in §1):

- The `Engine` stays single-connection. The run path adds a thin outer loop: partition the discovered
  manifest names by database, then per database build a connection + `Engine` and process just that
  partition (a small `Engine` database filter, or an explicit "process these names", empty filter =
  today's whole-queue behaviour). The engine is otherwise unchanged — same preflight, monitoring,
  reaction, reporting, `.log`, history.
- A manifest whose `database:` is empty defaults to the connection's database (back-compatible).
- A manifest whose `database:` is no longer eligible at run time (e.g. an AG failover since `plan`) is
  **left in the queue and logged**, not failed.

### 17.7 Concurrency — strictly sequential, server-wide

SPECS §2 forbids two heavy DDL statements at once on a database; M8 keeps that **server-wide**: at most
one maintenance operation runs at any moment across *all* databases. Databases are processed one fully
before the next; within a database the engine is already one-manifest-at-a-time. Connections are
opened per database and closed when its block finishes — no fan-out, no connection storm. (A bounded
cross-database parallelism is a possible future tuning, explicitly out of scope here.)

### 17.8 Crash recovery across databases

Recovery (SPECS §11) already scans `02.processing/`. With M8 each leftover manifest carries its
`database:`; recovery **groups orphans by database** and reconciles each against a connection in that
database's context (resumable check, adopt, requeue). The run-state sidecar records the database (or it
is read from the manifest) so the right connection is used. A database that became ineligible since the
crash (failover) is left for a later run that finds it primary again.

### 17.9 Preflight, history, dangers

- **Preflight.** The existing AG check (SPECS §4, warn-not-fail) becomes, in multi-database mode, a
  **hard skip** of secondaries during enumeration (§17.3) — we never queue work for a database we
  cannot write. Per-database preflight is otherwise unchanged.
- **History.** No schema change — `maintenance_plan.database` already distinguishes runs; trend
  queries gain a server-wide view across databases.
- **Dangers & mitigations:**
  - *A server with hundreds of databases overruns the window* → the §6 batching budget
    (`max_operations` / `time_budget_minutes`) applies **across the whole multi-database run**; when it
    truncates, log which databases/objects were dropped (no silent truncation).
  - *AG failover mid-run* → the per-database connection fails on write; treated as an interruption for
    that database (left recoverable), the run continues with the next database.
  - *Per-database permission differences* → skipped+logged at enumeration (`HAS_DBACCESS`).
  - *Connection storm* → at most one database's connection open at a time.
  - *A `--strict` flag* (future) to fail when an explicitly named `--databases` entry is ineligible.

### 17.10 Build sub-phases (M8)

- **M8.0 — planning.** This section.
- **M8.1 — connection + enumeration.** `mssql.OpenDatabase` (catalog override) and
  `mssql.UserDatabases` (eligibility sweep). Integration-tested (multiple DBs, an offline/read-only DB,
  and — where feasible — an AG secondary).
- **M8.2 — scope resolution (pure).** Flags + profile `scope.databases` (include/exclude globs) →
  resolved database list; table-tested against a fixed eligible set.
- **M8.3 — multi-database `plan`.** Per-database analysis loop → per-database manifest blocks; history
  per database.
- **M8.4 — multi-database run.** Group queued manifests by `database:`; one engine per database on its
  own connection; **honor `database:`** (close the §1 gap). Back-compatible when no `database:` /
  single database.
- **M8.5 — recovery across databases.** Group orphans by database; reconcile per-database connection.
- **M8.6 — preflight + docs.** *Done.* Secondary/read-only/offline are hard-skipped at enumeration
  (§17.3); a **run-start eligibility re-check** (`runnableTargets`) leaves a database that became
  ineligible between `plan` and the run in the queue rather than failing it; §1/§9/§10 and the README
  updated. The one outstanding item is a multi-database end-to-end run under `make e2e`.
