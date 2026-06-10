# SqlGoPace — Implementation Plan

> All code, in-code comments, identifiers, and project files are written in **English**
> (international project). This plan derives directly from `specs/SPECS.md`.

## Guiding principles — idiomatic, KISS, elegant

These are the non-negotiable fundamentals. Every phase below must respect them; PRs that violate
them get reworked before merge.

- **Data over code.** SQL Server version/edition rules and DDL shapes live in YAML and typed data,
  never in sprawling `if/else`. Adding a 2027 version = one YAML line.
- **Sum types over fat structs.** Each operation kind is its own small struct behind a narrow
  interface; switch on concrete types, never on a discriminator string (see §3).
- **Small interfaces, defined by the consumer.** "Accept interfaces, return structs." No god
  `SQLServer` interface; the reaction loop declares the one or two methods it needs (see §6).
  Introduce an interface only when a real second implementation or test fake demands it (YAGNI).
- **Concurrency by communication, not by locks.** The three monitors are producers on a single
  `Event` channel; the orchestrator is the *sole* consumer and decision-maker. With one owner per
  piece of state, the design needs essentially **no mutexes**. `context.Context` is the only
  cancellation mechanism, first parameter everywhere.
- **Errors wrap, don't log-and-continue.** Return `fmt.Errorf("...: %w", err)`; a handful of
  sentinel errors (`ErrPreflightFailed`, `ErrLogDrainTimeout`, …) for control-flow decisions,
  matched with `errors.Is`/`errors.As` at the boundary. Log once, at the top.
- **Constructors with many knobs use functional options.** `run.New(cfg, conn, opts...)` instead of
  a 9-argument constructor or a mutable config struct.
- **Pure core, impure edges.** `ddl` does no I/O; all SQL Server and filesystem effects live at the
  edges. This is what makes the correctness-critical layer trivially testable.
- **Boring, readable Go.** `gofmt`/`goimports` clean, guard clauses + early returns, no naked
  returns in non-trivial functions, no premature abstraction. If a reviewer has to ask "why is this
  here", it isn't elegant yet.

## 1. Tech stack & key dependencies

| Concern              | Choice                                            | Rationale |
|----------------------|---------------------------------------------------|-----------|
| Language             | Go 1.23+                                          | Spec requirement |
| SQL Server driver    | `github.com/microsoft/go-mssqldb`                 | Official driver; per-connection control, `KILL`, `CONTEXT_INFO` |
| YAML                 | `gopkg.in/yaml.v3`                                | Config, manifests, compatibility matrix |
| Env loading          | `github.com/joho/godotenv` + `os.ExpandEnv`       | `.env` secrets → `${VAR}` substitution in `config.yaml` |
| Logging              | `log/slog` (stdlib)                               | Structured logs; JSON handler for `.log` machine block |
| TUI                  | `github.com/charmbracelet/bubbletea` + `lipgloss` | Incident console (`--tui`) |
| Run history          | `modernc.org/sqlite` (pure Go, no cgo)            | No CGO toolchain dependency on Windows |
| CLI flags            | stdlib `flag`                                     | Few flags; avoid heavy CLI framework |
| Testing              | stdlib `testing` + `github.com/google/go-cmp`     | Table-driven tests; `cmp.Diff` assertions |
| Lint                 | `golangci-lint`                                   | CI gate |

Apply the installed `golang-skills` throughout (naming, error handling, context, concurrency,
interfaces, testing, documentation).

## 2. Package layout

```
sqlgopace/
├── cmd/sqlgopace/main.go        # entry point, flag parsing, wiring, exit codes
├── internal/
│   ├── ddl/                     # manifest + matrix + sqlgen: the pure DDL core (no I/O)
│   ├── mssql/                   # connection mgmt, target detection, DMV reads, control commands
│   ├── preflight/              # pre-flight checks (§4 of SPECS)
│   ├── run/                    # orchestrator: file lifecycle, monitors, reaction state machine,
│   │                          #   crash recovery, sidecar state — the engine
│   ├── report/                # run-log (.log JSON+human), history (SQLite), notifications
│   └── tui/                    # Bubble Tea incident console
└── testdata/                    # sample manifests, matrix fixtures, golden T-SQL
```

Package count is deliberately small. Resist one-package-per-noun: tiny packages (`state`, `notify`,
`runlog`…) add import ceremony without cohesion. They live as files inside a cohesive parent
(`run/state.go`, `report/notify.go`). Split a package only when it earns its own name and tests.

