# Spec review: TEMPDB-SHRINK.md

Status: issues found. The spec is solid overall, but a few integration points and defaults are under-specified and would trip up implementation planning.

## Issues (would affect planning)

1. Routing `ddl.ShrinkTempdb` into the existing `ShrinkDriver` is unspecified.

   The current `ShrinkDriver` interface is typed to `ddl.Shrink`:

   ```go
   Run(ctx context.Context, op ddl.Shrink, res ddl.ResolvedOptions, ...)
   ```

   The spec states `processOne` routes `ddl.ShrinkTempdb` to the same runner, but never says how the runner accepts the new type. Options include changing the interface to an `any`/union, adding a second method, or translating `ShrinkTempdb` into a `ddl.Shrink` with a sentinel `Type`. Without this decision, `internal/run/engine.go` cannot be planned.

2. The two-phase TRUNCATEONLY order is more than a localized branch.

   The existing `shrinkData` runs TRUNCATEONLY then chunks per file. The spec wants TRUNCATEONLY on all tempdb data files first, then the chunk loop across all files. That is a reordering of the outer loop, not a branch inside the per-file method. Calling it "localized branches" understates the change. The spec should either show the tempdb control flow explicitly or introduce a separate helper that reuses chunk primitives.

3. `NoProgressBeforeFlush` default is missing.

   Section 5 introduces `NoProgressBeforeFlush` as the trigger for the cache flush, but no default value or range is given. Section 7 only says `internal/config` gets defaults. A planner cannot size test cases or tune behavior without a starting value.

4. `ShrinkTempdb.Target()` shape is not explicit.

   The spec warns not to abuse `ObjectRef.table`, but does not say what `Target()` returns. `ObjectRef{Database: "tempdb"}` matches the `check_db` convention; `ObjectRef{Name: "tempdb"}` matches the `shrink` file convention. The preflight and existence-check paths depend on this choice, so it needs to be explicit.

5. Unbalanced-files warning threshold is undefined.

   Section 6 says the report warns when files "do not all end at the same size", but does not say whether any byte of difference triggers it or if a tolerance is allowed. File sizes are reported in whole MB, so exact equality is practical, but the spec should state it.

6. `flushcaches` versus `aggressive` interaction is ambiguous.

   Section 5 says `flushcaches: true` enables the targeted temp-object cache flush. It then mentions a config-level `aggressive` flag that may widen to `('ALL')`. It is unclear whether `aggressive` requires `flushcaches: true` or is independent, and whether `aggressive` is in v1 or Phase 2.

## Recommendations (advisory)

- Add a short "Wiring the runner" subsection under §7 that shows the chosen interface change.
- Either detail the tempdb two-phase loop or extract it into a dedicated helper that calls the existing chunk primitives; this makes the "no duplication" claim verifiable.
- Define `NoProgressBeforeFlush` default (for example, `3` no-progress events) in §5 and repeat it in §7.3.
- Explicitly state `func (o ShrinkTempdb) Target() ObjectRef { return ObjectRef{Database: "tempdb"} }` or equivalent.
- Decide whether `aggressive` belongs in v1; if yes, specify its relationship to `flushcaches`.
- Consider documenting how the existing log-pressure sampler interacts with tempdb shrinks (tempdb is SIMPLE, but each chunk still generates log).

## What is strong

- The incident grounding in the Problem section makes the non-goals defensible.
- Goals and non-goals are sharply separated; the "not a monitor" boundary is especially important.
- Reusing `ShrinkRunner` and the reaction hierarchy is the right architectural choice.
- The safety posture is clear: never KILL blockers, WALP only with `SELF`, clean give-up, work preserved.
- Limits are honest and well-explained.
- Test coverage is concrete and proportionate.

## Verdict

Approve for planning after the six issues above are clarified.
