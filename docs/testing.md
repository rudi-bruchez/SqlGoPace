# Testing

Most of SqlGoPace is unit-tested without a database (the pure DDL core, option
resolution, reaction logic, queue, recovery decisions, report rendering). The
parts that genuinely talk to SQL Server — connection management, target
detection, DMV reads, KILL/PAUSE/RESUME, and the full run pipeline — are covered
by **integration tests** guarded by the `integration` build tag, plus an
**end-to-end test** that drives the real CLI run path.

These tests are skipped unless `SQLGOPACE_TEST_DSN` is set.

## Unit tests, no database

```bash
make test     # go test -race ./...
make vet
make lint     # golangci-lint, config in .golangci.yml
gofmt -l .    # must print nothing
```

This is the suite to run while working. It needs nothing but Go, and it covers the
generation pipeline, option resolution, the reaction and recovery decisions, the queue and
the report rendering. [`../CONTRIBUTING.md`](../CONTRIBUTING.md) also explains how to run
the CI workflow itself locally with `act`, which is worth doing before touching
`.github/workflows/` or `.golangci.yml`.

## Quick start (Docker)

```bash
make e2e-up      # start SQL Server 2022 (Developer) and wait until healthy
make e2e-test    # run the integration + e2e tests against it
make e2e-down    # tear everything down

# or the whole cycle in one go:
make e2e
```

`make e2e-up` uses `docker-compose.yml` (SQL Server 2022 Developer, port 1433,
SA password `Str0ng_Passw0rd!`). Override the connection string with `E2E_DSN`:

```bash
make e2e-test E2E_DSN='sqlserver://sa:My_Pass1!@localhost:1433?database=tempdb&encrypt=disable'
```

## Podman instead of Docker

The container runtime is parameterized through the `CONTAINER` and `COMPOSE`
make variables (default `docker` / `docker compose`). To use Podman:

```bash
podman machine start    # Podman runs in a WSL2 VM on Windows; start it first
make e2e CONTAINER=podman COMPOSE="podman compose"
```

The Go tests connect over TCP to `localhost:1433`, and `podman machine`
forwards published ports to the host, so the tests themselves are
runtime-agnostic. Two things to know:

- `podman compose` needs a compose provider available (Podman 4.7+ delegates to
  the Docker compose plugin or to `podman-compose`).
- `make e2e-up` polls `'{{.State.Health.Status}}'`; older Podman versions expose
  the field as `.State.Healthcheck` instead, in which case the wait loop may not
  detect "healthy". If so, bring the server up by hand and run the tests directly
  (see [Running by hand](#running-by-hand)).

## Running by hand

```bash
export SQLGOPACE_TEST_DSN='sqlserver://sa:Str0ng_Passw0rd!@localhost:1433?database=tempdb&encrypt=disable'
go test -tags=integration ./...
```

The DSN is a `go-mssqldb` connection string (URL or ADO form). `encrypt=disable`
is convenient against a throwaway local container; production uses `encrypt=true`.

## Running against an existing / remote server

Nothing in the tests is tied to `localhost` — they are entirely driven by
`SQLGOPACE_TEST_DSN`. Point that DSN at any reachable SQL Server and a test
database of your choice. Skip `make e2e` / `e2e-up` / `e2e-down` (those manage
the local container) and run the tests directly, or via `make integration`:

```bash
make integration \
  E2E_DSN='sqlserver://user:pass@dev-sql.example.com:1433?database=SqlGoPaceTest&encrypt=true&trustServerCertificate=true'
```

Differences from the throwaway container:

- **`encrypt`** — a remote server usually requires `encrypt=true`; add
  `trustServerCertificate=true` if its certificate is not in your trust store.
- **`database=`** — point at a real **test database you accept mutating**, not
  necessarily `tempdb`.

**This is not read-only on the target database.** `TestE2ERebuildIndex` creates
`dbo.sqlgopace_e2e`, inserts ~1000 rows, creates `IX_sqlgopace_e2e`, runs a real
`ALTER INDEX … REBUILD` under monitoring, and drops the table on cleanup. Use a
dedicated test database — never a shared/sensitive one. The footprint is small
(the table is its own; the schema-modification lock only covers it).

### Required permissions for the login

For the full operation-by-operation reference and ready-to-run grant templates, see
[`permissions.md`](permissions.md) and [`permissions/`](permissions/). What follows is
what this test suite in particular needs.

- On the test database: create/drop tables and indexes, and insert — in practice
  `db_ddladmin` + `db_datawriter` (or `db_owner` on that test database).
- **`VIEW SERVER STATE`** (server level) — the monitoring connection reads
  server-scoped DMVs (active sessions, progress, log-space usage). Without it,
  sampling fails even on a clean rebuild.
- `ALTER ANY CONNECTION` (or sysadmin/processadmin) — only needed to exercise the
  `KILL` path (a blocker exceeding the timeout). Not required for a rebuild
  without contention.
- **`DBCC SHRINKFILE`** (the `shrink` operation / `TestE2EShrinkData`) requires
  `db_owner` on the test database (or `sysadmin`). `db_ddladmin` is **not** enough
  to shrink files. The shrink reads (file space, `sys.dm_db_log_info`) are covered
  by the database-level access already granted plus `VIEW SERVER STATE`. A login
  lacking `db_owner`/`sysadmin` now fails **preflight** with a clear `permissions`
  check (before any DBCC is issued), not with an opaque execution-time error. The
  same gate applies to `check_db` (DBCC CHECKDB also needs `db_owner`/`sysadmin`).
- **`shrink_tempdb`** needs `sysadmin`, and only that: its DBCC runs in tempdb, so
  `db_owner` of the connected database does not carry. Preflight gates it separately.

## What is covered

- **`internal/mssql`** (`integration_test.go`): `Open`, target `Detect`, `SPID`,
  `LogSpace`.
- **`cmd/sqlgopace`** (`e2e_integration_test.go`, `TestE2ERebuildIndex`): seeds a
  table + nonclustered index, writes a manifest and a `config.yaml`, runs the
  real `cli(--config …)` path (connect → detect → preflight → plan → execute under
  monitoring → move to `03.done` + write the run log), and asserts the manifest
  landed in `done` with its `.log`. Cleans up the table afterwards.
- **`cmd/sqlgopace`** (`TestE2EShrinkData`): grows the data file with a wide table
  then drops it, runs a `shrink` (`type: data`, `files: all`) manifest through the
  real CLI, and asserts a successful run carrying a per-file shrink summary in the
  log (a reduction or a no-op are both valid depending on the reclaimable space).
- **`internal/mssql`** (`shrink_integration_test.go`): `FileSpace`, `FileSizeMB`,
  `LogReuse`, `ActiveLogFloorMB` against the live database.

## CI

The `integration` job in `.github/workflows/ci.yml` runs the same tests against a
SQL Server service container on every push and pull request, after waiting for
the server to accept connections.

## Notes

- Tests create and drop `dbo.sqlgopace_e2e` in the connected database (the DSN
  points at `tempdb` by default), so they are self-contained.
- The SA password must satisfy SQL Server's complexity policy.
- To exercise the reaction paths (blocking → cancel/kill, log pressure,
  resumable pause/resume) interactively, open a second session that holds a lock
  on the target object while a rebuild runs, or watch it live with
  `sqlgopace --config config.yaml --tui`.