The **pure DDL core** (`ddl`: manifest parsing, matrix resolution, T-SQL generation) has **zero I/O**
and is the most-tested layer. Dependency direction flows inward: `run` → `mssql`/`ddl`/`preflight`;
nothing imports `run`. `cmd` does the wiring.

## 3. Core domain types (shared vocabulary)

Operations are modeled as a **sum type**, not a single fat struct with mostly-empty fields. Each
operation kind is its own small struct with only the fields it needs; they satisfy a narrow
`Operation` interface. This keeps every type honest (no nil-checking irrelevant fields), makes
validation and SQL generation per-type, and reads cleanly.

```go
// Operation is the closed set of supported DDL operations.
// CommandType() returns the matrix key (e.g. "rebuild_index").
type Operation interface {
    CommandType() string
    Target() ObjectRef     // schema + table + object name, for logging/preflight
    Validate() error
}

type ObjectRef struct {
    Schema, Table, Name string
}

type RebuildIndex struct {
    Ref              ObjectRef
    All              bool          // expands via sys.indexes (one op per index)
    DataCompression  string        // "", "NONE", "ROW", "PAGE"
    Overrides        OptionOverrides
}

type AddColumn struct {
    Ref      ObjectRef
    DataType string
    Nullable bool
    Default  *string               // constant only in v1 (nil = no default)
}
// CreateIndex, AlterColumn, DropIndex, AddConstraint, ... — same shape.

type Manifest struct {
    Description string
    Database    string
    Operations  []Operation
}
```

YAML decodes into the right concrete type via a custom `UnmarshalYAML` on a small envelope: read the
`operation` discriminator first, then decode the rest into the matching struct. The orchestrator and
`sqlgen` switch on the concrete type (`switch op := op.(type)`), never on a string.

```go
// matrix — data, not code
type OptionRule struct {
    MinMajor int
    Editions []string
    Requires []string
}

// ResolvedOptions is the decision; the explanation trail lives beside it, not inside it.
type ResolvedOptions struct {
    Online, Resumable, WaitAtLowPriority, SortInTempDB bool
    MaxDOP *int
}

// Target — the detected server.
type Target struct {
    EngineEdition int
    MajorVersion  int
    Tier          Tier   // typed, not a bare string (see below)
    ADREnabled    bool
    RecoveryModel string
}

// Tier is a small typed enum, not a stringly-typed value.
type Tier int
const (
    TierEnterprise Tier = iota
    TierStandard
    TierExpress
    TierAzure
)
```

## 4. Implementation phases

Each phase is independently buildable and testable. Phases 1–2 require **no database**.

### Phase 0 — Scaffolding
- `go mod init`, package skeleton, `Makefile`/`Taskfile`, `.golangci.yml`, GitHub Actions CI
  (build + vet + lint + test).
- **Deliverable:** `go build ./...` and `golangci-lint run` green on empty stubs.

### Phase 1 — Config, manifest, matrix loading (pure)
- `config`: load `config.yaml`, run `os.ExpandEnv` after `godotenv.Load`, validate (required dirs,
  positive intervals, sane thresholds). Fail with clear errors → exit code 2.
- `manifest`: parse + **strict validation** per operation type (required fields, unknown operation
  type rejected). No silent defaults that change semantics.
- `matrix`: load `ddl_compatibility.yaml`; implement `IsApplicable(target, command, option)` with
  the rule `target_major >= min_major AND tier ∈ editions`, Azure pseudo-major handling, and
  EngineEdition→tier mapping (3 → enterprise covers Developer/Evaluation).
- **Tests:** table-driven, golden fixtures in `testdata/`. Cover edition gating, Azure evergreen,
  `requires` dependencies, unknown version above all `min_major`.
- **Deliverable:** load real `config.yaml` + `ddl_compatibility.yaml` from repo root.

### Phase 2 — SQL generation (pure)
- `sqlgen`: for each operation type, build the T-SQL string from typed fields + `ResolvedOptions`.
- Merge all options into a **single `WITH (...)`** clause; enforce dependencies (RESUMABLE⇒ONLINE;
  WALP only with ONLINE; WALP injects `ABORT_AFTER_WAIT = SELF`, or `BLOCKERS` only when
  `allow_abort_blockers`).
- Wrap idempotency guards (`IF NOT EXISTS` / `IF EXISTS`) where applicable.
- Quote identifiers safely (`[schema].[table]`), reject identifiers containing `]`/`;` etc.
- Produce the `--explain` decision trail alongside the SQL.
- **Tests:** golden-file tests of generated SQL per (operation × resolved options). This is the
  correctness heart of the tool — high coverage here.
