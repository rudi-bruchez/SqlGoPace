# Changelog

Notable changes to SqlGoPace. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[semantic versioning](https://semver.org/spec/v2.0.0.html) with the caveat that
it is pre-1.0: the minor version moves for features and for behaviour changes
alike.

This file starts at 0.16.0. Earlier work is recorded in the git history, which is
the honest record of it; reconstructing per-release entries after the fact would
mean inventing boundaries the repository never had, since no release was tagged.

The version a run used is written into its `.log` sidecar and into the SQLite
history, so a report can always name the build that produced it.

## [0.26.0] - 2026-09-01

### Fixed

- `batch.strategy: key_range` now requires the clustered key to be **unique**, and refuses the
  table otherwise. A batch covers the key range `(watermark, next]`, where `next` is the
  batch-size-th smallest matching key, and the generated `UPDATE` carries no `TOP`: the range
  holds `batch_rows` rows only when one key means one row. The clustered-key read selected
  `sys.indexes.index_id = 1` without reading `is_unique`, and SQL Server does not require a
  clustered index to be unique — so on `CREATE CLUSTERED INDEX IX ON T(EventId)` one batch
  could update every row sharing an `EventId`, in a single transaction. That escalates to a
  table X lock at 5,000 locks and is what batching exists to prevent; `escalation_cap_rows` did
  not constrain it, because it sizes the boundary scan, not the UPDATE. `docs/specs/BATCH-DML.md`
  specified the uniqueness requirement from the start; nothing enforced it.
  **Migration:** a `key_range` manifest against a table whose clustered index is not unique now
  fails preflight-style at the driver with a message naming the key. Switch it to
  `strategy: predicate`, which is bounded by `UPDATE TOP (n)`.
- `batch.key` no longer bypasses the composite-key guard. The guard lived only in the branch
  that *inferred* the key, so naming `batch.key` skipped it — and the inference error read
  "specify a single integer batch.key", which on a `(TenantId, Id)` clustered key sends the
  operator to the most duplicated column in the table. Both branches are now one path, and the
  message points at the `predicate` strategy instead. **Migration:** `batch.key` now asserts
  which column the clustered key is rather than selecting among several; naming a column that
  is not the clustered key is an error.

## [0.25.0] - 2026-09-01

### Fixed

- A batched DML that stops early is reported INCOMPLETE instead of as a success. Log
  pressure, blocking, or the `self_wait_timeout_minutes` budget end the loop with its
  committed batches preserved and a reason, but no error, so the engine finalized the
  manifest into `03.done/`: an operator draining a queue from cron saw a completed purge that
  had abandoned most of its rows. It now takes the same path a shrink that stalls has taken
  since 0.17.0 — `04.failed/`, labelled INCOMPLETE, counted separately in the run summary.
  A `key_range` walk also keeps its watermark in that case; it was cleared on any nil-error
  return, so the walk could not be resumed either. **Migration:** batched DML that used to
  land in `03.done/` after stopping short will now land in `04.failed/`. Re-run it to
  continue; the committed batches are not repeated. If you alert on `fail`, add `incomplete`
  (`notifications.on_events`).
## [0.24.0] - 2026-09-01

### Changed

- The incident console's `x` key now asks for confirmation, and the prompt names the login,
  host, application and open transaction count. The list `x` acts on is the sessions waiting
  *on* the DDL — the ones it is holding up — so killing one frees nothing and only discards
  that session's work, rolling back anything it had open. It fired on a single keystroke with
  no confirmation, ungated by `kill_blockers`, and with none of the six conditions
  `kill_amplifying_maintenance` requires before killing a victim automatically.

### Removed

- The console's `X` ("kill + auto-kill") key. It appended to `kill_blocking_sessions`, which
  `BlockerKiller` only ever matches against the session blocking *us*; a session we are
  blocking can never be that, so the rule was inert by construction while leaving the operator
  believing recurrences were handled. `docs/blocking-and-kills.md` uses that exact mix-up as
  its worked example of getting the direction backwards. **Migration:** to arm a kill rule
  against a session that blocks the DDL, use the roster (`b`), which writes a rule that can
  actually fire. Rules already written by `X` never did anything and can be deleted.
## [0.23.0] - 2026-09-01

### Changed

- `abort-resumable` now requires a target. It read `sys.index_resumable_operations` with no
  `WHERE` clause, so "every paused resumable in the database" was the only mode, reachable
  with nothing but `--config`: one command on a shared server aborted every colleague's
  in-flight index build, and SQL Server cannot resume an aborted operation. New `--table`
  (`schema.table`, or a bare table name) and `--index` filters select what to abort; `--all` is
  the explicit way to decline a target and needs `--yes`, as does `--include-running`, which
  kills the sessions building those indexes. `--dry-run` needs neither, so the review path
  stays the easy one. The header now prints the resolved scope before the first `ABORT`
  instead of warning after the decision was made. **Migration:** `sqlgopace abort-resumable
  --config c.yaml` is now an error rather than a database-wide abort; add `--table`/`--index`,
  or `--all --yes` for the old behaviour.
- The engine's "a paused resumable operation blocks this rebuild" error now spells out the
  full targeted command, since the bare one it used to suggest is refused.
## [0.22.0] - 2026-09-01

### Added

- A run now takes an exclusive lock on its `02.processing/` directory and holds it until it
  exits; a second run against the same directory refuses to start and names the process
  holding it. Concurrency was previously ruled out by a comment in `matchesOrphan` and
  nothing else. Crash recovery sweeps `02.processing/` before anything is claimed and infers
  that an abandoned manifest is dead from the absence of a running request on its session —
  which is also true of a live run awaiting relief, between shrink chunks, between DML
  batches, or between operations. A cron tick landing in one of those windows requeued a live
  peer's in-flight manifest and ran it: two offline rebuilds of one index at best, and with
  `abort_blocking_resumable` an `ALTER INDEX ... ABORT` against the peer's paused build, which
  SQL Server documents as unresumable. The lock is an OS file lock, so a killed run leaves no
  stale lock to clear by hand — which matters, because a crash is exactly when the next run
  has recovery to do. A queue on a filesystem that does not honour locks (NFSv3) is not
  protected, and two runs on different processing directories never interfere.
  **Migration:** a deliberately overlapping setup — two schedules sharing one queue — will now
  see the second run exit immediately rather than interleave. Give them separate
  `directories.processing` values if the overlap was intended.
## [0.21.0] - 2026-09-01

### Fixed

- `confirm_full_table` now means what the documentation always claimed. It was a presence
  test on a YAML key, so any filter that was *written* satisfied it however little it
  excluded: `where_raw: "1=1"`, or `where: [{column: Id, op: ">=", value: 0}]` on an identity
  column, deleted every row of the table with no confirmation, no warning and no row-count
  preview. Preflight now asks the server how many rows the filter would spare, counted up to
  1000: zero fails the manifest, one to 999 warns with the number, the cap passes. The probe
  is skipped when `confirm_full_table: true` is already set, so a confirmed whole-table
  operation costs nothing. **Migration:** a manifest whose filter excludes no row now fails
  preflight instead of running — add `confirm_full_table: true` if that was the intent. The
  probe needs `SELECT` on the table, which batched DML already required.
## [0.20.0] - 2026-09-01

### Fixed

- `batch_update` with `set: {Col: null}` no longer generates broken SQL. A YAML null left
  the literal at its zero value (go-yaml does not call `UnmarshalYAML` for a `!!null` node
  decoded into a value type), so the statement rendered as `SET [Col] = ` — a syntax error
  that failed the operation on its first batch. `null`, `NULL`, `~` and an empty scalar now
  all render as `NULL`, and the self-limiting clause emits `[Col] IS NOT NULL` rather than
  `[Col] IS NULL OR [Col] <> NULL`, which is `UNKNOWN` for every row and would have put each
  completed row straight back into the match set.
- A manifest rewrite no longer loses a NULL `set:` value. `MarshalManifest` renders a null
  literal as an empty scalar, which `compact()` dropped as carrying no information; that
  emptied the `set:` map and then dropped `set:` itself, so a rewritten `batch_update` either
  lost the column silently or stopped parsing with "set exactly one of set or set_raw".
  Affects every in-place rewrite: recovery manifests, the blocked-session capture, and the
  TUI's `X` key.

### Added

- A cumulative-row ceiling on the `batch_update` / `batch_delete` predicate loop. The loop's
  only exit was "the last batch affected zero rows", with no iteration, row or wall-clock
  bound, and every batch autocommits. A `set_raw` that does not consume its own filter —
  `set_raw: "Counter = Counter + 1"` with `where_raw: "Status = 'A'"` — matches the same rows
  forever; validation checks that *a* predicate exists, never that it is self-consuming, which
  raw SQL text does not allow. The ceiling is twice the table's row estimate, or 1,000,000 when
  the estimate is unavailable: a terminating predicate affects each row at most once, so
  crossing it proves the predicate is not self-consuming. The operation fails with the
  committed row and batch counts in its report. **Migration:** an existing non-self-consuming
  `set_raw` manifest that previously ran until interrupted will now fail instead; the fix is a
  `where_raw` the `set_raw` invalidates. `key_range` is unaffected — its watermark ascends, so
  it terminates by construction.
## [0.19.0] - 2026-09-01

### Changed

- `kill_blockers.enabled` now ships `false` in `config.yaml` and in the copy `sqlgopace
  init` writes. It shipped `true` while five documents and the comment six lines above the
  value itself said blocker-killing was off by default, so a scaffolded install had the
  master arm on the destructive path enabled without the operator choosing it. The Go
  default was always `false`; only the shipped file disagreed. **Migration:** if you
  scaffolded a `config.yaml` before 0.19.0, check `kill_blockers.enabled` in your copy — it
  is unchanged and may still be armed. Nothing fires without a per-manifest
  `kill_blocking_sessions` rule, so a queue with no such rule is unaffected.

### Fixed

- `README.md` claimed "no raw SQL is ever accepted or executed". Four manifest fields are
  interpolated verbatim: `set_raw` and `where_raw` on the batched-DML operations, `type` on
  `add_column` / `alter_column` (presence-checked only), and `data_compression` on
  `rebuild_index` / `create_index` / `rebuild_heap` (not validated at all — nothing
  restricts it to `NONE`, `ROW` or `PAGE`). The README now says a manifest is a trusted
  input and points at `SECURITY.md`, which named two of the four and now names all four.
- `docs/configuration.md`'s shipped-versus-default table listed two divergences and there
  were three: `monitoring.max_retry_attempts` ships `1` against a default of `0`. The same
  page described that key as retrying "after a recoverable failure"; it retries after a
  pressure cancel only, and a statement that fails is not retried.

## [0.18.0] - 2026-09-01

### Fixed

- A shrink now honours `options.max_block_minutes`. `resolveShrink` never read the
  override, so the value was parsed, validated and then dropped: `runChunk` saw
  `blockCap(0)`, which means no cap. The generic DDL path and batched DML resolved it
  correctly; only the shrink path did not. This is the backstop that bounds an
  allow-listed blocker, so a shrink carrying an `ignore_blocked_sessions` list had no
  backstop at all, and one that quietly tolerated such a blocker will now yield at the
  configured minute. The value is resolved for all three shrink command types, and applied
  by `shrink_data` and `shrink_tempdb`, which run chunked through `runChunk`. `shrink_log`
  issues a single statement outside that path and stays uncapped; that gap is recorded in
  `docs/specs/TODO.md`. The 0.17.0 entry below describes a chunk "cut short for blocking other
  sessions past `max_block_minutes`"; that path could not fire, and the chunks it observed
  were cut short by `monitoring.blocking_timeout_minutes`.
- `--explain` reports a shrink's `max_block_minutes`, as it already did for index DDL and
  batched DML. Its absence is why the dropped value went unnoticed.

### Added

- `preflight.require_data_free_space` is implemented. It sizes each `rebuild_index` /
  `rebuild_heap` target from `sys.dm_db_partition_stats` and compares it against the
  database's `ROWS` free space plus `max_size - size`: a rebuild builds the new index
  before dropping the old, so it needs roughly the object's own size. The peak requirement
  is the largest single rebuild, not their sum. An unreadable size never fails a run, and
  `create_index` is not checked because it cannot be sized before it exists. A rebuild
  that fits only by growing warns rather than passes, since the growth is a blocking
  zero-fill without instant file initialization.
- A `file growth` preflight check, advisory and never fatal. It warns on percentage
  autogrowth, naming what one event costs at the file's current size, and on a data file
  that cannot grow when the manifest contains a shrink. There is deliberately no
  "increment too large" warning: Microsoft's guidance contradicts itself on the threshold,
  so the increment is reported and the judgement left to the operator.

### Removed

- `preflight.check_tempdb` and `preflight.ag_send_queue_warn`. Neither ever did anything:
  all three `preflight` keys were parsed into `PreflightConfig` and read by nothing, while
  `docs/configuration.md` described them as working and the shipped `config.yaml` set them
  to `true`. Configuration is parsed with `KnownFields(true)`, so a `config.yaml` still
  carrying either key fails to load with `field check_tempdb not found` — delete the two
  lines. `docs/specs/TODO.md` tracked the tempdb guard as partially covered on the strength
  of the phantom key, and is corrected to unstarted.

## [0.17.0] - 2026-09-01

### Fixed

- The shrink chunk size can grow again. `AdjustStepMB` halved the step under I/O
  pressure but would only raise it when latency was under 5 ms **and** the chunk
  had finished inside `target_batch_seconds`. A multi-GB `DBCC SHRINKFILE` chunk
  takes minutes and that knob defaulted to 5 seconds, so the second condition was
  unsatisfiable and every reduction was permanent. On a busy instance — where a
  shrink, being itself a WRITELOG and PAGEIOLATCH_EX generator, trips the reduce
  thresholds routinely — the step ratcheted down to `min_step_mb` over the hours
  a large reclaim runs, and the run ended up paying the fixed per-invocation cost
  of `DBCC SHRINKFILE` a hundred times over instead of a dozen. Observed on a
  multi-TB data file that started at 8 GB chunks and finished at 50 MB ones,
  slowing down as it went.
- The step no longer freezes between the thresholds. Growth required latency
  below 5 ms while reduction started at 10 ms (WRITELOG) or 20 ms
  (PAGEIOLATCH_EX); a shrink sustaining a healthy 7 ms sat in the gap and could
  never move in either direction.
- A chunk the supervisor had to stop now shrinks the next one instead of growing
  it. The blocking dimension was meant to reach the controller through
  `WaitDeltas.BlockingSeconds`, which `waitDeltas` never populated, so the signal
  was always zero: a chunk cut short for blocking other sessions past
  `max_block_minutes` could still be read as "cheap" and double the step. It now
  reads the supervisor's own outcome.

### Changed

- The shrink stepsize controller is AIMD: halve on pressure or on a supervisor
  stop, hold once a chunk reaches the duration ceiling, otherwise grow by a
  quarter. Recovering one halving costs about 3.1 clean chunks, so the loop
  trends upward while pressure is rarer than roughly one chunk in three and
  settles below the pressure threshold otherwise. A residual bound is deliberate:
  a reduction is recoverable only while the resulting chunk still finishes inside
  the ceiling, so the descent stops at the step that takes about
  `target_batch_seconds` rather than at `min_step_mb`.
- **Breaking:** `shrink.target_batch_seconds` is renamed
  `shrink.max_chunk_seconds` and defaults to 300 instead of 5. **A `config.yaml`
  still carrying the old key under `shrink:` now fails to load**, naming the key.
  That is deliberate rather than an oversight: the meaning inverted, and defaults
  only fill *absent* keys, so accepting the old name would have silently kept the
  pre-fix behaviour on exactly the deployments the fix is for. Rename the key and
  reconsider the number — 5 was reasonable for a target, it is far too low for a
  ceiling. `batch_dml.target_batch_seconds` is untouched and keeps both its name
  and its meaning.
- The knob is now a **ceiling on growth, never a target**: a chunk longer than it
  is not corrected downward. The old name and the old 5-second default came from
  a rationale that no longer holds — short chunks meant short reaction latency,
  back when the driver could only react at a chunk boundary. Reactions, live
  `percent_complete` and the `max_block_minutes` cap all apply *inside* a chunk
  today, so a long chunk is neither blind nor unstoppable, and shrink work is
  preserved and re-entrant at any point.

## [0.16.0] - 2026-08-31

### Fixed

- `RESUMABLE` is no longer injected where SQL Server refuses it. It is rejected
  outright in `tempdb` (Msg 11439) on every version and edition, while `ONLINE`
  alone is accepted there, and it cannot be combined with `SORT_IN_TEMPDB`
  (Msg 11438, raised at compile time, so the batch fails before doing any work).
  Forcing `sort_in_tempdb` in `config.yaml` used to break every rebuild on 2017
  and later.
- Option resolution now knows which database it is resolving for.
  `ServerInfo.Target` carries the connection's database and `Target.InDatabase`
  applies a manifest's own, at both plan sites, so `--explain` describes what the
  run will actually do.
- `maxdop` is bounded. A manifest or a config forcing a value outside 0 to 32767
  was accepted and rendered as SQL the server rejects with Msg 304. Through the
  config the whole queue failed, one statement at a time.
- `--dry-run` expands `index: ALL` in the database the manifest names. Expansion
  reads `sys.indexes`, which sees only the connection's own database, so a
  manifest naming another database rendered the wrong index list while the run,
  which opens one engine per target database, executed the right one.
- Batched DML preflight requires `SELECT` as well as `UPDATE` or `DELETE`. A
  login with `db_datawriter` and no read right passed every check and failed
  mid-batch on "The SELECT permission was denied on the object".
- `shrink_tempdb` preflight requires `sysadmin`. The elevated-rights probe asked
  whether the login was `db_owner` in the connected database, but the DBCC runs
  in `tempdb`, so a `db_owner` of a user database passed and then failed on the
  first chunk with Msg 7983.
- Crash recovery survives one unreadable orphan. A transient read error aborted
  the whole sweep, leaving every orphan behind it unexamined and discarding what
  the pass had already reconciled, and a failing recovery returns before the
  engine starts.
- A failed fallback `KILL` and a failed watermark save are narrated instead of
  discarded. The wait after a fallback kill is unbounded, so silence left an
  operator watching a run that never returns with no way to learn why.

### Added

- `sqlgopace init` scaffolds a working directory: `config.yaml`, the
  compatibility matrix, the maintenance profile, a `.env.example`, the four queue
  directories and a disabled example manifest. The templates are embedded in the
  binary, so a downloaded executable is self-sufficient; installing used to mean
  fetching three YAML files by hand before anything could run. It never
  overwrites without `--force`, since an edited `config.yaml` is the one thing in
  the directory that cannot be regenerated.
- A release workflow. Pushing a `vX.Y.Z` tag cross-compiles Linux, macOS and
  Windows on Intel and ARM, publishes the archives with a `sha256` checksum file,
  and refuses to release when the tag and `internal/version/VERSION` disagree.
  Before publishing it unpacks the Linux archive, runs `init` from it and renders
  a dry run, so what is verified is the artifact a user downloads.
- A documentation tree under `docs/`, split by the question a reader arrives with:
  getting started, the manifest format, the operations, running, configuration,
  blocking and kills, shrink, the maintenance planner and the compatibility
  matrix, with `docs/README.md` as the map. The README is 174 lines instead of
  1050 and carries the pitch, one worked example and the links.
- `batch_update` and `batch_delete` are documented for the first time. They were
  implemented, tested and reachable, and appeared in no user-facing document.
- A second Claude Code skill, `sqlgopace-run`, for executing a manifest and
  reading what came back. Writing a manifest and running one against production
  are different jobs with different failure modes.
- `docs/permissions.md`, an operation-by-operation reference for the grants each
  operation needs, measured against SQL Server 2022 CU26 with restricted logins
  rather than inferred.
- `docs/permissions/`, one T-SQL template per privilege tier, plus
  `99-verify.sql`, which reports what an existing login can run today using the
  same probes preflight uses.
- `SECURITY.md`, `CONTRIBUTING.md` and this file.

### Changed

- `.env` is loaded by a copied stdlib-only `internal/dotenv` instead of the
  `github.com/joho/godotenv` dependency. A real environment variable still wins
  over the file, and a missing `.env` is still a silent no-op.
- `Engine.processOne` keeps the preparation and hands each operation to
  `Engine.runStep`, with the state that outlives one operation in a `manifestRun`
  value. No behaviour change: the moved body is byte for byte the original.
- CI fails on unformatted code.
- The repository is entirely in English. The two raw research documents that had
  been kept in French deliberately are translated, and the header of each now says
  what it is and that it is not maintained. One bibliography entry was dropped
  with them: a dead presigned S3 URL carrying an AWS access key id.
- `specs/` moved to `docs/specs/`, and the internal plans to
  `docs/specs/superpowers/`, so `docs/` holds documentation and the design
  material sits under it rather than beside the program.
- The lint job runs again. It had been red for weeks because the action resolved
  golangci-lint within the v1 line, built with an older Go than this module
  targets, against a v2 configuration; it now pins a v2-aware action and binary.
  The nine findings it had stopped reporting are fixed: eight US-spelling slips,
  and one deliberate typo in a fixture that is now exempted where it lives.

### Removed

- `tmp/sqlexec`, a throwaway helper that read a `config-local.yaml` absent from
  the repository and that `abort-resumable` has superseded.
- `docs/login.sql`, superseded by `docs/permissions/`.

[0.16.0]: https://github.com/rudi-bruchez/SqlGoPace/releases/tag/v0.16.0
