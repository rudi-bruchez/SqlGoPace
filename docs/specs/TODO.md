# TODO — the live backlog

What is worth doing next, and what has already been done so nobody proposes it again.

Two kinds of entry live here. **Iterations** are designed features with a spec of their own,
awaiting brainstorming then implementation. **Follow-ups** are deliberate scoping decisions taken
while shipping something else — small, known, and easy to lose. Each one records *why* it was
deferred, because that reasoning is what decides whether it is still the right call.

Keep this file honest: when work ships, move its entry to *Shipped* with the evidence rather than
deleting it. A backlog that lists finished work as pending is worse than no backlog — it invites
re-implementing what already exists.

Status last verified against the tree at v0.22.0 (2026-09-01).

## Before advertising this publicly — the production-safety gate

The tool is about to be offered to DBAs who will point it at their own production servers.
Until now its entire production track record is its author's. Two things gate that offer.

- [x] **README carries a beta warning.** Done 2026-09-01: a `status: beta` badge plus a
  warning block above the fold — take a backup you have tested *restoring*, rehearse on a
  copy, `--dry-run --explain` before every new manifest, read
  [blocking-and-kills.md](../blocking-and-kills.md) before arming any kill policy, and a
  note that `shrink` is slow, fragments indexes, and is rarely the right answer to a full
  disk.

- [x] **Adversarial production-danger review — done 2026-09-01.** 22 findings, four
  CATASTROPHIC. Full evidence in
  [2026-09-01-production-harm-review.md](2026-09-01-production-harm-review.md); it is a
  historical record and is not updated as items are fixed.
  **Verdict: not responsible to advertise as-is.** The theme is not code quality — the review
  is complimentary about that — it is that **the shipped defaults and the documentation
  disagree about what is armed, and several guards the docs name as protections do not
  protect.** A stranger calibrates their caution from the README, and that calibration is
  currently wrong in the dangerous direction.

### The eight that gate advertising

Ordered as the review ordered them. **1, 2, 3, 4 and 8 are done; three remain.** Item 7 is
hours; 5 and 6 are days.

- [x] **1. `kill_blockers.enabled: false`** — done in 0.19.0. `config.yaml:105` and
  `internal/scaffold/assets/config.yaml:105`. The Go zero value was always `false`, so only
  the shipped file disagreed with the five documents that called it off by default. A
  `config.yaml` scaffolded earlier is unchanged and may still be armed — the migration note
  in the CHANGELOG and in `docs/configuration.md` says so.
- [x] **2. `batch_update` can loop forever, committing** — done in 0.20.0. Both halves:
  `selfLimitClause` emits `[Col] IS NOT NULL` for a NULL target (`internal/ddl/batch.go`), and
  `runPredicate` stops at `predicateRowCeiling` (`internal/run/batch_calc.go`) — twice the
  table's row estimate, or 1,000,000 with no estimate — failing with `ErrRowCeiling` and the
  committed counts. A third defect surfaced while fixing it: `MarshalManifest` dropped a null
  `set:` value, so any in-place rewrite lost the column or broke the manifest.
  **Correction to the review's finding 1.** Its trigger A does not reproduce as written. It
  claimed `set: {Col: null}` rendered `SET [Col] = null` and looped; go-yaml does not call
  `Literal.UnmarshalYAML` for a `!!null` node decoded into a value type, so the literal stayed
  at its zero value and the statement rendered `SET [Col] = ` — a **syntax error that failed
  the operation on the first batch**. Real bug, wrong mechanism, and not CATASTROPHIC: it
  fails fast rather than corrupting. Trigger B (`set_raw` that does not consume its own
  filter) stands exactly as written and is the one that ran forever; the ceiling is what
  bounds it. Read finding 1's severity as belonging to trigger B alone.
  *Left undone:* the preflight `WARN` for a non-idempotent `set_raw` that
  `docs/specs/BATCH-DML.md` §4 promised and that was never implemented. It would catch trigger
  B before the first row is written rather than after the ceiling's worth — but it is a
  heuristic over raw SQL text where the ceiling is a proof, so it is an improvement on a
  closed hole rather than the closing of one. The spec now says so in place.