- **Deliverable:** `--dry-run`/`--explain` can run end-to-end **without a server** once a target is
  supplied (allow a `--assume-version`/`--assume-edition` flag for offline dry-run).

### Phase 3 — SQL Server connection layer
- `sqlserver`: open the **dedicated execution connection** (`db.Conn`) and a **separate monitoring
  connection**. Set `SET XACT_ABORT ON; SET DEADLOCK_PRIORITY LOW;` on the exec session.
- Driver query timeout = 0 (no global timeout); `login_timeout` from config.
- Detect target: `EngineEdition`, `ProductMajorVersion`, ADR, recovery model. Capture `@@SPID` on
  the exec connection and write a `CONTEXT_INFO` GUID marker.
- Wrap DMV queries (log space, blocking with head-blocker walk, progress, resumable ops, HADR,
  tempdb, file sizes) as typed functions.
- **Tests:** integration tests behind a build tag `//go:build integration`, run against a
  Dockerized `mcr.microsoft.com/mssql/server` (Developer edition). Unit-test the SPID/CONTEXT_INFO
  correlation logic with a fake.
- **Deliverable:** connect + detect target + print server facts.

### Phase 4 — Preflight
- `preflight`: orchestrate all §4 checks; each check returns a structured result (pass/warn/fail).
- AG send-queue is **warn-not-fail** (configurable). Data/tempdb free-space estimate via
  `sys.allocation_units` (avoid `DETAILED` physical stats).
- Any **fail** → manifest goes straight to `04.failed/` with a `.log`, no locks taken.
- **Tests:** unit-test the decision aggregation with injected check results (interface seam);
  integration-test the real queries.

### Phase 5 — Orchestrator + monitoring + reaction state machine
- `monitor`: three independent pollers (`blocking_poll_seconds`, `log_poll_seconds`,
  `progress_poll_seconds`) emitting events on a channel. Use `context.Context` for cancellation
  (apply `go-context` + `go-concurrency` skills; protect shared state, no data races).
- `orchestrator`: sequential file processing in filename order; per operation runs the **reaction
  state machine**:
  `RUNNING → (pressure: blocking-timeout OR log-cap) → REACT → WAIT_RELIEF → RESUME/RETRY → DONE/FAIL`.
- Reaction selection (SPECS §9) by capability:
  1. resumable in progress → `PAUSE` / `RESUME`;
  2. WALP handles lock-acquisition (already injected); central-phase blocking → wait then escalate;
  3. cancel → `KILL` (Phase 6).
- ADR state biases the choice. Honor `max_retry_attempts`, `log_drain_timeout_minutes`,
  optional `CHECKPOINT` between operations (SIMPLE recovery only).
- **Tests:** drive the state machine with a mock SQL Server (interface over `sqlserver`) feeding
  scripted DMV snapshots — deterministic, no real DB. This is where most behavioral bugs hide.

### Phase 6 — Clean cancel / KILL / resumable PAUSE-RESUME
- Go context cancel first; if the request is still alive after `kill_grace_seconds`, issue
  `KILL <SPID>` on the monitoring connection.
- Poll `KILL <SPID> WITH STATUSONLY` to log rollback percent.
- Resumable: `ALTER INDEX ... PAUSE` / `RESUME`; never blind-restart.
- **Tests:** integration (Docker) — start a long rebuild, trigger blocking from a second session,
  assert PAUSE-then-RESUME and KILL-with-status paths.

### Phase 7 — Crash recovery
- On startup, scan `02.processing/`; for each, read `<name>.state.json` (SPID + login_time +
  CONTEXT_INFO GUID + exact command + start ts).
- Correlate with live sessions and `sys.index_resumable_operations`; decide
  resume / adopt-and-finish / kill+idempotent-retry / clean-restart.
- **Tests:** integration scenarios with simulated orphan sessions.

### Phase 8 — Output: run log, history, notifications, exit codes
- `runlog`: write `<name>.log` with a JSON block (slog JSON handler to a buffer) **and** a human
  summary. Move file to `03.done/` or `04.failed/`; delete sidecar on success.
- `history`: SQLite persistence (durations, retries, pauses, blockers, injected options, result).
- `notify`: webhook on `cancel|fail|pause|log_full|abort`.
- `main`: map outcomes to exit codes (0/1/2/3/4 per SPECS §16).
- **Tests:** golden `.log` output; history round-trip; notify with httptest server.

