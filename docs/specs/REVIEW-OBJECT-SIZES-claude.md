# Adversarial review — OBJECT-SIZES.md

Reviewed 2026-09-15 against `main` at 674d852 (0.34.0). Ranked most severe first. A historical
record of a review, not a living spec.

## Findings

### 1. MAJOR — the per-operation size line erases the rollback-on-cancel notice from the console

- **Claim:** §4 Console: "One narration line per finished operation, through the existing notice
  sink (`WithNoticeSink`, today used for the manifest-start rollback-on-cancel notice)".
- **Reality:** the TUI's notice is one slot. `tuiForwarder.notice` sends a `tui.LogMsg`
  (`cmd/sqlgopace/main.go:1227-1228`), and the model **overwrites** it: `case LogMsg:
  m.notice = msg.Line` (`internal/tui/model.go:578-579`). The view prints that single line
  (`internal/tui/view.go:92-93`). The rollback-on-cancel notice goes out once per manifest
  (`internal/run/engine.go:635-640`). It exists because of H2 in
  `REVIEW-2026-09-15-harm.md`, which was fixed today.
- **Consequence:** take a Standard-edition manifest with 200 `rebuild_index` operations under
  `--tui`. The console shows "200 of 200 operation(s) can only be canceled under pressure…" at
  start. When operation 1 finishes, its size line replaces that notice, and for the other 199
  operations the operator no longer sees the hazard. H2 comes back after one operation. The
  same slot carries "killed blocker SPID …" (`main.go:513`) and operator-action failures such
  as "kill SPID %d failed" (`main.go:1327`). On a run of short reorganizes, those survive only
  until the next operation finishes.
- **Fix:** keep size lines out of the notice sink. Write them to `e.out` and the report only,
  or attach them to the operation's row in the operations panel. The panel option is
  unverified: I did not check whether a row has room for it.

### 2. MAJOR — the `heap rebuild scope` Warn is never shown before the rewrite happens

- **Claim:** §2: "It is a `Warn` rather than a `Pass` because … the operator should read it
  once."
- **Reality:** preflight checks go only into `rep.Preflight` (`engine.go:596`), which only the
  `.log` renders (`internal/report/report.go:125-129`). Only `FAIL` lines reach the console
  alert (`failLines`, `engine.go:495-503`, used at `:1137` and `:1294`). Nothing writes a
  `Warn` to `e.out` or the TUI. This is the H2 defect class again: "The `.log` has the line,
  but it is read after the damage."
- **Consequence:** under `--auto`, or any run without a dry run first, nobody sees the warning
  until the heap and all its nonclustered indexes have already been rewritten under Sch-M.
  The check gets built and tested but does not do the one thing it is for.
- **Fix:** send the scope line with the manifest-start notice: once per manifest, to `e.out`,
  the report and the sink. Finding 1 applies here too: the TUI slot holds one line, so the
  two notices must be joined into one line or they overwrite each other at manifest start.
  Or drop the "should read it" rationale and call it a `.log` record.

### 3. MINOR — the "before" read cannot be placed where the spec puts it

- **Claim:** §4: "In `runStep`, next to the `waitsBefore` snapshot … Not read when the
  statement is the `RESUME` of a resumable paused by an earlier run".
- **Reality:** `waitsBefore` is at `engine.go:847`. The choice between RESUME and REBUILD is
  made later, at `:874-883`. `ownsPausedResumable` can be true while `resumeStatement`
  declines ("nothing is actually paused now … the planned REBUILD runs").
- **Consequence:** at `:847` the implementer can only test `ownsPausedResumable`. That drops
  the before read on a clean restart, which is a real REBUILD.
- **Fix:** read after the `switch`, and skip the read when `stmt != step.SQL`. Separately, the
  claim that a before read at RESUME "would be neither the old size nor the new one" is
  unverified. The source index stays intact during a resumable rebuild, and whether the
  partial target joins to `sys.indexes` is untested. Add it to "Still open".

### 4. MINOR — the reason given for "no after read on failure" is wrong for `reorganize_index`

- **Claim:** §4: "A failed or canceled operation rolled back to the old structure".
- **Reality:** REORGANIZE is "Not rolled back when performed within a transaction and the
  transaction is rolled back" (ALTER INDEX docs). The engine says the same:
  `internal/run/reaction.go:100-105`, `:185-186` ("committed work preserved, no rollback"), and
  `MAINTENANCE.md:563`.
