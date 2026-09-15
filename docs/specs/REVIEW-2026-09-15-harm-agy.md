# Adversarial harm and vulnerability review — SqlGoPace (Uncommitted Changes)

Run 2026-09-15. Threat model: an operator downloads a release, runs the scaffold command, edits the connection string, and points it at a production instance having read the README but not the source. They will not read every config key.

## Phase 1 — the scanner floor

| Tool | Result |
|---|---|
| `gosec` | 16 hits. Mostly G304 (Potential file inclusion) on expected CLI file paths. Found G306 (Expect WriteFile permissions to be 0600) on `engine.go:1309` and `report.go:201`. |
| `govulncheck` | No vulnerabilities found. |
| `semgrep` | Skipped (execution failed). |
| CodeQL | Skipped (separate binary download). |

Triage notes:
- The G304 findings are false positives for a CLI tool reading user-provided manifests and configs.
- The G306 finding on `.recovery.yaml` and `.report.json` is real and bounded (see MINOR section).

## Phase 2 — the harm review

### F1 — Immediate retry of log-filling operations guarantees a second full rollback

**Severity: SEVERE**
**Location:** `internal/run/monitored_runner.go` (implied by `internal/run/monitored_runner_retry_test.go:49` and `docs/specs/CANCEL-ONLY.md`).

**What goes wrong:** A non-resumable heavy builder (e.g., an offline index rebuild) fills the transaction log beyond `log_max_percent` and is canceled by the reaction hierarchy to protect the server. However, `MonitoredRunner.Run` immediately retries the operation. Because the operation's own log volume cannot be truncated until the operation completes, the retry starts against an already-full transaction log. It inevitably hits the cap again, gets canceled again, and triggers a second full rollback. This doubles the massive I/O, doubles the duration the table is locked (Sch-M), and completely fails to relieve the log pressure.

**Who it happens to:** Anyone running a rollback-on-cancel operation that generates enough transaction log to hit `log_max_percent`, using the default `max_retry_attempts: 1`.

**Evidence:** The newly added `docs/specs/CANCEL-ONLY.md` documents the design decision to "deliberately keep the retry as-is", ignoring the explicit warnings from the two adversarial spec reviews (`REVIEW-CANCEL-ONLY-claude.md` and `REVIEW-CANCEL-ONLY-agy.md`) which both pointed out that retrying on log pressure causes a guaranteed repeat failure. Microsoft documentation confirms the transaction log cannot be truncated during an active index operation.

**Smallest fix (code):** In `MonitoredRunner.Run`, do not retry operations that were canceled specifically due to log pressure (`Pressure.Kind == "log"`).

### F2 — TUI and Engine concurrently poll the monitoring pool for the same metrics

**Severity: MINOR**
**Location:** `cmd/sqlgopace/main.go:1062` (`feedConsole`), `internal/run/logwatch.go:14` (`watchLog`).

**What goes wrong:** The TUI queries `conn.FileSpace` and `conn.LogSpace` every second. Concurrently, the Engine (`watchLog`) independently polls `conn.LogSpace` every `log_poll` interval. Both use the same monitoring pool, resulting in duplicated and uncoordinated catalog queries.

**Who it happens to:** Anyone running with `--tui`.

**Evidence:** `feedConsole` creates its own `LogFullAlarm` and polls space metrics. `watchLog` does the exact same for the engine's `.log` warnings. SQL Server connection pools handle the concurrency, but it introduces unnecessary load on the server's catalog views.

**Smallest fix (code):** Have the engine's `watchLog` broadcast the log space reading to the TUI via a `SpaceMsg`, rather than having the TUI poll the database directly.

### F3 — Recovery manifest is written world-readable

**Severity: MINOR**
**Location:** `internal/run/engine.go:1309`.

**What goes wrong:** The engine writes `.recovery.yaml` at `0o644`. A recovery manifest contains the exact operations that failed, which may expose sensitive environment or schema details (table names, column structures, filter data). On a shared administrative host, this is third-party data readable by every local user. The previous harm review caught this for sidecars, but the recovery manifest was missed.

**Who it happens to:** Anyone running a manifest that fails and writes a recovery manifest on a shared machine.

**Smallest fix (code):** Change `0o644` to `0o600` in `internal/run/engine.go:1309`.

## Deliverable Summary

### What the software gets right
The addition of `CancelOnly` warnings in `--dry-run` and manifest startup adds critical visibility to operations that roll back all work when canceled. The predicate correctly targets only the specific heavy builders that suffer from this hazard, explicitly excluding operations that are cheap to cancel like `add_column`, `shrink`, and `batch_delete`. The TUI additions handle missing data cleanly without crashing.

### Is it responsible to ship this as it stands?
**No.** The immediate retry of a log-filling operation guarantees a second full rollback, compounding the exact production harm the tool is meant to mitigate. The fix for this must land before release: do not retry rollback-on-cancel operations that failed due to log pressure. The minor issues can be addressed as normal maintenance.

### The shortest honest warning its README should carry

> **Beta. This tool runs heavy DDL against production.**
>
> When a non-resumable operation (like an offline index rebuild) fills the transaction log, it will be canceled to protect the server but immediately retried by default, causing it to fill the log and roll back a second time. Set `max_retry_attempts: 0` for these operations.
>
> Run every new manifest through `--dry-run` first to see which operations will roll back all their work if canceled under pressure.