- [x] **3. Make the whole-table DELETE guard semantic** — done in 0.21.0.
  `ddl.BatchUnmatchedRowsSQL` counts the rows the filter would spare, capped at 1000;
  `preflight.CheckBatchDMLSelectivity` fails on zero, warns below the cap with the number,
  passes at it. Skipped when `confirm_full_table` is already set, so the confirmed path pays
  nothing. The `CASE WHEN (pred) THEN 1 ELSE 0 END = 0` wrapper is load-bearing: a plain
  `NOT (pred)` drops the rows where the predicate is UNKNOWN, which the DML spares too, so
  they would have been miscounted as matched.
  *Left undone:* the guard fires on **zero** spared rows, not on "essentially the whole
  table". A filter sparing one row of ten million passes with a warning naming the count.
  That is deliberate — zero is provable and needs no invented threshold, and the warning
  puts the number in front of the operator, who is the only one who can judge whether it is
  the number they expected. Revisit only if a real manifest slips through.
- [x] **4. Add a queue lock file, taken before recovery** — done in 0.22.0.
  `run.LockQueue` (`internal/run/lock.go`, with `lock_unix.go` / `lock_windows.go`) takes an
  OS file lock on `02.processing/`, ahead of both `--auto` and `Recover()`, held for the run.
  An **OS** lock rather than the `O_EXCL` file the review proposed, because the file would
  survive a crash and refusing to start on a leftover would disable crash recovery at exactly
  the moment it is needed; the kernel drops an OS lock when the process dies however it dies.
  On Windows the lock is one byte at offset 2^32 rather than the whole file: Windows locks are
  mandatory, so locking the holder line would make it unreadable and reduce the refusal to
  "holder unknown".
  *Left undone:* the lock is per processing directory, not per database. Two runs on separate
  queues pointed at one database still cannot see each other, so they can still rebuild the
  same index twice or kill each other through a `login_name` rule. Closing that needs a
  server-side lock (`sp_getapplock`, session-scoped so SQL Server releases it on disconnect),
  which is a different mechanism and a different decision — the recovery sweep, which is what
  made this CATASTROPHIC, is directory-scoped and is now closed. The two remaining sub-harms
  the review listed under finding 3 are separately actionable: `BlockerKiller` has no
  self-exclusion (`internal/run/kill.go`) where `VictimKiller` does (`internal/run/victim.go`),
  and that asymmetry is worth fixing on its own merits.
- [ ] **5. Give `abort-resumable` a target and a confirmation** (finding 4). It selects from
  `sys.index_resumable_operations` with no `WHERE`, so one command aborts every colleague's
  paused index build, unrecoverably.
- [ ] **6. Fix the TUI `x` / `X` semantics** (finding 5). `blockerGate.persistent`
  (`cmd/sqlgopace/main.go:841`) filters `s.BlockedBy(ddlSPID)` — sessions **our DDL is
  blocking**, i.e. victims — and feeds them to the TUI as "blockers". `x` kills one on a single
  keystroke, unconfirmed, ungated by `kill_blockers`, with no amplifier test; `X` then writes
  the rule into `kill_blocking_sessions`, which structurally can never match a victim. This is
  the direction error `docs/blocking-and-kills.md` uses as its own cautionary tale.
- [ ] **7. Report a stopped-short batch as incomplete, not done** (finding 8). Reuse the shrink
  path; today a half-finished purge is filed as complete and made unresumable.