### Phase 9 — TUI incident console (`--tui`)
- Bubble Tea model: live progress (`percent_complete`, ETA, rollback %), live blocked-session list
  with detail, and confirmed actions: kill specific blocker / kill DDL / pause (if resumable) /
  extend timer / snapshot to log. Clear visual distinction between "kill blocker" and "kill DDL".
- TUI consumes the **same** monitor event stream as silent mode (no duplicated logic).
- **Tests:** Bubble Tea `teatest` for key interactions; action dispatch unit-tested against a mock.

### Phase 10 — End-to-end & hardening
- Docker-Compose harness: SQL Server + seeded large table; scripted scenarios (blocking, log
  pressure, AG simulation where feasible, crash-restart).
- Security pass: never log secrets/connection string; document least-privilege grants
  (`VIEW SERVER STATE`, `ALTER ANY CONNECTION`, object `ALTER`).
- Cross-compile Windows/Linux; release artifacts.

## 5. Testing strategy

- **Pure core (config/manifest/matrix/sqlgen):** exhaustive table-driven unit tests, golden files.
  Target this layer hardest — it determines DDL correctness.
- **Behavioral (orchestrator/monitor/recovery):** drive against **small, consumer-defined**
  interfaces fed scripted DMV snapshots → deterministic, fast, no DB.
- **Integration (`//go:build integration`):** real Dockerized SQL Server (Developer edition) for
  driver behavior, KILL/rollback, resumable, CONTEXT_INFO correlation.
- CI runs unit + lint on every push; integration on demand / nightly.

## 6. Key design seams (for testability & safety)

Follow "accept interfaces, return structs", and **define interfaces where they are consumed**, not
next to the implementation. `mssql` exports concrete types; `run` declares the *narrow* slices of
behaviour it actually needs.

- Instead of one god `SQLServer` interface, the consumer declares small role interfaces, e.g.:
  ```go
  // in package run — only what the reaction loop needs.
  type logProbe     interface { LogSpace(ctx context.Context) (LogSpace, error) }
  type blockProbe   interface { Blockers(ctx context.Context, spid int) ([]Session, error) }
  type ddlControl   interface { Kill(ctx, spid) error; Pause(ctx, op) error; Resume(ctx, op) error }
  ```
  The real `*mssql.Conn` satisfies all of them; tests pass tiny fakes per role.
- `Clock` interface (no direct `time.Now`) → deterministic timeout tests. The single justified
  "infrastructure" interface.
- Reporting (run-log, history, notifications) starts as **plain concrete types** with no-op zero
  values — no interface until a second implementation actually exists (YAGNI).
- Manifest + matrix are **data, not code** → new SQL Server versions/operations are config changes,
  never new `if` branches.

## 7. Suggested milestone ordering (PR-sized)

1. Phase 0 + 1 (scaffold + pure loaders) — usable `--dry-run --assume-version`.
2. Phase 2 (sqlgen + golden tests) — full offline `--explain`.
3. Phase 3 + 4 (connection + preflight) — connects, validates, fails safely.
4. Phase 5 + 6 (orchestration + cancel/kill/pause) — the functional core.
5. Phase 7 (recovery).
6. Phase 8 (logs/history/notify/exit codes).
7. Phase 9 (TUI).
8. Phase 10 (e2e + release).

## 8. Resolved design decisions

1. **Column schema — minimal first.** `add_column` / `alter_column` support `type` + `nullable` +
   **constant** `default` only. This covers the metadata-only case and keeps validation and
   metadata-only detection simple. Computed columns, IDENTITY, collation, and non-constant default
   expressions are explicitly out of scope for v1 (extend later).

2. **`index: ALL` — internal expansion.** The tool queries `sys.indexes` and expands into **one
   operation per index**, so monitoring, preflight, retry, and resumable PAUSE/RESUME all work at
   per-index granularity (richer logs; also sidesteps that RESUMABLE is unsupported on a single
   `REBUILD ALL`).

3. **Manifest validation — fail-fast at startup.** All `*.yaml` in `01.to_run/` are parsed and
   validated **before any server work**. Invalid manifests are reported up front and routed to
   `04.failed/`; the run proceeds only with the valid set. Guarantees a coherent batch view.

4. **Resumable PAUSE/RESUME — uniform via DMV.** A single code path drives PAUSE/RESUME from
   `sys.index_resumable_operations`, regardless of `rebuild_index` vs `create_index`. One path to
   test; no per-type branching.
```
