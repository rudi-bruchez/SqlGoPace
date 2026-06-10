# End-to-end & integration testing

Most of SqlGoPace is unit-tested without a database (the pure DDL core, option
resolution, reaction logic, queue, recovery decisions, report rendering). The
parts that genuinely talk to SQL Server — connection management, target
detection, DMV reads, KILL/PAUSE/RESUME, and the full run pipeline — are covered
by **integration tests** guarded by the `integration` build tag, plus an
**end-to-end test** that drives the real CLI run path.

These tests are skipped unless `SQLGOPACE_TEST_DSN` is set.

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

## Running by hand

```bash
export SQLGOPACE_TEST_DSN='sqlserver://sa:Str0ng_Passw0rd!@localhost:1433?database=tempdb&encrypt=disable'
go test -tags=integration ./...
```

The DSN is a `go-mssqldb` connection string (URL or ADO form). `encrypt=disable`
is convenient against a throwaway local container; production uses `encrypt=true`.

## What is covered

- **`internal/mssql`** (`integration_test.go`): `Open`, target `Detect`, `SPID`,
  `LogSpace`.
- **`cmd/sqlgopace`** (`e2e_integration_test.go`, `TestE2ERebuildIndex`): seeds a
  table + nonclustered index, writes a manifest and a `config.yaml`, runs the
  real `cli(--config …)` path (connect → detect → preflight → plan → execute under
  monitoring → move to `03.done` + write the run log), and asserts the manifest
  landed in `done` with its `.log`. Cleans up the table afterwards.

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
