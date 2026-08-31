# Assessment: Transient-Maintenance-Blocker Recognition Design

Date: 2026-07-31
Status: Assessment of design spec `2026-07-31-transient-maintenance-blocker-design.md`

## Overall verdict

Sound, well-scoped, and feasible. The spec correctly diagnoses the two edge problems (false `confirmed_by: tail_position` entries and opaque messaging) and proposes a minimal, conservative fix that stays inside the existing reaction model. With a few clarifications, it is ready to implement.

## What works well

- Narrow scope. It touches exactly one classification helper, one interface extension, one new `confirmed_by` value, and messaging. It does not change the reaction hierarchy or timing.
- Conservative allow-list. `ALTER INDEX`, `DBCC`, `DbccFilesCompact` is short and deliberately restrictive. Defaulting to "not maintenance" preserves today’s behavior for application locks, ETL, and other workloads.
- No unbounded wait and no killing. The spec explicitly keeps the bounded backoff and clean give-up, avoiding the risk of pinning a manifest in `02.processing/`.
- Audit trail without false recommendation. Option (a) keeps a durable record of why the shrink stopped while preventing `plan --confirmed` from scheduling a pointless reorganize.
- Version-independent messaging. The classification only needs `ActiveSessions` / `VIEW SERVER STATE`; only the tail-object recording stays SQL Server 2019+.

## Feasibility against the existing code

The proposed changes map cleanly onto the current codebase:

- `mssql.Session.Command` is already populated by `ActiveSessions` (`internal/mssql/dmv.go`), so extending `SelfBlock` with `Command` is a one-field change.
- `*mssql.Conn` already implements `ActiveSessions`, so adding it to `ShrinkReader` is free in production; only test fakes need the method.
- `tailProbe` is the right place for per-operation state (`maintBlock` plus a once-per-operation warning guard).
- The no-gain / DBCC-error path already counts `noProgress` and calls `stall`, so the sampling hook fits at `noProgress >= 2`.
- `captureGiveUpTail` and the proactive `shrinkData` post-loop already decide when to record `tail_position`; they can check `tp.maintBlock` before downgrading to `transient_maintenance`.
- `ContendedObject` already accepts new fields via `yaml.Marshal`, and `ParseContended` uses `KnownFields(true)`, so adding `blocked_by_command` / `blocked_by_spid` is straightforward as long as the struct is updated.
- `confirmedSetFor` (`cmd/sqlgopace/shrink_plan.go`) is the single place where `ConfirmedBy` is interpreted; `DecidePreShrink` is the single consumer.

## Concerns and ambiguities to resolve before implementation

1. Warning guard: do not reuse `tailProbe.warned`.
   `warned` today guards the "tail-object identification needs SQL Server 2019+" warning. Reusing it for the maintenance-block warning would suppress one of the two messages. Add a separate `maintWarned *bool` (or equivalent) so both warnings can fire independently once per operation.

2. How `Confirmation` represents `transient_maintenance`.
   `confirmedSetFor` currently maps `ConfirmedBy == "tail_position"` to `Confirmation.ByTail`. For `transient_maintenance` you need either:
   - a new `Confirmation.TransientMaintenance bool` that `DecidePreShrink` checks and skips, or
   - filtering those entries out entirely in `confirmedSetFor` so they never reach `DecidePreShrink`.

   The spec says "DecidePreShrink skips", which favors the explicit flag. Either works, but pick one and make it consistent.

3. "Object name if resolvable" needs a defined source.
   The spec proposes recording the tail object name in `MaintBlock`, but at the first `noProgress >= 2` stall the reactive tail walk has not run yet and the proactive walk may not have found anything. The blocker’s `ActiveQuery` is too fragile to parse. I recommend making the object name truly optional and driving the message from the database/file name (as in the spec’s example wording), with the tail object name added only when a `TailFinding` is already available.

4. What "current" means for `maintBlock` at give-up.
   The spec says "if maintBlock current". Since give-up follows immediately after the stall that set it, it will always be current. Clarify the wording to "if `tp.maintBlock` is set" to avoid implying a timestamp freshness check.

5. Recording `transient_maintenance` when no tail object exists.
   On SQL Server < 2019 or when the tail walk fails, there is no object to record. The sidecar can only carry the message in the `.log`; no `contended.yaml` entry is possible. This is acceptable, but worth stating explicitly.

6. Verify the `DbccFilesCompact` command verb.
   The spec lists `DbccFilesCompact` as a recognized `dm_exec_requests.command` value. This should be confirmed against a real concurrent `DBCC SHRINKFILE` / `SHRINKDATABASE` before relying on it; otherwise keep the list to `ALTER INDEX` and `DBCC` initially.

## Minor recommendations

- Keep the `confirmed_by` enum values lowercase and consistent with the existing `lock` / `tail_position` style: `transient_maintenance`.
- Update the `contendedHeader` comment in `internal/run/contended.go` to document the third kind exactly as drafted in the spec.
- Add the round-trip test (`renderContended` → `ParseContended`) to `internal/run/contended_test.go` alongside the existing format-drift guard.
- When emitting the transient-maintenance `warn` event, include the elapsed wait time so the operator sees how long the shrink has already yielded.

## Conclusion

Approve the design, subject to resolving the six points above. The implementation should be small, testable without a database, and should not change any existing non-shrink behavior.
