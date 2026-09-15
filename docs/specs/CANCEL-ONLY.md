# Cancel-only operations — say so before the run

Status: design, revised after two adversarial reviews (2026-09-15). Target version 0.34.0.
Reviews: [REVIEW-CANCEL-ONLY-claude.md](REVIEW-CANCEL-ONLY-claude.md),
[REVIEW-CANCEL-ONLY-agy.md](REVIEW-CANCEL-ONLY-agy.md).

## Problem

The reaction hierarchy is `WAIT_AT_LOW_PRIORITY` → `RESUMABLE` pause/resume → cancel → `KILL`.
When an operation is not resumable — Standard edition, a version without `RESUMABLE`, an
operation type that has no resumable form, or resumable turned off by an override — the only
reaction left under pressure is cancel. For a heavy builder a cancel rolls back all the work
done so far, and the rollback holds the lock while it runs.

Nothing says so before the run. `--dry-run --explain` lists the options that were injected, and
on Standard without overrides no `resumable` decision is emitted at all (`pickBool` marks an
inapplicable option as not relevant), so the field `.log` of the campaign below lists only
`maxdop = 2` on each of its 33 operations. The run report explains each cancel on its own
(`operation canceled under pressure`) and never states the shared cause.

## Field evidence

Two run reports from one PAGE-compression campaign on SQL Server 2019 Standard (major 15,
SIMPLE recovery, ADR off, `blocking_timeout_minutes: 1`). Per-attempt durations are
reconstructed from the reaction timestamps and operation durations, to within about 20 s.

**33-operation manifest, `max_retry_attempts: 1`.** 44 cancels, all `blocking other sessions`.

| outcome | ops | cancels each | first attempt cancelled after |
|---|---|---|---|
| failed | 20 | 2 | 94–345 s |
| success after one cancel | 2 | 1 | 94 s and 165 s |
| success, no cancel | 11 | 0 | — |

The two operations the retry saved were cancelled after durations in the middle of the range of
those it did not save. **The length of the cancelled attempt does not predict whether the retry
succeeds**, and neither does the kind of pressure (every cancel here was blocking). The 20
doomed second attempts cost about 50 minutes of Sch-M plus rollback; the retry bought two
rebuilds.

**1-operation manifest, `max_retry_attempts: 3`.** Four blocking cancels, the first after about
35 minutes, then about 60 minutes each: roughly three hours, and no success.

## Decision: no change to the retry

Three retry rules were designed and rejected on this evidence:

- *No retry after a blocking cancel* (the first draft). It would have turned the two successes
  above into failures; the claim behind it — "a retry restarts from zero against the same
  writers" — is contradicted by 13 of 33 rebuilds completing in the same window.
- *Retry after a log cancel only once the log has drained.* The index operation's own log
  cannot be truncated until it completes, so when it is the operation that filled the log the
  retry generates the same volume again; and `waitForRelief` returns on the first sample under
  the cap, so a retry can start one percent below it. No field cancel was for log pressure.
  Note (harm review H1, `docs/specs/REVIEW-2026-09-15-harm.md`): the "cannot be truncated"
  argument holds for a *running* operation, not after its cancel and rollback — the attempt has
  then completed, and its log becomes truncatable at the next log backup (FULL) or checkpoint
  (SIMPLE). And "no field cancel was for log pressure" is true because the field campaign ran in
  SIMPLE recovery, where rebuilds are minimally logged; it is silent about FULL recovery, where
  this path matters. The decision stands as written — the retry itself is unchanged in 0.34.0 —
  because the immediate log sample added in `pumpSamples` (H1's fix) closes the blind window an
  immediate retry used to run through: a retry after a log-pressure cancel is re-canceled within
  seconds rather than writing into an over-cap log for up to `log_poll_seconds`.
- *Retry only when the cancelled attempt was short.* The table above refutes it.

What remains is a bet — two wins in 22 — whose cost depends on the campaign, so it stays the
operator's call through the existing key: `monitoring.max_retry_attempts` (`0` = never retry;
it also governs the cheap retries of `check_db` and `update_statistics`, and does not affect the
paced re-issue of `reorganize_index`). This design makes the cost of that bet visible instead
of deciding it.

## Definitions

An operation is **rollback-on-cancel** when it is one of the five heavy builders — `rebuild_index`,
`rebuild_heap`, `create_index`, `alter_column`, `add_constraint` (the set `cancelSafe`'s comment
in `internal/run/engine.go` names) — and its resolved options are not `Resumable`.

Deliberately excluded:

