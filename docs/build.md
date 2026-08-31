# Building & versioning

## Prerequisites

- Go 1.26+ (`go version`).
- The module fetches its dependencies on first build (`go mod download` to pre-fetch).

## Building

```bash
go build -o bin/sqlgopace ./cmd/sqlgopace
# or, equivalently:
make build
```

`make build` writes the binary to `bin/sqlgopace` (`bin/sqlgopace.exe` on Windows). On
Windows the binary file is locked while the program runs — stop a running instance before
rebuilding to the same path, or build to a different `-o` path.

Other useful targets (see the `Makefile`):

```bash
make test     # unit tests with the race detector, no database needed
make vet      # go vet
make lint     # golangci-lint
make build    # compile the CLI to bin/
make clean    # remove bin/ and coverage output
```

## Versioning

The version is not a hard-coded constant. It lives in a single file that is embedded
into the binary at build time:

```
internal/version/VERSION      # e.g. 0.1.0
```

`internal/version/version.go` embeds it with `//go:embed` and exposes `version.Version()`.
The CLI uses it in two places:

- `sqlgopace --version` prints `sqlgopace <version>`;
- every run writes a `-- sqlgopace <version>` banner at the top of its output / `.log`, so
  each run record states which build produced it.

### Bumping the version

1. Edit `internal/version/VERSION` (e.g. `0.1.0` → `0.2.0`). One line, trailing newline is
   fine — the value is trimmed.
2. Rebuild (`make build`). No build flags are required.
3. Commit the change: `git add internal/version/VERSION`.

```bash
$ sqlgopace --version
sqlgopace 0.2.0
```

### Overriding at build time (release pipelines)

A release build can stamp a version without editing the file, via `-ldflags`. The override
takes precedence over the `VERSION` file when non-empty:

```bash
go build \
  -ldflags "-X github.com/rudi-bruchez/SqlGoPace/internal/version.override=1.2.3" \
  -o bin/sqlgopace ./cmd/sqlgopace
```

This is handy to inject a tag or commit-derived version in CI (for example
`-X ...version.override=$(git describe --tags --always)`), while local developer builds keep
using the checked-in `VERSION` file.

## Cross-compilation

Go cross-compiles without a toolchain switch. The driver and SQLite dependencies used here
are pure Go, so no CGO is required:

```bash
# Linux amd64 binary from any host:
GOOS=linux GOARCH=amd64 go build -o bin/sqlgopace-linux ./cmd/sqlgopace

# Windows amd64:
GOOS=windows GOARCH=amd64 go build -o bin/sqlgopace.exe ./cmd/sqlgopace
```

Combine with the `-ldflags` override above to produce versioned release artifacts.

## Releasing

Releases are cut by pushing a tag. `.github/workflows/release.yml` cross-compiles every
target on one runner, packages each with `LICENSE` and `README.md`, writes a `sha256`
checksum file, and creates the GitHub release with generated notes.

```bash
# 1. bump the version and commit it
$EDITOR internal/version/VERSION        # 0.16.0 -> 0.17.0
git commit -am "chore(version): bump to 0.17.0"

# 2. tag and push
git tag v0.17.0
git push origin main --tags
```

The tag must match `internal/version/VERSION`, prefixed with `v`. The workflow checks this
first and fails the release rather than publishing binaries that report a version nobody
can find. `workflow_dispatch` re-runs the build for an existing tag, which is how you
recover from a failure after the tag is already public.

Targets built: `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`,
`windows/amd64`. Archives carry the binary alone, since `sqlgopace init` writes the
configuration templates from the executable itself.

Before publishing, the workflow unpacks the Linux archive it just built, checks the binary
reports the tagged version, runs `init` into a temporary directory and renders a dry run
from it. What is verified is the artifact a user downloads, not the tree it came from.

## See also

- [`docs/testing.md`](testing.md) — integration / end-to-end testing (Docker or Podman, or a remote
  server).
