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
