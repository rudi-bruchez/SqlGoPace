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