- [x] **8. Correct the false claims** — done in 0.19.0. `README.md` no longer says "no raw
  SQL is ever accepted or executed"; it says a manifest is a trusted input and points at
  `SECURITY.md`, which now names all four verbatim-interpolated fields instead of two:
  `set_raw`, `where_raw`, `type` (presence-checked only, `manifest.go:703,723`) and
  `data_compression` (unvalidated, `generate.go:110`). `docs/configuration.md`'s
  shipped-vs-default table gained the third divergence, `monitoring.max_retry_attempts`
  (ships `1`, default `0`), and its description of that key was corrected: it retries after
  a *pressure cancel* only (`monitored_runner.go:68`), not after a failure.
  *Left undone:* an allow-list validating `data_compression` against `NONE|ROW|PAGE`, and
  the same for `type`. Both are code, not documentation, and item 8 was scoped to making
  the docs true; the fields are now disclosed rather than silently unvalidated.

**Two to name in the README even if not fixed**, because an operator who knows can work around
them and one who does not cannot: nothing anywhere reads `sys.dm_os_volume_stats`, so free space
on the actual disk is never checked (finding 7); and on Standard edition the only reaction rung
left is `Cancel`, so a one-minute block rolls back a 50-minute offline rebuild while holding
`Sch-M` (finding 10).

The remaining fourteen findings are legitimate "known limitation, documented honestly" — a
defensible posture for beta software **provided the documentation actually says so**, which is
what item 8 is for.

### From the SAST scan (2026-09-01)

Scanner floor only — full triage in [2026-09-01-sast-scan.md](2026-09-01-sast-scan.md). None of
these can harm a database, so none gates advertising the way the eight above do. The first two
are worth doing before a public release anyway; both are minutes.

- [ ] **`.env.example` ships `0o644`, and the docs say to `cp` it** (`scaffold.go:95`,
  `docs/getting-started.md:77`). Under the default umask the resulting `.env` — holding
  `DB_PASSWORD` — is world-readable on Linux and macOS. Write the asset `0o600` instead.
  *Not done here because:* the scan was scoped to SAST with no code changes, and this touches
  the scaffold assets a test pins.
- [ ] **SHA-pin the two actions in `release.yml`.** `actions/checkout@v7` and
  `actions/setup-go@v7` are mutable tags in the job that publishes the binaries, with
  `contents: write`. The only finding a downstream user cannot protect themselves from.
- [ ] **Capture files expose third-party SQL text at `0o644`** (`capture.go:144`,
  `contended.go:158`, `amplifier_capture.go:158`, dirs at `queue.go:35`). `active_query` /
  `parent_query` of other people's sessions, world-readable under `02.processing/`. Advisory
  files never read back, so `0o600` / `0o700` costs nothing.
- [ ] **`release.yml` interpolates the tag into `run:`** (`:31`, `:93`). Needs repo write access
  to exploit, so it is the author alone today; bind it through `env:` before a second
  maintainer exists.
- [ ] **Bump the toolchain to `go1.26.6`.** Four reachable stdlib vulnerabilities, all fixed
  there; all reached only through operator-supplied input, so exposure is low and the fix is a
  version string in `go.mod` and both workflows.

**The scan's most useful result was a negative one:** `gosec` reported *zero* SQL-injection
findings from a codebase that builds T-SQL by concatenation throughout, because the taint
crosses a package boundary its AST rule cannot follow. Do not read a clean `gosec` run as
evidence about CWE-89 here.

## Follow-ups deferred from shipped work

- [ ] **`shrink_log` ignores `max_block_minutes`.** v0.18.0 made `resolveShrink` resolve the
  cap, and `shrink_data` / `shrink_tempdb` apply it because both run chunked through
  `runChunk` (`internal/run/shrink.go:489`), which builds
  `Capabilities{MaxBlock: blockCap(res.MaxBlockMinutes)}`. `shrinkLog`
  (`internal/run/shrink.go:589`) issues one statement through `runWatchedStatement`, which
  never builds `Capabilities`, so the cap is resolved and then unused.
  *Deferred because:* a log shrink is a single short statement rather than an hours-long
  chunk loop, so the exposure is small, and giving `runWatchedStatement` a supervisor is a
  change to a path shared with the unchunked data shrink — worth doing deliberately rather
  than as a rider on a preflight change. Until then the CHANGELOG and `CLAUDE.md` both say
  plainly that `shrink_log` is uncapped.

