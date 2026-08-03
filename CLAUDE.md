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
at a throwaway DB. See `docs/e2e.md` for required login permissions (`VIEW SERVER STATE` is
mandatory for monitoring).

## Conventions (non-obvious, enforced)

- **Idiomatic Go, KISS.** Write plain, idiomatic Go that reads like the surrounding code; favour
  the simplest thing that works over cleverness or premature abstraction. Don't add layers,
  interfaces, generics, or options the current need doesn't justify.
- **English only** — all code, comments, identifiers, file names, and committed docs are in
  English, **including the design docs under `specs/`**. The training material under
  `formation/` is the only exception (it is delivered in French). Two raw AI research dumps
  kept as historical source material — `specs/gemini-shrink.md` and `specs/SQL Server Shrink -
  Document de référence technique - Perplexity.md` — remain in French deliberately; they are
  inputs to a decision already made, not specs.
- **Manifest-driven, never raw SQL.** Adding a DDL capability means adding an `operation` type
  end-to-end (parse → resolve → generate → plan), not parsing user SQL.
- **No query timeout.** Operation duration is governed by the monitoring loop and the reaction
  hierarchy, never a fixed timer. Don't add `context.WithTimeout` around the executing DDL.
- **Secrets via `${VAR}`** from `.env` (gitignored); never put credentials in `config.yaml`.
- **Never commit client identifiers.** Real database names, server/host names, table and index
  names, logins, domains, and company names from a client engagement must not appear anywhere
  in the repo — not in code, tests, docs, `specs/`, or `docs/superpowers/` designs and plans.
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
  `AdjustStepMB`, clamp); `shrink.go` drives the loop, sampling between chunks and reacting (only
  `WAIT_AT_LOW_PRIORITY` is meaningful for a shrink). Reads come through the narrow `ShrinkReader`
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
- **`internal/preflight`** — pre-run checks (free space, tempdb, AG send-queue). Database- and
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

`specs/` holds the design docs: `SPECS.md` (core engine), `MAINTENANCE.md` (the `plan`
subcommand / maintenance planner, including multi-database mode §17), `IMPLEMENTATION.md`,
and `SHRINK.md` (the shrink driver — now implemented; see the `ShrinkRunner` notes above). These
are the source of truth for intended behaviour —
consult the relevant spec before changing engine, planner, or reaction semantics.
