# TODO — the live backlog

What is worth doing next, and what has already been done so nobody proposes it again.

Two kinds of entry live here. **Iterations** are designed features with a spec of their own,
awaiting brainstorming then implementation. **Follow-ups** are deliberate scoping decisions taken
while shipping something else — small, known, and easy to lose. Each one records *why* it was
deferred, because that reasoning is what decides whether it is still the right call.

Keep this file honest: when work ships, move its entry to *Shipped* with the evidence rather than
deleting it. A backlog that lists finished work as pending is worse than no backlog — it invites
re-implementing what already exists.

Status last verified against the tree at v0.30.0 (2026-09-02).

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

- [x] **`.env.example` ships `0o644`, and the docs say to `cp` it** — done in v0.30.0.
  `scaffold.File` carries a per-file `mode` and `.env.example` is the one written `0600`
  (`internal/scaffold/scaffold.go`), pinned by `TestEnvExampleIsPrivate` and, on POSIX only,
  by `TestWriteAppliesTheDeclaredModes`. `docs/getting-started.md` says to `chmod 600 .env`
  for a file written by hand or refreshed with `--force`, since `os.WriteFile` keeps an
  existing file's mode.
  *Left undone:* nothing here protects Windows, which ignores every mode bit but read-only;
  there the directory's ACL is the control.

- [x] **Capture files expose third-party SQL text at `0o644`** — done in v0.29.0
  (`capture.go`, `contended.go`, `amplifier_capture.go` now write `0o600`). The queue
  directories and the remaining `0644` writes were deliberately left; see "The remaining
  `0644` writes" below for the reasoning and the correction to it.

- [x] **`release.yml` interpolates the tag into `run:`** — done in v0.29.0; both sites bind
  it through `env: RELEASE_TAG`.

- [x] **Bump the toolchain to `go1.26.6`** — done in v0.29.0 (`go.mod`). `govulncheck` now
  reports zero reachable vulnerabilities, down from four.

- [x] **SHA-pin the actions** — done in v0.30.0, and the duplicate entry below is merged
  into this one. `actions/checkout@3d3c42e5…` (v7.0.1), `actions/setup-go@b7ad1dad…`
  (v7.0.0) and `golangci/golangci-lint-action@ba0d7d2e…` (v9.3.0) across `ci.yml` and
  `release.yml`. `gh` is still not installed here; the SHAs were resolved through the public
  GitHub API (`/repos/<owner>/<repo>/git/ref/tags/<tag>`, dereferencing the annotated tag for
  golangci-lint) and each was verified to be a real commit before being written. A new
  `.github/dependabot.yml` bumps them weekly — without it, pinning trades a mutable tag for a
  frozen version whose vulnerabilities get published.

**The scan's most useful result was a negative one:** `gosec` reported *zero* SQL-injection
findings from a codebase that builds T-SQL by concatenation throughout, because the taint
crosses a package boundary its AST rule cannot follow. Do not read a clean `gosec` run as
evidence about CWE-89 here.

## From the field (2026-09-02)

### Planning a compression campaign

Five findings from staging a PAGE-compression campaign over an 8.5 TB `EXAMPLEDB` on **SQL Server
2019 Standard**: 912 objects not yet PAGE, 7.8 TB of them, 19 objects carrying 6.1 TB and two of
those 1.4 TB and 1.25 TB each. An earlier attempt on the same database — a size-split of the
trial the original specs came from — had ended with **20 of a 33-operation manifest cancelled**,
each after 2 to 14 minutes of work that was then rolled back.

None of these is a bug. Every one of them is the tool doing exactly what it was told while the
operator did, by hand and in SQL, the reasoning the tool had the facts to do itself. That is the
common thread, and it is why they are grouped rather than filed one by one.

- [ ] **Standard edition has no reaction available, and nothing says so before the run.** The
  hierarchy is `WAIT_AT_LOW_PRIORITY` → `RESUMABLE` pause/resume → `KILL`. The first two are
  Enterprise. On Standard the only lever left for a rebuild is cancel — and a cancelled rebuild
  rolls back completely, unlike a shrink, which keeps the space it already freed. So an offline
  rebuild that runs longer than `blocking_timeout_minutes` on a table anything writes to
  **cannot complete**, no matter how many times it is retried: `max_retry_attempts` just spends
  the cost again. That is what produced the 20 cancellations, and the run report explained each
  one individually (`operation canceled under pressure`) without ever stating the shared cause.
  `Resolve` already knows the edition and already emits `Decision`s; preflight already knows the
  object's size. Predicting whether a *specific* operation will exceed the blocking timeout still
  needs a previous run's measured throughput, which the history DB does **not** store — `runs`
  records one duration per manifest, not per operation (see "The history DB is a run ledger"
  below). The verdict is computable *before the first statement*: "no reaction is available on
  this target; this operation is expected to exceed the blocking timeout; it will be cancelled".
  *Why it is worth doing rather than documenting:* the operator who most needs it is the one who
  wrote a manifest that looks exactly like a working one. Nothing in the manifest, the `--explain`
  output or the plan distinguishes an operation that will finish from one that structurally
  cannot, and the cost of finding out is hours of production locking for no result.
  *Open question the design has to answer:* what the tool should then do — refuse, warn, or
  reorder. Refusing is wrong for a genuine maintenance window where nothing is writing.
  **0.34.0 shipped the narration half of this** (`docs/specs/CANCEL-ONLY.md`): a plan holding a
  rollback-on-cancel operation (a heavy builder with no `RESUMABLE`) says so in `--dry-run`, once
  at manifest start, and in the run report's summary — naming the cause, and how many were
  actually canceled and how many a retry saved. It does **not** change `DecideReaction`: every
  cancel this entry describes still happens exactly as before. The cancel itself — the 2026-09-01
  production harm review, finding 10 (SEVERE), which proposes not canceling a non-resumable,
  non-cancel-safe operation on blocking pressure alone and letting `max_block_minutes` be the
  operator's explicit opt-in to paying for one — is deferred, not rejected, and stays open here:
  it is a change to the reaction hierarchy that needs its own design (what "hold and narrate" does
  to the sessions queued behind a Sch-M).

- [ ] **`max_block_minutes` means the opposite thing on a rebuild and on a shrink, under one
  key.** On a shrink, yielding at the cap is nearly free: the pages already moved stay moved and a
  re-run continues from the smaller file — which is exactly what v0.30.0 leaned on when it
  extended the cap to the two unchunked statements. On a rebuild, yielding at the cap throws away
  the entire operation, and the rollback holds the lock while it happens. The same number is a
  cheap safety valve in one manifest and a "waste N minutes and change nothing" switch in the
  other. `docs/manifests.md` documents the mechanism identically for both.
  *Deferred rather than obvious:* the fix is not a second key. It is deciding whether the engine
  should say so — in `--explain`, in the run report, or by resolving a different default per
  operation kind — and a per-kind default is a behaviour change that needs its own migration note.

- [ ] **The data-free-space check sizes a compression rebuild from the wrong number, in both
  directions.** `CheckDataFreeSpace` takes `needMB` from the object's *current* size. Microsoft's
  rule (*Disk space requirements for index DDL operations*, *SORT_IN_TEMPDB Option For Indexes*)
  is that the destination filegroup needs roughly the size of the **new** structure, the old one
  being deallocated only at commit. For a `data_compression: PAGE` rebuild the new structure is
  the smaller one, so the check overstates the need and warns on operations that would have fit.
  The other direction is worse. When the files are uncapped — the common case — a shortfall
  degrades to a `Warn` naming the autogrowth, and the run proceeds. On a database that has just
  been shrunk that means **the rebuild campaign silently gives back the space the shrink spent
  hours reclaiming**: on this one, 797 GB free at 9.2 %, against a single object of 1.4 TB, with a
  data file whose `FILEGROWTH` is 1 MB.
  *The general point, which is bigger than the check:* a shrink and a compression campaign on the
  same database are in direct conflict, and the tool models neither side as knowing about the
  other. It has every fact needed to say "this operation is expected to grow the file by N GB",
  which is the sentence the operator actually needs. Wire the estimate through preflight first;
  a real space budget across a queue is a larger design.

- [ ] **`.blocked.yaml` is the most useful artifact the tool produces, and it is per-run only.**
  Every ignore rule in the new campaign came from reading those captures: which logins were
  blocked, how often, and which of them were read-only reporting sessions safe to hold the lock
  through versus writers that must never be held up. That reasoning was done by grepping and
  counting across two runs months apart. The captures are advisory-only by design and should stay
  that way — copying one into a manifest must remain a deliberate act — but the aggregate is a
  read, not an action: *"these four logins account for every cancellation you have had; three of
  them never held a write transaction."*
  *Deferred because:* it wants the history DB (`internal/report/history.go`) rather than the
  sidecars, and it is a reporting feature, not a safety one. Cheap, and it compounds with every
  run.

- [ ] **A campaign is not an object the tool models.** 912 objects, five manifests, a maintenance
  window that opens for four hours at a time, partial completion, re-runs across months. The one
  primitive that fits is already right: `intent: compression` makes a re-run skip whatever already
  carries the target, so a half-finished stage is safe to re-queue. What is missing is the
  question that follows every window — *how far through am I* — which today is answered by
  re-querying the catalog by hand. History has the runs, the catalog has the state; a
  `--campaign`-style status could join them.
  *Deferred because:* it needs a definition of what a campaign is (a filename prefix? a tag in the
  manifest? a set of manifests sharing a `description`?), and picking the wrong one bakes a
  concept into the queue that the queue currently does not need. Design before building.

