# COMPRESSION-SCOPE — compress every object below target

> Depends on `specs/OPERATION-INTENT.md` (the planner must mark an operation
> compression-motivated). Sequenced after it.
>
> Citations are against mainline at `2e2c8ca`. An earlier draft was written against a stale
> worktree and proposed a new `compression` decision category; that design is abandoned — see
> §3.

## 1. Problem

There is no way to compress all objects in a database. The 74-operation hand-written campaign
manifests in `04.failed/` are the symptom: an operator enumerated every index by hand, and the
`.033` header comment records that they hand-filtered the already-PAGE ones too.

Four gates block it, all verified:

| Gate | Effect |
|---|---|
| `page_count_floor: 1000` (`internal/maint/decide.go:195-197`) | Returns `skipDecision` **before** `decideCompression` runs at `decide.go:203`. Any index under 1000 pages is never compressed. |
| `min_gain_percent: 5` (`internal/maint/profile.go:319-321`) | An object gaining 3% is never compressed. Setting `0` does not work: `applyDefaults` treats a Go zero as "unset" and restores 5. `MinGainPercent` is a plain `float64` (`profile.go:95`), unlike `Statistics.ModificationPercent` (`profile.go:141`) which uses a pointer for exactly this reason. |
| `decideHeap` trigger (`decide.go:294-298`) | Returns **before** `decideCompression` unless forwarded ≥10%, frag ≥15%, or free-space dev ≥30% already fired. A clean heap is never compressed — not even with an `overrides[].compression` pin, because the pin lives inside `decideCompression` (`decide.go:392-395`) and is never reached. |
| `heap.min/max_size_mb` (`cmd/sqlgopace/plan.go:178-180`) | Heaps outside 10 MB–10 GB are dropped **in `buildInput`, before any Decision exists** — `return maint.HeapMeasurement{}, false` with no log line, unlike the failure path two lines below (`plan.go:183`). Invisible to `--explain`, to the plan, and to history. The `decide.go:283` check is dead in production. |

The sharpest is the heap asymmetry. For an index, `decide.go:204`:

```go
needRebuild := fragRebuild || comp.change   // compression alone justifies a rebuild
```

For a heap, compression cannot justify a rebuild at all.

**What is *not* a gate:** an earlier draft claimed `--categories compression` alone "emits
nothing". It emits `checkdb` — `decideCheckDB` is ungated (`decide.go:173`), and
`cmd/sqlgopace/plan_test.go:131-134` pins that. What is true is narrower: `compression` is not
a decision category. It only sets `wantComp` (`plan.go:73`), which gates three reads — entry
into `indexMeasurements` (`plan.go:86`), the per-partition split (`plan.go:132`), and the
`IndexOperationalStats` read (`plan.go:144,168`). Compression always rides inside a
`rebuild_index`/`rebuild_heap` emitted by the `index`/`heap` categories.

## 2. Goals

- One command compresses every object below target in a database, minus exclusions.
- The same across every eligible database, reusing the existing per-database machinery.
- Selection is **objective**: an object is selected iff its current compression is below
  target. No thresholds, no estimates, no tuning.
- Idempotent by construction: a re-run selects only what is still below target.
- Never downgrade.
- Nothing skipped silently.

## 3. Non-goals, and one abandoned design

- **A new `compression` decision category.** The earlier draft proposed one. It is the wrong
  shape, for five independent reasons found in the code:
  - `manifestCategories` (`plan.go:320-329`) is the manifest grouping key; a `Decision` whose
    category matches no entry produces **no manifest at all**.
  - Adding a 5th entry renumbers every manifest: `n := (ordinal*len(manifestCategories) + pos + 1) * 10`
    (`plan.go:349`) and `prefixWidth` (`plan.go:366`) both key off the slice length. `020_index`
    → `030_index`, and every multi-database block shifts. Breaks `plan_test.go:144-147,231-232`
    and contradicts `specs/MAINTENANCE.md:517-520,800,827,936-937`.
  - `Decide`'s cross-cutting suppression (`decide.go:153-160`) matches on the **`ddl.RebuildIndex`
    type**, not on category, and rewrites the decision as `skipDecision("index", …)` — silently
    re-categorizing a compression op and moving it to another manifest.
  - Nothing dedups across categories, so `--categories index,compression` would emit **two**
    rebuilds of the same index, into two different manifests.
  - The CLI already uses `compression`/`heaps` while decisions use `heap`; a `compression`
    decision category would make one word mean two things.

  All of it is avoided by keeping `compression` exactly what it is — a read gate — and
  changing only how `decideCompression` chooses. See §4.
