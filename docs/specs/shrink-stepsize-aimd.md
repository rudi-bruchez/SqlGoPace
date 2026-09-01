# Shrink stepsize controller — fixing the one-way ratchet

Status: **IMPLEMENTED** 2026-09-01, v0.17.0. Kept as the reasoning record; the living design doc is
now [SHRINK.md](SHRINK.md) §7.2 (see its *Superseded* block). Two departures from what is written
below: the knob was not only re-defaulted but **renamed** `target_batch_seconds` →
`max_chunk_seconds` (§7, so an old config fails loudly at load instead of silently keeping the old
behaviour), and the §4 claim of "no ratchet" was corrected to a *bounded* ratchet before coding —
a reduction is recoverable only while the chunk still fits inside the ceiling. §8's follow-ups moved
to [TODO.md](TODO.md), which is where they will actually be seen.
Anchored against `cc6cb05` (v0.16.0), i.e. after the `specs/` → `docs/specs/` move. Every line
citation below was re-verified against that tree; re-check them if the file drifts.
Scope: `run.AdjustStepMB` and its call site. The settled rules were folded into
[SHRINK.md](SHRINK.md) §7.2/§7.3, which is the source of truth; this document is kept for the
diagnosis and the alternatives that were weighed and rejected, which a spec does not carry.

## 1. Observed symptom

On a live multi-TB data shrink (`PRODDB`, `target_chunks: 1000`, `max_step_mb: 8192`), the
chunk size starts at 8 GB and decreases monotonically toward `min_step_mb`. Wall-clock
throughput degrades as it does: `DBCC SHRINKFILE` has a largely fixed per-invocation cost (it
restarts the end-of-file page walk on every call), and SqlGoPace adds a tail-object walk, two
`SessionWaits` reads and a `FileSizeMB` read between chunks. Going from 8192 MB to 50 MB is
160x more invocations for the same reclaimed volume, so the fixed cost comes to dominate the
useful work. The operator's reading — "it went faster with 8 GB chunks" — is correct.

## 2. Diagnosis

`internal/run/shrink_calc.go:153` (`AdjustStepMB`):

```go
reduce := w.WriteLogAvgMs > writeLogReduceMs ||         // 10 ms
          w.PageIOLatchExAvgMs > pageIOLatchReduceMs || // 20 ms
          w.BlockingSeconds > blockingReduceSeconds     // 30 s
grow   := w.WriteLogAvgMs < ioLatchGrowMs &&            //  5 ms
          w.PageIOLatchExAvgMs < ioLatchGrowMs &&       //  5 ms
          w.BlockingSeconds == 0 &&
          elapsed < t.TargetBatch                       //  5 s
```

### D1 — A reduction is irreversible once chunks are longer than `TargetBatch`

`grow` requires `elapsed < t.TargetBatch`. With `target_batch_seconds: 5` and chunks measured
in gigabytes, that condition is unsatisfiable by construction: a multi-GB `SHRINKFILE` chunk
takes minutes. So the step can fall but never climb back.

This is the load-bearing defect. It is not that the step halves often — it is that **every
halving is permanent**, so pressure events accumulate monotonically over a run lasting hours.
Two paths halve the step:

- `AdjustStepMB`, on any chunk where WRITELOG avg > 10 ms or PAGEIOLATCH_EX avg > 20 ms;
- `shrink.go:506`, explicitly, on any chunk that errored (Msg 3140, latch timeout, WALP timeout).

A `DBCC SHRINKFILE` chunk *is* a WRITELOG and PAGEIOLATCH_EX generator — it reads pages at the
end of the file and rewrites them elsewhere, logging every move. On a busy production instance,
exceeding those thresholds is the normal state, not an anomaly. So halvings are frequent, and
none of them is ever undone.

### D2 — A dead band between the grow ceiling and the reduce floor

Growth requires latency < 5 ms; reduction requires > 10 ms (WRITELOG) or > 20 ms
(PAGEIOLATCH_EX). Between those, the controller is frozen. A shrink sustaining a healthy 7 ms
WRITELOG can never grow its step, whatever the duration. Two thresholds per wait type express
one decision, and the gap between them is a trap.

### D3 — `BlockingSeconds` is a dead signal