### An 18-run shrink, and the plan behind it

Read from one completed run's report and the SQLite history beside it: a `shrink_data` that
reached its target after **18 runs spread over six weeks** (11 `INCOMPLETE`, 4 `FAILED`, 2
`INTERRUPTED`, 1 `SUCCESS`), plus the `maintenance_analysis` rows of the planner run that
preceded it. The findings below are ordered by how much they cost, not by how hard they are.

- [ ] **`index.rebuild_max_size_mb` silently vetoes a compression change, on exactly the objects
  where compression pays.** `decideIndex` (`internal/maint/decide.go`) evaluates
  `decideCompression` first, then applies the size ceiling: over it, a wanted REBUILD is
  downgraded to a REORGANIZE, and since a reorganize cannot change compression the change is
  dropped with `; compression change dropped (needs rebuild)`. The ceiling exists for a
  *fragmentation* reason — a huge REBUILD is expensive — and it silently decides a *compression*
  question that has no cheaper alternative. On the database this was read from, it vetoed the
  27 largest objects: **6.4 TB, 83 % of everything not yet compressed**, every one of them over
  the 50 GB default.
  Three separate defects sit inside that one behaviour, and each is worth its own fix:
  1. **The measurement is taken and thrown away.** `plan.estimateFor` runs
     `sp_estimate_data_compression_savings` for ROW and PAGE with no size gate, so the planner
     paid to estimate a 1.4 TB index, fed the result to `decideCompression`, and then discarded
     the decision. `maintenance_analysis` stores `current_compression` and `chosen_compression`
     but **not the estimate**, so nothing survives. Re-answering the question means paying for
     the same estimate again.
  2. **`chosen_compression` conflates "chose not to" with "could not".** A genuine "PAGE gains
     less than `min_gain_percent`, keep NONE" and a "we gave up because of the ceiling" land in
     the same column with the same value. Only the free-text `reason` distinguishes them, so any
     aggregate over that column is misleading — a reader of this history concluded the planner
     had measured no benefit on 6.4 TB, which is the opposite of what happened.
  3. **The ceiling has no compression-specific escape.** `rebuild_over_ceiling` offers
     `reorganize` or `skip`; neither is "rebuild anyway, because only a rebuild can do this".
  *What the design has to decide:* whether the ceiling should apply to a compression-motivated
  rebuild at all. Arguments both ways — a 1.4 TB offline rebuild on Standard is genuinely
  dangerous, which is what the ceiling is protecting against; but silently answering "no
  compression, forever" for every large table makes the planner useless precisely where it
  matters. A third option is to keep the veto and **say so loudly** in the plan output, which is
  the smallest honest change.

- [ ] **The shrink report merges the two phases, so nobody can tell which one did the work.**
  `ShrinkResult` carries `chunks` and `gained_mb`, but `result.Chunks++` happens only in the
  page-moving loop: the Phase A `TRUNCATEONLY` contributes to `gained_mb` and to nothing else.
  The run that prompted this reported *6 312 671 MB gained over 100 chunks* — 61.6 GB per chunk,
  against a `max_step_mb` of 8192. Arithmetically impossible for Phase B, and the reader is left
  to deduce that a single truncate released most of it because seventeen earlier runs had already
  moved the pages forward. Split the two in `shrink[]`: MB and elapsed for the truncate, MB,
  chunks and elapsed for the loop. Small, and it is the number an operator needs to plan the
  next shrink.

- [ ] **The history DB is a run ledger, not an outcome ledger.** `runs` has `manifest`,
  `outcome`, timings, `operations` (a count), `peak_blocked`, `skipped`, `error` — and nothing
  about what any operation *did*. No MB reclaimed, no chunks, no file, no object. Across 18 runs
  of one shrink the history cannot answer "are we converging?"; that lives only in the `.log`
  sidecars, which follow the manifest through the queue and are overwritten by the next run.
  An `operations` table keyed to `runs.id`, carrying the same fields the JSON block already
  computes, would make a campaign readable. It also feeds the "no reaction available" verdict
  above, which wants a previous run's measured throughput.

- [ ] **"No further progress" is a pause condition treated as a stop condition.** 11 of the 18
  runs ended `INCOMPLETE` with `stopped short of target, work preserved — no further progress`,
  and every one of them made progress again when a human restarted it, sometimes an hour later,
  sometimes eight. So what `max_no_progress: 3` detects is not "this file cannot shrink further",
  it is "not at this step size, not right now". The backoff ladder tops out at
  `no_progress_backoff_max_seconds: 300`; the remedy that actually worked was two orders of
  magnitude longer.
  *Deferred rather than obvious:* the fix is not simply a bigger ceiling — a run that sleeps for
  hours holds its queue lock and its connection, and an operator watching a TUI that says
  "waiting" for six hours will kill it. The options are a much longer ladder, a halve-and-retry
  before giving up (the AIMD law already halves on pressure; a no-progress chunk is arguably the
  same signal), or a genuine requeue-with-delay that releases everything. Pick one deliberately.

- [ ] **The same physical situation is fatal on one path and benign on another.** Two runs died
  `FAILED` on `mssql: Could not adjust the space allocation for file 'PRODDB'`, and one on
  `truncateonly: SQL Server had internal error`. Eleven other runs met what is very likely the
  same condition — the file will not give up more space right now — and reported it as
  `no further progress (work preserved)`, leaving the work banked and the manifest resumable.
  A hard `FAILED` moves the manifest to `04.failed`, which needs a human to put it back.
  *Worth checking before fixing:* whether these are really the same state. The error text comes
  from the server, so the classification is a mapping question (which `mssql` error numbers mean
  "cannot shrink now" rather than "something is broken"), and getting it wrong in the permissive
  direction would retry a genuine failure forever.

- [ ] **An unknown manifest field is rejected without a suggestion, and the two field names most
  easily confused are exactly the ones that got confused.** One run failed at load with
  `unknown field "kill_blocked_sessions"` — a cross of `ignore_blocked_sessions` (sessions *we*
  block) and `kill_blocking_sessions` (sessions blocking *us*). The pair is documented, has its
  own section in `blocking-and-kills.md`, and is still the trap. A Levenshtein match against the
  known key set — `did you mean "kill_blocking_sessions"?` — is a few lines in the decoder's
  error path and turns a lost run into a corrected typo.

- [ ] **The tool's own advisory sidecars are discoverable as manifests.** `Queue.Discover` accepts
  any `*.yaml` / `*.yml` not starting with a dot (`internal/run/queue.go:64`). The sidecars are
  named `<manifest>.blocked.yaml`, `.contended.yaml`, `.amplifiers.yaml`, so a copy that lands in
  `01.to_run` is picked up and executed as a manifest. It happened twice: two `FAILED` runs on
  `unknown field "observed"`, two junk rows in the history, and the sidecars moved out of the
  queue into `03.done` / `04.failed`. They are documented as "advisory only — SqlGoPace never
  reads this file back", which is exactly the promise being broken.
  *Note when fixing:* `.recovery.yaml` is the odd one out — it is *meant* to be re-queued. So the
  rule is a suffix denylist, not "anything with two dots", and it should skip with a clear
  message rather than a `FAILED` run.

- [ ] **The shrink ETA is a backward-looking average, and it is never recorded.** `estimateShrink`
  projects the remaining MB over the rate achieved so far. While the step size is still growing —
  which is the whole point of the AIMD law — that projection is structurally pessimistic, and the
  operator who prompted this reported the run finishing far sooner than the console had promised.
  Nothing in the report or the history keeps the ETA, so the size of the error cannot be measured
  after the fact. Record the projection alongside the outcome first; only then is there evidence
  to decide whether the estimator needs a step-size-aware term.

- [ ] **`write_ratio` is stored without the counts that make it trustworthy.**
  `decideCompression` applies the write-intensive cap only when `reads + writes >=
  activity_floor` (1000). `maintenance_analysis` records the ratio and not the counts, so a
  recorded `0.500` on a barely-touched index is indistinguishable from a well-measured one — and
  in the data read here, eight objects sit at exactly `0.500` and four at exactly `0.000`, which
  is the shape of a low-count artifact. Anyone reusing the stored ratio to make a decision (which
  is exactly what happened, to re-target a campaign from PAGE to ROW) cannot tell which rows to
  trust. Store `reads`, `writes`, and whether the floor was met.

## Follow-ups deferred from shipped work

- [x] **Five of the eight findings of the 2026-09-17 harm review are fixed in 0.40.0**
  (`docs/specs/REVIEW-2026-09-17-harm.md`): the shipped connection string no longer trusts any
  certificate (finding 3, `config.yaml` + the scaffold twin), planned manifests are published by
  rename (finding 4, `cmd/sqlgopace/plan.go` `stagedName`), a failed monitoring poll is narrated
  once per outage (half of finding 2, `internal/run/executor.go` `pollHealth`), the `.log` is
  written `0600` and `docs/running.md` says what each artifact carries (finding 5, restated there
  as MINOR after the draft overstated it), and both pages that describe `max_block_minutes` now
  agree and say what its coverage is worth (finding 8, and the documentation half of finding 1).

