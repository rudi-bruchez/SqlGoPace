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

## [Unreleased]

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

[Unreleased]: https://github.com/rudi-bruchez/SqlGoPace/compare/main...HEAD
