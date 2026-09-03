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
  object's size, and a previous run's measured throughput is in the history DB. The verdict is
  computable *before the first statement*: "no reaction is available on this target; this
  operation is expected to exceed the blocking timeout; it will be cancelled".
  *Why it is worth doing rather than documenting:* the operator who most needs it is the one who
  wrote a manifest that looks exactly like a working one. Nothing in the manifest, the `--explain`
  output or the plan distinguishes an operation that will finish from one that structurally
  cannot, and the cost of finding out is hours of production locking for no result.
  *Open question the design has to answer:* what the tool should then do — refuse, warn, or
  reorder. Refusing is wrong for a genuine maintenance window where nothing is writing.

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
