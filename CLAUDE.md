# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

SqlGoPace is a resilient DDL task runner for Microsoft SQL Server, written in Go. It does
not run arbitrary `.sql`; it **generates** T-SQL from declarative YAML manifests, **injects**
the correct `WITH (...)` options for the detected version/edition, **monitors** locking and
transaction-log pressure while the DDL runs, and **reacts** with the least destructive
mechanism available (prefer `WAIT_AT_LOW_PRIORITY` → `RESUMABLE` pause/resume → `KILL`).

The `README.md` is the canonical user-facing reference (manifest format, supported operations,
flags, subcommands). Read it before changing CLI surface or manifest semantics.

## Commands

```bash
make setup      # linter at the CI-pinned version + pre-push hook (run once per machine)
make setup-check # report what setup is missing, change nothing
make build      # -> bin/sqlgopace (bin/sqlgopace.exe on Windows)
make test       # unit tests with -race, NO database needed
make vet
make lint       # golangci-lint (config in .golangci.yml)
make cover      # coverage profile + func summary

# Single test / package:
go test -race ./internal/ddl -run TestResolve
go test -race ./internal/run -run TestProcessAll
```

Integration & e2e tests talk to a real SQL Server and are guarded by the `integration` build
tag; they are skipped unless `SQLGOPACE_TEST_DSN` is set.

```bash
make e2e                # docker compose up SQL Server 2022 -> run -> tear down
make integration E2E_DSN='sqlserver://user:pass@host:1433?database=Test&encrypt=true'
make e2e CONTAINER=podman COMPOSE="podman compose"   # Podman instead of Docker
```

The e2e tests **mutate the target database** (create/drop `dbo.sqlgopace_e2e`). Point the DSN
at a throwaway DB. See `docs/testing.md` for required login permissions (`VIEW SERVER STATE` is
mandatory for monitoring).

## Conventions (non-obvious, enforced)

- **Idiomatic Go, KISS.** Write plain, idiomatic Go that reads like the surrounding code; favour
  the simplest thing that works over cleverness or premature abstraction. Don't add layers,
  interfaces, generics, or options the current need doesn't justify.
- **English only, with no exception in the repository.** All code, comments, identifiers, file
  names, and committed docs are in English, **including the design docs under `docs/specs/`**.
  The two raw AI research dumps kept as historical source material
  (`docs/specs/gemini-shrink.md`, `docs/specs/shrink-reference-perplexity.md`) were translated
  in the 2026-08-31 documentation pass; they are inputs to decisions already made, not specs,
  and they carry a header saying so. The only French thing the project produces is the
  *delivered* training material under `formation/` (gitignored), which is written in French
  because it is taught in French; the command that generates it is in English.
- **Manifest-driven, never raw SQL.** Adding a DDL capability means adding an `operation` type
  end-to-end (parse → resolve → generate → plan), not parsing user SQL.
- **No query timeout.** Operation duration is governed by the monitoring loop and the reaction
  hierarchy, never a fixed timer. Don't add `context.WithTimeout` around the executing DDL.
- **Secrets via `${VAR}`** from `.env` (gitignored); never put credentials in `config.yaml`.
- **Never commit client identifiers.** Real database names, server/host names, table and index
  names, logins, domains, and company names from a client engagement must not appear anywhere
  in the repo — not in code, tests, docs, `docs/specs/`, or `docs/specs/superpowers/` designs and plans.
  Findings from a live campaign are exactly where these leak in, because a DMV screenshot or a
  blocking chain is the most convincing motivation to quote. Anonymize at the moment of writing,
  not in a later cleanup pass: once committed, the name is in the history. Use neutral
  placeholders — `PRODDB` for a database, `dbo.MEASUREMENT` / `PK_MEASUREMENT` for objects,
  `SQLPROD01` for a host, `CORP\svc_sqlagent` for a service account. Keeping the *shape* of a
  real incident (session ids, wait types, chain depth, elapsed times) is encouraged; keeping its
  names is not.