- **Scoped `rebuild` / `reorganize` / `alter_table` operations.** Their selection is heuristic
  and already implemented in `decideIndex`/`decideHeap` with Ola's calibrated defaults.
  Re-implementing it engine-side would be a second planner with no review step. Compression is
  the only member with a threshold-free rule.
- **A new `operation:` type.** No `operation: compress`, no engine change. The engine keeps
  executing per-object `rebuild_index`/`rebuild_heap`.
- **Columnstore.** Not in `maint.Compression` (`none|row|page`, `profile.go:36-40`), not in the
  matrix, not estimable (`plan.go:257-263`). Reported, never selected.
- **Parallel per-database execution.** `--all-databases` runs databases sequentially
  (`plan.go:485-491`, `main.go:275-320`, alphabetical via `main.go:530`). Subprocess/parallel
  looping is a separate future feature; nothing here precludes it — selection is per-database
  and stateless.

## 4. Design

### 4.1 The ladder

```
NONE  <  ROW  <  PAGE
```

Selected iff `current < target`:

| target | selects | never selects |
|---|---|---|
| `row` | `NONE` | `ROW`, `PAGE` |
| `page` | `NONE`, `ROW` | `PAGE` |

`current > target` is **never** selected: the mode raises only. This is strictly safer than
what exists — `overrides[].compression` computes `change: !sameCompression(...)`
(`decide.go:393`) against a rank equality with no direction (`sameCompression`, `decide.go:532`),
so a `compression: row` pin on a PAGE index today emits a **downgrade** rebuild.

### 4.2 The knob

`CompressionRules` (`profile.go:92-101`) gains two fields. `Parse` uses `KnownFields(true)`
(`profile.go:206`), so they must exist before any profile may name them:

```yaml
compression:
  enabled: true
  mode: gain_based        # default — today's behavior, unchanged
  # mode: raise_to_target # campaign mode: everything below target
  target: page            # row | page — required in raise_to_target, rejected in gain_based
  per_partition: true     # see §4.5
  objects:                # EXISTING mechanism — this is the exclusion list
    include: []
    exclude:
      - "tmp.*"
      - "*.AuditLog"
```

`gain_based` stays the default, so no existing profile changes behavior. In `raise_to_target`,
`min_gain_percent`, `page_min_extra_gain_percent`, and the write-intensity downgrade
(`write_intensive_ratio` → `write_intensive_compression`, `decide.go:422-433`) are **ignored** —
the target is declared, not inferred. That is the point of the mode.

`validate` (`profile.go:400`) rejects an unknown `mode`, a missing `target` in
`raise_to_target`, and a `target` set in `gain_based`.

### 4.3 Exclusions already exist — use them

