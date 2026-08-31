# Transient-Maintenance-Blocker Recognition — Design Spec

**Date:** 2026-07-31
**Status:** Design (follow-up to the tail-object-identification feature)
**Scope:** small — one classification helper, one narrow read wired into the
shrink driver, one new `confirmed_by` value, and clear operator messaging.
**Revision:** 2026-07-31 — resolves the six points from the Kimi assessment
(`…-kimi.md`): separate warning guard (§3), `transient_maintenance` filtered in
`confirmedSetFor` not surfaced as a `Confirmation` (§5), object-name source made
explicit (§4), "maintBlock set" wording (§6), the no-tail-object case stated (§5),
and the allow-list narrowed to verified `command` verbs (§1).

## Motivation

When a data-file shrink runs while a concurrent **index-maintenance** operation
(`ALTER INDEX … REBUILD` / `REORGANIZE`, legacy `DBCC INDEXDEFRAG`) holds locks
on the object at the file tail, the shrink cannot relocate those pages. Today the
driver reacts purely on impact/progress: it stalls, backs off, and gives up
cleanly with work preserved — which is correct. But two things are wrong at the
edges:

1. **False confirmed blocker.** On give-up, `captureGiveUpTail` records the tail
   object into `<manifest>.contended.yaml` as `confirmed_by: tail_position`. If
   that object was merely being rebuilt at the same time, it is **not** a
   structural tail blocker — it was transiently locked. `plan --confirmed` then
   recommends a pointless pre-shrink reorganize of an object whose only sin was
   overlapping a maintenance window.
2. **Opaque operator experience.** The `.log` and TUI show generic "no further
   progress (work preserved)" with no hint that the cause was another session's
   maintenance — the single most actionable fact ("re-run after the rebuild
   finishes").

This follow-up teaches the shrink to **recognize** that its blocker is a
transient maintenance operation and to say so clearly, without changing the
reaction model (still bounded, still yields, still re-queues — never waits
indefinitely, never kills the blocker).

## Non-goals (deliberately out of scope)

- **No unbounded in-process wait.** The shrink does not park itself until the
  maintenance clears. A REBUILD can run for hours and a RESUMABLE rebuild can sit
  PAUSED for days; an open-ended wait would pin the manifest in `02.processing/`
  and a held connection with no termination guarantee. The existing
  stop-with-work-preserved + re-queue (and `deferredByWindow`) remains the
  "retry when the path is clear" mechanism.
- **No killing the blocker.** The shrink stays the yielding party.
- **No new blocker taxonomy beyond maintenance.** We do not try to classify
  application transactions, ETL, bulk loads, or reporting queries as
  "wait-for-me". Only a conservative allow-list of maintenance commands is
  recognized; everything else keeps today's behavior verbatim.

## Design

### 1. Classification helper (pure)

Add a pure predicate over the blocker's `command` verb from
`sys.dm_exec_requests` (already surfaced as `mssql.Session.Command`).

```go
// internal/mssql/analysis.go (or a small maint helper — pure, table-driven)

// IsMaintenanceCommand reports whether cmd (dm_exec_requests.command) is a known
// index-maintenance / file-compaction operation the shrink treats as a transient,
// self-clearing blocker rather than a structural tail blocker. Conservative
// allow-list; case-insensitive; unknown verbs return false (today's behavior).
func IsMaintenanceCommand(cmd string) bool
```

Recognized verbs (case-insensitive, trimmed):

| `command` value | Covers                                                        |
|-----------------|---------------------------------------------------------------|
| `ALTER INDEX`   | `ALTER INDEX … REBUILD` / `… REORGANIZE`                      |
| `DBCC`          | `DBCC INDEXDEFRAG`, and a concurrent `DBCC SHRINKFILE`/`SHRINKDATABASE` |

Verified against the `sys.dm_exec_requests` reference: `command` is `nvarchar(32)`
holding a **command verb**, and every DBCC statement (including `SHRINKFILE`,
`SHRINKDATABASE`, `INDEXDEFRAG`) reports the verb `DBCC`. The earlier draft's
`DbccFilesCompact` was wrong — that string is an internal wait/task name, never a
`command` value — so it is dropped; `DBCC` already subsumes a concurrent shrink.
`ALTER INDEX` is the verb for both REBUILD and REORGANIZE; `command` does not
distinguish them, which is fine — the operator message names the *object* and the
raw verb, and both are transient for our purposes. The list is intentionally short
and lives in one place so it is trivial to extend.

`DBCC` also matches read-only checks (`DBCC CHECKDB`, which runs against a
snapshot and rarely blocks a shrink); classifying those as transient is harmless —
they are transient too, and yield-then-re-run is the right response regardless.

### 2. Observe the self-blocker (narrow read, no new SQL)

`mssql.FindSelfBlock(sessions, ourSPID)` already converts an `ActiveSessions`
snapshot into the direct blocker's SPID, wait type, and identity. The shrink
driver does not read `ActiveSessions` today, so:

- Extend the `ShrinkReader` interface with `ActiveSessions(ctx) ([]mssql.Session,
  error)` — `*mssql.Conn` already implements it (used by the engine's blocker
  path), so production wiring is free; fakes gain one method.
- The blocker's `command` is read from the same snapshot: find the session whose
  `SPID == selfBlock.SPID` and take its `Command`. (Extend `SelfBlock` with a
  `Command` field, filled alongside `Login`/`Program` in `FindSelfBlock`, so
  callers do not re-scan the slice.)

Caveat (acceptable): `activeSessionsSQL` inner-joins `dm_exec_requests`, so a
blocker that is **idle-in-transaction** (holding locks but not executing a
request) has no `command` and will not appear as a maintenance blocker — it falls
through to today's behavior. That is correct: an idle session is not a running
maintenance op, and a running REBUILD/REORGANIZE/DBCC always has a live request
row, so the case we target is covered.

### 3. When we sample it

Sample the self-block **on the stall backoff cadence** — the loop is already
sleeping there, so this adds one lightweight `ActiveSessions` read per backoff,
not a hot-path cost. Concretely, inside the no-gain / DBCC-error stall path
(`shrink.go` `stall` → callers), when `noProgress` first crosses a small
threshold (e.g. ≥ 2, so a one-off blip is not mislabeled):

1. Read `ActiveSessions`, compute `FindSelfBlock`.
2. If `Blocked && IsMaintenanceCommand(sb.Command)` → we have a **transient
   maintenance block**. Record it on the operation's `tailProbe` (a new
   `maintBlock *MaintBlock` field: blocker SPID, command verb, and
   first-observed timestamp; see §4 on the object name).