- **Version** lives in `internal/version/VERSION` (embedded with `//go:embed`). Bump that file,
  no build flags. CI can override via `-ldflags "-X ...internal/version.override=X.Y.Z"`.
- **Windows binary lock**: `bin/sqlgopace.exe` is locked while running — stop a running instance
  before rebuilding to the same path.
- **Simplify after building.** After a feature lands (and tests pass), run a `/simplify` pass over
  the diff before committing — collapse the layers/duplication that accrete during development.
  KISS is easier to enforce in cleanup than to maintain while building.
- **Docs and CHANGELOG move with the code, every session.** Before committing, walk the whole
  documentation surface a change touches — not just the file that was easiest to spot. In
  practice that means: `CHANGELOG.md` (a new `## [x.y.z]` section whenever `internal/version/
  VERSION` is bumped — the project moves the minor for a behaviour change, not only for a
  feature), the **living** design doc under `docs/specs/` (`SHRINK.md`, `MAINTENANCE.md`,
  `SPECS.md` — these state intended behaviour, so a stale one silently becomes a lie), the
  operator-facing page in `docs/` (`shrink.md`, `configuration.md`, `running.md`, …), and
  `config.yaml` **plus its embedded twin** `internal/scaffold/assets/config.yaml`, which a test
  pins byte-for-byte. When a default or a semantic changes, say so as a migration note: an
  operator whose `config.yaml` sets the old value explicitly needs to be told to revisit it.
- **Write CHANGELOG entries sober and short.** One bullet per change, a few lines, in the style
  of the `0.16.0` section: what changed, the symptom or mechanism that made it wrong, and what
  an operator must do. No sub-paragraphs under a bullet, no bold headings inside one, no essay
  on the reasoning — that belongs in the spec or the commit message. Facts earn their space
  (a message number, a symbol, a measured figure); restating the docs or explaining why a
  decision was hard does not. A migration note is the one thing never to cut: if a config or
  manifest stops loading, say so and name the key.
  Two categories are deliberately **not** updated: the raw AI research dumps
  (`docs/specs/gemini-shrink.md`, `docs/specs/shrink-reference-perplexity.md`) and the
  implementation plans (`docs/specs/*-IMPL.md`, `docs/specs/superpowers/`), which are historical
  records of a decision, not statements of current behaviour. Supersede a living spec in place
  with a short note saying what changed and why the old rationale no longer holds — future
  readers need the reasoning that was abandoned, not just the rule that replaced it. The same pass
  covers `docs/specs/TODO.md` — see the backlog section below; anything you deliberately left
  undone goes there, with its reasoning, before the session ends.
- **Three audits walk a type instead of reading a diff; none is a `/simplify` candidate.**
  `internal/config/audit_test.go` walks `Config` by reflection rather than reading a diff, because
  the two defect classes it covers survived both TDD and a diff-scoped review.
  `TestNoInertConfigKey` fails on a key nothing outside `internal/config` reads, directly or
  through an accessor — applying a default to a key is not using it, which is how
  `checkpoint_between_operations` shipped parsed, documented and dead.
  `TestShippedConfigStatesTheRealDefaults` compares the shipped `config.yaml` against a minimal
  one, so a key the file presents as documentation cannot quietly mean something else when an
  operator deletes it. **Neither is dead weight and neither is a candidate for a `/simplify`
  pass.** The entries in `documentedDivergences` marked `OPEN` are known defects, not blessed
  exceptions: they are listed in `docs/specs/TODO.md` and the map empties as the defaults land.
  A defect class found twice belongs here as a test, not in a sixth review.
  The third is `internal/tui/harm_audit_test.go`, which ranks every console `ActionKind` by
  what it costs and whom, measures the gate by driving `Model.Update`, and fails when a more
  harmful action is reachable with a weaker gesture than a less harmful one. That class had
  been fixed by hand four times, one per release (0.23.0, 0.24.0, twice in 0.28.0), because
  it is invisible in a diff — each handler is correct on its own terms — and invisible to
  TDD, which asserts that a key does what it was meant to do. **Adding an `ActionKind`
  means ranking it there**; the harm ordering is stated in that file because the code states
  it nowhere, so argue with the ordering rather than deleting the row.