- **Consequence:** when a reorganize canceled under pressure ends as failed, it keeps real
  compaction but reports no size, and the manifest total leaves it out. Those are exactly the
  operations pressure cuts short. Low harm, but the stated reason is false.
- **Fix:** for `reorganize_index`, read "after" on any outcome and label it `(partial)` when the
  operation did not succeed.

### 5. MINOR — prior art not cited: the preflight rule was already specified and never built

- **Claim:** "The defect", presented as a new finding.
- **Reality:** `MAINTENANCE.md` §9 (line 587) already requires this: "`rebuild_heap`: needs free
  space for a copy of the heap **plus** its nonclustered indexes (the rebuild re-creates them
  all)". `COMPRESSION-SCOPE.md` §4.5 (lines 214-218) already recorded that the heap size is
  "partition 1's size, not the sum". `MAINTENANCE.md:698-700` already names `max_size_mb` as
  the guard against the heap rebuild being "silently expensive". That supports §5's split and
  should be quoted there.
- **Consequence:** the docs list misses `MAINTENANCE.md` §9 and `COMPRESSION-SCOPE.md` §4.5,
  which says "judged by partition 1". The CHANGELOG would describe a regression-style defect
  when the truth is a requirement that was never implemented.
- **Fix:** cite all three, and add §9 and §4.5 to "Docs to update".

### 6. MINOR — "supersede in place" targets a design that has not shipped and edits the same lines

- **Claim:** "`docs/specs/COMPRESSION-SCOPE.md` §4: the silent heap skip is now loud; supersede
  in place."