- [ ] **A shrink still ignores `options.ignore_blocking`.** Fixed alongside it in v0.18.0:
  `max_block_minutes`, which `resolveShrink` (`internal/ddl/resolve.go:231`) dropped the same
  way. `IgnoreBlocking` remains unresolved there, and even resolving it would change nothing
  today, because `ShrinkRunner.runChunk` (`internal/run/shrink.go:708`) builds
  `Capabilities{CancelSafe: true, MaxBlock: …}` and leaves `IgnoreBlocking` false. So it is a
  two-part gap, not the one-line sibling of the v0.18.0 fix.
  *Deferred because:* it is not obviously wanted. `ignore_blocking: true` means "hold the lock
  through **everyone**", which is a far larger commitment for a chunked shrink running for hours
  than for one index rebuild, and a shrink already has the safer, more precise
  `ignore_blocked_sessions` allow-list. Decide whether a shrink should be able to hold through
  *unnamed* sessions at all before wiring it — and if the answer is no, delete the override from
  `Shrink.Options`'s reachable surface rather than leaving a key that parses and does nothing.

- [ ] **A batch DML cut short by the supervisor retries at the same size.**
  `internal/run/batch_dml.go:204` and `:279` — when `runBatch` returns `stopped`, the loop calls
  `handleStop` and `continue`s, skipping `AdjustBatchRows` entirely. So a batch that was cut short
  for blocking other sessions is retried unchanged, gets cut short again, and burns the whole
  `self_wait_timeout_minutes` budget (5 min default) before giving up — **without ever having tried
  a smaller batch**. Bounded, so not a hang, but the operation is not adaptive where it claims to
  be. Fix: halve `size` in the stop branch before `continue`, roughly three lines and one test.
  This is the same defect class fixed for shrink in v0.17.0 — the supervisor's verdict never
  reaching the controller — in its other failure mode.
  *Deferred because:* batch DML is not in use on the current engagement. Do it in a session that
  touches `batch_update` / `batch_delete` anyway.

- [ ] **`AdjustBatchRows` still runs the pre-v0.17.0 law, dead band included.**
  `internal/run/batch_calc.go:46`. Growth requires latency < 5 ms while reduction starts at 10 ms
  (WRITELOG) or 20 ms (PAGEIOLATCH_EX), so a batch sustaining anything in between is frozen at its
  initial size and never climbs toward `max_rows`.
  *Deferred because:* the failure that made this urgent for shrink **cannot happen here**. The
  shrink ratchet came from a growth gate (`elapsed < target`) that a multi-GB chunk could never
  satisfy; a DML batch is sized in rows and calibrated toward ~5 s, so its gate is reachable and
  the size does not walk down to the floor. Freezing at a *sane* initial size (1000/5000/20000 by
  table size) is a throughput loss, not a degradation. Note this is **not** a call to port AIMD:
  the duration objective is legitimate for DML, which holds locks for a batch's whole duration and
  has no per-invocation restart cost the way `DBCC SHRINKFILE` does.

- [ ] **`WaitDeltas.BlockingSeconds` is never populated and can go.**
  `internal/run/shrink_calc.go:82`, read only by `AdjustBatchRows`. `waitDeltas`
  (`internal/run/shrink.go:1051`) fills only the two latency fields, so the blocking clause in the
  batch DML law is inert — a maintainer reading it believes DML throttles when it blocks others,
  and it cannot. Delete the field and `blockingReduceSeconds` once the two entries above are done;
  doing it before would only move the dead code.

- [ ] **Real cumulative blocking time per chunk, if `stopped` proves too coarse.**
  `supervise` (`internal/run/executor.go:214`) tracks `blockingStart` as a *current streak*, reset
  on every clear, so a per-chunk total means changing its return type — which `MonitoredRunner`
  shares. The boolean `stopped` was chosen instead in v0.17.0 as a sufficient, already-computed
  proxy. Revisit only if field evidence shows the shrink backing off too late or too coarsely.

