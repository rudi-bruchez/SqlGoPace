# OPERATION-INTENT — why a rebuild was scheduled

> Implemented before `specs/COMPRESSION-SCOPE.md`, which needs the planner able to mark an
> operation compression-motivated.

## 1. Problem

`rebuild_index` does two unrelated things:

1. **Applies a compression target** — a state. Idempotent: if the index is already `PAGE`, the
   goal holds and there is nothing to do.
2. **Rebuilds the index** — defragments, rebuilds statistics at fullscan, reclaims pages. An
   act. It has no "already true".

The manifest cannot say which one motivated the operation, so the engine cannot decide whether
an already-compressed index still needs rebuilding. `skip_if_satisfied`
(`internal/ddl/manifest.go:204`) is a manifest-level flag that makes `skipSatisfied`
(`internal/run/skip.go:47`) skip any `rebuild_index` whose `data_compression` already holds. It
applies to every operation in the manifest, and it inspects only compression.

### When the skip is wrong

The planner's fresh output is safe. `decideIndex` sets the field only on a real change
(`internal/maint/decide.go:239-242`):

```go
dataCompression := ""
if comp.change {
    dataCompression = comp.target.DataCompression()
}
```

and `comp.change` is true only when the target differs from current (`decide.go:438-441`, and
`decide.go:393` for the override pin). So a frag-only rebuild carries no `data_compression` and
exits `skipSatisfied` at `skip.go:52`; a frag+compression rebuild carries one that differs from
current by construction, so `compressionSatisfied` is false. Neither is ever skipped.