- [x] **The last two decision-bound harm-review findings landed in 0.41.0.** Finding 1: an
  absent `max_block_minutes` on a shrink now resolves to two minutes
  (`ddl.DefaultShrinkMaxBlockMinutes`), so the shipped profile no longer needs a `shrink`
  block for planned manifests to carry a cap, and an explicit `0` stays the way to opt out.
  Finding 2, second half: the two monitoring polls run on their own goroutines, and a channel
  that stops producing readings stops the operation (`Sample.Blind`, `ErrMonitorBlind`,
  `internal/run/executor.go`) rather than only narrating. The decision on both was the user's,
  taken 2026-09-17.

- [x] **Six findings of the second codex review are fixed in 0.42.0**
  (`docs/specs/REVIEW-2026-09-17-codex-branch.md`, verdicts recorded there): the tempdb sampler
  no longer arms the killers a documentation page promised it never used (F-03), the fallback
  KILL proves the session is still ours before issuing (F-01), the tempdb shrink watches
  tempdb's log rather than the user database's (F-03 second half), a measured over-cap survives
  a failed reuse-wait read (F-02 remnant), releasing the queue lock no longer unlinks the file
  (F-07), and a `key_range` watermark is bound to the statement it walked (F-04).

- [ ] **What the second codex review leaves open.**
  **(F-04, the part the chosen fix does not reach)** `planFingerprint` still hashes command and
  target only, and is still compared only when the resume cursor is past zero. Binding the
  watermark to its statement closes the case that skips rows; it does not close "an operation
  the cursor has already passed is edited before the resume", which runs the new SQL for
  operations after the cursor and never runs it for those before. Widening the plan fingerprint
  to the full resolved plan is the fix and was declined on 2026-09-17 because it invalidates
  every existing sidecar and restarts every interrupted manifest from operation zero. Revisit
  when a format version is being introduced for another reason.
  **(F-10)** `updateSidecar` returns silently when the sidecar cannot be read, so a precise
  resume can degrade to a restart with no signal. Worth a look when the resume path is next
  touched — it is the one maintainability finding of that review with a harm argument.
  **(F-08)** `type` and `data_compression` reach generated SQL without an allowlist. This is
  inside the trust boundary `SECURITY.md` declares, so it is hardening rather than a defect, but
  a field that looks like an enum should be one.

- [ ] **The tempdb no-kill invariant is held by a comment, not a test.** `cmd/sqlgopace/main.go`
  deliberately attaches no killer to the tempdb sampler, and `docs/shrink.md` promises it. The
  wiring needs a live tempdb connection, so there is nothing to assert without a server — which
  is exactly how it was armed for twenty-nine releases without anyone noticing. Either extract
  the wiring far enough to test it, or add it to the integration suite.

- [ ] **What the harm review still leaves open.**
  **(6)** `SizedOperation` covers `rebuild_index`/`rebuild_heap` only, so `create_index` and a
  table-rewriting `alter_column` get no data-free-space check. The `create_index` half is
  mechanical; `alter_column` needs a judgement about which changes rewrite.
  **(7)** `progress_poll_seconds` is required, documented without qualification, and read only by
  `runWithTUI` — the second question `TestNoInertConfigKey` does not ask. Write that test when the
  next monitoring key is added.

