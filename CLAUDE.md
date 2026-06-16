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
  English (the spec docs under `specs/` and `formation/` are the exception).
- **Manifest-driven, never raw SQL.** Adding a DDL capability means adding an `operation` type
  end-to-end (parse → resolve → generate → plan), not parsing user SQL.
- **No query timeout.** Operation duration is governed by the monitoring loop and the reaction
  hierarchy, never a fixed timer. Don't add `context.WithTimeout` around the executing DDL.
- **Secrets via `${VAR}`** from `.env` (gitignored); never put credentials in `config.yaml`.
- **Version** lives in `internal/version/VERSION` (embedded with `//go:embed`). Bump that file,
  no build flags. CI can override via `-ldflags "-X ...internal/version.override=X.Y.Z"`.
- **Windows binary lock**: `bin/sqlgopace.exe` is locked while running — stop a running instance
  before rebuilding to the same path.

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
- **`recovery.go`** (`Recoverer`) reconciles anything left in `02.processing/` after a crash
  (adopt a live op, resume a paused resumable, or requeue). Database-aware: each in-flight op
  records its database; an orphan whose DB is unreachable (e.g. now an AG secondary) is left
  for a later run.
- **`executor.go`** (`ServerSampler`) reads the sampling DMVs; `preflight.go` adapts the checker.

### SQL-touching adapters & I/O

- **`internal/mssql`** — connection (`conn.go`), target `Detect` (`server.go`), DMV reads
  (`dmv.go`, `waits.go`, `analysis.go`, `indexes.go`, `databases.go`, `recovery.go`). This is the
  only package that issues SQL directly; everything DB-specific lives here behind interfaces the
  core consumes.
- **`internal/config`** — `config.yaml` parsing + `${VAR}` env injection.
- **`internal/preflight`** — pre-run checks (free space, tempdb, AG send-queue).
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
and `SHRINK.md` (in-progress feature). These are the source of truth for intended behaviour —
consult the relevant spec before changing engine, planner, or reaction semantics.