## Iterations still to design / implement

- [ ] **[Remote TUI (server / client)](remote-tui.md)** — follow and act on a run from another
  process. Proposes `--serve :port` (SSE broadcast hub) plus `--connect host:port` (reuses the
  TUI). The real cost is the **security** of remote actions (KILL). Builds on the step sink, which
  now exists (`internal/run/step.go`).

- [ ] **[tempdb guard (alert + self-attributed stop)](TEMPDB-GUARD.md)** — tempdb is shared by the
  whole instance, so the blast radius is every database. Proposes a **preflight no-start** when
  tempdb is already over threshold, a **runtime alert**, and above all a **stop conditioned on
  self-attribution**: only stop (pause → cancel) when tempdb is full *and it is us*
  (`sys.dm_db_session_space_usage` per SPID), otherwise alert only — stopping for someone else's
  fault frees nothing. Covered today only by the shrink driver's tempdb cache-flush escalation:
  the self-attribution read is missing, and so is any preflight tempdb check at all.
  **Correction (v0.18.0):** this entry used to claim `preflight.check_tempdb` existed. It never
  did — the key was parsed into `PreflightConfig`, never read, and no tempdb-space check was ever
  written. The key has been removed rather than left as a promise, so a tempdb guard starts from
  nothing here, not from "partially covered".

- [ ] **[Wait observability — the live panel](WAIT-OBSERVABILITY.md)** — the `.log` already
  summarizes our session's waits; what is missing is the **live TUI panel** showing the sliding
  delta from `sys.dm_exec_session_wait_stats`. **Observability, not reaction**: waits explain the
  "why" and drive nothing, since blocking and log already have dedicated reads and the
  WRITELOG/PAGEIOLATCH throttle already exists per driver. Reuses `SessionWaits` / `DiffWaits` /
  `CategorizeWaits`; `internal/tui` does not read them yet.

## Shipped

Kept so the entries above are not re-proposed. Each names the evidence in the tree.

- [x] **Batched DML** ([BATCH-DML.md](BATCH-DML.md)) — `internal/run/batch_dml.go`,
  `batch_calc.go`; `batch_update` / `batch_delete` documented in `docs/operations.md`. See the
  follow-ups above for what its controller still owes.
- [x] **Graceful stop / drain** ([graceful-stop.md](graceful-stop.md)) — `internal/run/drain.go`.
- [x] **Resume after interruption / metadata skip** ([crash-resumable.md](crash-resumable.md)) —
  `internal/run/skip.go`. The `skip_if_satisfied` flag it proposed was superseded by the
  per-operation `intent` field (`docs/manifests.md:75`).
- [x] **Manifest progress / step sink** ([progress-tui.md](progress-tui.md)) —
  `internal/run/step.go` feeding stdout and the TUI.
- [x] **Shrink stepsize AIMD** (v0.17.0) — `internal/run/shrink_calc.go`; the rules live in the
  *Superseded* block of [SHRINK.md](SHRINK.md) §7.2, the diagnosis and rejected alternatives in
  [shrink-stepsize-aimd.md](shrink-stepsize-aimd.md).

## Suggested order

1. The batch DML stop-branch follow-up is the cheapest real gain here — three lines, and it makes
   an operation adaptive that currently is not. Take it the next time batch DML is in scope.
2. `remote-tui.md` is unblocked now that the step sink exists.
3. `TEMPDB-GUARD.md` is cross-cutting (it serves `SORT_IN_TEMPDB` rebuilds, shrink and batched DML
   alike), so it is the one whose value grows with every driver added.
4. `WAIT-OBSERVABILITY.md` is the smallest of the three iterations and depends on nothing.

## Context

The original specs were born from the compression trial
`01.to_run/030_compress_exampledb_indexes.yaml` (74 PAGE indexes on `EXAMPLEDB`, Standard edition,
so offline rebuilds). See also `docs/llm-operator-guide.md` and the
`.claude/skills/sqlgopace-operator/` skill.