- [x] **Three of codex's four highest-harm findings were verified and fixed in 0.42.0; the
  fourth is still open.** The second codex run (`REVIEW-2026-09-17-codex-branch.md`) re-found
  three of them independently, which is the strongest signal either report carries: F02 (a
  fallback KILL identified only by a reusable session id) is `b677ca5`, F03 (a tempdb shrink
  killing the live workload despite a documented prohibition) is `1b5b5d6`, and F04 (a tempdb
  shrink watching the wrong database's log) is `50c6024`. **F08 is not fixed** and is carried
  below: the watermark half of it — resuming behind a position recorded against different SQL —
  is closed by `8660c8a`, but the replay of committed side effects on the boundary batch is not.
  Of the four findings from that reader that have now been checked, four held.

- [ ] **`key_range` is at-least-once across a crash, and the docs imply better.** Codex F08
  (first run) and F-05 (second run), both rated SEVERE, neither verified by running. The range
  `UPDATE` commits before its watermark is saved, so a crash replays the boundary range. The
  `literal SET` restriction makes the *column value* idempotent and says nothing about an
  `AFTER UPDATE` trigger, an audit or billing row, a temporal write, or a downstream consumer —
  and a key-range `UPDATE` carries no self-limiting predicate, so it re-touches rows that are
  already satisfied. A second shape: with `on_failure: continue`, a failed earlier operation
  freezes the resume cursor, so later successful operations run again after an interruption.
  Verify first, on the throwaway instance, with a trigger that counts its firings. If it holds,
  the cheap fix is to name the guarantee in `docs/operations.md` instead of implying idempotence
  from the literal-`SET` rule; the real fix is to exclude already-satisfied rows, or to require
  reconciliation before replaying side-effectful work.

- [ ] **A second question for the inert-key audit: is a key read on every path its documentation
  claims it for?** From the same review (finding 5). `TestNoInertConfigKey` asks whether a key is
  read *anywhere*, which `progress_poll_seconds` satisfies through its single caller —
  `runWithTUI`. The key is required, is documented without qualification as pacing progress, waits
  and space, and does nothing at all for an unattended run. That is the
  `checkpoint_between_operations` class one level down, and it is mechanically detectable: a key
  whose only consumer sits behind a flag, documented as if it always applied. Worth writing when
  the next monitoring key is added rather than on its own, and worth remembering that the fix for
  the instance (one sentence in `docs/configuration.md` and in `config.yaml`) is not the fix for
  the class.

- [ ] **The console's active-request count counts SqlGoPace itself.** From 0.39.0, raised by the
  code review. `ActiveRequestCount` reads the `ActiveSessions` snapshot, whose only session filter
  is `is_user_process = 1`, so the monitoring read (a running request at the instant it reads) and
  the operation's own session are both in it: a quiet server reports 1 or 2 rather than 0. The
  clean fix is `AND r.session_id <> @@SPID` in `activeSessionsSQL`, but that query is also read by
  the reaction path (`internal/run/executor.go`), the shrink driver, preflight and capture — too
  much safety-critical surface to move for a header line. Documented instead, in `docs/running.md`
  and beside the function. Worth doing if a second consumer ever needs a count that excludes the
  observer.

- [ ] **`CPUPressureAlarm` and `LogFullAlarm` are the same latch, written twice.** From 0.39.0,
  raised by three of the four cleanup reviewers. Same `armed bool`, same constructor, same
  three-branch `Observe`, same "fires once per episode, re-arms strictly below" contract; only
  the threshold pair and the observed value differ. The shared form is one unexported
  `latch{fire, rearm float64; armed bool}` in `internal/run` with both alarms as thin wrappers.
  Left out of the 0.39.0 cleanup because the fix edits `logalarm.go` and its tests, which are
  outside that diff, and two copies is where extraction only barely pays. What was done instead:
  `TestCPUPressureAlarmRearmExactBoundary` now mirrors `TestLogFullAlarmRearmExactBoundary`, so
  the second copy is no longer the untested one. **Extract when a third alarm lands** — that is
  the point where the copies stop being reviewable by eye.

- [ ] **A banner alarm's threshold is evaluated twice: once for the edge, once for the styling.**
  From 0.39.0. `Observe` knows the threshold and computes the comparison, then discards the level
  and returns only the edge — so each caller re-derives "is it over right now" from the exported
  constant (`fire := logAlarm.Observe(...)` beside `alert := ... >= LogFullThresholdPercent`, and
  the same shape in `serverLoadMsg`). Having `Observe` return both would delete that re-derivation
  and the risk of a sender that styles nothing. Deferred with the latch extraction above: it is
  the same committed mechanism, and changing one without the other trades a duplication for an
  asymmetry.

- [ ] **The CPU-pressure narration reaches the console only, not the manifest's `.log`.** From
  0.39.0. `feedConsole` lives outside the engine, so its line goes to the TUI narration and
  vanishes with the session; a run without `--tui`, or read afterwards from `03.done/`, has no
  record that the server was short of CPU while the operation crawled. The transaction-log alarm
  has both halves (console alert *and* a `warn` in the `.log`, wired through `WithLogWatch`),
  which is the shape to copy. Left out because it means giving the engine a load reader and a
  per-manifest alarm for a figure nothing reacts to — worth doing when the first post-mortem asks
  why an operation took four hours, not before.

- [ ] **A reflection audit for `internal/maint`, in the spirit of `internal/config/audit_test.go`.**
  From the 2026-09-16 harm review. `HeapMeasurement.DisabledIndexes` shipped parsed, documented by
  its own comment as deciding the outcome, and populated by nobody: the guard in `DecideHeap` that
  read it was unreachable in production for a whole release. That is the same defect class
  `TestNoInertConfigKey` exists to catch — a field a type presents as load-bearing that nothing
  upstream ever sets — and it survived TDD (every test set the field by hand) and a diff-scoped
  review (each half is correct on its own). Fixed in `internal/plan/plan.go` for this one field;
  the audit that would have caught it, and would catch the next one, is not written. It would walk
  `maint.Input`'s measurement types and fail on a field no planner code path assigns.

- [ ] **`TestTailAndMaintWarningsAreIndependent` is flaky under load.** Seen failing once during
  the 2026-09-16 verification run (`tailWarn=true maintWarn=false`) under a full `-race ./...`,
  then green on five consecutive runs of the package alone and two more full-suite passes. It
  drives goroutines through `sampledOnce` and `newSelfBlockTestRunner`/`runHeld`, so the likely
  cause is the maintenance warning racing the release rather than a defect in the warning itself.
  Not chased at the time because nothing in that change touches the shrink driver. A flake that
  fires once a suite is worse than a failure: it teaches the next reader to re-run rather than
  look.

- [x] **The 2026-09-03 harm review is closed out** — findings 1, 2, 3 and 4 fixed in 0.33.0
  ([REVIEW-2026-09-03-harm.md](REVIEW-2026-09-03-harm.md); it is a historical record and is
  not updated as items are fixed). Evidence: `(*mssql.Conn).stopOrphan` and its four tests in
  `internal/mssql/conn_repair_test.go` (1); `outcomeSkipped` in `internal/run/engine.go` with
  `TestAManifestClaimedByAPeerIsNotAFailure` (2); `spidAnnouncer` in `cmd/sqlgopace/main.go`
  with `TestSPIDAnnouncerFollowsTheExecutionSession` (3); `mssql.WithReconnectTimeout` and
  `connOptions` with `TestRepairGivesUpAfterTheConfiguredReconnectTimeout` (4).
  *Left undone:* the wait for an abandoned session to stop is a fixed two minutes
  (`orphanStopTimeout`). It is deliberately not `reconnect_timeout_minutes` — that key asks
  whether the server is reachable, this asks whether our own statement has finished rolling
  back. Making it configurable would mean a new key, its shipped-file twin, its docs row and
  its two audit entries; nobody has asked, and the failure it produces is loud and
  actionable rather than silent. Revisit if an operator reports rollbacks that routinely
  outlast it.
  Its finding 5 (`0o644` writes) is **withdrawn**: it is the entry *The remaining `0644`
  writes* below, whose reasoning is better than the finding's and which already corrects the
  claim the finding repeated. Two reviewers have now reached the same wrong conclusion about
  it; read that entry before raising it a third time.

- [ ] **The log-full warning reaches no notifier.** From the 2026-09-15 harm review
  ([REVIEW-2026-09-15-harm.md](REVIEW-2026-09-15-harm.md), H6): `feedConsole`/`watchLog` fire
  a log-full alarm (`LogFullAlarm` crossing `LogFullThresholdPercent`) into the console and the
  `.log`, but `e.notify` only fans a webhook/email out for `pause`/`cancel`/`abort`, so a
  scheduled unattended run with a notifier configured learns of a filling log only by reading
  the `.log` afterward. *Why deferred:* on the shipped `log_max_percent: 80`, a notified
  `cancel` or `pause` has normally already fired by the time the alarm's (higher, fixed) 90%
  threshold crosses — so the gap mostly matters exactly where H1's retry window used to bite
  (log pressure surviving a cancel unnoticed), and that window is now closed by the immediate
  log sample (H1, fixed). *What it needs:* a distinct `log_full` event so
  `notifications.on_events` can subscribe to it independently of `cancel` — a config surface
  change (the shipped `config.yaml`, its embedded twin, and the two config audits), not a
  one-line fix.

- [ ] **Wait for the log to drain before retrying after a log-pressure cancel** (the review's
  second, narrower fix for H1, `REVIEW-2026-09-15-harm.md`): when a `Cancel` was for
  `LogOverCap`, have `MonitoredRunner.Run` call the existing `awaitRelief` (bounded by
  `log_drain_timeout_minutes`) before retrying, instead of retrying immediately. *Why
  deferred:* the immediate log sample landed instead (`pumpSamples` now reads the log before
  its ticker loop) and already stops a *blind* retry — a retry after a log-pressure cancel is
  re-canceled within seconds rather than running for up to `log_poll_seconds`. The user
  decided not to change the retry policy itself for this pass (see CANCEL-ONLY.md "Decision:
  no change to the retry", which this note extends). Revisit if FULL-recovery campaigns show
  retries being repeatedly re-canceled on log pressure rather than succeeding once the log has
  actually drained — the immediate sample prevents the *outage*, not the wasted retry.

- [ ] **A paused resumable rebuild holds a hidden copy that no object-size read shows.** From the
  resumable-pause probe ([OBJECT-SIZES-ANALYSIS.md](OBJECT-SIZES-ANALYSIS.md), 2026-09-15): while a
  1 GB clustered index rebuild was paused at 95.67%, the data file's used space was about twice
  the table, yet `sys.indexes` and `sys.dm_db_partition_stats` showed the source index alone. The
  partial target sits under internal index ids visible only through `sys.partitions` joined to
  `sys.allocation_units`. File-level reads (`FileSpace`) include those pages, so the header and the
  data-free-space check see the space as used; what does not is anything reasoning from object
  sizes. Nothing in `internal/preflight` or the shrink driver reads
  `sys.index_resumable_operations`. *Why deferred:* no failure has been observed, and the question
  is where it could bite, not how to fix a known defect. *What to check:* a shrink run while a
  resumable is paused (SqlGoPace pauses by canceling and resumes on the next run, so a drained or
  crashed manifest can leave one for days) cannot move the hidden target's pages and has no object
  to name for the stall; the pre-shrink tail-object and heap advisories, which read partition
  stats, would not point at it. A preflight line naming paused resumables and their `page_count`
  may be enough.

- [ ] **A fourth audit: the statement-executing drivers against the rules that must hold on
  all of them.** Deferred deliberately on 2026-09-03 — the work is wanted, not urgent. What
  follows is the analysis, so whoever picks it up does not have to re-derive it.

  **The class.** Three code paths run a statement on the pinned execution connection. Every
  cross-cutting rule has to land in all three, nothing enforces that, and — this is why it
  survives review — *each path is correct on its own terms*. A diff-scoped reader opens
  `runChunk` and finds nothing wrong: the function does what it says. The gap is only visible
  with the three side by side, which is a view no diff ever produces.

  | rule | `MonitoredRunner.runStatement` | `ShrinkRunner.runChunk` / `runWatchedStatement` | `BatchDMLRunner.runBatch` |
  |---|---|---|---|
  | fallback `KILL` after the grace | `monitored_runner.go:204,210` | `shrink.go:752,758` | `batch_dml.go:422,428` |
  | `max_block` cap | via `Capabilities.MaxBlock`, `engine.go:703` | `shrink.go:722,824` — **hand-fixed in 0.30.0** | `batch_dml.go:344` |
  | drain / graceful stop | `ErrStopped` | `stopRequested` | `stopRequested` |
  | `ignore_blocked_sessions` | `caps.Ignore` | `IgnoreSource` | `IgnoreSource` |
  | re-pin narration (`noteRepin`) | `monitored_runner.go:180,193` | **missing** | **missing** |

  **The instances, honestly counted: two.** 0.30.0, where `max_block_minutes` was enforced by
  the chunked shrink path but not by `runWatchedStatement`, so the two unchunked statements
  ignored the safety cap. And 0.33.0, where the re-pin narration reached only the DDL path, so
  a shrink can continue under a new server session with nothing in the `.log` saying so — that
  one was left in knowingly. (An earlier draft of this entry, and a session summary, said
  "four" or "five". That was the count for the *TUI harm* class, which CLAUDE.md records as
  hand-fixed once per release across 0.23.0, 0.24.0 and twice in 0.28.0. Two is still the
  threshold this project sets: *a defect class found twice belongs here as a test*.)

  Rewrite either instance changing only a noun and you get the other. That is the tell.

  **The structural cause, and it is worth fixing alongside.** All three carry their own copy of
  the abort → wait for grace → `KILL` block, and even the field holding the same
  `kill_grace_seconds` is named differently: `killGrace` in `monitored_runner.go:27`, `killGr`
  in `shrink.go:155` and `batch_dml.go:122`. There is nowhere in the tree that states "these
  are the rules for running a statement", so each new rule has to be *remembered* three times,
  by a person. The audit makes the omission fail the build; extracting the shared block would
  remove most of the occasions for it.

  **Shape of the audit**, following `internal/config/audit_test.go` and
  `internal/tui/harm_audit_test.go`: enumerate the statement-executing paths and the rules,
  then *drive* each path and assert the rule fired. Both halves must come from the source or it
  rots — a new driver nobody listed has to fail the test, exactly as an unranked `ActionKind`
  does today.

  **Expect writing the rule list to be most of the work**, the way ranking harm was in the TUI
  ledger, and expect the next defect to appear while writing it. Note that "the harness is
  expensive" is precisely the reasoning that shipped instance two; if it is used again, it
  should be because someone weighed it, not because it went unnoticed.

- [ ] **The `.state.json` sidecar keeps the SPID and `login_time` of the session that started
  the manifest.** They are written once, in `Engine.freshState`, and a re-pin (0.33.0) makes
  both stale for the rest of that manifest. A crash in that window leaves an orphan whose
  recorded signature matches nothing, so `Recoverer` requeues the manifest instead of
  adopting it. Safe — the signature is `SPID` + `login_time` + `CONTEXT_INFO` (SPECS §16), so
  a stale triple fails closed and cannot be mistaken for somebody else's session — and
  correct for a non-resumable operation, whose work rolled back anyway; a resumable one is
  found through `sys.index_resumable_operations`, which is server-side. Deferred because the
  fix is not "rewrite the sidecar on re-pin" but a decision about who owns that write: the
  connection knows it re-pinned, the engine knows which manifest is in flight, and nothing
  currently connects them.

- [x] **Five config keys whose only statement of their default is the shipped file** — done in
  v0.31.0. `applyDefaults` (`internal/config/config.go`) now materializes all five:
  `monitoring.max_retry_attempts` 1, `preflight.require_data_free_space` true,
  `history.enabled` true, `history.destination` `sqlite://./sqlgopace_history.db`,
  `notifications.on_events` the five events the file lists. The three whose zero value is a
  setting became tri-state (`*int` / `*bool`) with accessors `MaxRetries()`,
  `DataFreeSpaceRequired()` and `IsEnabled()`, so an explicit `0`, `false` or
  `on_events: []` is still honoured; the pointers are filled in `applyDefaults` rather than
  left nil so the parsed config carries the value it will act on. The five OPEN entries are
  gone from `documentedDivergences`; `docs/configuration.md` no longer has a
  shipped-versus-default table, and the CHANGELOG carries the migration note.

- [ ] **Two defaulting mechanisms for one config surface.** The two entries left in
  `documentedDivergences` are intended, not defects: `kill_amplifying_maintenance.
  min_blocked_behind` and `after_seconds` default through the `MinBehind()` / `After()`
  accessors instead of `applyDefaults`, so the parsed field stays zero while the behaviour
  matches the file. The wart is having both mechanisms; moving these two to `applyDefaults`
  would empty the ledger and make the audit's remaining output pure signal. Deferred because
  it is cosmetic — the behaviour is already what the file says — and because the accessor
  pattern is what the tri-state fields now use too, so the right unification is a decision
  about which mechanism wins, not a two-line move.

- [x] **The third audit: a destructive-action ledger** — done in 0.32.0,
  `internal/tui/harm_audit_test.go`. It ranks every console `ActionKind` by what it costs
  and whom, measures each gate by driving the real `Model.Update`, and fails on any action
  reachable with a weaker gesture than a less harmful one; a new `ActionKind` that nobody
  ranked also fails. It found the fifth instance of the class on its first run:
  `ActionArmKillRule` fired on one keystroke from the roster while `x` and `k` — both less
  harmful — had confirmed since 0.24.0 and 0.28.0. Arming now confirms; disarming does not.
  `docs/running.md`'s claim that `k` was the most destructive key was corrected at the same
  time. The harm ordering is stated in the test because the code states it nowhere; that
  was the part deferred as "most of the work", and writing it down is what exposed the
  defect.
  *Left undone:* the CLI half — its own entry below.

- [ ] **The harm ledger covers the console, not the CLI.** `internal/tui/harm_audit_test.go`
  ranks every `ActionKind` and measures its gate, but the same class lives on the
  command-line surface and is not audited: `abort-resumable --yes` (gated in 0.23.0 after
  shipping with no target and no confirmation), the batched-DML whole-table guard
  (`confirm_full_table`), and the DDL delete confirmation. The blocker is the completeness
  half, not the ranking: console actions are enumerable because every one is an `ActionKind`
  constant in a single type, whereas the CLI's destructive operations are flags on
  subcommands with no shared type to walk. A hand-maintained list of them is exactly what
  this audit exists to avoid — it would go stale the first time someone adds a subcommand,
  which is the failure it is meant to prevent. So the work is: find something to derive the
  destructive CLI set from (a marker on the flag registration, or a `destructive: true`
  field on the subcommand struct), then the pairwise check is the same twenty lines. Worth
  doing before the next destructive subcommand, not after.

- [ ] **The inert-key audit stops at the config surface.** `TestNoInertConfigKey` walks
  `Config` and fails on a key nothing outside `internal/config` reads, directly or through an
  accessor — the F4 class (`checkpoint_between_operations`, parsed and documented and read by
  nothing) mechanized. It is not extended to manifest fields, where the same class lives:
  matching is by identifier name, and operation fields are `Schema`, `Table`, `Index`, names
  shared across every operation type, so a genuinely inert one would be laundered by a
  sibling's use. Doing it properly needs type-accurate reachability (`x/tools/go/packages`),
  a dependency the audit does not justify on its own.

- [x] **The `key_range` uniqueness check runs in the driver, not preflight** — done in
  v0.30.0, all four together as the entry required. The rule is
  `preflight.KeyRangeColumn`, reported by `CheckBatchDMLKeyRange`; `Prober` gained
  `ClusteringKeyColumns`, and `BatchDMLRunner.resolveKeyColumn` is now three lines calling the
  same function from the read the walk performs anyway. Reaching the verdict twice is
  deliberate rather than redundant: a clustered index dropped or recreated between preflight
  and the run is still caught, and the two can never disagree because there is one rule.

- [x] **`true` / `false` in a manifest scalar generate invalid T-SQL** — done in v0.30.0,
  both halves of the entry, in `Literal.UnmarshalYAML`. `!!bool` maps to `1` / `0`; the
  numeric spellings T-SQL cannot read are refused at parse time by `checkNumericLiteral`.
  **One of them turned out not to be a message-quality fix at all.** A leading zero is
  accepted by *both* languages and read differently — `017` is octal 15 in YAML and decimal
  17 in SQL Server — so it was the F3 class exactly: valid SQL against the wrong value,
  silent. It is refused rather than converted, because converting it would mean guessing
  which of the two the author meant.
  *Left undone:* the conversion is one-way. `set: {Archived: true}` round-trips through
  `MarshalManifest` as `1`, which is the same value written the way the server reads it, but
  an operator diffing a rewritten manifest against their original will see it.

- [x] **The 2026-09-01 harm review is closed** —
  [REVIEW-2026-09-01-harm.md](REVIEW-2026-09-01-harm.md), untracked, alongside this file.
  F0 (unbounded `key_range` UPDATE) v0.26.0; F3 (unquoted dates as arithmetic) v0.27.0;
  F1 (unconfirmed TUI kill of our own DDL) v0.28.0; F2, F4, F5, F6 and the minor items
  v0.29.0. Two of its minor items were **not** taken, below.

- [x] **SHA-pin the GitHub Actions** — done in v0.30.0. This entry was the duplicate of the
  one in the SAST section above, which now carries the details.

- [ ] **The remaining `0644` writes and `0755` directories.** v0.29.0 tightened the three
  capture sidecars to `0600` because they carry other sessions' identities and SQL text.
  `gosec` also flags the run report (`internal/report/report.go`), the recovery manifest
  (`internal/run/engine.go`), the scaffold's files (`internal/scaffold/scaffold.go` — all
  but `.env.example`, which v0.30.0 made `0600`), the planner's output
  (`cmd/sqlgopace/plan.go`, `shrink_plan.go`) and the queue directories
  (`internal/run/queue.go`, `lock.go`).
  *Deferred because:* those are the operator-facing artifacts. A `.log` a colleague reads,
  a manifest a second person reviews before it runs, and a queue directory a scheduler
  writes into are all workflows `0600`/`0700` would break, so the right control is the
  directory's permissions rather than a hard-coded mode on each write.
  **Correcting an earlier claim here:** this entry first said the review found no
  third-party data in them beyond what the capture sidecars hold. That is false. The run
  report carries it too — `internal/run/victim.go:534` appends `"; source: %s (login=%s
  host=%s)"` to a reaction detail, which the engine stores as a `report.ReactionLine` in
  the `.log`. So the choice is a real one about who may read the queue, not a free pass;
  make it deliberately. Note also that file modes are ignored on Windows apart from the
  read-only bit, so the `0600` already applied to the sidecars protects the POSIX
  deployments only.

- [x] **`ddl_compatibility.yaml`'s `data_compression` entry is both dead and wrong** —
  deleted in v0.30.0; the reasoning that decided it is at the end of this entry. It read
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
  **Deleted in v0.30.0**, with a note in `ddl_compatibility.yaml` saying what belongs there
  and why this did not. What decided it: a *correct* gate has to express "2016 SP1", and
  `min_major` cannot — it keys on the major version alone, so the honest options were an
  entry that is wrong or no entry. `docs/llm-operator-guide.md`'s option table carried the
  same wrong fact and now says `data_compression` is ungated, on every edition.
  *Still open, and unaffected by the deletion:* the field is an unvalidated string
  interpolated into the `WITH` clause (`SECURITY.md` names it). An allow-list is not simply
  `NONE|ROW|PAGE` — `COLUMNSTORE` and `COLUMNSTORE_ARCHIVE` are valid on a columnstore index,
  which `expand.go` only strips on the `index: ALL` path — so it needs the operation's index
  type, not just the string. Wiring the field through `Resolve` would catch an operator
  asking for compression on 2014 Standard, and remains the larger fix.
- [x] **`shrink_log` ignores `max_block_minutes`** — done in v0.30.0.
  `runWatchedStatement` takes `ddl.ResolvedOptions` and applies the cap itself, on the same
  rule `supervise` uses (a continuous streak of blocking *any* session, ignored or not, so
  the cap overrides every ignore policy). It returns a `watchedOutcome` rather than a bool,
  which is what let the two callers differ: a capped `TRUNCATEONLY` falls through to the
  page-moving loop, which caps per chunk; a capped log shrink has no second phase, so the
  operation ends cleanly with the freed space kept. `docs/blocking-and-kills.md` no longer
  carries the exception.
  *Deliberately not done:* the chunked path answers a cap with `awaitRelief` and a retry.
  `runWatchedStatement` does not — for a single statement that would be a re-issue loop with
  no bound but the log-drain timeout, and the operator re-running is both simpler and
  honest, since the statement is re-entrant. Revisit if a real log shrink turns out to need
  several passes to finish.

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

- [ ] **The amplifying-command allow-list is documented nowhere an operator can read it.**
  `config.yaml:123` says `commands: []  # empty = the built-in allow-list` and never names the
  nine verbs. `mssql.DefaultAmplifyingCommands` (`internal/mssql/maintenance.go:42`) was added
  for exactly that, "for config validation and for documenting the effective set"
  ([2026-08-03-amplifying-maintenance-victim.md](superpowers/plans/2026-08-03-amplifying-maintenance-victim.md)),
  and does neither: only its own copy-checking test calls it. It is the `TestNoInertConfigKey`
  class one step out, a key whose default is unreadable rather than unread. Pick one direction,
  not both: name the nine verbs in the comment of `config.yaml` *and* its byte-pinned twin
  `internal/scaffold/assets/config.yaml`, or print the effective set under `--explain`. Then
  delete the exported function and its copy-only test, which the remaining direction makes dead.
  Deferred 2026-09-11 because the choice is editorial, about where an operator actually looks.

- [x] **The 2026-09-11 ponytail (over-engineering) audit is closed out.** One of its five
  findings landed: `writeManifest` in `internal/ddl/edit.go`, where the three manifest-editing
  functions repeated the same marshal plus atomic-write tail. The report itself was not
  committed; its four other findings were declined, and the reasoning is here so they are not
  raised a second time.
  - `trimLine` to `bytes.IndexAny` (`internal/run/lock.go:120`): three lines against an import,
    on a loop that does not allocate. Worse, the spec wrote the set as `"\\n\\r\\x00"`, with the
    backslashes doubled, which searches for `\`, `n`, `r`, `x` and `0`. Applied verbatim it
    truncates the holder line at the `n` of `on`, and `TestQueueLockExcludesASecondHolder`
    catches that only when the running process's pid happens to contain a zero.
  - `sort.Strings` in `Queue.Discover` (`internal/run/queue.go:56`): redundant, `os.ReadDir`
    does sort by filename. It stays anyway. Those two lines are the local statement of the
    `010_`/`020_` execution-order contract, and once removed no test can observe their absence:
    `TestQueueDiscoverSorted` is green either way, since the ordering would then live in the
    standard library.
  - the `tui.Program` wrapper (`internal/tui/program.go`): removing it moves the `bubbletea`
    import into `cmd/sqlgopace/main.go` and rewrites nine signatures there, to save twenty
    forwarding lines.
  - deleting `DefaultAmplifyingCommands` on its own: right in isolation, but it is the entry
    above, which has to be settled first.

- [ ] **`feedConsole` cannot be unit-tested, so its cadence is verified by reading.**
  `cmd/sqlgopace/main.go` — it takes a concrete `*mssql.Conn` and drives its own tickers, so
  nothing asserts that blockers refresh on `blocking_poll_seconds` and progress on
  `progress_poll_seconds`. The 0.38.0 change split those two cadences
  ([BLOCKER-VISIBILITY.md](BLOCKER-VISIBILITY.md)) and only the pure part (`blockersOf`) is
  covered by a test; the wiring is covered by the eye.
  *Deferred because:* the seam is a narrow reader interface over the three reads the loop makes,
  which is a small refactor but a real one, and this change did not need it to be correct. Take
  it the next time the console feed is opened — the same seam would let the suspension tracker's
  accrual be tested, which is also currently uncovered.


### Resumable-rebuild follow-ups deferred from 0.44.0 (2026-09-20)

Raised by two external code reviews and an external harm review of the 0.44.0 resume work.
Each was verified as real and deliberately left undone; the reasoning is what decides
whether it is still the right call.

- **Automatic resumption has no gate.** A paused rebuild whose stored statement matches is
  now resumed without confirmation. The harm review's first finding: a DBA may have paused
  that rebuild *deliberately* to relieve pressure, and the tool would silently undo it. The
  statement match is strong evidence of ownership (it includes the manifest's exact option
  string) and the resumed statement now yields at low priority, which bounds the damage —
  but on a shared server "same statement" is not "my work". Decide whether adoption should
  require the sidecar, or a manifest opt-in, or stay automatic.
- **`PausedResumable` and `ResumableOps` overlap on `ResumableProbe`.** One reviewer
  wants the index-scoped method deleted and its two callers (`resumeStatement`,
  `resumableInterruption`) filtering the table-wide list instead; the other argues both
  are justified because the Engine genuinely needs a targeted check and a table-wide
  enumeration. Not resolved. Deleting it is the KISS answer if the filtering reads as well.
- **Several paused rebuilds on one table are handled first-come.** `resumableStandingFor`
  records only the first foreign blocker and the DMV read has no `ORDER BY`. With
  `abort_blocking_resumable` and two foreign pauses, aborting the first still leaves the
  operation blocked by the second. Either inspect every blocker before any ABORT and refuse
  while an unabortable sibling remains, or establish that SQL Server cannot produce that
  state and write the invariant down instead.
- **Same-table ordering.** A manifest holding two indexes of one table, one of them paused,
  now fails the earlier operation cleanly (Msg 10637 is table-scoped) instead of failing at
  the server. Resuming the paused sibling *first* would let the manifest complete, but that
  is a scheduling change, not a check. The startup scan added in 0.44.0 at least names the
  pause before the run starts.
- **The startup scan's coverage claim is shallow.** It matches a queued `ObjectRef` only:
  no operation kind (a `reorganize_index` on that index reads as covering a paused
  rebuild), no database scope (a manifest for another database counts, though `ownsManifest`
  will skip it), and no `index: ALL` expansion. The wording was softened to "named by a
  queued operation" rather than deepening the check.

**Rejected, with the reason, so it is not re-raised:** the harm review called the startup
scan a client-identifier leak. It is not a new class — the tool already writes schema,
table and index names to stdout, the `.log` sidecar and the SQLite history for every
operation it runs, and `CLAUDE.md`'s rule is about what reaches *the repository*, not what
a run prints about the database it is pointed at. Scoping the scan to the run's database
would still be reasonable on noise grounds.
### Found while shipping 0.45.0 (2026-09-20)

The first two were seen in a live production campaign; the rest come from the harm review and
the external codex review of the same day and were verified here in code. None is scheduled.

- **A graceful stop is invisible while waiting for relief.** The drain is checked in the
  statement supervisor (`internal/run/executor.go:273`), which only runs while a statement is
  executing — and a paused operation is running none. `waitForRelief` has its own loop and
  consults monitor blindness, the log cap and the drain timeout, never `caps.Stop`. Measured:
  `d` pressed while the operation was paused on `LOG_BACKUP`; the stop was honoured at
  14:32:25Z, one second after the 14:32:24Z resume, fifteen minutes later. So the drain waits
  out the pressure, spends a full resume cycle and its log, and pauses again immediately. The
  operation is *already* paused when the request arrives: there is nothing to finish. Fix:
  check `stopRequested(caps.Stop)` in `waitForRelief` and return a stop, so `runLoop` ends
  without resuming. Deferred only because it surfaced mid-campaign.
- **The operation index disagrees with itself in the failure message.** The report lists
  `[10] rebuild_index …` and the error underneath reads `operation 9 (rebuild_index)
  interrupted by a graceful stop` — the display is 1-based, the internal cursor 0-based.
  Cosmetic, but an operator grepping a log for the operation they just watched finds nothing.
- **The ETA is still wrong, and the 0.43.0 change was cosmetic.** Measured against a live
  rebuild: at 44.66% with 128 s of request elapsed, `Progress.ETASeconds()` returns 158.6 s and
  `sys.dm_exec_requests.estimated_completion_time` returns 158 s. They are the same number —
  the server computes that column the same way, so replacing it changed the provenance and not
  the value. Real remaining, from the observed rate of 4.39 pct/min, was ~12.6 minutes. Cause:
  `percent_complete` is cumulative over the resumable operation's whole life while
  `total_elapsed_time` restarts at each RESUME, so after a pause the two are on different
  clocks and the error is the ratio between them (720 s / 128 s = 5.6 here). Fix: use
  `sys.index_resumable_operations.total_execution_time`, already read into
  `ResumableOp.ExecutionMinutes` since 0.44.0, as the elapsed term — or derive the rate from
  two successive samples, which is immune to any clock mismatch. Neither is implemented or
  tested. Until one is, the console shows a number that is wrong by about 5x while it matters
  most, which is worse than showing none.
- **`plan` estimates compression on every eligible object before the size ceiling applies.**
  `internal/plan/plan.go:158` and `:181` call `estimateFor` gated only by `estimable(row)` and
  the include/exclude rules; `rebuild_max_size_mb` is applied later, in the decision layer. A
  1.4 TB index the ceiling will reject is therefore sampled twice, ROW then PAGE, and
  per-partition mode multiplies the calls. Microsoft documents that
  `sp_estimate_data_compression_savings` scans the source under read committed, acquires an IS
  lock and loads a sampled equivalent into tempdb, while `docs/specs/MAINTENANCE.md:72`
  promises "no locks ... beyond cheap reads" and `docs/maintenance-planner.md:40` says dry-run
  takes "no locks". The documentation half is the dangerous one: that claim is what makes an
  operator willing to run it against production. Found by codex, verified here in code.
- **The resume cursor's fingerprint ignores operation content.** `planFingerprint`
  (`internal/run/engine.go`) hashes only `CommandType()` and `opTarget()`, so editing a
  completed operation's compression, partition, column type, default or batch predicate leaves
  the fingerprint unchanged and the cursor skips it. For compression the damage is bounded —
  `skipSatisfied` re-reads the server and rebuilds when the target differs, observed working on
  a live manifest — but nothing re-checks a batch-DML predicate or a column type. Found by
  codex, verified here in code.

- **A log-pressure pause never says which threshold fired, or at what value.** `LogSample`
  (`internal/run/executor.go:60`) carries only `OverCap bool` and `ReuseWait string`.
  `ServerSampler.Log` computes `used >= logMaxBytes || used% >= logMaxPercent` and then
  discards both measurements, so `Pressure.reason()` can only say "transaction log over cap".
  The operator cannot tell whether the absolute byte cap or the percentage tripped, nor how
  far over it was. Found the hard way: an operator watching a campaign pause repeatedly asked
  why, with 79% of a 260 GB log file free — answering it took reading the source and querying
  the server, because the report could not. The byte cap had been left at the shipped 50 GB
  while the file was 260 GB, so it fired at 20% full. Fix: carry `UsedBytes`, `UsedPercent`
  and the rule that fired in `LogSample`, and render them — `used 53.7 GB >= 50 GB cap; file
  20% used; reuse_wait=LOG_BACKUP`. Cheap, and it converts a support question into a line of
  the report.
- **Nothing says which configuration file the run loaded.** The banner names the server,
  edition, version and recovery model, and the `.log` names the manifest and the binary
  version, but neither names the resolved `--config` path or the thresholds it carried. A
  checkout can easily hold two divergent configs — this one had `config-local.yaml` and
  `local/config.yaml` disagreeing on `blocking_timeout_minutes`, `max_retry_attempts`, the
  notification events and the whole `shrink` block — and editing the wrong one is silent. It
  was caught here only because the unused file's `matrix_file` resolved to a path that does
  not exist. Fix: print the absolute config path at startup and record it in the `.log`
  sidecar, next to the version already recorded there.

Five more came from the same codex review, concluded by reading and **not** verified here, so
re-derive each before acting: the data-space preflight warns and proceeds on a file with
unlimited growth without ever reading volume free space; `max_chunk_seconds` does not cancel a
chunk already running, and a failed self-KILL is followed by an unbounded join; `finalize`
deletes the sidecar before the queue move has succeeded and can report SUCCESS with the
manifest stranded in `02.processing`; the README's categorical "takes no lock" for dry-run is
false in connected mode; and the queue lock is keyed by directory rather than by database, so
two queues against one database can KILL each other once kill rules are armed.

### Found in a live shrink, blocked on a tail object (2026-09-20)

A 500 GB `DBCC SHRINKFILE` against `PRODDB` on a synchronous AG stopped after 15 minutes
having reclaimed 33.6 GB of it. Outcome `INCOMPLETE`, work preserved, which is the designed
behaviour. What is wrong is how little it tried and what it did with what it already knew.

- **The tail object is identified before the give-up but never feeds the decision.** The
  backward page walk ran at `+12s`, long before the first failure, and named the object owning
  the file's last allocated page: `dbo.MEASUREMENT` `index_id = 8`, `0 pages from end`, a
  covering nonclustered index of 57.7 GB. That fact reached the operator only in the report,
  after the run ended. It should steer the loop instead: re-probe the tail on each no-progress
  event and branch on whether it moved. **A tail object that is the same object across
  successive retries while `halveStep` is shrinking the ask is a structural blocker** and
  waiting is pointless, so stop at once with the object named in the reason rather than burning
  the whole budget. A tail object that keeps changing is workload churn re-using the freed
  space, which is exactly the case Microsoft documents for Azure SQL ("a workload might start
  using the storage space freed by shrink before shrink truncates the file"), and there waiting
  is the right answer and the budget should be generous. Today both cases get the same 90
  seconds. The measured run: three attempts asking for 8 GB, then 4, then 2, each returning
  `Could not adjust the space allocation for file`, with nothing in the server error log for
  the window (no 665, no 1450, no 5202/5203) — the signature of a structural blocker, and the
  driver could have said so at the second retry.
  **CORRECTION, measured 2026-09-20 22:10-23:34, and it invalidates the heuristic above as
  stated.** The rule proposed here was "same tail object across successive retries while
  `halveStep` shrinks the ask = structural blocker, stop at once". A later run of the same
  campaign falsifies it. The shrink stalled **five separate times**, each episode backing off
  30 s then 1 m then 2 m, with `dbo.MEASUREMENT` index_id 5 named as the tail object
  throughout — the same object, across retries, exactly the signature this entry calls
  structural. It was not. With the budget raised to `max_no_progress: 10` and
  `self_wait_timeout_minutes: 30`, every one of those five episodes recovered on its own and
  the run finished at 100 % of target: 746 347 MB -> 381 298 MB, 365 049 MB reclaimed in 39
  chunks, 1 h 22 m. Total blocked time 1 042 s, i.e. 17 minutes of pure waiting that turned
  into 356 GB.

  Two consequences. **The defaults would have thrown that away**: at `max_no_progress: 3` the
  first episode stops the run on its third backoff, and the 5-minute cumulative cap kills it
  regardless, so the shrink would have ended around 680 GB instead of 372 GB. That is the
  second point of this entry proven in production, and it is now the higher-value half.
  **And identity of the tail object is not the discriminator.** The same object can mean
  "actively being written into the space you just freed", which is churn and wants patience,
  or "cannot be moved", which is structural and wants a relocation. Distinguishing them needs
  something else — whether the file size moved at all between episodes, whether the object is
  taking writes, whether the *page* at the tail changed even though the object did not. Do not
  build the identity heuristic; it would have stopped a run that was working.
- **The no-progress budget is sized for a chunk, not for an overnight shrink.** Defaults are
  `max_no_progress: 3` with `no_progress_backoff_seconds: 30` doubling to a 300 s ceiling, so
  a shrink planned to run for a night gives up after **90 seconds of waiting across three
  tries**. Worse, `self_wait_timeout_minutes: 5` caps the *cumulative* wait, so raising
  `max_no_progress` alone changes almost nothing: the two interact and neither comment says so.
  Fix is small and in two parts. State the interaction in `config.yaml` next to both keys, and
  make the give-up reason name which bound tripped (count or cumulative wait) rather than the
  present single `no further progress` for both. Then consider scaling the budget to the size
  of the reclaim: a shrink whose remaining work is measured in hundreds of gigabytes should
  wait minutes, not seconds, before concluding that a transient blocker is permanent.

### Found relocating a tail object, and in the run that did nothing (2026-09-20)

Same campaign as the entry above, the next two hours of it. All three were measured, not
reasoned about.

- **Asking for the tail-object walk makes the give-up record *less* fresh, not more.**
  `chunkLoop` runs the proactive walk once at loop entry when `identify_tail_object: true`
  and stashes it in `tp.finding` (`internal/run/shrink.go`). `captureGiveUpTail` then
  returns early precisely *because* a finding is already stashed, "so the give-up path stays
  a single read". So the record written at give-up is a re-emit of a measurement taken
  before the first chunk executed. Measured: the walk ran at `+12s`, five chunks moved
  33.6 GB over the next fourteen minutes, and at `+14m50s` the run emitted the `+12s`
  finding and wrote `confirmed_by: tail_position` into the `.contended.yaml` sidecar. In
  that run the answer happened to still be right, because 33.6 GB off a 57.7 GB tail object
  cannot have moved its ownership of the last page. It is right by arithmetic that the
  driver did not do. A run *without* `identify_tail_object` walks fresh at give-up and gets
  a genuinely current answer, which inverts the meaning of the flag. Worse, the sidecar
  feeds `plan --confirmed`: a stale reading is promoted to a confirmed structural blocker
  and generates a relocation manifest for whatever owned the tail some minutes earlier.
  Fix: walk again at give-up even when a proactive finding exists, and keep both — the entry
  walk and the give-up walk answer different questions, and *comparing* them is the signal
  the entry above asks for (same object = structural, moved = churn). The saved read is a
  micro-optimization on a path that has already decided to stop.

- **A manifest whose every operation was skipped reports `SUCCESS`, indistinguishable from
  one that did the work.** Measured: a `rebuild_index` manifest carrying `intent:
  compression` against an index already at the target compression produced
  `outcome: SUCCESS`, `duration: 4307ms`, one operation line reading
  `skipped: already ROW (563ms)`. The skip is correct (`skipSatisfied`,
  `internal/run/skip.go`) and the per-operation line is honest; the manifest-level verdict
  is not, and the verdict is what an operator reads first, what the queue acts on (the file
  moves to `03.done/`) and what the history records. The codebase already names this exact
  defect class one screen away: `reconcileResumePlan` restarts clean rather than "silently
  skip operations (which would report SUCCESS having executed nothing)" (`engine.go`). The
  guard exists for the resume cursor and not for its sibling. `rep.Skipped` is already
  counted in `summarize`, so the fix is small: when every operation in the plan was skipped,
  say so in the outcome rather than only in a count nobody reads. `docs/manifests.md` states
  the danger in its own words under `intent` — "a wrongly skipped rebuild is silent,
  reported as a success that did nothing" — which is a specification of the symptom, written
  before it was observed.

- **`intent` has no word for relocation, which is a third thing the two it has cannot say.**
  `compression` means "skip if already at target"; `fragmentation` means "always run". A
  rebuild whose purpose is to *move* an object off the end of a file is neither: its
  `data_compression:` is there to preserve the current setting, not to change it, and it must
  run whatever the catalog says. Today it is expressed as `intent: fragmentation`, which
  works by side effect. This is a small gap while an operator writes the manifest by hand and
  a real one the moment the first entry above lands: once the driver names a structural tail
  blocker, the obvious next step is `plan` generating the relocation manifest, and a generator
  cannot emit a word that means something else and hope the reader understands. Consider
  `intent: relocation` (always runs, like `fragmentation`, but says why) before building the
  generator, not after. Counter-argument worth keeping: on the measured run the tail index was
  at **53.5 % logical fragmentation** against **0.62 %** for the clustered index of the same
  table, so `fragmentation` was literally true and the vocabulary gap cost nothing that day.

- **Shrink fragments what it relocates, and here is the number.** `docs/specs/SHRINK.md` and
  `docs/shrink.md` both warn about post-shrink fragmentation in general terms. Measured on the
  campaign above, after five chunks moved 33.6 GB: the nonclustered index the shrink was
  relocating stood at **53.46 %** logical fragmentation (7 363 368 pages), the clustered index
  of the same table, which the shrink never touched, at **0.62 %** (7 077 868 pages). Same
  table, same rebuild pass a few hours earlier, one order of magnitude apart. Put the figure in
  the docs — a warning with a measurement behind it is acted on and a warning without one is
  not. Do NOT propose the post-shrink defrag chaining as new work here: it is designed in
  `SHRINK.md` §12.1 (Phase 2, layering settled, deliberately not a field of the `shrink`
  operation), and §12 already lists the before/after `sys.dm_db_index_physical_stats` report.
  What that design lacked was a measured number justifying an extra read on a path that has
  just finished a long operation. This is that number.

## Iterations still to design / implement

- [ ] **Autonomous tail unblocking: let a stalled shrink relocate its own blocker.** Requested
  after a live campaign spent an evening doing this by hand, three rounds of the same loop. A
  shrink that stops `INCOMPLETE` on a tail object already knows the name of what blocks it: the
  `.contended.yaml` sidecar records it with `confirmed_by: tail_position`. Today the operator
  reads the report, writes a relocation manifest by hand, runs it, and re-arms the shrink. The
  measured case, on one `PRODDB` data file:

  | Round | Tail object | Before | After | Gain |
  |---|---|---:|---:|---:|
  | 1 | an unused NC index, 57.7 GB | 898 184 MB | 864 568 MB | 33.6 GB |
  | 2 | *(after rebuilding that index)* | 864 568 MB | 746 347 MB | **115.4 GB** |
  | 3 | a 2.3 GB log heap | 746 347 MB | 715 831 MB | 29.8 GB on TRUNCATEONLY alone |

  Relocation works and the effect is not marginal: 33.6 GB reclaimed before it, 115.4 GB after.
  The heap rebuild was verified to land at **22.2 % of the file, 580 GB before the end**
  (`sys.dm_db_database_page_allocations`), so the allocator did cooperate. Proposed shape,
  declared **in the manifest and never on by default**, because this is unrequested DDL on
  production:

  ```yaml
  - operation: shrink
    identify_tail_object: true
    unblock_tail:
      enabled: true
      max_rounds: 3
      max_object_mb: 5000        # above this, stop and ask
      require_online: true
      require_low_priority: true
      preserve_compression: true
  ```

  **The guard rails are the feature, not decoration.** ONLINE mandatory, and stop with the object
  named if the matrix resolves it off. `WAIT_AT_LOW_PRIORITY` mandatory where it exists, or an
  unattended unblock can block production while nobody watches. A size ceiling: relocating 2.3 GB
  unattended is reasonable, relocating 57.7 GB is an hour of DDL and ~58 GB of log on a
  synchronous AG. Compression preserved, because changing it mid-operation is an architecture
  decision taken at night by a machine. RESUMABLE where the operation has it, and a much lower
  ceiling where it does not: `rebuild_heap` (`ALTER TABLE ... REBUILD`) has **no RESUMABLE form**,
  so it is one transaction and a cancel under pressure rolls back everything — the planner already
  says so (`reaction = cancel only`). And a bounded round count, or it is a treadmill: each object
  moved uncovers the next.

  **Hard dependency, stated so nobody builds this first.** It cannot be built before the entry
  above about the tail object not steering the decision: an automatic unblock acting on a stale
  reading relocates the wrong object. It also wants `intent: relocation`, because a generator
  cannot emit `fragmentation` and mean "move this".

- [ ] **A DBCC error in the TRUNCATEONLY phase is fatal; the identical error in the chunk loop is
  by design not.** Measured on a production run that was lost to it:
  `shrink "PRODDB": truncateonly: execute ddl: mssql: Could not adjust the space allocation for
  file 'PRODDB'.`, outcome `FAILED`. The chunk loop states the opposite policy in its own comment
  — "A DBCC SHRINKFILE chunk error almost never means the operation is broken ... We decide
  success by progress, not by matching a specific message number" — while `shrinkData` returns
  the phase A error straight out. **The same Msg 3140 is benign in phase B and fatal in phase A.**
  Two costs, and the second is worse than the lost run. The manifest lands in `04.failed` having
  attempted nothing: no TRUNCATEONLY, not one chunk. And **the tail-object walk never runs**,
  because it sits at `chunkLoop`'s entry, behind phase A — so the operator gets a failure with no
  diagnosis at all, on the one operation whose entire subject is "what is in the way". In the
  measured case the cause was transient: a `rebuild_heap` had finished two minutes earlier and the
  old copy's extents were not yet released (verified by hand: no allocated page in the last
  200 000 pages, no backup running). A plain re-run then released 29.8 GB in the TRUNCATEONLY it
  had just called fatal. Fix: decide phase A by result rather than by message — re-read the file
  size, and treat "released nothing" as information, continuing into the chunk loop, which exists
  precisely to move what TRUNCATEONLY cannot release. At minimum, run the tail walk before giving
  up so the failure names something.

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

- [x] **`RESUME` keeps the manifest's lock policy** (0.45.0) — `ddl.ResumeSQL` in
  `internal/ddl/control.go`, wired through `Capabilities.Options` so all three resume paths
  use it: `internal/run/monitored_runner.go` (the pressure loop), `resumeStatement` in
  `internal/run/engine.go`, and recovery. Tests: `TestResumeSQLCarriesWaitAtLowPriority`,
  `TestRunnerGetsLowPriorityOptions`, and the strengthened `TestResumeAdoptsOwnPausedResumable`.
  The entry deferred from 0.44.0 named only the sidecar path; the **pressure loop had the same
  bare RESUME**, and it is the path that fires most — once per transaction-log pause.
- [x] **Batched DML** ([BATCH-DML.md](BATCH-DML.md)) — `internal/run/batch_dml.go`,
  `batch_calc.go`; `batch_update` / `batch_delete` documented in `docs/operations.md`. See the
  follow-ups above for what its controller still owes.
- [x] **Object sizes before and after** ([OBJECT-SIZES.md](OBJECT-SIZES.md), 0.35.0) —
  `internal/mssql/indexes.go` (`TableStructureSizes`, `DisabledIndexes`),
  `internal/preflight/sizes.go`, `internal/run/sizes.go`, the size rendering in
  `internal/report/report.go` and the two `runs` columns in `history.go`. It also fixed two
  long-specified gaps: the preflight space check and the planner's `heap.max_size_mb` now count
  everything `ALTER TABLE … REBUILD` rewrites, not the heap alone. The probe behind the design's
  last open question is [OBJECT-SIZES-resume-probe.sql](OBJECT-SIZES-resume-probe.sql), its result
  [OBJECT-SIZES-ANALYSIS.md](OBJECT-SIZES-ANALYSIS.md).
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

1. **"No reaction is available on this target"**, from the field section above. It is the only
   entry here that prevents hours of production locking that cannot succeed, it needs no new
   read — edition, size and measured throughput are all already in hand — and the same pass
   naturally carries the free-space estimate for a compression rebuild, which is the other half
   of the same blind spot.
2. **`rebuild_max_size_mb` vetoing compression**, from the same section. It makes the
   maintenance planner answer "no compression" for every table over 50 GB — the only ones where
   it pays — and says so only in a free-text reason nobody aggregates. Even if the veto turns out
   to be the right call on Standard, storing the estimate and separating "chose not to" from
   "could not" are both small and both stop the history from misleading its next reader.
3. The batch DML stop-branch follow-up is the cheapest real gain here — three lines, and it makes
   an operation adaptive that currently is not. Take it the next time batch DML is in scope.
4. `remote-tui.md` is unblocked now that the step sink exists.
5. `TEMPDB-GUARD.md` is cross-cutting (it serves `SORT_IN_TEMPDB` rebuilds, shrink and batched DML
   alike), so it is the one whose value grows with every driver added.
6. `WAIT-OBSERVABILITY.md` is the smallest of the three iterations and depends on nothing.

## Context

The original specs were born from the compression trial
`01.to_run/030_compress_exampledb_indexes.yaml` (74 PAGE indexes on `EXAMPLEDB`, Standard edition,
so offline rebuilds). See also `docs/llm-operator-guide.md` and the
`.claude/skills/sqlgopace-operator/` skill.