The operator asked for an exclusion list. **`compression.objects.include/exclude` is already
it**: a glob allow/deny list scoped to compression alone, enforced by `CompressesObject`
(`profile.go:114-124`, "exclude wins"), applied at `plan.go:143,166`, validated with
`path.Match` (`profile.go:400`), documented at `maintenance_profile.yaml:22-29` ("This governs
ONLY compression"), and tested (`profile_test.go:238,267`, `decide_test.go:134`).

An earlier draft proposed a *second* `compression.exclude` regex list beside it, with
unspecified precedence, and justified regex by comparing against `overrides[].match` — the
wrong feature. It also cited `CompileIgnoredSessions` as reusable; that is a **session**
matcher in `internal/run` (app/host/login/statement against a live SPID, `executor.go:86,121`),
cannot match `schema.object`, and importing `run` from `maint` would drag the SQL driver into
the pure decision core (`profile.go:1-5` documents it as I/O-free).

Globs are also **more** expressive here: `objects.*` matches `schema.table` **and**
`schema.table.index` (`profile.go:119-124`), so a single index can be excluded — the regex
proposal was `schema.object` only. `raise_to_target` honors `objects` exactly as `gain_based`
does. No new mechanism.

### 4.4 Gate resolution

**1. `page_count_floor` gates fragmentation only.** Ola's floor exists because fragmentation is
meaningless on a small index; it was never about compression. The restructure must keep the
reorganize return — an earlier draft dropped it, which would have sent every frag-12% index
into the rebuild branch and failed `decide_test.go:42`:

```go
belowFloor := m.PageCount < int64(p.Index.PageCountFloor)
frag := m.FragmentationPercent
fragRebuild := !belowFloor && frag >= p.Index.RebuildFromPercent
fragReorganize := !belowFloor && !fragRebuild && frag >= p.Index.ReorganizeFromPercent

comp := decideCompression(...)
needRebuild := fragRebuild || comp.change
if !needRebuild {
    if fragReorganize {
        return reorganizeDecision(m, p, ...)
    }
    return skipDecision("index", target, reasonFor(belowFloor, frag, comp))
}
```

No existing test breaks: the only floor test (`decide_test.go:44`, `{"skip below page floor",
99, 500, "skip"}`) supplies no `Estimate`, so `decideCompression` returns `change: false`
(`decide.go:402-404`) and the index still skips. It asserts `d.Kind`, not `d.Reason`.

**2. `min_gain_percent`** is bypassed by `raise_to_target`, not fixed. The zero-means-unset trap
(`profile.go:292-295`) is a `gain_based` concern; changing it would alter existing behavior.
Worth its own issue.

**3. `decideHeap` gains the index's rule.** Compute compression before the trigger check:

```go
comp := decideCompression(m.Schema, m.Table, "", m.Current, m.Estimate, m.Write, ov, p)
trigger := forwardedPct >= p.Heap.ForwardedRecordPercent ||
    m.FragmentationPercent >= p.Heap.FragmentationPercent ||
    freeSpaceDev >= p.Heap.FreeSpaceDeviationPercent
if !trigger && !comp.change {
    return skipDecision("heap", target, "no trigger and no compression change: "+reason)
}
```

Every argument is in scope (`ov` at `decide.go:273`, `reason` computed at `decide.go:292-293`,
before the trigger check). `decide.go:299-302` already reads `comp.change`, so no duplicate
work. No test breaks: `decide_test.go:277-280` ("no trigger") has `Estimate` nil → `comp.change`
false → still skips.

**This changes `gain_based` too**: a clean heap whose measured gain clears the bar now gets
compressed where it previously did not. Deliberate — the asymmetry was never justified — but it
is a behavior change on existing profiles and needs a changelog line.

**4. `heap.min/max_size_mb` stays a guard, and becomes loud.** A heap rebuild is offline and
size-of-data; on Standard it blocks for its full duration. The ceiling is a deliberate guard.
But the skip must be **visible**, and that is impossible in `decide.go` — `plan.go:178-180`
drops the heap before `decideHeap` ever runs. `buildInput` must emit the reason instead of a
bare `false`, matching the log line it already writes two lines down (`plan.go:183`):

```
-- skip heap dbo.BigLog: size 42000 MB outside [10, 10000] MB (compress it in a dedicated manifest)
```

`estimable()` (`plan.go:257-263`) and `parseCompression` (`plan.go:274`) drop non-rowstore
objects the same silent way; they get the same treatment. Without this, §2's "nothing skipped
silently" is unachievable — no amount of `decide.go` work reaches those objects.

### 4.5 Partitions — what the mode can and cannot promise

The inventory reads `data_compression_desc` **per partition** (`internal/mssql/analysis.go:33`),
but the planner collapses it:

- **Indexes**: per-partition measurements only when `granular := wantComp &&
  p.Compression.PerPartition && len(group) > 1` (`plan.go:132`). `PerPartition` is a plain
  `bool` (`profile.go:99`) that `applyDefaults` never sets — **default false**. Non-granular,
  `Current: parseCompression(head.Compression)` (`plan.go:164`) takes **partition 1's**
  compression for the whole index, so a partitioned index with P1=PAGE and the rest NONE reads
  as PAGE and is never selected. **`raise_to_target` therefore requires `per_partition: true`**;
  validation rejects the combination `mode: raise_to_target` + `per_partition: false` rather
  than silently under-selecting.
- **Heaps**: `HeapMeasurement` has a single `Current` (`decide.go:50`) built from `head` only
  (`plan.go:206`), `sizeMB` is partition 1's size not the sum (`plan.go:177`, unlike the index
  path at `plan.go:156`), and **`ddl.RebuildHeap` has no `Partition` field**
  (`manifest.go:710-715`). A partitioned heap cannot be compressed per partition even in
  principle. Documented limitation: heaps are all-or-nothing, judged by partition 1.

### 4.6 Intent, and the heap gap

`decideIndex` emits `rebuild_index` with `intent: compression` when `!fragRebuild`
(OPERATION-INTENT §4.5) — nothing extra is needed here.