- **Lint config is golangci-lint v2** (`.golangci.yml`, `version: "2"`). errcheck/govet/staticcheck/
  ineffassign/unused are in the v2 default set and are not listed. Comments/identifiers use **US
  spelling** (normalized in 46cf1f4).

## Architecture

CLI dispatch is in `cmd/sqlgopace/main.go` (`cli()` parses flags and routes). Three entry paths:
default run, the `plan` subcommand (`plan.go`/`scope.go`), and `abort-resumable` (`abort.go`).

The codebase splits into a **pure core** (unit-testable, no DB) and **SQL-touching adapters**.

### Pure core

- **`internal/ddl`** — the generation pipeline, all pure functions over data:
  `ParseManifest` → `Resolve` (decide eligible options from `Target` + `Matrix` + `Policy`,
  emitting `Decision`s that power `--explain`) → `Generate` (build the T-SQL) → `Plan`
  (`PlannedOperation` per op). `matrix.go` loads `ddl_compatibility.yaml` (option eligibility
  keyed by SQL major version × edition `Tier`, with `requires` dependencies). `expand.go`
  turns `index: ALL` into one op per index. `control.go` builds RESUMABLE pause/resume SQL.
  `shrink.go` parses `targetfreespace` (percent or absolute MB) and builds per-chunk
  `DBCC SHRINKFILE` SQL — the only op whose statements are generated at run time, not up front.
- **`internal/maint`** — maintenance *decisions*: `profile.go` parses `maintenance_profile.yaml`;
  `decide.go` turns DMV facts + thresholds into maintenance operations (reorganize/rebuild,
  compression, heaps, statistics, checkdb). The `plan` subcommand feeds this into `ddl`/`render`
  to emit reviewable manifests into the queue — nothing executes until run through the engine.
- **`internal/run`** reaction & recovery logic: `reaction.go` (`DecideReaction(Pressure, Capabilities)`),
  `recovery.go` (`DecideRecovery(RecoveryFacts)`), `queue.go`, `state.go` are pure/testable.

### Orchestration — `internal/run`

- **`engine.go`** (`Engine.ProcessAll` → `processOne` → `finalize`) drives one manifest at a time
  through the queue: discover → preflight → expand → execute under monitoring → move to
  `done`/`failed` + write `.log` sidecar + record history. `Engine` is wired with functional
  `WithX` options (`WithProgress`, `WithWaits`, `WithHistory`, `WithDatabase`, ...).
- **`monitored_runner.go`** (`MonitoredRunner`) runs each statement while `pump` samples the
  server; `runLoop` applies the reaction decision (continue / wait for relief / cancel / pause →
  resume). Pausing a resumable is done by **aborting the running statement** (cancel), then
  resuming — not a separate `ALTER INDEX PAUSE`.
- **`shrink.go` + `shrink_calc.go`** (`ShrinkRunner`) is a **second, parallel driver**: a shrink is
  a chunked `DBCC SHRINKFILE` loop, not one DDL statement, so it does not go through
  `MonitoredRunner`. The engine routes `ddl.Shrink` ops to whatever satisfies `ShrinkDriver`
  (wired via `WithShrinkRunner`; `*ShrinkRunner` is the production impl) — `processOne` branches on
  `step.Operation.(ddl.Shrink)`. `shrink_calc.go` holds the pure step-size math (`InitialStepMB`,
  `AdjustStepMB`, clamp); `shrink.go` drives the loop, sampling between chunks and reacting
  (`WAIT_AT_LOW_PRIORITY`, plus the `max_block_minutes` safety cap — applied by the chunked
  path and, since 0.30.0, by `runWatchedStatement` for the two unchunked statements —
  `shrink_log` and the `TRUNCATEONLY` pass). Reads come through the narrow `ShrinkReader`
  interface (`*mssql.Conn` in prod, fakes in tests).