The skip misfires once the manifest and the server have **drifted** — which is what the flag
exists for (`manifest.go:200-203`: "a compression manifest re-run after an interruption sets it
to skip finished work"). Three ways in:

1. **A dual-motive rebuild, re-run after drift.** `fragRebuild && comp.change` — scheduled for
   *both* 40% fragmentation and NONE→PAGE. If the compression lands and the run is retried
   (crash, drain, window close, or a continue-on-failure cursor gap replaying it), the
   compression now matches, the operation is skipped, and **the defrag never happens**. The log
   says `skipped: already PAGE`; the outcome is SUCCESS.
2. **A hand-written manifest** setting `data_compression` on an operation meant to defragment.
3. **A resumed or recovery run** of either.

Narrow, but silent and mis-reported.

### The asymmetry that decides the design

| Setting wrong | Outcome |
|---|---|
| Skip disabled when it could have skipped | Re-compresses an already-compressed index. Wasted time, correct result, visible in the log. |
| Skip enabled when it should have run | Skips a needed rebuild. Wrong result, invisible, reported as success. |

One wastes hours; the other lies. A manifest-level boolean cannot tell them apart, because the
distinction is per-operation.

### The discriminator already exists and is discarded

`decide.go:243-247`:

```go
reason := fmt.Sprintf("fragmentation %.0f%%", frag)
if !fragRebuild { // rebuild is purely compression-motivated
    reason = "compression change"
}
```

`fragRebuild` (`decide.go:200`) separates **compression-only** — `!fragRebuild`, which past the
`if !needRebuild` return at `decide.go:206` forces `comp.change` — from
**fragmentation-involved**, which may *also* change compression. It is not a motive:
`fragRebuild && comp.change` is dual-motive. But it is the split the skip needs, because a
dual-motive rebuild must run.

The value is flattened into prose on `Decision.Reason`, which `OperationsByCategory`
(`decide.go:115-123`) drops on the way to YAML; `MarshalManifest` (`internal/ddl/render.go:16-78`)
emits no comments. A generated `020_maint_MyDb_index.yaml` cannot say why any line is in it.

## 2. Goals

- A `rebuild_index` declares **why** it exists.
- Skip an already-compressed index **only** when the intent is compression.
- A fragmentation-involved rebuild **always** runs, whatever the current compression.
- Generated manifests become self-documenting.
- Retire `skip_if_satisfied`.

## 3. Non-goals

- **Intent on other operation types.** `reorganize_index` cannot set compression
  (`manifest.go:687-693`, no `DataCompression` field); `add_column` and friends are unambiguous.
  Only `rebuild_index` conflates a state and an act. `COMPRESSION-SCOPE` therefore cannot rely
  on `rebuild_heap` carrying intent; see that spec §4.6.
- **A general `Operation.Satisfied()` predicate** (`specs/FIXES.md` #28/#158). A pure defrag
  rebuild has no target state to probe, so the predicate would be partial and its absence
  indistinguishable from "not satisfied". Deferred — and not blocked: a later predicate can read
  this field.
- **Fragmentation thresholds.** Already implemented with Ola Hallengren's defaults:
  `page_count_floor: 1000`, `reorganize_from_percent: 5`, `rebuild_from_percent: 30`
  (`decide.go:195,200,201`; defaults `internal/maint/profile.go:298-305`).

## 4. Design

### 4.1 The field

On `RebuildIndex` only (`internal/ddl/manifest.go`), keeping the existing `Partition` and `Kind`
doc comments:

```go
type Intent string

const (
    IntentCompression   Intent = "compression"
    IntentFragmentation Intent = "fragmentation"
)

// Intent records why a rebuild was scheduled. A rebuild both applies a compression target (a
// state, idempotent) and defragments (an act, never idempotent); only the manifest knows which
// motivated it, and the engine cannot skip correctly without being told.
Intent Intent `yaml:"intent,omitempty"`
```

`Validate()` rejects any value but the two constants, naming the offending value. It does **not**
reject `intent: compression` on an operation without `data_compression` — §4.3 explains why.

No operation-level renderer change: `operationNode` (`render.go:82-90`) encodes the struct, and
`compact` (`render.go:108-126`) already drops empty scalars via `isEmptyScalar`
(`render.go:129-143`).

Expansion carries it: `expandedRebuild` (`internal/ddl/expand.go:90`) copies the whole operation
and overrides only `Index`/`Kind`, so a new field survives `index: ALL` by default.

### 4.2 Semantics

| effective intent | meaning | already at target compression |
|---|---|---|
| `compression` | the goal is the state | **skipped** |
| `fragmentation` | the goal is the act | **runs** |
| *unset* | unknown | **runs** — safe default |

Unset runs, per the asymmetry in §1: wasted work is recoverable, a silent skip is not.

### 4.3 Manifest-level default

A campaign should not repeat `intent: compression` on every operation:

```yaml
description: "Recompress DISPATCH indexes"
intent: compression          # default for every rebuild_index below
operations:
  - operation: rebuild_index
    schema: dbo
    table: DISPATCH
    index: IX_DISPATCH
    data_compression: PAGE
  - operation: rebuild_index
    schema: dbo
    table: ORDERS
    index: IX_ORDERS
    data_compression: PAGE
    intent: fragmentation    # overrides the default
```

Precedence: operation > manifest > unset (runs).

**The default resolves where it is used, not at load.** Three constraints force this:

- the `MarshalManifest`/`ParseManifest` round-trip asserted by `render_test.go:15-52` requires
  parsed operations to match the source; a load-time push-down gives them intents the source
  operations lacked;
- `Intent: ""` is the inherit signal, so a pushed-down default leaves no way for an operation to
  opt *out*;
- a manifest may legitimately mix a compression default with operations that carry no
  `data_compression` at all (`01.to_run/.010_test.yaml` is that shape), and those must stay
  loadable.

So resolution is one helper at the single call site:

```go
// effectiveIntent resolves an operation's intent against the manifest default.
func effectiveIntent(manifestIntent ddl.Intent, op ddl.RebuildIndex) ddl.Intent {
    if op.Intent != "" {
        return op.Intent
    }
    return manifestIntent
}
```

`Manifest.Intent` is validated like the field, and **must be emitted by `MarshalManifest`** — a
manifest-level field needs an explicit `addPair` (`render.go:36-38` is the pattern).

### 4.4 Engine

`skipSatisfied` (`skip.go:47`) trades its `enabled bool` for the manifest default:

```go
func (e *Engine) skipSatisfied(ctx context.Context, manifestIntent ddl.Intent, op ddl.Operation) (string, bool) {
    if e.compression == nil {
        return "", false
    }
    ri, ok := op.(ddl.RebuildIndex)
    if !ok || ri.DataCompression == "" || effectiveIntent(manifestIntent, ri) != ddl.IntentCompression {
        return "", false
    }
    // … unchanged: IndexCompression read, compressionSatisfied, "already PAGE"
}
```

The call site (`engine.go:480`) passes `manifest.Intent`. The `ownsPausedResumable` carve-out,
the read-error-means-not-satisfied rule (`skip.go:56`), and `advanceCursor` on skip
(`engine.go:482`) are unchanged.

A compression-intent operation needs no opt-in: it is a no-op by definition when its goal holds.
The flag's only job was standing in for the intent the manifest could not express.

### 4.5 Planner

`decideIndex` (`decide.go:239-251`) sets the field from the boolean it already computes:

```go
intent := ddl.IntentFragmentation
if !fragRebuild { // rebuild is purely compression-motivated
    intent = ddl.IntentCompression
}
op := ddl.RebuildIndex{
    Schema: m.Schema, Table: m.Table, Index: m.Index,
    Partition: m.Partition, DataCompression: dataCompression, Intent: intent,
}
```

A dual-motive rebuild gets `IntentFragmentation`: it must run, and running is what fragmentation
intent means. The planner emits per-operation intent and no manifest-level default — it knows
each motive exactly.

`renderDecisions` (`cmd/sqlgopace/plan.go:614-620`) prints `Decision.Reason`, built from the same
`fragRebuild`, so `--explain` already conveys the motive in prose. Printing the field itself is
optional polish.

### 4.6 Removing `skip_if_satisfied`

Manifest decoding rejects unknown keys, naming the key and its line, so a manifest still carrying
the flag fails to load:

```text
decode manifest: line 4: unknown field "skip_if_satisfied": invalid manifest
```

No deprecation scaffolding needed. The removal touches 46 references across 15 files. The
load-bearing ones:

| Site | What |
|---|---|
| `internal/ddl/manifest.go:204,258,271` | field, tag, assignment |
| **`internal/ddl/render.go:36-38`** | **`MarshalManifest` *writes* the key — deleting the field is a compile error.** Replaced by the `intent` `addPair`. |
| `internal/run/skip.go:45,47` | the `enabled` param and its doc |
| `internal/run/engine.go:480` | call site |
| `internal/run/engine.go:161,209,476,758,812,817,1038` | comments; **758** names the flag among settings a recovery run "must still honor" |
| `internal/report/history.go:21` | `RunRecord.Skipped` is *defined* as "(skip_if_satisfied)" — §6 |
| `internal/mssql/indexes.go:71` | comment |

Tests migrate to `intent: compression` rather than being deleted:
`internal/run/skip_engine_test.go:26-89` (fixture + 3 tests, one of which `strings.Replace`s the
flag out), `internal/run/resume_test.go:277` (**guards FIXES #6 — must survive**),
`internal/ddl/manifest_test.go:114-131`, `internal/ddl/render_test.go:20,64`.

Docs to rewrite: `README.md:73,92` (§7); `specs/crash-resumable.md:4,252`, which **normatively
defines the flag and its default-off rationale**; `specs/graceful-stop.md:21`;
`specs/FIXES.md:28,115-117,137,158,417` (#6 is defined by the flag, #11 by `RunRecord.Skipped`);
`docs/superpowers/plans/2026-07-03-maintenance-window.md:992`.

## 5. Migration

No manifest uses the flag — verified across all 13 that parse in the queue (`030`/`031` at 74
operations, `032` at 33, `.033` at 38, `032`'s recovery at 21, two shrinks, two examples).
`02.processing/` is empty; nothing is in flight. Removing the flag breaks nothing that exists.

Adding `intent: compression` to the `03X` campaign manifests is an improvement — it makes a
re-run skip what is already PAGE — not a repair.

## 6. Consequences

- **`RunRecord.Skipped` changes population, not shape.** `history.go:21` defines it as
  skip_if_satisfied skips (excluding resume-cursor skips via `resumeSkipReason`,
  `engine.go:812`; counted at `engine.go:1038`). It becomes "compression-intent skips" — same
  column, different meaning, no migration. `specs/FIXES.md:137` (#11) is already open on this
  field; reconcile there.
- **`planFingerprint` ignores intent** (`engine.go:1171`: `CommandType` + target only). Editing
  `intent:` on an interrupted manifest does not invalidate the resume cursor
  (`reconcileResumePlan`, `engine.go:1140`), so operations already skipped as satisfied stay
  skipped. Consistent with a target-only fingerprint, but it means the fix does not retroactively
  reclaim skipped work: an operator adding `intent: fragmentation` mid-campaign must clear the
  sidecar to re-run those operations.
- **`intent: fragmentation` is inert on a frag-only rebuild** — that operation has
  `DataCompression == ""` and already exits `skipSatisfied` at `skip.go:52`. The field only
  changes behavior for the dual-motive case (§1). It is still worth writing: it records the
  motive, and it is what makes a hand-written manifest safe.
- **The pure-defrag hole stands.** A `rebuild_index` with no `data_compression` has no target
  state, so nothing can be skipped for it — a re-run always redoes the work. A run-time
  fragmentation re-probe would be the fix (`FIXES.md` #28).

## 7. Documentation

`README.md:88-93` is a paragraph of operational advice built on the flag ("pair a windowed
`continue` manifest with `skip_if_satisfied: true` so the already-done operations after the gap
collapse to a catalog read"). It is **rewritten** around `intent: compression`, not deleted — the
advice stays true, the mechanism changes.

`README.md:73`'s "`skip_if_satisfied` (below)" is a dangling forward reference: the
manifest-format section (`README.md:184-227`) documents `description`, `database`, `on_failure`,
`ignore_blocked_sessions` and `operations`, and never documented the flag. Document `intent`
there.

## 8. Testing

Pure, no database.

- **`internal/ddl`** — each intent value parses; an unknown value is rejected naming it;
  `MarshalManifest`→`ParseManifest` round-trips both `Manifest.Intent` and the operation field
  (extend `render_test.go:15-52`); unset intent is absent from rendered YAML; `ExpandRebuildAll`
  carries intent through `index: ALL`.
- **`internal/maint`** — via the exported `DecideIndex` (`decide.go:179`; `decideIndex` is
  unexported and every test package here is external): `IntentFragmentation` when
  `frag >= rebuild_from_percent`, **including the dual-motive case**; `IntentCompression` when
  the rebuild is compression-only. Table-driven over the ladder boundaries (below floor, <5,
  5–30, ≥30), asserted through `OperationsByCategory("index")` as `decide_test.go:445` does.
- **`internal/run`** — through `ProcessAll` with `fakeCompression` + `WithCompressionReader`
  (`skip_engine_test.go:21`): compression-intent + satisfied → skip; **fragmentation-intent +
  satisfied → runs** (the regression this spec exists to prevent); unset + satisfied → runs;
  the manifest default applies to an operation with no intent; an operation's own intent beats
  the default; compression-intent + unsatisfied → runs; read error → runs; the
  `ownsPausedResumable` carve-out still wins (`resume_test.go:277`).