`waitDeltas` (`shrink.go:1056`) only ever populates `WriteLogAvgMs` and `PageIOLatchExAvgMs`;
the comment at `shrink.go:1054` states the intent ("the blocking dimension is handled by the
pressure-stop path"). So `w.BlockingSeconds` is always 0 and `blockingReduceSeconds` is
unreachable in the shrink path. The controller therefore has **no** blocking input at all,
even though "we are hurting other sessions" is precisely the signal a maintenance tool should
back off on.

### D4 — `target_batch_seconds` rests on an assumption that no longer holds

[SHRINK.md](SHRINK.md) §7.3 justifies a 5-second target as "snappy reactions". That was true when
written; it is not true of the current driver. `runChunk` (`shrink.go:685`) runs the statement
under `supervise` with live sampling, `pumpServerProgress` re-emits server-side
`percent_complete` while the chunk runs, and `caps.MaxBlock` enforces `max_block_minutes`
*inside* the chunk. Reactions, progress and the blocking cap are all mid-chunk today. A long
chunk is neither blind nor unstoppable, and shrink work is preserved and re-entrant at any
point. The remaining cost of a long chunk is only the coarser granularity of the stepsize
feedback loop itself.

Consequence: the current defaults contradict each other by three orders of magnitude.
`target_chunks: 1000` sizes the first chunk by volume (8 GB after the `max_step_mb` clamp);
`target_batch_seconds: 5` then asks the regulator to converge on chunks of a few seconds.
1000 chunks x 5 s = 83 minutes total, which is not a plausible budget for a multi-TB reclaim.

## 3. Objective

For a shrink, chunk *duration* is not a goal in itself (D4). The goal is: **use the largest
step the storage and the concurrent workload tolerate**, bounded by `max_step_mb` and
`max_step_pct_of_file`. Latency and blocking are the constraint; duration is at most a ceiling
an operator may impose.

## 4. Proposed control law

AIMD (additive increase, multiplicative decrease), evaluated in order:

| # | Condition | Action |
|---|-----------|--------|
| R1 | `stopped` OR `WriteLogAvgMs > 10` OR `PageIOLatchExAvgMs > 20` | `step / 2` |
| R2 | `elapsed >= TargetBatch` | unchanged (hold) |
| R3 | otherwise | `step * 5 / 4` (at least +1 MB) |
| R4 | always | clamp to `[MinStepMB, maxStep]` |

New signature — `stopped` replaces the dead `BlockingSeconds` (D3) and is already computed by
`runChunk`, so nothing has to be plumbed through `supervise`:

```go
func AdjustStepMB(step int, elapsed time.Duration, w WaitDeltas, stopped bool,
                  t ShrinkTuning, maxStep int) int
```

`stopped == true` means the supervisor cut the chunk short — the blocking cap
(`max_block_minutes`), a non-ignored blocked session, or log pressure. That is the real "we are
hurting the server" signal, measured rather than assumed.

### Why this shape

- **Bounded ratchet, not no ratchet (bounds D1).** Be precise about what R3 buys, because the
  obvious overclaim is wrong. A reduction is recoverable only once the resulting chunk runs
  *shorter* than `TargetBatch`; above the ceiling, R2 holds and the reduction is permanent. That
  is deliberate — the ceiling means "no chunk longer than this" — but it means the descent stops
  at **the step whose duration is ≈ `TargetBatch`**, not at `min_step_mb` as it does today. The
  ratchet is bounded by a knob the operator sets, instead of running to the floor. This is the
  second, independent reason §7 matters: leave `target_batch_seconds` at 5 and that equilibrium
  is a small chunk again, reached by a different route.
- **Stability is quantifiable.** Recovering one halving takes `log2 / log1.25 ≈ 3.1` clean
  chunks. So the loop trends upward as long as pressure events are rarer than roughly one chunk
  in three, and settles below the pressure threshold otherwise. That ratio, not a hand-wave about
  "the right pace", is what makes `/2` against `*1.25` the right pair — and T2 pins it.
- **`ioLatchGrowMs` is deleted (fixes D2).** One threshold per wait type instead of two. Growth
  is the default when nothing is wrong, not a privilege earned by near-idle storage.
- **Asymmetric rates are deliberate.** `/2` down against `*1.25` up puts the equilibrium *below*
  the pressure threshold, answers a spike immediately, and recovers gradually. Climbing the full
  `min_step_mb` (50 MB) → `max_step_mb` (8 GB) range takes ~23 clean chunks, the right pace for
  a run measured in hours.
- **`TargetBatch` becomes a ceiling, not a target (addresses D4).** It no longer gates growth,
  it only stops it: an operator who genuinely wants short chunks still gets them by lowering the
  value, and one who does not is no longer punished for it.
- The explicit `step/2` on a chunk error (`shrink.go:506`) is unchanged. It stays multiplicative
  and now has a recovery path it did not have before.

### Alternatives rejected

- **Proportional control** (`ideal = step * TargetBatch / elapsed`, ratio clamped to `[0.5, 2]`).
  Given a `[TargetBatch/2, 2*TargetBatch]` dead band the clamp swallows the proportional term
  entirely — it degenerates to `*2` / `/2` while costing float arithmetic and a wider test
  surface. Complexity with no behavioural gain.
- **Symmetric duration targeting** (grow when fast, shrink when slow, converge on `TargetBatch`).
  Fixes the ratchet but keeps duration as the objective, so it still converges on small chunks —
  exactly the regime the operator reports as slower. Treats the symptom.
- **Wiring real `BlockingSeconds` through `supervise`.** `supervise` tracks `blockingStart` as a
  *current streak*, reset on every clear (`executor.go:236-252`), so a cumulative per-chunk total
  means changing its return type, which is shared with `MonitoredRunner`. `stopped` is a
  sufficient, already-available proxy. Kept as a follow-up, not needed here.

## 5. TDD plan

Order matters: T1 is the regression that reproduces the reported behaviour and must fail against
`main` before anything is changed.

### Step 1 — red

`internal/run/shrink_calc_test.go`:

These two must **override the fixture's `TargetBatch`** (`shrink_calc_test.go:18` sets 5 s). A
chunk duration at or above the ceiling makes R2 hold, so a recovery test written against the 5 s
fixture proves nothing: it would fail *with* the fix, not without it. Use `TargetBatch = 300s`
and `elapsed = 60s`, which is the post-§7 regime this change is actually for.

The `WriteLogAvgMs: 7` in the clean chunks is load-bearing: it sits in the D2 dead band
(above the 5 ms grow ceiling, below the 10 ms reduce floor), which is what freezes `main`. A
genuinely idle value like 2 ms would let `main` recover too and the regression would not bite.

- **T1 `recovers_after_a_pressure_event` (the reported bug).** `TargetBatch = 300s`. One chunk at
  `WriteLogAvgMs: 15, elapsed: 60s` (halves 400 → 200), then 10 chunks at `WriteLogAvgMs: 7,
  PageIOLatchExAvgMs: 8, elapsed: 60s`. New law: R3 each time → clamps at `MaxStepMB` (1024).
  Against `main`: `grow` needs latency < 5 ms, so it stays pinned at 200 forever. Assert > 200 —
  this single test states D1 and D2 together.
- **T2 `equilibrium_under_periodic_pressure`.** `TargetBatch = 300s`, starting at 400. Six cycles
  of [1 chunk at `WriteLogAvgMs: 15`, then 4 clean chunks at `WriteLogAvgMs: 7, elapsed: 60s`].
  Each cycle is `/2` then `*1.25^4 = 2.44`, a net gain, so the new law ends at `MaxStepMB`;
  `main` freezes on every clean chunk and walks 400 → `MinStepMB`. Assert the final step >= 400.
  This is the test that pins the ~3.1 stability ratio from §4 — it is the reason the increase
  factor cannot be lowered without re-deriving it.
- **T3 `grows_in_the_former_dead_band`.** `WriteLogAvgMs: 7, PageIOLatchExAvgMs: 15, elapsed: 1s`,
  step 400 → 500. Both values sit between the old grow ceiling and the reduce floor.
- **T4 `halves_when_the_chunk_was_stopped`.** `stopped: true` with perfect latency and
  `elapsed: 1s` → `step/2`. Growth must not win over a supervisor stop.
- **T5 `holds_at_the_duration_ceiling`.** Clean waits, `elapsed == TargetBatch` and
  `elapsed > TargetBatch` → unchanged.
- **T6 `halves_on_high_WRITELOG` / T7 `halves_on_high_PAGEIOLATCH_EX`.** Preserved from the
  existing table (lines 86-99), with the new `stopped` argument.
- **T8 `clamps_to_bounds`.** Growth stops at `maxStep`; reduction stops at `MinStepMB`; a step
  already at `maxStep` under clean conditions is a fixed point.
- **T9 `growth_always_advances`.** `step * 5 / 4` must not return `step` for any value in
  `[MinStepMB, maxStep]` — integer-truncation guard.
- **T12 `reduction_above_the_ceiling_is_permanent`.** `TargetBatch = 300s`, step 8192 at
  `elapsed: 600s`, clean waits → hold; then one pressure chunk → 4096 at `elapsed: 300s` → hold
  again. This documents the accepted limit of §4 (the ratchet is bounded, not removed) so that a
  later reader does not "fix" it back into unbounded growth.

`internal/run/shrink_test.go`, driver level:

- **T10 `step_sequence_is_not_monotonically_decreasing`.** A fake `ShrinkReader`/executor emitting
  constant benign waits over ~15 chunks; assert the `ShrinkProgress.StepMB` sequence emitted by
  `shrinkData` ends at or above where it started. This is the test that would have caught the
  production behaviour, since it exercises the loop rather than the pure function.

`internal/config/config_test.go`:

- **T11** default `TargetBatchSeconds` for `shrink` (see §7) — update lines 163, 176-177, 199-200.
  `BatchDMLConfig.TargetBatchSeconds` keeps its own default of 5 and its own test.

### Step 2 — green

1. `shrink_calc.go`: new `AdjustStepMB` per §4; drop the `ioLatchGrowMs` reference from it; add a
   documented `stepGrowNum/stepGrowDen = 5/4` constant next to the wait thresholds.
2. `shrink.go:569`: pass `stopped` at the call site. It is returned by `runChunk` (`:509`) and
   read again by the `awaitRelief` branch (`:531-547`), but never reassigned in between, so it
   can be passed straight through — no local needed. Add a test (T4) that pins this, since a
   future refactor of that branch could silently break it.
3. `shrink.go:1051-1056`: update the `waitDeltas` comment — the blocking dimension now reaches the
   controller through `stopped`, so `BlockingSeconds` is unused *by the shrink path*.

### Step 3 — refactor / verify

- `go test -race ./internal/run ./internal/config`
- `make vet`, then a `/simplify` pass over the diff before committing (CLAUDE.md convention).
- Bump `internal/version/VERSION` — this changes the behaviour of a production tuning loop.

## 6. Blast radius

| File | Change |
|------|--------|
| `internal/run/shrink_calc.go` | new law, new signature, `stepGrow*` constants |
| `internal/run/shrink.go` | call site (`:569`), `waitDeltas` comment (`:1052`) |
| `internal/run/shrink_calc_test.go` | T1–T9 |
| `internal/run/shrink_test.go` | T10 |
| `internal/config/config.go` | `setIf(&s.TargetBatchSeconds, ...)` at `:399` (§7) |
| `internal/config/config_test.go` | T11 |
| `config.yaml:77` | new default + new rationale for the knob |
| `docs/configuration.md:195` | new default + prose on the ceiling semantics and the migration note (the `target_batch_seconds` row in the table at `:220` belongs to `batch_dml` and stays at 5) |
| `docs/specs/SHRINK.md` §7.2, §7.3 | the adjustment rules and the defaults block |

**Do not touch `internal/run/batch_calc.go`.** `AdjustBatchRows` (`batch_calc.go:46`) shares
`writeLogReduceMs`, `pageIOLatchReduceMs`, `ioLatchGrowMs` and `blockingReduceSeconds` with the
shrink path and implements the identical law. Leaving `ioLatchGrowMs` and `blockingReduceSeconds`
declared keeps it compiling unchanged and keeps this diff reviewable. See §8.

## 7. Decision — SETTLED 2026-09-01: shrink default 5 → 300

`target_batch_seconds` for `shrink` currently defaults to **5**. Under the new law it is a
ceiling on growth, so leaving it at 5 caps every shrink at chunks of a few seconds and the
ratchet fix buys nothing in practice.

Recommendation: **raise the shrink default to 300** (5 minutes), and keep `batch_dml`'s at 5 —
there the duration objective is legitimate, since a DML batch holds locks for its whole duration
and has no fixed per-invocation restart cost.

This changes behaviour for anyone relying on the default. An operator who has explicitly set
`target_batch_seconds: 5` keeps small chunks and must be told to revisit it — worth a line in the
release note. If you would rather not move the default, the spec drops to a control-law-only
change: the ratchet is still fixed, but the equilibrium step stays small.

## 8. Out of scope / follow-ups

- **`AdjustBatchRows` has the same defects.** Same conjunction, same 5 ms dead band, same
  never-populated `BlockingSeconds` (`batch_dml_test.go:391` tests a case production cannot
  produce). It deserves the same treatment, in its own change, after this one has run against a
  real shrink.
- **Real cumulative blocking time per chunk** from `supervise`, if `stopped` proves too coarse.
- **`min_step_mb: 50`** is now less load-bearing, but its documented rationale should state the
  invariant explicitly: the floor must be large enough that the fixed per-chunk cost (shrink
  restart + tail walk + 3 DMV reads) stays a small fraction of chunk time.
- The interaction between `target_chunks` (volume-based initial step) and the new duration ceiling
  is not covered end to end; T10 exercises the loop but not a realistic multi-TB profile.
