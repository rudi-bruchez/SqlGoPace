# Adversarial harm review — 0.34.0 working tree (2026-09-15)

Scope: the uncommitted 0.34.0 change set — rollback-on-cancel narration
(`run.RollbackOnCancel`, the manifest-start notice, the end-of-run summary, the dry-run hazard
line), the transaction-log-full alarm (`internal/run/logalarm.go`, `logwatch.go`), the TUI
header space line, and the documentation that now recommends `max_retry_attempts: 0` —
read inside the system it runs in. The whole-repository review of 2026-09-01
(`REVIEW-2026-09-01-harm.md`) is not repeated; its findings are cited where this change
touches them.

Threat model: an operator downloads 0.34.0, keeps the shipped `config.yaml`, points it at a
production database, reads `docs/running.md` and `docs/configuration.md`, and runs a
Standard-edition compression campaign — sometimes under `--tui`, often from a scheduler with
no one watching.

## Phase 1 — scanner floor

| Tool | Result |
|---|---|
| `govulncheck ./...` | No reachable vulnerability. 1 in an imported package and 18 in required modules, none called. |
| `gosec ./...` | 16 hits, all pre-existing and none in the files this change touches: G304 (file path from a variable: config, manifest, matrix, profile, dotenv, state, lock), G301 (directories created 0755), G306 (report `.log`, recovery manifest and plan files written 0644). Triage below. |
| `semgrep` | **Did not run.** `--config auto` refuses to run with metrics off (`Cannot create auto config when metrics are off`). `--config p/golang` fails to download its ruleset from semgrep.dev: `CERTIFICATE_VERIFY_FAILED … unable to get local issuer certificate` (intercepting proxy; semgrep's Python does not trust the OS store). No semgrep coverage in this review. |

Triage: the G304 paths are operator-supplied files (the config, the queue directories) the
operator already controls, so a hit means "the tool reads the file you told it to", not an
injection. The G306 hits are real but minor and not new: a `.log` report holds the executed
SQL, logins, host names and program names of blocking sessions, and it is written 0644. On a
shared jump host that makes every blocker's login and host readable by every local user.
That was already true before 0.34.0.

## Findings

### H2 — MODERATE: under `--tui`, the operator never sees the rollback-on-cancel notice the docs say the console shows

- **Location:** `internal/run/engine.go` (`processOne`, `fmt.Fprintln(e.out, notice)`);
  `cmd/sqlgopace/main.go:269-272` (`engineOut = io.Discard` when `useTUI`);
  `docs/blocking-and-kills.md` ("the console and the `.log` get one line").
- **What goes wrong:** the manifest-start line `N of M operation(s) can only be canceled under
  pressure; a cancel rolls back all their work…` is written to `e.out` and to the report. With
  `--tui`, `e.out` is `io.Discard`, and no `tui.LogMsg` or `AlertMsg` carries the notice. The
  operator who chose the console precisely so they could watch a risky campaign gets no warning
  on screen. The `.log` has the line, but it is read after the damage.
- **Who:** every `--tui` run of a manifest holding a non-resumable heavy builder. On Standard
  edition that means every offline rebuild. This is the default path for the console user.
- **Evidence:** `engineOut := stdout; if useTUI { engineOut = io.Discard }`. The notice has no
  other sink (`grep LogMsg{` finds only kill and amplifier narration). "The console" in the doc
  reads as the `--tui` console to anyone who has used it.
- **Smallest fix (code):** in `processOne`, send the notice through the same forwarder as the
  failure alerts (`alertSink`, or a `tui.LogMsg` via `fwd`). Alternatively fix the doc to say
  "stdout". The code fix is better, because this is exactly the audience the notice was written
  for. Unverified: I have not checked whether a sticky `AlertMsg` is the right weight for a line
  that fires at every manifest start.

### H1 — SEVERE: after a log-pressure cancel, the retry runs up to `log_poll_seconds` (60 s shipped) blind to the log it was just canceled for — and 0.34.0 re-documents that as intended

- **Location:** `internal/run/monitored_runner.go:67-76` (`Run` retries `runOnce` at once);
  `internal/run/executor.go:289-320` (`pumpSamples`: `var cur Sample` starts with
  `LogOverCap=false`, and the first log read happens only on the first `logTicker` tick);
  `docs/specs/SPECS.md` §9 and `docs/configuration.md` (`max_retry_attempts`: "immediately — no
  wait"); `docs/specs/CANCEL-ONLY.md` "Decision: no change to the retry".
- **What goes wrong, concretely:** take the shipped config (`log_max_percent: 80`,
  `log_poll_seconds: 60`, `blocking_poll_seconds: 10`, `max_retry_attempts: 1`) and a database
  in FULL recovery. Standard and Enterprise both create databases in FULL by default. Run a
  `rebuild_index` on Standard, or a `rebuild_heap` / `alter_column` on any edition, so no
  RESUMABLE is available. The log reaches 80%. The trigger can be the rebuild itself, a log
  backup job that did not run, or an AG secondary falling behind (`AVAILABILITY_REPLICA`).
  1. The sampler sees `LogOverCap`, `DecideReaction` returns `Cancel`, and the statement is
     aborted and rolled back. The rollback writes compensation records. In FULL the log does not
     truncate until the next log backup, so used space stays at or above the cap.
  2. `Run` calls `runOnce` again in the same instant. `runStatement` starts a fresh
     `pumpSamples` whose `cur.LogOverCap` is `false`. Blocking samples arrive every 10 s and
     carry `LogOverCap=false`, so `supervise` answers `Continue`. The log is not read until
     60 s have passed.
  3. For those 60 s, a fully logged rebuild writes into a log already over its cap. If the log
     file has a `MAXSIZE`, has autogrowth off, or sits on a nearly full volume, it reaches 100%.
     SQL Server raises **9002** and the database "remains online but can only be read, not
     updated" until someone frees log space. That is an application outage on the production
     database, caused by the tool's reaction to log pressure.

  The 0.34.0 log-full alarm does not help: `watchLog` also waits a full `logWatchEvery` (60 s)
  before its first read, and it only warns.
- **Who:** the default path. It needs FULL recovery (the vendor default), a non-resumable heavy
  builder (every offline rebuild on Standard), and log pressure — the exact situation
  `log_max_percent` exists for. Nothing is opt-in. `max_retry_attempts: 0` avoids it, but the
  docs recommend 0 for a different reason, blocking.
- **Why 0.34.0 makes it worse, not just older:** until this change, SPECS §9 said "wait until
  … the log has dropped, then retry". Code and spec disagreed, and the spec described the safe
  behaviour. 0.34.0 resolves the disagreement in favour of the code ("superseded 0.34.0"). The
  rejection in CANCEL-ONLY.md does not hold for this case:
  - "The index operation's own log cannot be truncated until it completes" is true of a
    *running* operation. After a cancel and rollback, the attempt *has* completed, and its log
    becomes truncatable at the next log backup (FULL) or checkpoint (SIMPLE). The argument shows
    that an immediate retry is futile. It is not an argument against waiting.
  - "`waitForRelief` returns on the first sample under the cap" argues for a better wait, not
    for none.
  - "No field cancel was for log pressure" is true because the field campaign ran in SIMPLE
    recovery, where rebuilds are minimally logged. The only evidence cited is silent about the
    recovery model where this path matters.
- **Evidence (vendor, Microsoft Learn):**
  - *The transaction log*: CREATE INDEX and ALTER INDEX REBUILD are minimally logged only "if
    the database is set to the simple or bulk-logged recovery model".
  - *Guidelines for online index operations*: "both offline and online index rebuild
    operations are fully logged … the transaction log can't be truncated until the index
    operation is completed".
  - *Troubleshoot a full transaction log (SQL Server Error 9002)*: "If the log fills while the
    database is online, the database remains online but can only be read, not updated".
  - *Recovery models*: Standard and Enterprise use FULL by default.

  **Inference, not measured:** how much log 60 s of rebuild writes. It depends on I/O
  throughput and row width. On a server sized for the rebuild it can be several GB, the same
  order as the remaining 20% of a tens-of-GB log.
- **Smallest fix (code, unverified against the test suite):** have `pumpSamples` take one log
  sample (and one blocking sample) *before* entering its ticker loop, so every statement —
  first attempt, retry, and RESUME — starts with the pressure state known rather than assumed
  clear. That is a few lines in `executor.go` and closes the window for every runner that uses
  the pump. The alternative, narrower fix: when the `Cancel` was for `LogOverCap`, have `Run`
  call the existing `awaitRelief` (bounded by `log_drain_timeout_minutes`) before retrying. The
  first fix is the one I would take. The second is still worth having, because an immediate
  sample only turns 60 s of logging into a few seconds plus another cancel. Either way,
  withdraw the "superseded 0.34.0 … no wait" wording from SPECS §9 until one lands, and file
  the path in TODO.md. Check before merging: the deterministic `supervise`/`runLoop` tests may
  assume the first sample arrives after a tick.

### H3 — MINOR: repeated log-full alerts accumulate without bound and push the oldest sticky alerts — including why a manifest failed — off the top of the console

- **Location:** `internal/tui/model.go:548-549` (`m.alerts = append(m.alerts, msg)`, no cap, no
  dismissal); `internal/tui/view.go:82` (alerts render first);
  `bubbletea@v1.3.10/standard_renderer.go:186-187` (an over-tall view keeps the *last*
  `height` lines).
- **What goes wrong:** `feedConsole` has one `LogFullAlarm` per database run. Each time the log
  crosses 90%, drops below 85% (a log backup between operations) and climbs again, it sends a
  new `AlertMsg`. Over a long campaign on a FULL database with 15-minute log backups, alerts
  stack up. `opsBudget` subtracts them, so the operations panel shrinks to `minOpsRows`. Past
  the terminal height the renderer cuts from the top, which is the oldest alerts: the manifest
  failure reasons the console exists to keep on screen. The header is cut only when the fixed
  blocks alone exceed the terminal height, so the SPID banner survives.
- **Who:** `--tui` users on a database whose log oscillates across the thresholds.
- **Smallest fix (code):** keep log alerts in their own single slot (latest only) instead of
  appending to the failure-alert list.

### H4 — MINOR: the rollback-on-cancel summary points at a recovery manifest that may not exist

- **Location:** `internal/run/engine.go` `finalizePartial` (the pointer is appended after
  `writeRecovery` whether or not it failed).
- **What goes wrong:** when `writeRecovery` fails (disk full, permissions on `04.failed/`), the
  error goes to `e.out` (discarded under `--tui`), yet the `.log` tells the operator to re-run
  `<name>.recovery.yaml`. `rep.Error` has had the same defect since before 0.34.0; the new line
  repeats it. The pointer is also added when every rollback-on-cancel operation was saved by
  its retry and some *other* operation failed. The recovery manifest then holds none of the
  operations the sentence is about.
- **Smallest fix (code):** append the pointer only when `writeRecovery` returned nil and
  `cancelOnlyFailed > 0`.

### H5 — MINOR: docs say the console log alert is "tracked for the whole run"; it is per database

- **Location:** `docs/running.md` (new header-space paragraph); `cmd/sqlgopace/main.go`
  (`runWithTUI` → `feedConsole` → `run.NewLogFullAlarm()`, called once per database inside
  the `targets` loop).
- **What goes wrong:** a multi-database `--tui` run re-arms the alarm for each database. This
  is harmless (more alerts, not fewer), but the doc claims a guarantee the code does not make.
- **Smallest fix (documentation):** "tracked per database".

### H6 — MINOR: the log-full warning reaches no notifier, so an unattended run learns of it only from the `.log`

- **Location:** `internal/run/engine.go` sink (`e.notify` only for `pause`/`cancel`/`abort`);
  `internal/run/logwatch.go`.
- **What goes wrong:** a scheduled run with a webhook configured gets no push when the log
  passes 90%. The impact is limited: on the shipped `log_max_percent: 80`, a notified `cancel`
  or `pause` has normally fired first. The case where no notified reaction precedes it is H1's
  retry window, which is where the warning would matter most.
- **Smallest fix:** notify on the log-full warn (a distinct event such as `log_full`, so
  `on_events` can subscribe to it), or document that it is `.log`-only.

### H7 — MINOR: on a resumed manifest the notice counts operations already done

- **Location:** `internal/run/engine.go` `processOne` (`rollbackOnCancelNotice(planned, …)`
  counts every planned operation; operations below the cursor are then skipped with
  `resumeSkipReason`).
- **What goes wrong:** "20 of 33 operation(s) can only be canceled…" on a run that will execute
  3 of them. This overstates exposure and so misleads, but in the safe direction.
- **Smallest fix (code):** count only the operations from the resume cursor onward.

## Ranking by expected harm to a stranger

1. **H1** (SEVERE): 9002 on a production database from the default reaction to log pressure,
   in FULL recovery with a non-resumable rebuild. 0.34.0 turns the spec from the safe behaviour
   to the unsafe one.
2. **H2** (MODERATE): the notice 0.34.0 exists to deliver is invisible to the `--tui` operator.
3. **H6**, **H3**, **H4**, **H7**, **H5** (MINOR).

## What the change gets right

- It changes no reaction. `DecideReaction`, `Run` and the kill path are untouched, so the
  narration cannot itself hurt anyone. The spec says so explicitly and the diff honours it.
- Both per-operation pollers (the new `watchLog`, and `narrateHeld` refactored onto
  `pollWhileRunning`) are cancelled *and joined* before the report state is read. They read
  through the monitoring pool, not the pinned execution connection, and the pool has no
  open-connection cap for them to contend on.
- The alarm has real hysteresis. The TUI stays dumb about thresholds. One wording function
  (`LogFullMessage`) feeds both the `.log` and the console.
- The dry-run hazard line prints without `--explain`, and names a real cause, including the
  "no decision emitted" case that made the problem invisible.
- The TODO correction (the history DB stores no per-operation throughput) is right, checked
  against `internal/report/history.go`. Correcting a false claim in the backlog is uncommon.
- `max_retry_attempts: 0` is honoured, not silently defaulted (`config.go:167-172`). The
  docs' advice to set it is therefore real.

## Is it responsible to ship 0.34.0 as it stands?

As code, 0.34.0 is not more dangerous than 0.33.0: H1's window exists in both. As a *release
with documentation*, no — it tells the operator that the immediate, blind retry is the
intended design, on evidence that cannot speak to the case where it hurts. Minimum, in order:

1. **H1, code:** take an initial sample at pump start (`executor.go`), with a test that a
   statement started while the log is over cap is reacted to before the first `log_poll`
   tick. Roughly 10 lines plus one test, possibly adjusting deterministic tests that assume
   tick-first. If that cannot land in 0.34.0, then at minimum revert the "superseded … no wait"
   wording in SPECS §9 and `configuration.md` to state the gap as a known defect, and file it
   in TODO.md. Cost: a paragraph.
2. **H2, code:** forward the manifest-start notice to the console. Roughly 5 lines through the
   existing `alertSink`/`fwd`.
3. H4 and H7: one condition each. H3, H5 and H6 can go to TODO.md.

## The shortest honest warning the README should carry

> SqlGoPace reacts to blocking and transaction-log pressure by pausing what it can and
> *cancelling* what it cannot. On Standard edition, and for heap rebuilds and column changes on
> any edition, a cancel throws away all the work done so far, and the operation is retried
> once by default. In FULL recovery, that retry writes log before it re-checks the log: with a
> capped or nearly full log it can fill it, and a full log stops every write to the database.
> Set `max_retry_attempts: 0` for non-resumable campaigns on FULL-recovery databases, and make
> sure log backups run during the window.

