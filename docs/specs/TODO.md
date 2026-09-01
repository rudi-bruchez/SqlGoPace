# TODO — the live backlog

What is worth doing next, and what has already been done so nobody proposes it again.

Two kinds of entry live here. **Iterations** are designed features with a spec of their own,
awaiting brainstorming then implementation. **Follow-ups** are deliberate scoping decisions taken
while shipping something else — small, known, and easy to lose. Each one records *why* it was
deferred, because that reasoning is what decides whether it is still the right call.

Keep this file honest: when work ships, move its entry to *Shipped* with the evidence rather than
deleting it. A backlog that lists finished work as pending is worse than no backlog — it invites
re-implementing what already exists.

Status last verified against the tree at v0.25.0 (2026-09-01).

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

Ordered as the review ordered them. **All eight are done (0.19.0–0.25.0).** What each one
left undone is recorded with it; none of those residues is a gate.

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
- [x] **5. Give `abort-resumable` a target and a confirmation** — done in 0.23.0.
  `--table` / `--index` filters, `--all` as the explicit no-target mode, `--yes` required for
  `--all` and for `--include-running`, and the resolved scope printed before the first
  `ABORT` rather than a warning after the decision. `--dry-run` needs no confirmation, on
  purpose: it is the review path, and ceremony there pushes operators toward the destructive
  form. `parseAbortFlags` is split out from `runAbortResumable` so the gate is tested without
  a database.
  *Left undone:* the review's better suggestion — default to aborting only the operations
  this tool paused, which the sidecar already records in `State.Paused`. It needs the
  subcommand to read the queue's state sidecars, which today it does not open at all, and
  ownership would still be unknowable for anything paused by an earlier install. The target
  filter closes the CATASTROPHIC part (a bare command can no longer touch a colleague's
  build); owner-defaulting would make the common case shorter to type, which is a usability
  gain rather than a safety one.
- [x] **6. Fix the TUI `x` / `X` semantics** — done in 0.24.0. `x` opens a confirmation naming
  the login, host, application and **open transaction count**, and stating that the session is
  waiting on us so the kill frees nothing; only `y` proceeds. `X` is removed, along with
  `ActionKillBlockerAuto` and `killBlockerAuto`, because the rule it wrote could never fire.
  *Left undone, deliberately:* the review also proposed restricting `x` to victims passing
  `IsAmplifyingCommand`. Not done — that is the automated killer's criterion, and making the
  *manual* key strictly narrower than the operator's own judgement would block the legitimate
  case where they know something the allow-list does not. The prompt gives them what they were
  missing (what it is, and what killing it costs) rather than deciding for them. Revisit if a
  real incident shows the prompt is not enough.
  *Also left undone:* renaming `tui.Blocker` → `Victim` through the console, which the review
  called the fix that stops the next person re-introducing this. 158 occurrences across seven
  files, and `blocker` legitimately means the other direction in the roster, `blockerGate` and
  `BlockerKiller` — a blind rename would corrupt those. Worth its own commit; the type comment
  now states the direction in the strongest terms available. Note the old comment already said
  "one session blocked by the running DDL" and the bug happened anyway, which is the argument
  for doing the rename.
- [x] **7. Report a stopped-short batch as incomplete, not done** — done in 0.25.0.
  `batchStoppedShort` routes through the existing `finalizeIncomplete`, exactly as the review
  suggested, and the `key_range` watermark is no longer cleared on that path — it was cleared
  on any nil-error return, so the walk that abandoned most of its rows was unresumable as well
  as misreported. `docs/configuration.md`'s `incomplete` notification event now says it covers
  batched DML.
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

- [x] **The two to name in the README even if not fixed** — done in 0.25.0, in the beta
  warning block. Both re-verified against the tree first: `sys.dm_os_volume_stats` appears
  nowhere in the repository, so disk free space is genuinely never read (finding 7); and
  `ddl_compatibility.yaml` gates `online`, `wait_at_low_priority` and `resumable` to
  `[enterprise, azure]`, so a Standard-edition `rebuild_index` really does have `Cancel` as its
  only rung (finding 10). Naming them is not fixing them: reading the volume, and giving
  Standard something better than cancel, are both still open.

The remaining fourteen findings are legitimate "known limitation, documented honestly" — a
defensible posture for beta software **provided the documentation actually says so**, which is
what item 8 is for.