- **`recovery.go`** (`Recoverer`) reconciles anything left in `02.processing/` after a crash
  (adopt a live op, resume a paused resumable, or requeue). Database-aware: each in-flight op
  records its database; an orphan whose DB is unreachable (e.g. now an AG secondary) is left
  for a later run.
- **`executor.go`** (`ServerSampler`) reads the sampling DMVs; `preflight.go` adapts the checker.

### SQL-touching adapters & I/O

- **`internal/mssql`** — connection (`conn.go`), target `Detect` (`server.go`), DMV reads
  (`dmv.go`, `waits.go`, `analysis.go`, `indexes.go`, `databases.go`, `recovery.go`,
  `shrink.go` — file-space/size + log-reuse reads for the shrink driver). This is the
  only package that issues SQL directly; everything DB-specific lives here behind interfaces the
  core consumes.
- **`internal/config`** — `config.yaml` parsing + `${VAR}` env injection.
- **`internal/preflight`** — pre-run checks (server, log, blocking, permissions, data free
  space, file autogrowth, the batched-DML whole-table guard and `key_range` key). It owns the
  rules a manifest must satisfy *before* the engine moves it to `02.processing/`; a rule the
  driver also needs at run time lives here and is called from there (`KeyRangeColumn`), never
  duplicated. Database- and
  file-scoped operations (`check_db`, `shrink`) have empty `Schema`/`Table`, so they **skip the
  `schema.table` existence check** in both `CheckOperation` and `objectExistence` — the engine
  validates the database/file at run time (fixed in 028602a; was failing with `table [].[] does
  not exist`).
- **`internal/report`** — `.log` run reports, optional SQLite history (`history.go`), webhook
  `notify.go`.
- **`internal/tui`** — Bubble Tea incident console (`--tui`); `cmd/sqlgopace/main.go` feeds it.

### Manifest queue lifecycle

```
01.to_run/  →  02.processing/  →  03.done/  (+ .log)
                              ↘   04.failed/ (+ .log)
```

Directories are configurable in `config.yaml`. The `.log` sidecar and SQLite history record
which `--version` produced each run.

## Where the specs live

`docs/specs/` holds the design docs: `SPECS.md` (core engine), `MAINTENANCE.md` (the `plan`
subcommand / maintenance planner, including multi-database mode §17), `IMPLEMENTATION.md`,
and `SHRINK.md` (the shrink driver — now implemented; see the `ShrinkRunner` notes above). These
are the source of truth for intended behaviour —
consult the relevant spec before changing engine, planner, or reaction semantics.

## The backlog — `docs/specs/TODO.md`

**Read it when the user asks what to work on next, and whenever a session opens in an area it
covers.** It is the one place recording work that is known, wanted, and not done: designed
iterations awaiting implementation, and follow-ups deliberately scoped out of a change that
shipped. Each entry says *why* it was deferred — that reasoning is what decides whether it is
still the right call, so quote it when proposing the work rather than just the title.

It also carries a **Shipped** section naming the evidence in the tree for each finished item.
Check it before proposing anything: the file was once stale enough to list four implemented
features as "not coded yet", which is how an assistant ends up offering to rebuild what already
exists.

**Keep it current, in both directions.** When you defer something while building — a
special case left alone, a sibling that shares the defect you just fixed, a test you chose not to
write — add it with its reasoning before the session ends, or it is lost. When work lands, move
its entry to *Shipped* with the file that proves it, rather than deleting the line. Take the same
care with client identifiers here as everywhere else.
