# Deep Research Report — "The Hard Truth About Shrinking SQL Server Data Files"

**Purpose:** fact-check, correct, and deepen the draft article for www.pachadata.com.
Every claim below was verified against primary sources (Microsoft Learn, the SQL Server
support blog, SQLskills) as of **2026-07-28**. Sources are listed in §8 with URLs.

---

## 1. Verdict at a glance

The draft's *structure and core thesis are sound and well-supported*: shrink relocates
trailing pages, the common wall on a live OLTP database is concurrency (IAM Sch-M/Sch-S),
LOB is "movable but pathologically slow," diagnose from catalog metadata, reorganize
before shrink, incremental targets, low-write window. All of that checks out against
current Microsoft documentation.

However, the research surfaced **4 factual errors, 2 attribution problems, and 1
now-outdated version claim** — plus a large body of documented internals (phases,
transaction batching, the full error catalog, snapshot-isolation blocking, PVS/ADR,
official tooling) that would make the article substantially deeper and more precise.

### Critical corrections

| # | Draft says | Reality (verified) |
|---|-----------|--------------------|
| 1 | **Msg 5240** = *"Could not adjust the space allocation for file '…'"* = the immovable-page wall | **Wrong message.** Msg 5240 is *"File ID %d of database ID %d cannot be shrunk as it is either being shrunk by another process or is empty."* (severity 10). *"Could not adjust the space allocation for file '%ls'"* is **error 3140** — a restore-context error. The shrink-side "adjust allocation failed" message is **error 5234**. See §3.1. |
| 2 | LOB slowness: shrink "rescans for free space each time"; quote attributed to "Microsoft Q&A" | Wrong mechanism + wrong attribution. The documented mechanism is a **table scan per moved LOB value** (no backlink from text page to owning row). The *"slooooow"* quote is **Paul Randal's SQLskills blog**, not Microsoft Q&A. See §3.2. |
| 3 | "You **cannot** REORGANIZE a heap" | **Wrong.** `ALTER INDEX ALL … REORGANIZE WITH (LOB_COMPACTION = ON)` *does* run on heaps and is the only online way to compact **heap LOB pages**. What reorganize can't touch is the heap's *in-row* structure/forwarded records. Bonus counterintuitive fact: `ALTER TABLE … REBUILD` on a heap **does not move its LOB pages** either. See §3.3. |
| 4 | Min-size floor: "cannot shrink below its original creation size… a hard floor" | **Overstated.** The docs state you *can* shrink a file below its creation size, which **resets the minimum file size**. Creation size is the default *target* (when `target_size` = 0), not an immovable floor. See §3.4. |
| 5 | Columnstore-LOB limitation: "verify before publishing, 2025 behavior may have shifted" | **Resolved.** SQL Server 2025 (17.x) **can** move LOB pages in compressed columnstore segments, all editions. The limitation now applies to SQL Server 2022 (16.x) and older (and Azure SQL MI 2022). See §3.5. |

---

## 2. What the draft gets right (verified, keep as-is)

- **Page-relocation mechanics.** `DBCC SHRINKFILE` moves used pages from the tail "into
  any unallocated pages in the file's kept areas," truncating at the last allocated
  extent. It never shrinks past stored data. [S1]
- **Fragmentation as a side effect.** Official wording: "A shrink operation doesn't
  preserve the fragmentation state of indexes… can increase index fragmentation."
  The `sys.dm_db_index_physical_stats` doc adds: shrink "can introduce fragmentation
  if an index is partly or completely moved during the shrink operation." Pages are
  moved **individually** (page-by-page), which is why shrink scatters previously
  contiguous pages — Andy Mallon's "backwards REORGANIZE" framing is accurate. [S1][S8][S11]
- **Work preservation.** "You can stop DBCC SHRINKFILE operations at any point and any
  completed work is preserved." Documented verbatim. [S1]
- **TRUNCATEONLY semantics.** Releases trailing free space only, no page movement;
  shrinks only to the last allocated extent. Caveat worth adding: *if `target_size`
  is specified together with TRUNCATEONLY, trailing free space might not be released.* [S1]