- **Reality:** COMPRESSION-SCOPE is not implemented: `raise_to_target` does not appear in
  `internal/maint`, and `plan.go:184-186` is still silent. `TODO.md` does not name it.
  Its §4.4.3 rewrites `decideHeap`. Its §4 prescribes a different skip line ("size 42000 MB
  outside [10, 10000] MB …"), and its §7 tests "skipped **with the size in the reason**".
- **Consequence:** both designs change `buildInput`'s heap skip and `decideHeap`. Whichever
  lands second contradicts its own spec's tests and log format.
- **Fix:** say that OBJECT-SIZES owns the heap skip line. Amend COMPRESSION-SCOPE §4 and §7 to
  point here, and don't call it superseding shipped behaviour.

### 7. MINOR — `docs/permissions.md` becomes false, and a login without the grant gets "unknown" on every operation

- **Claim:** the error table treats the missing grant as "unknown" everywhere, and the docs list
  does not include `permissions.md`.
- **Reality:** `docs/permissions.md:25-38` ties `VIEW DEFINITION` to `require_data_free_space`
  and ends "Nothing else in the tool needs it." The engine size lines and the connected dry-run
  heap line would read `sys.dm_db_partition_stats` whatever `require_data_free_space` is set
  to.
- **Consequence:** the permission page is wrong. An operator without the grant gets a
  `size: unknown -> unknown` line on every operation of a campaign.
- **Fix:** add `permissions.md` to the docs list. When both sides are unknown, print no line
  for the operation and write one manifest-level line: "sizes not measured: <error>".

### 8. MINOR — the reason for `HumanizeKB` is wrong, and it has no TB step

- **Claim:** "the console's `tui.HumanizeMB` cannot be imported there"; "Sizes escalate KB → MB
  → GB".
- **Reality:** `internal/tui` imports no internal package, so importing it from
  `internal/report` is not a cycle. It is a layering reason (it would pull Bubble Tea into
  `report`), not an impossibility. `tui.HumanizeMB` does go up to TB (`model.go:801-810`).
  `TODO.md:272` records a single 1.4 TB object.
- **Consequence:** that object would render as `1433.6 GB` in the `.log`, while the console
  header shows TB for the same database.
- **Fix:** add the TB step and reword the reason.

### 9. MINOR — manifest totals double-count overlapping operations

- **Claim:** "`SizeBeforeKB`, `SizeAfterKB` … summed over the operations that have both sides
  measured."
- **Reality:** the planner suppresses index operations on a heap it rebuilds
  (`decide.go:152-160`). A hand-written manifest does not.
- **Consequence:** a manifest that does `rebuild_heap dbo.MEASUREMENT` and then
  `rebuild_index … IX_MEASUREMENT_TS`, or reorganizes and then rebuilds the same index, counts
  that index twice in "before" and "after", which inflates the summary figure.
- **Fix:** sum per distinct structure (first before, last after), or label the figure "sum over
  operations; may overlap".

### 10. MINOR — undecided which size the heap history row records

- **Reality:** `recordPlanHistory` stores `d.Metrics.SizeMB` in `maintenance_analysis.size_mb`
  (`cmd/sqlgopace/plan.go:339`, `internal/report/history.go:58`). §5 adds `RewriteMB` but does
  not say whether `Metrics.SizeMB` is the heap alone or the rewrite. On a partitioned heap the
  value changes either way (partition 1 today, a sum afterwards).
- **Consequence:** history rows before and after 0.35.0 mean different things with nothing to
  tell them apart. `TODO.md` reads these rows for campaign analysis.
- **Fix:** state that `size_mb` is the heap alone summed over partitions, and put that change in
  the CHANGELOG.

### 11. MINOR — "Neither changes the design; each changes one filter" does not hold in one branch

- **Reality:** "Disabling a nonclustered index … physically deletes the index data" (ALTER INDEX
  docs), so a disabled index has nothing to measure before. When a clustered index is dropped,
  a disabled nonclustered index is "Rebuilt and enabled" (Enable indexes docs), and "compression
  settings metadata is lost when nonclustered indexes are disabled". The query's inner `JOIN` to
  `sys.dm_db_partition_stats` would not list such an index at all.
- **Consequence:** suppose verification shows `ALTER TABLE … REBUILD` on a heap re-enables
  disabled indexes. No filter can then size them. The rewrite is bigger than the preflight
  check counts, and the operator's deliberate `DISABLE` is silently undone, probably without
  its compression.
- **Fix:** have the verification decide between "filter on `is_disabled`" and "LEFT JOIN, plus a
  warning that the rebuild re-enables `<index>`".

### 12. MINOR — the motivation promises a campaign figure; the design delivers per-manifest figures

- **Claim:** "On a compression campaign that is the number the operator is asked for."
- **Reality:** `TODO.md:292-301` describes a campaign of 912 objects, five manifests, spread
  over months. The design writes totals into each manifest's `.log`. The history `runs` table
  gets no size columns (`history.go:33-42`), although additive migrations are already set up
  for that (`history.go:72-75`).
- **Consequence:** the figure the operator is asked for still has to be totalled by hand across
  `03.done/*.log`.
- **Fix:** narrow the claim to per-manifest figures, or add `size_before_kb` / `size_after_kb`
  to `runs` through `columnMigrations`. That this is cheap is unverified beyond the migration
  mechanism existing.

### 13. MINOR — the pure `Rewritten` is placed in the SQL adapter

- **Claim:** "The selection … is pure and lives next to the type" in `internal/mssql`.
- **Reality:** CLAUDE.md splits a pure core from `internal/mssql`, "the only package that issues
  SQL directly". The spec's own reason, that `mssql` already imports `ddl` (`server.go:15`), is
  true, but it allows the placement rather than justifying it.
- **Fix:** put `Rewritten` in `internal/run` or `internal/preflight`, next to `rebuiltObject`,
  which it replaces. Minor either way.

## Sound

- The preflight defect is real: `rebuiltObject` returns an empty index name for a heap
  (`preflight.go:198-199`), and `IndexSizeMB` then reads `index_id = 0` only (`indexes.go:104`).
- The planner defect is real: `InventoryObject.SizeMB` is per partition, `heapMeasurement` reads
  `head` only (`plan.go:183-184`), and `plan/shrink.go:91-94` sums correctly.
- `decide.go:282-287` is dead in production, as the spec claims.
- `IndexSizeMB` has one production caller, and its integration test has the four cases named.
- Both vendor quotes match word for word (ALTER TABLE example A; Index disk space example).
- The `VIEW DEFINITION` rationale matches Microsoft's page and `permissions.md`.
- Reads land in the right database: one engine per database (`main.go:650`) and one dry-run
  connection per database (`main.go:161-168`).

## Verdict

**Revise before planning, but only §4's console path and §2's Warn** (findings 1-2). The rest
are wording, docs-list, or one-condition corrections.

Parts that can ship on their own:

- **§5 (planner)** can be planned now, once the owner of the skip line is settled with
  COMPRESSION-SCOPE (finding 6) and the history column is decided (finding 10).
- **§1 + §2's space sum** can be planned now without the Warn.
- **§3 (dry run)** is not blocked by anything here.
- **§4 (engine report)** can be planned once console output is routed away from the notice
  sink; `.log` rendering and totals are unaffected by finding 1.