3. Emit the clear message (§4) **once per operation** so the operator learns
   *why* early, not only at give-up.

**Warning guard — do not reuse `tailProbe.warned`.** `warned` guards the
"tail-object identification needs SQL 2019+" message; reusing it would let one
warning suppress the other. Add a separate `maintWarned *bool` on `tailProbe` so
the two once-per-operation messages fire independently.

Best-effort throughout: a nil reader, a read error, or a blocker missing from the
snapshot degrades silently to today's behavior.

### 4. Clear operator messaging — log AND TUI (primary deliverable)

Both surfaces are driven by the same `ReactionSink`, so one well-worded event
reaches the `.log` run report and the TUI incident console. The wording must make
the transient-maintenance nature unmistakable and state the recommended action.

- **First recognition (while still trying), once per operation:**
  `Kind: "warn"`, e.g.
  `shrink of "PRODDB" is blocked by a concurrent maintenance operation —
  ALTER INDEX on session 104 (waiting 12m31s). Transient; SqlGoPace is yielding,
  not forcing. Re-run after maintenance completes.`
  (The runner message is **file-scoped** — `shrink of "<file>"` — matching every
  other `ShrinkRunner` message, which name the file, not the database; the runner
  is not passed a database name. The engine's narration supplies the manifest/database
  context around the event.)

- **At give-up attributable to maintenance:** the give-up reason string names it
  explicitly instead of the generic "no further progress":
  `stopped: file tail pinned by concurrent maintenance (ALTER INDEX, session 104)
  — transient, not a structural blocker; work preserved, re-run after
  maintenance.`

Message content requirements:
- name the **operation verb** (from `command`) and the **blocker SPID**;
- include the **elapsed wait** so the operator sees how long the shrink has
  already yielded (from `SelfBlock.WaitMS`, or the time since first observation);
- name the **object** at the tail **only when one is already available** — i.e. a
  `TailFinding` from the proactive walk or an earlier reactive walk. At the first
  `noProgress ≥ 2` stall the reactive tail walk has not run yet, so the message is
  driven by the **file name** (as in the example above), never by parsing the
  blocker's `ActiveQuery` (too fragile). The object name is added to the give-up
  message and the sidecar entry once a walk has produced it;
- state it is **transient** and that SqlGoPace is **yielding, not killing**;
- give the **action**: re-run after maintenance (or schedule via the maintenance
  window).

Implementation note: confirm the TUI renders the chosen `Kind` distinctly (it
already handles `warn`/`info`/`pause`/`cancel`/`kill`); if a dedicated visual
treatment is wanted, add a `deferred`/`maintenance` kind rather than overloading
`warn`. Keep the `.log` line and the TUI line semantically identical.

### 5. Sidecar behavior — do not mislead `plan --confirmed`

When the give-up is attributable to a recognized maintenance blocker, **do not
record the tail object as a confirmed structural blocker.** Two options; the spec
chooses (a):

- **(a) Record it with a distinct, non-confirming kind (chosen).** Add
  `ConfirmedBy: "transient_maintenance"` to the `confirmed_by` enum. The entry is
  still written to `.contended.yaml` (so the operator sees a durable record with
  the blocker command and timestamp), but it never drives a pre-shrink
  reorganize. This preserves the audit trail without the false recommendation.
- (b) Omit the entry entirely. Rejected: loses the durable "why did this shrink
  stop" record that makes the sidecar useful post-mortem.