- the shrink, tempdb-shrink and batch-DML drivers, whose cancels keep committed work;
- `reorganize_index`, `check_db`, `update_statistics` (`cancelSafe`);
- `add_column`, `drop_column`, `drop_constraint`, `drop_index`: in the common metadata-only case
  their whole cost is the wait for Sch-M, and a cancel rolls back almost nothing. *Known gap:* a
  `drop_index` on a clustered index and a size-of-data `add_column` are expensive and are not
  flagged.

Online non-resumable operations (Enterprise `rebuild_heap` or `alter_column` ONLINE,
`create_index` ONLINE before 2019, `add_constraint` before 2022) are included: their cancel also
discards the work done.

The predicate is a pure function over `ddl.PlannedOperation`, in `internal/run` next to
`cancelSafe`, used by the dry-run renderer and the engine.

## Design

### 1. The dry run names the hazard and its real cause

`renderPlan` (`cmd/sqlgopace/main.go`) prints, under every rollback-on-cancel operation and
regardless of `--explain`, one comment line:

`--     reaction = cancel only (<cause>): a cancel under pressure rolls back all work`

where `<cause>` is, in order:

1. the `Reason` of the operation's `resumable` decision when one exists (for example
   `per-operation override`);
2. otherwise `resumable not supported by <tier> major <n>` when the matrix has a `resumable`
   entry for the command — which requires passing the `Target` (and matrix) to `renderPlan`;
3. otherwise `<command> has no RESUMABLE form`.

It is printed without `--explain` because it is a hazard, not the explanation of an option.

### 2. The run states it once per manifest

At manifest start, when at least one planned operation is rollback-on-cancel, the engine writes
one line to stdout and to the run report — not one per operation, which on an 800-operation
Standard manifest would be 800 identical lines:

`N of M operation(s) can only be cancelled under pressure; a cancel rolls back all their work and
is retried up to max_retry_attempts (K)`

### 3. The report names the shared cause

When at least one rollback-on-cancel operation was cancelled, the `.log` report gains one
summary line: how many were cancelled, how many of those still succeeded after a retry, and how
many failed. When the manifest runs with `on_failure: continue`, the line points at the
`<name>.recovery.yaml` written to `04.failed/`: it holds only the failed operations and is the
supported way to re-run them deliberately — for example under `--tui`, in a quieter window, or
with `max_retry_attempts: 0`.

## Documentation to supersede and update

- `docs/specs/SPECS.md` §9 says "after a cancellation, we wait until there is no more blocking /
  the log has dropped, then we retry". The code has never waited (`MonitoredRunner.Run` retries
  immediately), and the wait as written is vacuous for blocking: `BlockingOthers` only counts
  sessions blocked by our own session, which after a cancel is none. Supersede in place.
- `docs/specs/MAINTENANCE.md` (`rebuild_heap` row, "wait then cancel→KILL (and retry)"): same.
- `docs/configuration.md` (`max_retry_attempts`): state the field evidence and when `0` is the
  better setting.
- `docs/running.md`, `docs/blocking-and-kills.md`: the new manifest-start line and summary line.
- `docs/specs/TODO.md`: the "Standard edition has no reaction available" entry is **not** moved to
  Shipped — see below. Its claim that "a previous run's measured throughput is in the history DB"
  is false (`internal/report/history.go` stores one duration per manifest) and is corrected.
- `CHANGELOG.md` 0.34.0. No migration note: no default and no behaviour changes.

## Still open, deliberately

- **The first cancel itself.** The 2026-09-01 production harm review, finding 10 (SEVERE),
  proposes not cancelling a non-resumable, non-cancel-safe operation on blocking pressure alone,
  and letting `max_block_minutes` be the operator's explicit opt-in to paying for a cancel. This
  design does not change `DecideReaction`: every cancel in the table above still happens. It is
  deferred, not rejected — it is a change to the reaction hierarchy that needs its own design
  (what "hold and narrate" does to the sessions queued behind a Sch-M), and the narration here is
  what an operator needs whichever way it is decided. The TODO entry stays open for it.
- **Predicting that an operation will exceed `blocking_timeout_minutes`** needs per-operation
  throughput, which the history DB does not store (TODO, "The history DB is a run ledger").

## Testing

- The predicate: table over operation types × `Resumable`, including the four excluded DDL.
- `renderPlan`: the line and each of the three causes; absent for a resumable rebuild, a
  shrink, a batch DML, a reorganize, an `add_column`.
- Engine: the manifest-start line counts correctly and is absent when no operation qualifies;
  the summary line appears when, and only when, such an operation was cancelled, with the
  success/failure split and the recovery-manifest pointer under `on_failure: continue`.
- `MonitoredRunner.Run`'s bounded retry has no unit test today (no test sets `MaxRetries`). Add
  one pinning the current behaviour — immediate retry, error after `MaxRetries + 1` attempts —
  since this design deliberately keeps it.