- **IAM Sch-M/Sch-S concurrency model.** Documented almost word-for-word as the draft
  states: queries hold Sch-S on IAM pages; shrink needs Sch-M to move/delete IAM pages;
  long queries block shrink; new Sch-S queries queue behind the waiting shrink. [S1][S2]
- **WAIT_AT_LOW_PRIORITY.** SQL Server 2022 (16.x)+, fixed **one-minute** low-priority
  lock timeout (no `MAX_DURATION` — unlike online index rebuild), `ABORT_AFTER_WAIT`
  supports only `SELF` (default) / `BLOCKERS` (no `NONE`). [S1][S2]
- **Allocation churn.** Documented: "a workload might start using the space freed by
  shrink before shrink truncates the file." [S4]
- **Reorganize-before-shrink**, both documented scenarios (many files/tables/large
  deletes; LOB/row-overflow/columnstore present), and the `LOB_COMPACTION = ON`
  recommendation. [S4]
- **`LOB_COMPACTION = ON` is the default** for `REORGANIZE` — confirmed verbatim in
  the ALTER INDEX examples: "Specifying the `WITH (LOB_COMPACTION = ON)` option isn't
  required because the default value is ON." [S5]
- **Never rebuild before shrink.** Official Azure guidance: "Index rebuilds require
  free space in the database, so they might cause the allocated space to increase,
  counteracting the effect of a shrink." [S4]
- **Create/drop-clustered-index trick** "rebuilds all nonclustered indexes on that
  table twice" — verbatim caution in `sys.dm_db_index_physical_stats` docs; Paul Randal
  explains why (RID ↔ clustering-key linkage flips twice). [S8][S10]
- **Incremental steps** and **low-usage window**: both are official recommendations;
  suggested increment is **10–20 GB** per step. [S4]
- **Standard edition**: online index rebuild is Enterprise/Azure-only — the offline/
  blocking consequence for heaps is real. [S8]
- **sys.dm_db_database_page_allocations warning**: undocumented/unsupported, walks IAM
  chains, very heavy at database scope, `DETAILED` mode worse, repeated calls compound
  the cost; scope it to a single object or avoid it. None of Microsoft's shrink
  guidance uses it. [S12]

---

## 3. Corrections in detail

### 3.1 Msg 5240 is misidentified — and this changes the PRODDB story

From `sys.messages` (consistent across multiple error references, and quoted verbatim
in the SQL Server support blog):

- **Msg 5240, Level 10**: *"File ID %d of database ID %d cannot be shrunk as it is
  either being shrunk by another process or is empty."* [S3][S14]
- **Msg 3140, Level 16**: *"Could not adjust the space allocation for file '%ls'."* —
  this is a **restore**-context error, not a shrink error. [S14]
- **Msg 5234**: *"DBCC SHRINKDATABASE: File ID %d of database ID %d was skipped
  because trying to adjust the space allocation for the file was failed."* — this is
  the shrink-side per-file skip message. [S14]
- Also adjacent: **Msg 5241** — target size greater than actual file size. [S14]