`rebuild_heap` **cannot carry intent**: OPERATION-INTENT scopes the field to `RebuildIndex`,
and `skipSatisfied` type-asserts `ddl.RebuildIndex` (`skip.go:51`) and returns false for
anything else, so a heap is never skipped anyway. Consequence: **re-running a heap compression
manifest redundantly rebuilds heaps already at target.** The planner will not re-select them
(the ladder does that at plan time), so this only bites a literal re-run of a materialized
manifest. Accepted; the alternative is an `Intent` field plus a heap compression reader
(`CompressionReader.IndexCompression`, `skip.go:14-16`, is index-only), which is not justified
by one case. An earlier draft asserted `rebuild_heap` would carry intent "per OPERATION-INTENT
§4.4" — that spec says the opposite. Contradiction resolved here.

## 5. What the operator runs

```bash
# Everything below PAGE in one database, review first:
sqlgopace plan --config config.yaml --categories index,heaps,compression --database MYDB
sqlgopace --config config.yaml

# Unattended:
sqlgopace --config config.yaml --auto --categories index,heaps,compression --database MYDB

# Every eligible database on the server:
sqlgopace --config config.yaml --auto --all-databases --categories index,heaps,compression
```

`index,heaps,compression` — not `compression` alone — because `compression` is a read gate and
the object categories are what enumerate indexes (`plan.go:86`) and heaps (`plan.go:80`). In
`raise_to_target` the `index`/`heaps` categories contribute their enumeration; their
fragmentation decisions still run and may emit their own reorganize/rebuild ops, which is
usually wanted on a maintenance run and is the existing behavior.

All three parse today (`plan.go:36,384-385`; `main.go:68-74`). `--auto` + `--dry-run` is
rejected (`main.go:100`) and is not used here.

## 6. Multi-database

Already built, reused unchanged: `--all-databases`/`--databases` (`plan.go:384`, `main.go:72`),
scope selector `resolveDatabases` → `ScopeIncludes` (`cmd/sqlgopace/scope.go:56`,
`profile.go:254-263`), per-database manifest blocks (`plan.go:342-361`, `planMulti`
`plan.go:482-501`), one connection per database sequentially (`plan.go:485-491`,
`main.go:275-320`), database-aware recovery (`main.go:190-198`, `internal/run/recovery.go:175-179`).

## 7. Testing

Pure, table-driven, no database, via the exported `DecideIndex`/`DecideHeap` (`decide.go:179`)
and `OperationsByCategory`.

- **Ladder**: every (current, target) pair. `NONE→ROW` selects; `ROW→ROW` does not; **`PAGE→ROW`
  does not** (no downgrade); `NONE→PAGE`, `ROW→PAGE` select; `PAGE→PAGE` does not.
- **Floor**: an index below `page_count_floor` and below target is **selected** (the regression
  this spec exists to fix); at target it skips; its fragmentation decision stays suppressed;
  a frag-12% index still returns `reorganize_index` (guards the restructure).
- **Heap**: clean heap below target → `rebuild_heap` emitted; clean heap at target → skip; heap
  outside the size bounds → skipped **with the size in the reason**.
- **Mode**: `raise_to_target` ignores `min_gain_percent`, `page_min_extra_gain_percent` and the
  write-intensity downgrade; `gain_based` honors all three, unchanged.
- **Objects scope**: an excluded object below target is not selected; `objects.exclude` beats an
  `overrides[].compression` pin; a 3-part `schema.table.index` exclusion works.
- **Profile validation**: unknown `mode`; `target` required in `raise_to_target` and rejected in
  `gain_based`; `raise_to_target` + `per_partition: false` rejected.
- **Silent-skip regression**: `buildInput` emits a reason for an oversized heap and for a
  non-estimable object (assert via the fake readers and the log writer).

## 8. Consequences

- `decideHeap` learning `|| comp.change` changes `gain_based` behavior (§4.4.3). Changelog.
- `raise_to_target` selects negligible-gain objects — a 3%-gain table is rebuilt for little
  benefit. That is the declared intent of the mode; `gain_based` remains the default.
- Compressing a heap rewrites all its nonclustered indexes; on Standard it is offline and
  blocking for the duration. `max_size_mb` is the guard and the reaction hierarchy applies, but
  an operator running `--auto --all-databases` on Standard should know what they asked for.
- `data_compression` is an unvalidated raw string interpolated into T-SQL
  (`internal/ddl/generate.go:107`); `Validate()` never checks it (`manifest.go:578-582`). This
  spec puts a profile-driven value into that path. The profile side is validated (`row|page`),
  but `overrides[].compression` is not — so §4.1's never-downgrade guarantee holds for the mode
  and is bypassable by a pin. Making it an enum is a separate issue.
