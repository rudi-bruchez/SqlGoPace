# Adversarial Spec Review: OBJECT-SIZES.md

**1. MAJOR: Canceled reorganize operations do not roll back**
- **Claim:** "A failed or canceled operation rolled back to the old structure (or, for a paused resumable, left a partial one); an 'after' would be a misleading equal or a meaningless number, so none is recorded."
- **Reality:** `ALTER INDEX ... REORGANIZE` is always committed partially. If it is canceled, the work already completed is preserved and the index's size has genuinely changed (unlike a `REBUILD`, which rolls back completely).
- **Consequence:** Bailing out of the "after" size read on cancel for a reorganize throws away the measurement of the partial space recovered. The "after" size is completely valid and highly useful to the operator.
- **Fix:** Read the "after" size for a canceled `reorganize_index` operation. Only skip the "after" read for failed or canceled `rebuild` operations.

**2. MINOR: Dropping oversized heaps in `buildInput` hides them from the SQLite history**
- **Claim:** "A heap skipped on either bound is no longer dropped silently: `buildInput` writes the log line `COMPRESSION-SCOPE.md` §4 already asked for"
- **Reality:** While the skip is no longer silent in the text log, dropping it in `internal/plan/plan.go`'s `buildInput` means it never reaches `Decide`, which means it never becomes a `Decision`. Therefore, it never enters the `maintenance_analysis` SQLite history table in `internal/report/history.go`. 
- **Consequence:** Trend analysis via the history database on why oversized heaps are being skipped (or tracking their growth over time) is impossible, as the planner discards them entirely before emitting decisions.
- **Fix:** If history tracking for oversized heaps is desired, emit a synthetic `skipDecision` directly from `buildInput` (which avoids the expensive `PhysicalSampled` reads), rather than dropping them entirely.

**Verified claims that are genuinely sound:**
- `rebuiltObject` does correctly return an empty string for heaps, forcing `IndexSizeMB` to only read `index_id = 0`.
- The planner's `heapMeasurement` does indeed only sum the first group in the inventory partition array, ignoring nonclustered indexes (and `shrink.go` correctly iterates the group to sum it).
- `TableStructureSizes` groups safely even when `sys.indexes.name` is NULL for a heap.
- The `TableStructureSizes` query correctly drops missing or hypothetical indexes that have no partitions in `sys.dm_db_partition_stats`.
- `tui.HumanizeMB` really cannot be imported into `internal/report` without introducing an import cycle (`internal/tui` imports `internal/report`).

**Verdict:** 
Must be revised first. The spec is mostly solid and the data-free-space logic holds up, but the `reorganize_index` cancel logic must be corrected to prevent losing partial progress measurements. The SQLite history gap for oversized heaps should also be explicitly addressed or accepted.