**Why this matters for the article:** Msg 5240 means **a second shrink touched the same
file** (shrink is strictly one-per-file; it's also single-threaded within a file) or
the file was empty. If the PRODDB manifest logged two hard `FAILED` runs on Msg
5240, the most likely cause is **overlapping shrink invocations from the tooling
itself** (e.g. a previous run still active, a retry racing a hung session), not
immovable data. That reframes §3 and §9: 5240 is a *concurrency-of-maintenance*
signal, not a "wall" signal. Recommend:

- Rewrite §3 around the **documented** error taxonomy (see §4.3 of this report):
  silent no-progress, transient retryables (1205 deadlock, 49516 WLP timeout),
  informational blockers (5202/5203 snapshot waits), hard data-level immovables
  (49503 PVS, "work table page", columnstore-LOB pre-2025), and maintenance-level
  misuse (5240, 5241, 5234).
- Keep the "INCOMPLETE is a first-class outcome" design note — it's excellent and
  original — but re-anchor the error examples.

### 3.2 LOB slowness: wrong mechanism, wrong attribution

The draft: *"modern shrink can move LOB pages — it just does so pathologically slowly,
page by page, rescanning for free space each time. The Microsoft Q&A puts it bluntly:
'if you have LOB data, the shrink can be very sloooooow.'"*

Two fixes:

1. **Mechanism.** The authoritative internals explanation (Paul Randal, who was on the
   SQL Server storage engine team): an off-row LOB value is a tree of text records;
   the in-row record holds a *blob root* pointer to the top text page — **but there is
   no backlink from the text page to the owning row**. So when shrink moves a text
   page, it must **scan the whole table (or index) to find the owning record** and
   update the pointer — **for every LOB value moved**; for >8 KB LOB chains, all text
   pages of that table must be scanned. *"Very slow. Very, very slow."* Randal pushed
   for a backlink during SQL 2005 development; it was rejected on engineering cost.
   **The post was updated in March 2026: "Everything in this post applies to all
   versions of SQL Server still."** [S9]
2. **Attribution.** The *"slooooow"* quote is the title of that SQLskills post —
   not a Microsoft Q&A. Cite: Paul Randal, *"Why LOB data makes shrink run
   slooooowly (T-SQL Tuesday #006)"*, SQLskills. [S9]

Suggested rewrite of the mechanism sentence: *"Modern shrink can move LOB pages — it
just pays a brutal price per page: with no backlink from a text page to its owning
row, every moved LOB page triggers a scan of the owning table to patch the pointer.
LOB-heavy files don't stall; they crawl."*

Also from the 2008 SQL Server support blog (Bob Dorr): shrink uses the **same
underlying code path as `ALTER INDEX … LOB_COMPACTION`** to compact LOB space; legacy
SQL 2000-format LOB chains are upgraded to the 2005 format on encounter (extra cost);
and **trace flag 2548** skips LOB compaction entirely (support-guided only). [S3]

### 3.3 Heaps: the draft's claim is inverted

The draft: *"You cannot REORGANIZE a heap (`LOB_COMPACTION = OFF` 'has no effect on a
heap'). A heap pinned near the tail needs ALTER TABLE … REBUILD…"*

What's actually documented and empirically demonstrated:

- `ALTER INDEX ALL ON <heap> REORGANIZE WITH (LOB_COMPACTION = ON)` **runs fine on a
  heap and compacts the heap's LOB allocation unit** — it's in fact the *only* online
  operation that compacts heap LOB pages. The doc line "OFF has no effect on a heap"
  means `OFF` is a no-op for heaps — it's describing the OFF branch, not banning
  reorganize. [S5][S13]
- What reorganize **cannot** do for a heap: rebuild the **in-row** structure, remove
  forwarded records, or consolidate extent fragmentation. For that you need
  `ALTER TABLE … REBUILD` (SQL 2008+; online only in Enterprise) or the
  create/drop-clustered trick. [S8][S10]
- **Counterintuitive gem for the article** (empirically demonstrated, worth stating
  with the source): `ALTER TABLE … REBUILD` rebuilds the heap's **in-row** data but
  leaves **heap LOB pages in place**; and `ALTER INDEX ALL … REBUILD` doesn't touch
  the heap at all. So a heap with heavy LOB needs **both**: reorganize (LOB) **and**
  table rebuild (in-row). [S13]
- Practical alternative from the field: **"un-heap" the heap** — add a (well-chosen)
  clustered index and leave it; then reorganize/rebuild become available online
  levers. [S11]

Suggested §7.3 rewrite: *"Heaps are a split case, not a dead end. `REORGANIZE …
LOB_COMPACTION = ON` works on heaps and compacts their LOB pages — but it does nothing
for the heap's in-row data or forwarded records. `ALTER TABLE … REBUILD` fixes in-row
data but — surprisingly — doesn't move the heap's LOB pages. A LOB-heavy heap pinned
at the tail needs both, and the table rebuild is offline on Standard edition. The
durable fix is to stop being a heap: add a clustered index."*

### 3.4 The "minimum-size floor" is softer than stated

- The `DBCC SHRINKFILE` doc opens with: *"You can shrink a file to less than its size
  at creation, resetting the minimum file size to the new value."* [S1]
- `target_size = 0` (or omitted) means "shrink toward the creation size" — creation
  size is the **default target**, and the result-set column `MinimumSize` merely
  reports "the minimum size or originally created size." [S1]

So §6.3 should say: the file's *configured* minimum/creation size shows up as
`MinimumSize` in the result set and as the default target, but shrink itself can push
below it (resetting the floor); the genuine floors are **allocated pages at the tail**
and, for log files, **VLF boundaries**. Keep the query (`sys.database_files`), drop
the "hard floor" framing. Also worth one line: log files shrink only to a VLF
boundary, and a log that won't shrink usually means missing log backups. [S1]

### 3.5 SQL Server 2025 columnstore-LOB — now confirmed, no hedging needed

- Microsoft Learn (SHRINKFILE *Known issues*): "In versions **earlier than SQL Server
  2025 (17.x)**, the pages used by LOB column types (varbinary(max), varchar(max),
  nvarchar(max)) in compressed columnstore segments can't be moved." [S1]
- *What's new in columnstore indexes*: "In SQL Server 2025, both DBCC SHRINKDATABASE
  and DBCC SHRINKFILE can move data pages used by the LOB columns in columnstore
  indexes." The known issue is scoped to "SQL Server 2022 (16.x) and older, Azure SQL
  MI 2022." [S6]
- Independent coverage (Redgate/Simple Talk) confirms it applies to **all editions**
  and explains the old workaround (normalize strings into a page-compressed rowstore
  lookup table). [S7]

The draft can state this cleanly: pre-2025 = immovable; 2025+ = movable. Note also
that `xml` and legacy `text`/`image` aren't in the fixed list — the 2025 fix names
exactly `varchar(max)`, `nvarchar(max)`, `varbinary(max)`.

---

## 4. Deep internals worth adding (this is where the article can become reference-grade)

### 4.1 The three documented phases of a shrink

Visible in `sys.dm_exec_requests.command` (SQL Server support blog, Bob Dorr): [S3]

| Phase | Command value | What it does |
|-------|--------------|--------------|
| 1 | `DbccSpaceReclaim` | Cleans up **deferred allocations** (e.g. dropped objects ≥128 extents pending deferred deallocation) and purges empty extents, preparing for data moves |
| 2 | `DbccFilesCompact` | Moves pages beyond the target below it; truncates the file as required |
| 3 | `DbccLOBCompact` | LOB compaction, **after** all pages are below the truncation target — skipped entirely with `TRUNCATEONLY` (and by `AUTO_SHRINK`) |

Operational gold for the article:

- **Transaction batching:** shrink moves pages in batches of **~32 pages per
  transaction**, committing between batches. This makes it **restart-capable** — kill
  it and only the in-flight batch rolls back — and explains why it no longer pins the
  log with one giant transaction. Named transactions (e.g.
  `DeferredAllocUnitDrop::ReclaimSp`) are observable in
  `sys.dm_tran_active_transactions`. [S3]
- **Single-threaded:** a shrink runs on one thread; and only **one shrink per file**
  can run at a time (else → **Msg 5240** — see §3.1; a nice full-circle detail). [S3]
- **Deferred deallocation** (documented under ALTER INDEX/drop): objects with ≥128
  extents are deallocated asynchronously after commit — so right after a big
  `DROP`/`TRUNCATE`, "free" space may still be allocated until phase 1 / the
  background thread reclaims it. This is a *very* common reason a fresh post-purge
  shrink under-delivers. [S5]
- **AUTO_SHRINK skips LOB compaction** (too intensive) — one more reason auto-shrink
  is ineffective. [S3]

### 4.2 Progress measurement — the right instrument exists

The draft's §4 "subtle trap" (whole-MB progress misreads a LOB crawl) is real, and
Microsoft's own monitoring guidance hands you the fix, verbatim: [S4]

> "Shrink progress might be nonlinear, and the value in the `percent_complete` column
> might remain unchanged for long periods, even though shrink is still in progress.
> **An increase in the `cpu_time`, `reads`, or `writes` values for the same
> `session_id` between two executions of the query means that shrink continues making
> progress.**"

So the recommendation for tooling becomes sharper: **never** use file size or
`percent_complete` alone for stall detection — use deltas of `cpu_time`/`reads`/
`writes` in `sys.dm_exec_requests` (filter
`command IN ('DbccSpaceReclaim','DbccFilesCompact','DbccLOBCompact','DBCC')`, and you
also get `wait_type` / `blocking_session_id` for the concurrency story in one query).
Include the official monitoring query in the article — it's directly paste-able. [S4]

### 4.3 The complete error/message taxonomy (replaces the draft's two-bucket model)

| Code | Level/type | Meaning | Article framing |
|------|-----------|---------|-----------------|
| *(silent)* | — | Statement completes, file size unchanged | "No-gain" — wall or crawl; measure with I/O deltas |
| 5202 / 5203 | Info, error log every 5 min (first hour) then hourly | Shrink **waiting on snapshot/row-versioning transactions** (5202 = SHRINKDATABASE, 5203 = SHRINKFILE) | A whole concurrency class the draft misses — see §4.4 |
| 49516 | Error log; WLP timeout | Couldn't get IAM `Sch-M` within the fixed 1-minute low-priority window | Expected on busy systems; retry |
| 1205 | Error | Deadlock (shrink chosen as victim) | Transient — retry; Microsoft's official loop retries 1205 + 49516 with jittered backoff [S4] |
| 49503 | Error | *"Page … could not be moved because it is an off-row persistent version store (PVS) page"* | ADR/PVS — genuinely immovable until long transactions close; see §4.4 |
| 5223 | Error | *"Empty page … could not be deallocated"* | Usually concurrent `ALTER INDEX`; retry; if persistent, identify the index via `sys.dm_db_page_info` (query provided in docs) and rebuild it [S4] |
| 5201 | Info | *"File … was skipped because the file does not have enough free space to reclaim"* | Benign; move to next file |
| 5234 | Info | SHRINKDATABASE skipped a file — "adjusting the space allocation for the file failed" | The true "couldn't adjust allocation" shrink message |
| 5240 | Level 10 | File "being shrunk by another process or is empty" | **Overlapping shrink / empty file — not a data wall** (§3.1) |
| 5241 | Level 10 | Target size > actual file size | Parameter error |
| "work table page" | Message + Msg 2555 on EMPTYFILE | *"Page … could not be moved because it is a work table page"* | Mostly **tempdb**; resolve by clearing caches (`FREEPROCCACHE`/`FREESYSTEMCACHE`) or resizing tempdb files at startup; relevant only if the article covers tempdb [S15] |

### 4.4 Two concurrency walls the draft doesn't mention

1. **Row versioning / snapshot isolation.** A transaction running under a
   row-versioning isolation level (e.g. a big delete under RCSI/SI) blocks shrink
   until it completes — documented with the exact error-log messages (5202/5203) and
   the diagnostic join to `sys.dm_tran_active_snapshot_database_transactions`
   (`transaction_sequence_num` / `first_snapshot_sequence_num`). On a system with
   RCSI enabled (very common for reporting-friendly apps — plausibly the Grafana
   shop in the case study), this is as likely a wall as IAM Sch-M/Sch-S. [S1][S2]
2. **ADR / Persistent Version Store.** Error **49503**: shrink cannot move off-row
   PVS pages pinned by long-running transactions. Fix: let them finish (or kill
   them); troubleshoot via the ADR PVS-cleanup guidance. On SQL Server 2019+ with ADR
   enabled, this is a *genuinely immovable* page class the draft's §4 ("only
   columnstore LOB is immovable") should add. [S4]

### 4.5 Official playbooks and tooling to cite

- **Page density pre-check** (Azure large-database playbook): query
  `sys.dm_db_index_physical_stats … 'SAMPLED'` for `avg_page_space_used_in_percent`;
  indexes with high `page_count` and density **< 60–70 %** are the reorganize/rebuild
  candidates before shrink; shrink moves fewer pages when density is high. [S4]
- **Parallel shrink across files:** run concurrent `DBCC SHRINKFILE` sessions on
  *different* `file_id`s; Microsoft's observed sweet spot is **4–8 parallel**;
  beyond that you get resource saturation and lock contention *between* shrinks. [S4]
- **Incremental targets:** official suggestion **10–20 GB per step**. [S4]
- **Official retry loop:** TRY/CATCH retrying `ERROR_NUMBER() IN (1205, 49516)` with a
  randomized 1–10 s backoff — publishable almost verbatim. [S4]
- **ShrinkDriver** (Microsoft, in the `microsoft/sql-server-samples` GitHub repo):
  a PowerShell script that automates large-database shrink — parallel across files,
  resumable, retries on interruption, detailed status reports. It is now referenced
  in *both* the SHRINKFILE and SHRINKDATABASE *Best practices* sections, so citing it
  is citing Microsoft. Its existence also validates the draft's "shrink tooling needs
  first-class INCOMPLETE/retry semantics" design note. [S1][S2][S16]
- **Post-shrink index maintenance:** Microsoft explicitly notes fragmentation after
  shrink mostly matters for **large-scan workloads**; rebuild if needed, accepting
  the allocated-space regrowth. [S4]

### 4.6 The "nuclear" alternative strategies (add a short section)

For cases where shrink is the wrong tool entirely — high-value, sourced material:

1. **New-filegroup migration:** create a new filegroup, rebuild indexes into it
   (rebuilds are **parallel**, unlike single-threaded shrink — often much faster),
   then drop/shrink the old filegroup. Caveat: index `REBUILD` **does not move LOB
   allocation units** between filegroups — LOB-heavy tables need a
   copy-to-new-table-and-swap approach. [S11]
2. **`EMPTYFILE` + `REMOVE FILE`:** migrate all data off one file into its siblings
   (same filegroup), then `ALTER DATABASE … REMOVE FILE`. Guarantees no new
   allocations land on the file during the operation. (Not supported in Azure SQL
   DB / Fabric.) [S1]
3. **Partition-level thinking:** switching/truncating partitions is a far cheaper
   "delete" than mass DELETE — the best shrink is the one you never need. (General
   best practice; frame as your own recommendation.)

### 4.7 Cheap diagnostics — two additions to §6

- `sys.dm_db_page_info` (SQL Server 2019+): maps a **single** `file_id:page_id` to its
  object cheaply — the documented way to chase a specific blocking page (Microsoft
  uses it in the 5223 mitigation query). Pair it with any concrete page number a
  shrink message gives you. [S4]
- `DBCC EXTENTINFO` (undocumented): extent-level allocation map (file_id, page_id,
  pg_alloc, object_id, index_id, iam_chain_type) — historically *the* tool for
  finding which objects hold the high extents (it powered the old KB324432 sparse-LOB
  workflow). Lighter than `dm_db_database_page_allocations`, but still returns one
  row per extent — materialize it into a table, don't re-run it repeatedly. Flag it
  as undocumented/unsupported. [S12][S17]

---

## 5. Section-by-section edit map

| Draft section | Action |
|---------------|--------|
| §2 Mechanics | Add: phases (DbccSpaceReclaim/FilesCompact/LOBCompact), ~32-page batches, single-threaded, TRUNCATEONLY+target_size caveat, LOB compaction skipped under TRUNCATEONLY/AUTO_SHRINK. |
| §3 Two faces of stuck | **Replace Msg 5240 content** (§3.1). Rebuild around the full taxonomy (§4.3). Keep the INCOMPLETE design note. |
| §4 LOB myth | Fix mechanism (no-backlink table scan) + attribution (Randal, not MS Q&A) (§3.2). Add PVS/49503 and work-table pages to the "genuinely immovable" list. Keep columnstore-LOB but state 2025 resolution plainly (§3.5). |
| §4 automation trap | Strengthen: official guidance confirms `percent_complete` stalls; prescribe cpu_time/reads/writes deltas (§4.2). |
| §5 Concurrency | Add row-versioning blocking (5202/5203) as a second documented class; note WLP's fixed 1-minute timeout + 49516 semantics (docs differ slightly on "silent" vs error — see note below); `ABORT_AFTER_WAIT = BLOCKERS` exists but needs `ALTER ANY CONNECTION`/`KILL DATABASE CONNECTION`. |
| §6 Diagnostics | Fix §6.3 floor framing (§3.4); add `sys.dm_db_page_info` and `DBCC EXTENTINFO` (§4.7); add deferred-deallocation check (recent DROP/TRUNCATE of big objects). |
| §7 Remediation | Rewrite §7.3 heaps (§3.3); add page-density triage (<60–70 %), parallel-file shrink (4–8), official 10–20 GB increments, ShrinkDriver, retry loop; consider a short "alternatives" subsection (§4.6). |
| §8 Sequence | Insert: check for snapshot/ADR blockers *before* the window; after shrink, re-check fragmentation for large-scan workloads. |
| §9 Epilogue | Reinterpret the two 5240 failures (likely overlapping shrink runs — §3.1); if RCSI was enabled on PRODDB, the 5202/5203 class deserves a mention. |
| §10 Takeaways | Update: 5240 bullet; "LOB is movable but slow *because of the missing backlink*"; add PVS; add "measure progress by I/O deltas, not file size." |
| Appendix | Fix attributions; add the 8 new sources (§8 below). |

**Note on WLP "silent exit":** the SHRINKDATABASE page says the operation "will exit
with **no error**" and 49516 goes to the error log; the SHRINKFILE page says it "times
out **with error 49516**"; and Microsoft's own retry script catches 49516 via
TRY/CATCH — i.e. the session *can* receive it. Safest phrasing for the article:
*"...times out after one minute and aborts (error 49516 is written to the error log;
catch it with TRY/CATCH if you're scripting retries)."* [S1][S2][S4]

---

## 6. Anonymization checklist for the PRODDB case

Per the draft's own note: scrub the database name (`PRODDB`), the Grafana login
name, host/instance names, and exact run timestamps. The technical details (7 runs,
wait types `LCK_M_SCH_S` / `LCK_M_IX`, Standard edition, zero LOB/row-overflow) are
safe to keep — they're the pedagogically valuable part.

---

## 7. New technical details at a glance (quotable)

1. Shrink = 3 observable phases: `DbccSpaceReclaim` → `DbccFilesCompact` → `DbccLOBCompact`.
2. Pages move in **~32-page transaction batches** → restart-capable by design.
3. Shrink is **single-threaded**, one shrink per file (violation → Msg 5240).
4. LOB slowness = **missing backlink**: every moved text page costs a table scan.
5. `LOB_COMPACTION = ON` is the **default**; shrink itself runs LOB compaction as its
   final phase unless TRUNCATEONLY.
6. **Heap paradox**: REORGANIZE compacts heap LOB but not in-row data;
   `ALTER TABLE … REBUILD` fixes in-row data but **not** LOB pages.
7. SQL Server **2025** moves compressed-columnstore LOB pages; ≤2022 cannot.
8. `percent_complete` **can flatline for hours** on a healthy shrink — use I/O deltas.
9. Row-versioning transactions block shrink (5202/5203); ADR PVS pages are immovable (49503).
10. Microsoft now ships an official shrink orchestrator (**ShrinkDriver**) — parallel,
    resumable, retrying — and recommends 4–8 parallel file shrinks in 10–20 GB steps.

---

## 8. Source bibliography

- **[S1]** DBCC SHRINKFILE (Transact-SQL) — Microsoft Learn (updated 2026-07-24).
  https://learn.microsoft.com/en-us/sql/t-sql/database-console-commands/dbcc-shrinkfile-transact-sql
- **[S2]** DBCC SHRINKDATABASE (Transact-SQL) — Microsoft Learn (updated 2026-07-24).
  https://learn.microsoft.com/en-us/sql/t-sql/database-console-commands/dbcc-shrinkdatabase-transact-sql
- **[S3]** *How It Works: SQL Server 2005 DBCC Shrink\* May Take Longer Than SQL Server 2000* —
  Bob Dorr, SQL Server Support blog (2008).
  https://techcommunity.microsoft.com/blog/sqlserversupport/how-it-works-sql-server-2005-dbcc-shrink-may-take-longer-than-sql-server-2000/315471
- **[S4]** *Manage file space for databases in Azure SQL Database* — Microsoft Learn
  (monitoring query, page-density triage, parallel/incremental shrink, retry loop,
  errors 49503/5223/5201, ShrinkDriver).
  https://learn.microsoft.com/en-us/azure/azure-sql/database/file-space-manage
- **[S5]** ALTER INDEX (Transact-SQL) — Microsoft Learn (LOB_COMPACTION default ON;
  REORGANIZE semantics; deferred deallocation).
  https://learn.microsoft.com/en-us/sql/t-sql/statements/alter-index-transact-sql
- **[S6]** *What's new in columnstore indexes* — Microsoft Learn (SQL Server 2025
  shrink improvement; known issue scoped to ≤2022 / Azure SQL MI 2022).
  https://learn.microsoft.com/en-us/sql/relational-databases/indexes/columnstore-indexes-what-s-new
- **[S7]** *Columnstore Index Improvements in SQL Server 2025* — Redgate Simple Talk (2025-06-30).
  https://www.red-gate.com/simple-talk/databases/sql-server/columnstore-index-improvements-in-sql-server-2025/
- **[S8]** sys.dm_db_index_physical_stats (Transact-SQL) — Microsoft Learn (heap
  fragmentation via create/drop clustered, "rebuilds all nonclustered indexes twice";
  shrink introduces fragmentation).
  https://learn.microsoft.com/en-us/sql/relational-databases/system-dynamic-management-objects/sys-dm-db-index-physical-stats-transact-sql
- **[S9]** Paul Randal, *Why LOB data makes shrink run slooooowly (T-SQL Tuesday #006)*
  — SQLskills (no-backlink mechanism; "applies to all versions still," edit March 2026).
  https://www.sqlskills.com/blogs/paul/why-lob-data-makes-shrink-run-slooooowly-t-sql-tuesday-006/
- **[S10]** Paul Randal, *A SQL Server DBA myth a day: (29/30) fixing heap
  fragmentation* — SQLskills (NC-index rebuilds ×2; ALTER TABLE REBUILD side effects).
  https://www.sqlskills.com/blogs/paul/a-sql-server-dba-myth-a-day-2930-fixing-heap-fragmentation/
- **[S11]** Andy Mallon, *Fastest way to shrink LOB data in SQL Server* (rebuild-into-
  new-filegroup strategy; REBUILD doesn't move LOB; single-threaded shrink).
  https://am2.co/2020/01/fastest-way-to-shrink-lob-data-in-sql-server/
- **[S12]** Aaron Bertrand, *Use caution with sys.dm_db_database_page_allocations* —
  MSSQLTips. https://www.mssqltips.com/sqlservertip/6309/
- **[S13]** Stack Overflow: *Deleting/Updating LOB data in a Heap* (empirical
  rebuild/reorganize × in-row/LOB matrix for heaps).
  https://stackoverflow.com/questions/49610084/
- **[S14]** SQL Server error-message listings for 3140 / 5201 / 5234 / 5240 / 5241
  (sys.messages texts). e.g. https://solutions.dbwatch.com/Sqlserver/
- **[S15]** Erin Stellato, *Remove Files From tempdb* — SQLskills ("work table page"
  behavior, Msg 2555). https://www.sqlskills.com/blogs/erin/remove-files-from-tempdb/
- **[S16]** ShrinkDriver sample — microsoft/sql-server-samples (GitHub).
  https://github.com/microsoft/sql-server-samples
- **[S17]** Eitan Blumin, *Troubleshooting Long-Running SHRINK Operations* (un-heap,
  move LOB to another filegroup, TRUNCATEONLY-after-rebuild pattern).
  https://eitanblumin.com/2020/04/07/troubleshooting-long-running-shrink-operations/

*Report compiled 2026-07-28. All version-specific claims re-verified against the
current (sql-server-ver17) Microsoft Learn pages on that date.*
