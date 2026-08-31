# Contributing

Thanks for looking. SqlGoPace runs DDL against production SQL Server instances,
so the bar for a change is less "does it work" than "does it still behave when
the server pushes back". What follows is what that means in practice.

## Getting a build and the tests green

```bash
make build      # -> bin/sqlgopace
make test       # unit tests with -race, no database needed
make vet
make lint       # golangci-lint, config in .golangci.yml
gofmt -l .      # must print nothing; CI fails on it
```

The pure core (manifest parsing, option resolution, T-SQL generation, reaction
and recovery decisions, the queue) is unit-testable without a database, and most
changes belong there.

## Testing against a real server

Anything that touches SQL Server is behind the `integration` build tag and is
skipped unless `SQLGOPACE_TEST_DSN` is set.

```bash
make e2e        # docker compose up SQL Server 2022, run, tear down
make e2e CONTAINER=podman COMPOSE="podman compose"
```

These tests mutate the target database. Point the DSN at a throwaway instance,
never at anything you care about. `docs/e2e.md` covers the login the suite needs
and `docs/permissions.md` covers permissions generally.

If your change fixes a bug that only shows when the tool runs, the fix needs an
integration test, not a unit test that models the bug. Several defects in this
repository's history passed a green unit suite and were found by driving the
tool.

## Running CI locally

The workflow can be run on your machine with [act](https://github.com/nektos/act),
which is worth doing before a push when you have touched `.github/workflows/` or
`.golangci.yml`: a broken lint job is invisible from a green local `make test`, and
this repository once spent three weeks red for exactly that reason.

With Podman, start its Docker-compatible socket first:

```bash
systemctl --user start podman.socket
export DOCKER_HOST="unix://${XDG_RUNTIME_DIR}/podman/podman.sock"

act pull_request -j lint       -P ubuntu-latest=catthehacker/ubuntu:act-latest
act pull_request -j build-test -P ubuntu-latest=catthehacker/ubuntu:act-latest
```

The runner image is about 1.7 GB on first use. Each job then takes under a minute.
This exercises the real actions, so it catches what running the underlying commands
by hand cannot: an action version that refuses to start, or a linter binary that
does not match the configuration schema.

## Conventions that are not obvious

Plain idiomatic Go, and the simplest thing that works. No layer, interface,
generic or option that the present need does not justify. Code should read like
the code around it.

Manifest-driven, never raw user SQL. Adding a DDL capability means adding an
`operation` type end to end (parse, resolve, generate, plan), not parsing SQL
that a user supplied. The two `set_raw` and `where_raw` fields on batched DML are
the deliberate exception and they are not a precedent.

No query timeout. Operation duration is governed by the monitoring loop and the
reaction hierarchy, never by a fixed timer. Do not wrap the executing DDL in a
`context.WithTimeout`.

English only, in code, comments, identifiers, file names and committed docs, the
design documents under `docs/specs/` included. US spelling.

The version lives in `internal/version/VERSION` and is embedded with `go:embed`.
Bump that file, do not add build flags.

Secrets come from the environment or an optional `.env`, expanded into
`config.yaml` through `${VAR}`. Never put a credential in a committed file.

Never commit an identifier from a real engagement. Database, server, table,
index, login, domain and company names from a client belong nowhere in this
repository, including in tests, specs and design documents. A blocking chain
from a real incident is the most convincing thing to quote and the easiest way
to leak a name, so anonymize as you write rather than in a later pass. Keep the
shape of the incident, which is what makes it legible: session ids, wait types,
chain depth, elapsed times. Drop the names. Use `PRODDB`, `dbo.MEASUREMENT`,
`PK_MEASUREMENT`, `SQLPROD01`, `CORP\svc_sqlagent`. Once it is committed, it is
in the history.

## Claims, and how to earn one

A statement about what SQL Server does belongs in a comment or a document only
once it has been run. The repository is full of measured claims that carry their
message number and the version they were measured against, because more than one
plausible reading of the documentation turned out to be wrong when tried. If you
add a restriction, a bound or a permission requirement, measure it and say what
you measured it against.

Then break your own fix and watch the test fail. A test that passes both with
and without the change under it is not pinning anything, and that has happened
here often enough to be worth the extra minute.

## Commits and pull requests

One coherent change per commit, with a message whose body explains why rather
than listing what changed. The subject follows the `type(scope): summary`
convention already in the history. No attribution trailers.

A pull request should say what you measured, and against which SQL Server
version and edition when the change touches generated T-SQL or permissions.
Mention explicitly anything you could not test, which is more useful than
silence.

## Where the design lives

`docs/specs/` holds the design documents and is the source of truth for intended
behaviour: `SPECS.md` for the engine, `MAINTENANCE.md` for the `plan`
subcommand, `SHRINK.md` for the shrink driver. Read the relevant one before
changing engine, planner or reaction semantics. `CLAUDE.md` carries the same
conventions in the form the repository's tooling reads.