**Where that leaves the verdict.** All eight gate items and both README notes are done, so the
specific objections the review raised are addressed. That is not the same as the review
re-running clean: it was a point-in-time reading of a tree that has since changed in eight
places, and three of those changes turned up defects the review had not found (a manifest
rewrite dropping a null `set:` value, a watermark cleared on a stopped-short walk, a Windows
lock making its own holder line unreadable). Re-run the harm review against the current tree
before treating the gate as cleared — and note that its finding 1 trigger A did not reproduce,
so treat its severities as claims to check rather than facts.

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

- [ ] **The `key_range` uniqueness check runs in the driver, not preflight.** v0.26.0 put it in
  `BatchDMLRunner.resolveKeyColumn` (`internal/run/batch_dml.go`), where the clustered-key read
  the walk already performs is to hand. The cost is that the manifest has already been moved to
  `02.processing/` before it is refused, so a table that can never be walked this way fails as a
  run rather than as a rejected plan.
  *Deferred because:* it matches where the sibling checks (non-integer key, composite key,
  missing clustered key) already live, so moving one without the others would split the rule
  across two packages. `internal/preflight` would need its own `ClusteringKeyColumns` read.
  Move all four together, or none.

- [ ] **The rest of the 2026-09-01 harm review is unaddressed** —
  [REVIEW-2026-09-01-harm.md](REVIEW-2026-09-01-harm.md), untracked, alongside this file. F0 (the
  unbounded `key_range` UPDATE) is fixed in v0.26.0. Still open, in the review's own ranking:
  F1 (TUI `k` kills the running DDL on one unconfirmed keystroke, while the less harmful `x` was
  given a confirmation in v0.24.0), F3 (an unquoted YAML date reaches T-SQL as bare arithmetic,
  silently changing which rows a `batch_delete` matches), F2 (quitting the console neither stops
  the run nor says so), F4 (`checkpoint_between_operations` is parsed, documented in four places,
  and read by nothing), F5 (the TUI kills with `kill_blockers.enabled: false`, which the shipped
  config calls the master arm), F6 (`max_block_minutes` excludes `shrink_log`, said only in
  `CLAUDE.md`/`CHANGELOG.md`/`TODO.md` and not in the operator docs).
  *Note on F3:* it is the cheapest of them — one line in `Literal.UnmarshalYAML` — and the only
  one whose failure is silent data loss with no error to investigate afterwards. Take it next.

- [ ] **`ddl_compatibility.yaml`'s `data_compression` entry is both dead and wrong.** It reads
  `{ min_major: 10, editions: [enterprise, azure] }` for `rebuild_index`, `create_index` and
  `rebuild_heap`. Two separate problems, found while verifying the Standard-edition warning
  added to the README in 0.25.0:
  1. **It gates nothing.** `data_compression` is a manifest field, not a resolved option:
     `generateRebuildIndex` passes `o.DataCompression` straight into `withClause`
     (`internal/ddl/generate.go`), and `Resolve` never reads the matrix entry. Verified by
     planning the same manifest against `TierStandard` and `TierEnterprise` — both emit
     `DATA_COMPRESSION = PAGE`. So the live Standard-edition compression work is unaffected;
     this is not a bug in the field, it is an entry that does nothing.
  2. **The fact it states is wrong.** Microsoft Learn's *Editions and supported features of
     SQL Server 2016* lists Data compression as Yes for Enterprise, Standard, Web and Express,
     footnoted "Applies to SQL Server 2016 (13.x) SP1 as part of creating a Common
     Programmability Surface Area (CPSA) across editions". So the correct gate would be
     `min_major: 13` for Standard/Express (10 for Enterprise), not enterprise-only.
  *Deferred because:* deciding between the two fixes needs a call the matrix's own rule
  frames — `CLAUDE.md` says the matrix carries version × edition gates *only*. Either wire
  `data_compression` through `Resolve` so the gate is real (and then it must be correct, which
  means SP1-aware and would refuse compression on Standard below 2016 SP1 — a behaviour
  change), or delete the entry as documentation masquerading as a gate. Deleting is the
  smaller, more honest change; wiring it is the one that would catch an operator asking for
  compression on 2014 Standard.
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