**Where the filtering happens.** `confirmedSetFor` (`cmd/sqlgopace/shrink_plan.go`)
is the single place that interprets `ConfirmedBy` into a `maint.Confirmation`, and
`DecidePreShrink` is the single consumer. Rather than add a
`Confirmation.TransientMaintenance` flag that `DecidePreShrink` must then remember
to skip, **filter `transient_maintenance` entries out in `confirmedSetFor`** so
they never become a `Confirmation` at all. This keeps `DecidePreShrink` unchanged
and puts the "informational only" decision at the one interpretation point. The
entry remains in the YAML for the operator; it is simply absent from the confirmed
set the planner acts on.

**When there is no object to record (SQL < 2019, or the tail walk fails).** The
`.contended.yaml` sidecar is keyed by object; with no `TailFinding` there is no
entry to write. That is acceptable: the transient-maintenance fact still reaches
the operator through the **`.log` and TUI messages** (§4), which are
version-independent. Only the *durable sidecar record* requires a resolved tail
object; its absence loses no reaction correctness.

`ContendedObject` gains an optional `BlockedByCommand string
\`yaml:"blocked_by_command,omitempty"\`` and `BlockedBySPID int
\`yaml:"blocked_by_spid,omitempty"\`` so the transient record carries the cause.
The header comment in `renderContended` documents the third `confirmed_by` value:

```
#   confirmed_by: transient_maintenance — the file tail was pinned by a concurrent
#                                 maintenance op (e.g. ALTER INDEX) at capture time.
#                                 Informational only — NOT fed to a pre-shrink reorg.
```

### 6. Interaction with the existing proactive/reactive tail walk

- The **reactive give-up walk** (`captureGiveUpTail`): before recording a tail as
  `tail_position`, check whether `tp.maintBlock` **is set**. Give-up follows
  immediately from the stall that set it, so no timestamp-freshness check is
  needed — a set `maintBlock` means "this give-up was under maintenance blocking".
  If set, record `transient_maintenance` (with the blocker command) instead of
  `tail_position`.
- The **proactive walk** stash (recorded post-loop in `shrinkData` only on a
  missed target): same guard — if `tp.maintBlock` is set, downgrade the recorded
  kind to `transient_maintenance`.
- A give-up **not** attributable to maintenance (application lock, WALP timeout,
  genuinely pinned heap/LOB) keeps `confirmed_by: tail_position` exactly as today.

## Data flow (summary)

```
stall (no-gain / DBCC error, noProgress ≥ threshold)
  → reader.ActiveSessions()  → mssql.FindSelfBlock(ours)
    → IsMaintenanceCommand(sb.Command)?
        yes → tailProbe.maintBlock = {spid, command, since}
              emit clear "blocked by maintenance" warn (once/op)   → .log + TUI
        no  → unchanged
give-up
  → if maintBlock current: emitTail/record confirmed_by=transient_maintenance
                           give-up reason names the maintenance op   → .log + TUI
    else:                  record confirmed_by=tail_position (today)
plan --confirmed
  → confirmedSetFor drops confirmed_by=transient_maintenance (never a Confirmation)
  → DecidePreShrink unchanged
```

## Testing

Pure, no database — extend the existing shrink/contended unit tests:

- `IsMaintenanceCommand`: table test over `ALTER INDEX`, `DBCC` (and
  lower/mixed case, leading/trailing space) → true; non-maintenance verbs
  (`SELECT`, `INSERT`, `BACKUP DATABASE`, `""`, and the internal wait-name
  `DbccFilesCompact`, which must NOT be treated as a command verb) → false.
- `FindSelfBlock` fills `Command` from the blocker row.
- Shrink give-up with a fake `ActiveSessions` where our SPID is blocked by an
  `ALTER INDEX` session → sidecar entry is `transient_maintenance`, carries
  `blocked_by_command`/`blocked_by_spid`, and a `warn` event whose detail names
  the verb, SPID, and elapsed wait was emitted to the sink.
- The tail-2019+ warning and the maintenance-block warning can both fire in one
  operation (separate guards) — neither suppresses the other.
- Same setup but blocker command `UPDATE` (application) → still
  `tail_position` (unchanged behavior).
- `confirmedSetFor` drops a `transient_maintenance` entry (it never becomes a
  `Confirmation`) while still mapping a sibling `tail_position`/`lock` entry, so
  `DecidePreShrink` recommends a pre-shrink reorg for the sibling only.
- Round-trip: `renderContended` → `ParseContended` accepts the new fields and the
  third `confirmed_by` value (guards format drift).

## Docs

- `docs/specs/SHRINK.md`: add a short "Concurrency with index maintenance" subsection
  describing the yield-and-recognize behavior and the `transient_maintenance`
  sidecar kind.
- `README.md`: one line under the shrink/contended-capture description noting that
  a shrink blocked by concurrent maintenance is reported as transient and not fed
  to `plan --confirmed`.

## Rollout / compatibility

- Additive only: new `confirmed_by` value and two optional YAML fields; legacy
  sidecars (no field / `lock` / `tail_position`) parse unchanged.
- Servers < 2019 (no tail walk) still get the **message** — self-block
  classification uses `ActiveSessions`, which needs only `VIEW SERVER STATE`,
  already mandatory. Only the tail-object *recording* is 2019+; the transient
  give-up reason and warn line are version-independent.
