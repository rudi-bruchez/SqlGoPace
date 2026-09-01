# SqlGoPace — production-harm review

**Status:** historical record of a review, not a statement of current behaviour. Findings are
tracked in [TODO.md](TODO.md); this file is the evidence behind them and is not updated as they
are fixed.
**Date:** 2026-09-01, against the tree at v0.18.0.
**Method:** independent adversarial review, run with no leads supplied, reasoning from the
shipped defaults rather than from what a careful expert would configure.


**Scope:** what in this tool can harm, block, corrupt, or take down a production SQL Server, or
destroy data. Nothing else. Ranked by expected harm to a stranger who downloads a release, runs
`sqlgopace init`, edits the connection string, and points it at a busy production database at
22:00 having read `README.md` and `docs/` but not the source.

**Method note.** Claims about SQL Server behaviour are cited to Microsoft Learn where they are
load-bearing; where I am reasoning rather than citing, the finding says so. Claims about this
codebase are cited to `file:line` and were read, not inferred.

---

## What a scaffolded install actually has armed

`sqlgopace init` writes `config.yaml` byte-identical to the repo's (`diff`-verified against
`internal/scaffold/assets/config.yaml`). That file arms:

| Key | Shipped | What the docs say |
|---|---|---|
| `kill_blockers.enabled` | **`true`** | "Off by default" — `docs/configuration.md:52`, `:277`, `SECURITY.md:39`, `docs/permissions.md:65`, `README.md:48`, and the comment six lines above the value itself (`config.yaml:99`) |
| `max_retry_attempts` | `1` (= 2 attempts) | default `0` — `docs/configuration.md` monitoring table |
| `blocking_timeout_minutes` | `1` | matches |
| `log_max_size_bytes` | 50 GB | matches |
| `preflight.require_data_free_space` | `true` | disclosed as a deliberate divergence |
| `history.enabled` | `true` | disclosed as a deliberate divergence |

`docs/configuration.md:26-27` is the section that exists to disclose exactly these divergences.
It says the shipped file and the bare defaults differ **"in three places"**, then lists two. The
ones it omits are `max_retry_attempts` and `kill_blockers.enabled` — the second being the master
arm on terminating other people's sessions.

---

# Tier 1 — CATASTROPHIC

## 1. `batch_update` can loop forever, committing as it goes

**Severity:** CATASTROPHIC — unbounded, durable data corruption with no rollback
**Location:** `internal/ddl/batch.go:81-95` (`selfLimitClause`), `internal/run/batch_dml.go:216-224`
**Who:** anyone who writes a `batch_update`. Two independent triggers, one of them an ordinary manifest.

The only loop exit is "the last batch affected zero rows":

```go
result.FinalRows = size
if rows == 0 {
    // The predicate matched nothing: the loop is exhausted — done.
    return result, nil
}
result.Rows += rows
result.Batches++
stallWaited = 0
```

No iteration cap, no cumulative-row cap, no wall-clock cap. `stallWaited = 0` resets the
self-wait budget after every productive batch, so `self_wait_timeout_minutes` cannot backstop a
loop that keeps affecting rows. Every batch autocommits (`internal/mssql/conn.go:191`; there is
no `BEGIN TRAN` anywhere in the batch path), so damage is durable as it accrues.

Termination is supposed to be guaranteed by `selfLimitClause`, which excludes rows already at the
target value:

```go
parts = append(parts, q+" IS NULL OR "+q+" <> "+renderLiteral(o.Set[col]))
```

**Trigger A — `set: {Column: null}`.** `Literal.UnmarshalYAML` (`internal/ddl/manifest.go:96-104`)
records `Raw:"null", String:false` for a `!!null` scalar, so `renderLiteral` emits a bare `null`:

```sql
UPDATE TOP (5000) [dbo].[Orders] SET [Status] = null
WHERE ([Status] IS NULL OR [Status] <> null);
```

`[Status] <> null` is `UNKNOWN` for every row; `[Status] IS NULL` is `TRUE` for exactly the rows
the update just finished setting. **The clause written to guarantee termination guarantees
non-termination.** Every completed row re-enters the match set immediately. There is no `ORDER BY`
on the `UPDATE TOP` (`batch.go:145`), so SQL Server churns arbitrary rows forever at full write
rate, generating log, until a human notices. "Null out a deprecated column in batches" is an
ordinary request.

**Trigger B — a `set_raw` that does not consume its own predicate.**
`set_raw: "Counter = Counter + 1"` with `where_raw: "Status = 'A'"`. `selfLimitClause` returns
`""` for any raw SET (`batch.go:83`), and the compensating validation at `manifest.go:1095` checks
only that *a* predicate exists, never that it is self-consuming. Arbitrary rows get incremented
arbitrarily many times; the original values are unrecoverable.

`docs/specs/BATCH-DML.md` §4 requires a preflight `WARN` for non-idempotent `set_raw`.
`grep SetRaw internal/preflight/` returns nothing — never implemented.

**Smallest fix (code):** (a) emit `IS NOT NULL` rather than `<> null` when the literal is null, or
reject a null literal under the `predicate` strategy — a few lines in `selfLimitClause`;
(b) add an absolute row/iteration ceiling to the loop, using the `TableRowEstimate` the driver
already reads at `batch_dml.go:181`; (c) implement the BATCH-DML.md §4 warning. I have read the
code paths for (a) and (b) but have not run either change.

---

## 2. A whole-table `DELETE` needs no confirmation the operator would notice

**Severity:** CATASTROPHIC — total data loss on a production table
**Location:** `internal/ddl/manifest.go:1090` (the guard), `internal/ddl/batch.go:101-111`
**Who:** anyone writing a `batch_delete`. Not a misconfiguration — the guard is documented as *the*
protection, and it does not protect.

`docs/operations.md` promises: *"No predicate at all is refused unless `confirm_full_table: true`
says you meant it."* The implementation is a presence check on a YAML key:

```go
if !hasWhere && !hasWhereRaw && !o.ConfirmFullTable {
    return fmt.Errorf("%s: no where/where_raw targets the whole table; set confirm_full_table: true ...")
}
```

`predicateWhere` returns the operator's raw text unwrapped for a DELETE (there is no self-limit
clause for delete), so:

```yaml
- operation: batch_delete
  schema: dbo
  table: Orders
  where_raw: "1=1"
```

emits `DELETE TOP (4000) FROM [dbo].[Orders] WHERE 1=1;` looped to exhaustion — an identical
whole-table purge, with **no `confirm_full_table` required, no warning anywhere in the run, and no
row-count preview**. The same holds for `where: [{column: Id, op: ">=", value: 0}]` on an identity
column, which is the shape a tired DBA actually writes. The guard stops the typo, not the mistake.

The confirmed path is itself one line: `confirm_full_table: true` yields
`DELETE TOP (4000) FROM [dbo].[Scratch];` (pinned at `internal/ddl/batch_test.go:311`) with no
prompt and no second gesture. Dropping the file into `01.to_run/` is the entire commit action.

Nothing ever runs a `SELECT COUNT(*)` against the predicate. Preflight
(`internal/preflight/preflight.go:414-431`) checks table existence and `DELETE`+`SELECT` permission
and nothing else; the predicate is first evaluated by SQL Server, on live rows.

**Smallest fix (code):** run a row-count probe at preflight and **fail** when the predicate matches
essentially the whole table unless `confirm_full_table: true` is set. That makes the existing flag
mean what the documentation already claims it means.

---

## 3. Two instances on one queue: recovery requeues a live peer's in-flight manifest

**Severity:** CATASTROPHIC — concurrent DDL on the same object, mutual `KILL`, and irreversible
loss of a paused resumable's progress
**Location:** `internal/run/recovery.go:65-82`, `internal/mssql/recovery.go:22-28`
**Who:** the default path for anyone who schedules it — a cron overlap, a long-running Agent job,
or a forgotten terminal plus a scheduled run.

There is no lock file, PID file, or advisory lock anywhere (`grep O_EXCL|Flock|LockFileEx` returns
nothing). The manifest *claim* is safe — it is an `os.Rename`, and the loser gets ENOENT and skips
(`internal/run/queue.go:100`, `internal/run/engine.go:489`).

The breach is that **crash recovery runs before any claim** (`cmd/sqlgopace/main.go:241-252`) and
sweeps `02.processing/` — exactly where a live peer's manifest sits. Whether it leaves it alone
turns on `id.Active`:

```sql
CASE WHEN EXISTS (SELECT 1 FROM sys.dm_exec_requests r
                  WHERE r.session_id = s.session_id) THEN 1 ELSE 0 END
```

Instance A's pinned execution session has **no row in `sys.dm_exec_requests` whenever it is between
statements** — during `awaitRelief` after a pressure pause (bounded only by
`log_drain_timeout_minutes`, default 30), between shrink chunks, between batch-DML batches, and
between operations. In every one of those windows instance B concludes the run is dead, requeues
A's manifest to `01.to_run/`, then claims and runs it.

The code states the assumption in its own comment and does not enforce it:

> *"This assumes recovery runs against a crashed prior run, **never concurrently with a live
> SqlGoPace process** — the only case where our session is legitimately idle between requests
> (e.g. a shrink between chunks); the tool does not support concurrent instances on one queue."*

Consequences, ascending:
- Two offline rebuilds of the same index serialize on `Sch-M`, then rebuild it twice — double
  duration, double log, mid-window.
- `BlockerKiller` has **no self-exclusion** (`internal/run/kill.go:229-285`). Under the same service
  login, a `login_name` rule makes B kill A's rebuild. `VictimKiller` *does* guard this
  (`internal/run/victim.go:443`), and the reasoning is spelled out at `cmd/sqlgopace/main.go:515-518`
  — it simply was not applied to the other killer.
- With `abort_blocking_resumable: true` in the manifest, B issues `ALTER INDEX … ABORT` against A's
  paused rebuild (`internal/run/engine.go:1373-1392`). Microsoft Learn: *"The `ABORT` command kills
  the session that is running an index build and cancels the index operation. You cannot resume an
  index operation that has been aborted."* Hours of progress destroyed, A's session killed.

**Smallest fix (code):** an `O_EXCL` lock file in the queue root, taken before `Recover()` and
released at exit, refusing to start if held. The recovery sweep is what must be exclusive; the
claim already is. "Documented as unsupported" is not the same as prevented, and the operator has
no way to know they violated it.

---

## 4. `abort-resumable` is database-wide, unfiltered, unconfirmed, and irreversible

**Severity:** CATASTROPHIC — destroys arbitrary amounts of other people's work, unrecoverably
**Location:** `cmd/sqlgopace/abort.go:44-115`, `internal/mssql/dmv.go:106-110`
**Who:** the documented remedy. `docs/running.md:187` and the engine's own error message send the
operator here.

```sql
SELECT ... FROM sys.index_resumable_operations;
```

No `WHERE` clause. Then:

```go
case "PAUSED":  return true
case "RUNNING": return opts.includeRunning
```

The complete flag set is `--config`, `--dry-run`, `--include-running`. **There is no `--schema`,
`--table`, `--index` or `--spid` filter — "all" is the only mode.** No confirmation prompt, no
`--yes`. Ownership is never consulted: the tool has no idea which paused operations it started.

One command, typed once on a shared production server, aborts every colleague's in-flight resumable
index build in that database. Microsoft Learn confirms it is unrecoverable: *"You cannot resume an
index operation that has been aborted."* With `--include-running` it also kills the running
sessions — and the warning line is printed *after* the decision is already made.

`docs/running.md` explains the trade-off correctly in prose. The command does not make the operator
acknowledge it.

**Smallest fix (code):** require either an explicit target or an explicit `--all`, and require
`--yes` for `--all` / `--include-running`. Better: default to aborting only operations this tool
paused — the sidecar already records them in `State.Paused`.

---

# Tier 2 — SEVERE

## 5. The incident console's `x` key kills the wrong class of session, with one keystroke

**Severity:** SEVERE — terminates a production application session and rolls back its transaction;
the follow-up action writes a permanently inert rule
**Location:** `internal/tui/model.go:579-581`, `cmd/sqlgopace/main.go:836-843`, `:1148-1149`, `:1199-1215`
**Who:** the default path for `--tui`, which the README showcases with a screenshot.

The list the console shows, and the list `x` acts on, is the sessions **our DDL is blocking** — our
victims, not our aggressors. `blockerGate.persistent` filters on `s.BlockedBy(ddlSPID)`:

```go
for _, s := range sessions {
    if !s.BlockedBy(ddlSPID) {
        continue
    }
```

`internal/tui/view.go:337` says so plainly — *"blockedBody renders the actionable blocked-session
list (**the sessions our DDL blocks**)"* — as does the type comment at `internal/tui/model.go:56`:
*"Blocker is one session blocked by the running DDL."* The panel header is `blocked sessions (N):`
and the footer offers `[i] ignore  [x] kill  [X] kill+auto`.

`x` fires immediately, with no confirmation mode, and the error is discarded:

```go
case "x":
    if len(m.blockers) > 0 {
        m.emit(Action{Kind: ActionKillBlocker, SPID: m.blockers[m.cursor].SPID})
    }
```
```go
case tui.ActionKillBlocker:
    _ = conn.Kill(ctx, a.SPID)
```

Three things make this dangerous rather than merely mislabelled:

1. **It is not gated by `kill_blockers`.** The code says so: *"The kill takes effect regardless of
   whether kill_blockers is armed in config — it is an explicit operator act."* An operator who
   deliberately left the master arm off still has a live kill key.
2. **There is no amplifier test.** The automated equivalent (`kill_amplifying_maintenance`) requires
   six conditions before killing a victim — command allow-list, fan-out ≥ N, dwell, not-ignored,
   not-us, direct victim. The `x` key requires none of them. It will kill a plain application
   `SELECT`, or an `INSERT` with open transactions, neither of which does anything to unblock our
   DDL. It just destroys a user's work.
3. **`X` writes a rule into the wrong list.** `killBlockerAuto` calls `AppendKilledSession`, i.e.
   `kill_blocking_sessions` — consumed by `BlockerKiller`, which only ever matches
   `blockerSession(sessions, ddlSPID)`, the session blocking *us*. A victim will never be our
   blocker, so the rule can never fire. The operator is left believing recurrences are handled.

The bitter part: `docs/blocking-and-kills.md:34` tells the operator that sessions in *"the console's
blocked list"* belong in `ignore_blocked_sessions`, and `:163-174` ("Getting it backwards: a worked
example") describes putting them in `kill_blocking_sessions` as the canonical operator error. The
`X` key commits that exact error automatically.

**Smallest fix (code):** make `x` require a confirmation keystroke and show the victim's command and
open-transaction count in the prompt; restrict it to victims passing `IsAmplifyingCommand`; make `X`
append to `ignore_blocked_sessions`, or remove it. Renaming `Blocker` → `Victim` through the TUI
would stop the next person re-introducing this.

---

## 6. `sqlgopace init` arms session-killing while five documents say it is off

**Severity:** SEVERE — arms the destructive path against the operator's documented understanding
**Location:** `config.yaml:97-106` and its byte-pinned twin `internal/scaffold/assets/config.yaml:104-106`
**Who:** every scaffolded install.

```yaml
# Killing is destructive, so it is OFF by
# default; preflight WARNS (does not fail) if the login lacks ALTER ANY CONNECTION.
...
kill_blockers:
  enabled: true                # master arm; kills only happen when true
```

The comment and the value contradict each other six lines apart, and so do
`docs/configuration.md:52`, `docs/configuration.md:277` (whose example block shows
`enabled: false`), `SECURITY.md:39`, `docs/permissions.md:65`, `README.md:48`, and the Go doc
comment at `internal/config/config.go:42-44`.

The Go zero value is `false`, so deleting the block is safe — the divergence is entirely in the
shipped file. Blast radius is bounded (no manifest rule ⇒ no kill), but that is exactly the
assumption an operator relies on when they press `X` in the console, or paste a rule from an
example, having read that the master gate is closed.

**Smallest fix (default change):** set `enabled: false` in both files. One character each, and it
restores every document to truth. Then add the two missing rows to the divergence table at
`docs/configuration.md:26-33`.

---

## 7. Nothing anywhere checks free space on the actual disk

**Severity:** SEVERE — a full data or log volume is a production outage
**Location:** `internal/preflight/preflight.go:129-159`, `internal/mssql/dmv.go:24-27`
**Who:** the default path, on any server whose files can autogrow — which is most of them.

`grep -r "dm_os_volume_stats\|available_bytes" internal/` returns **nothing**. The tool never asks
the operating system how much space is left. Everything it knows is file-level: free space *inside*
the file, plus `max_size − size` headroom.

**Data files.** `CheckDataFreeSpace` degrades to a warning precisely in the unbounded case:

```go
if f.Unlimited() {
    return Check{name, Warn, fmt.Sprintf(
        "%s needs ~%d MB, data files have %d MB free; %q grows until the disk fills, ...")}
}
```

The reasoning is honest — *"We cannot prove the run will fail, so we must not fail it"* — but the
effect is that a rebuild needing 400 GB on a volume with 40 GB free passes preflight with warnings
only, and then fills the volume. Preflight warnings never stop a run; only `HasFailure()` does
(`internal/run/engine.go:532`).

**Transaction log.** The shipped ceiling is 50 GB absolute or 80% of the *current* file size. For an
autogrowing log the percentage never trips — the file grows, so the ratio stays low. The real bound
is "50 GB of log growth before the tool reacts at all". On a log volume with less than that free,
the disk fills first. Microsoft Learn is explicit about the shape of this risk: *"the transaction
log can't be truncated until the index operation is completed"* and *"the transaction log must have
sufficient space to store both the index operation transactions and any concurrent user
transactions."*

A full log volume under FULL recovery stops all writes (error 9002). A full data volume stops all
allocation. Neither is something the operator can undo from the tool.

**Smallest fix (code):** read `sys.dm_os_volume_stats` per file at preflight and (a) **fail** when a
rebuild's estimated need exceeds volume free space, (b) clamp the effective log ceiling to
`min(log_max_size_bytes, volume_free − margin)`. That DMV needs only `VIEW SERVER STATE`, which the
tool already requires.

---

## 8. A batch-DML operation that stops early is recorded as a success

**Severity:** SEVERE — a half-finished purge filed as complete, and made unresumable
**Location:** `internal/run/batch_dml.go:365-376`, `:207-212`; `internal/run/engine.go:824-829`
**Who:** the default path, whenever log pressure or blocking outlasts the timeouts.

```go
if errors.Is(err, ErrLogDrainTimeout) {
    return true, "stopped: log did not drain before timeout (committed batches preserved)", nil
}
```
```go
if stop {
    result.Reason = reason
    return result, nil          // nil error
}
```

`runErr == nil` reaches the engine, so the operation is recorded as succeeding, the resume cursor
advances, and the manifest is finalized into `03.done/`. Then:

```go
if op.Batch.IsKeyRange() && runErr == nil {
    store.clear()
}
```

**the key-range resume watermark is deleted**, so the walk that abandoned 90% of its rows cannot be
resumed either. The self-wait timeout takes the same path.

The `Reason` string does reach the `.log` JSON, but the manifest's outcome, its directory, and the
run summary all say it completed. An operator draining a queue overnight from cron sees `03.done/`
and moves on. The shrink driver handles the same class of event correctly —
`shrinkStoppedShort` → `finalizeIncomplete` moves the manifest to `04.failed/` with "work preserved
— re-run to continue" (`internal/run/engine.go:1033`, `:1053`). Batch DML does not.

**Smallest fix (code):** return a distinct sentinel from `handleStop` and route it through the
existing `finalizeIncomplete` path exactly as the shrink driver does, and do not clear the watermark
on it. This reuses machinery that already exists.

---

## 9. A connection loss on a non-resumable operation is reported as a failure, not an interruption

**Severity:** SEVERE — invites the operator to re-run DDL that is still executing server-side
**Location:** `internal/run/engine.go` (`runStep`, the `caps.Resumable` gate), `:1297-1334`
**Who:** everyone on Standard edition, plus heap rebuilds, `check_db`, `update_statistics`, shrink
and batch DML on any edition.

The `reconnect_timeout_minutes` machinery is careful and correct — it probes for a paused resumable
and, if the server stays unreachable, declares the run INTERRUPTED and leaves the manifest in
`02.processing/` for recovery. But the whole branch is gated on `caps.Resumable`:

```go
if stopped || (prepErr == nil && caps.Resumable && e.resumableInterruption(ctx, step.Operation)) {
    ... "interrupted" ...
}
opRep.Outcome = "failed"
```

`RESUMABLE` requires `ONLINE`, which per the shipped matrix (`ddl_compatibility.yaml:25-27`) is
`editions: [enterprise, azure]`. **On Standard edition nothing is ever resumable**, so every
connection loss lands on the "failed" branch. The manifest goes to `04.failed/`, and the operator's
natural next move — move it back and re-run — starts the same DDL again while the orphaned statement
may still be rebuilding under a session the client no longer holds. Nothing in the tool, the `.log`,
or the docs warns against it.

The same hazard exists on Enterprise under a network partition: if the exec socket dies but the
monitoring pool reconnects, `PausedResumable` answers conclusively `false` (the orphan is RUNNING,
not PAUSED), so even a resumable operation is declared failed while it is still building.

**Smallest fix (code):** when the failure is a connection/driver error rather than a SQL error,
classify it as INTERRUPTED regardless of `caps.Resumable` and leave the manifest in `02.processing/`.
Recovery already identifies the orphan correctly by SPID + `login_time` + `CONTEXT_INFO` marker
(`internal/mssql/recovery.go:22-28`).

---

## 10. On Standard edition the default reaction to blocking is a rollback that blocks harder

**Severity:** SEVERE — converts a one-minute block into an outage as long as the rebuild has run,
then repeats it
**Location:** `internal/run/reaction.go:148-157`, `internal/run/monitored_runner.go:68-76`,
`config.yaml:27,29`
**Who:** the default path on Standard, Web and Express — where "run heavy DDL without holding your
breath" is most needed.

```go
switch {
case !p.Any():    return Continue
case c.Resumable: return Pause
default:          return Cancel
}
```

On Standard, `ONLINE`, `RESUMABLE` and `WAIT_AT_LOW_PRIORITY` are all unavailable
(`ddl_compatibility.yaml:25-27`), so an index rebuild is offline and non-resumable and the hierarchy
has exactly one rung left: `Cancel`. With the shipped `blocking_timeout_minutes: 1`, an offline
rebuild fifty minutes in, having blocked one session for sixty seconds, is cancelled.

Microsoft Learn: *"An offline index rebuild… holds object-level locks for the duration of the
rebuild operation, blocking queries from accessing the table or view."* Those locks are held until
the transaction ends — **and the rollback is part of the transaction**. So the cancel starts a
rollback that can take as long as the rebuild did, during which `Sch-M` is still held and the table
is still entirely unavailable. The tool has traded a one-session block for a whole-table outage and
made the operation take longer than doing nothing. (That the rollback holds the lock for its
duration is standard lock-duration semantics rather than a sentence I found stated verbatim in the
docs — flagging it as inference from documented behaviour.)

Then `max_retry_attempts: 1` (shipped; documented default 0) runs the whole thing again. Two full
attempts, each capable of the same cancel-and-rollback. `README.md:121` says *"a timer would abort a
rebuild three hours in and about to finish"* — the blocking timer does precisely that, on the
edition where it costs most.

**Smallest fix (code + default):** do not `Cancel` a non-resumable, non-cancel-safe operation on
blocking pressure alone. Yield for log pressure, otherwise hold and narrate, and let
`max_block_minutes` be the operator's explicit opt-in to paying for a cancel. At minimum, raise the
shipped `blocking_timeout_minutes` and state the expected rollback cost in the `.log` before
cancelling. This is a design change, not a one-liner; I have not verified it against the suite.

---

## 11. A paused resumable is left on the server indefinitely, at a cost Microsoft documents

**Severity:** SEVERE — permanent write amplification, doubled index space, and a class of
transaction that now fails outright
**Location:** `internal/run/monitored_runner.go:150-153`, `internal/run/engine.go:1224-1238`
**Who:** the default path for anyone who presses Ctrl+C, drains, or lets a window close.

Graceful stop pauses the resumable and returns without resuming, leaving it for the next run.
`docs/running.md:158-160` mentions the cost in one sentence. Microsoft Learn documents more than
that sentence conveys:

- *"When an index operation is paused, both the original index and the newly created one require
  disk space and need to be updated during DML operations."*
- *"If an online resumable index operation is paused, this performance impact persists until the
  resumable operation either completes or is aborted. **If you don't intend to complete a resumable
  index operation, abort it instead of pausing it.**"*
- *"While an online index operation is paused, any transaction that requires a table-level exclusive
  (X) lock on the table that contains the paused index **fails**"* — error 10637. That includes
  `INSERT … WITH (TABLOCK)`, which is how most bulk-load ETL writes.

So a drained overnight run leaves a production table paying double write cost on every DML, holding
two copies of a large index, and failing any `TABLOCK` load — silently, until someone re-runs the
manifest. If the campaign is abandoned or the manifest later lands in `04.failed/`, that state is
permanent and the tool never mentions it again. The interrupt message
(`cmd/sqlgopace/main.go:296-314`) says the operation "resumes next run"; it does not say what it
costs until then.

**Smallest fix (docs + code):** state those three consequences in the drain message and in the
`.log` interruption record, naming the object, and print the `abort-resumable` alternative.

---

## 12. Nothing binds a manifest to a database or a server

**Severity:** SEVERE — a destructive manifest runs against the wrong database
**Location:** `internal/run/engine.go:452-461`, `docs/manifests.md:20`
**Who:** anyone with more than one environment.

```go
func (e *Engine) ownsManifest(name string) bool {
	if e.database == "" { return true }
	m, err := ddl.LoadManifestFile(...)
	if err != nil { return true }
	return m.Database == "" || strings.EqualFold(m.Database, e.database)
}
```

`database:` is **optional**, and an empty value matches every database unconditionally. There is no
server-name assertion, no expected-`DB_NAME()` check, and no database fingerprint recorded anywhere.
Where matching does happen it is `EqualFold` on a name — so a restore of PROD onto the DR box under
the same name matches perfectly.

The connection comes from `config.yaml` with `${VAR}` expansion from a gitignored `.env` in the
**working directory**. A scheduler launching the binary from elsewhere picks up a different `.env`
(`docs/configuration.md:76-77` notes the relative-path hazard for the queue directories but not for
`.env`). A `batch_delete` manifest authored against a staging copy and left in `01.to_run/` deletes
from whatever the DSN currently resolves to.

Related: system databases are not excluded from run targets. `internal/mssql/databases.go:38` filters
`WHERE d.database_id > 4`, so `master`/`model`/`msdb` are simply absent from the eligibility map and
fall through to runnable in `cmd/sqlgopace/scope.go:75-92`. A hand-written `database: master` +
`shrink` manifest is accepted.

**Smallest fix (code):** make `database:` required for the irreversible three (`batch_update`,
`batch_delete`, `shrink`) and fail when `DB_NAME()` does not match it. Add `database_id > 4` to the
runnable-target filter.

---

## 13. The shrink loop has no absolute stop, and `files: all` escapes the maintenance window

**Severity:** SEVERE — a shrink runs past its window into the business day, unbounded
**Location:** `internal/run/shrink.go:466`, `:566-568`; `internal/run/engine.go:669`;
`internal/ddl/expand.go` (no `Shrink` case)
**Who:** the default path for any `shrink`, which defaults to `files: all`.

**No absolute stop.** The loop is `for current > final`. There is no wall-clock cap, no chunk cap,
and no cumulative-log cap — only an *instantaneous* log ceiling that pauses and resumes, so a long
shrink generates unbounded cumulative log in 50 GB sawteeth. The give-up path is three *consecutive*
no-progress chunks, and every counter resets on any gain:

```go
noProgress = 0
backoff = r.tuning.NoProgressBackoff
stallWaited = 0
```

So a file yielding 1 MB every few minutes, or alternating gain / no-gain / no-gain, never reaches
three consecutive failures. `self_wait_timeout_minutes` is a per-streak budget, not a run budget. A
dead-stalled shrink stops cleanly in about ninety seconds; a **slow** one runs until a human stops it.

**The window does not bound it.** `windowOpen` is checked in `runStep`, i.e. *between operations*.
And `files: all` is **not** expanded into one operation per file — there is no `Shrink` case in
`internal/ddl/expand.go`; expansion happens at run time inside `ShrinkRunner.resolveFiles`. So one
shrink operation is every data file in the database, sequentially, with no window re-check until the
whole set finishes. `docs/specs/SHRINK.md:198-202` still claims `files: all` expands to one operation
per file, which is what makes this invisible to anyone reasoning from the spec.

**Smallest fix (code):** check the window (and the drain flag, which *is* already checked) between
files inside the `resolveFiles` loop, and add an operation-level wall-clock ceiling that stops via
the existing "work preserved" outcome. Correct the stale spec.

---

## 14. `--auto` runs unreviewed generated maintenance across every database

**Severity:** SEVERE — unattended heavy maintenance, instance-wide
**Location:** `cmd/sqlgopace/main.go:71-76`, shipped `maintenance_profile.yaml`
**Who:** opt-in by flag, but documented as a normal mode (`docs/maintenance-planner.md:77-82`).

```
-auto            analyze the database and run generated maintenance, unattended (no review)
-all-databases   with --auto: maintain every eligible user database
```

The shipped profile enables `index`, `compression`, `heap`, `statistics` and `checkdb`, with
`statistics.sample: fullscan`, `checkdb.physical_only: false`, and `scope.databases.include: ["*"]`.
So `sqlgopace --auto --all-databases` on a scaffolded install will, with no human between analysis
and execution:

- rebuild every index over 30% fragmented up to 50 GB — **offline, holding `Sch-M`, on Standard**
  (see finding 10);
- apply PAGE/ROW compression wherever the gain heuristic fires, which is another full rebuild of the
  same objects;
- `UPDATE STATISTICS … WITH FULLSCAN` — a full scan of every qualifying table;
- run `DBCC CHECKDB` with no `PHYSICAL_ONLY`, on every database.

That last is the sharpest. Microsoft Learn documents that `DBCC CHECKDB` creates an internal
database snapshot as sparse files **on the same drive as the data files**, that it *"grows in
proportion to data changes in the database"*, and that if the volume runs out *"the internal
database snapshot used by `DBCC` is marked as suspect… the `DBCC` commands produce errors and can't
complete"* (errors 17053/926, OS error 112). Combined with finding 7 — no volume free-space check
anywhere — an unattended `--auto` on a tight volume is a plausible route to filling the disk.

The blast radius is genuinely bounded to five operation kinds: `internal/maint/decide.go` can emit
only `RebuildIndex`, `RebuildHeap`, `ReorganizeIndex`, `UpdateStatistics` and `CheckDB` — **no shrink
and no batched DML**. That is a real, deliberate safety property worth keeping.

**Smallest fix (defaults + docs):** ship `checkdb.physical_only: true` (Microsoft's own
recommendation for frequent production use) and consider `checkdb.enabled: false` in the scaffolded
profile; make `--auto --all-databases` print the operation count and require `--yes`. The flag's help
text should say what it can generate, not only that there is no review.

---

## 15. The blocker killer will kill another SqlGoPace, and never re-verifies its target

**Severity:** SEVERE — kills the wrong session
**Location:** `internal/run/kill.go:303-323`, `:236→271`; `internal/mssql/conn.go:205-210`
**Who:** armed by default in the shipped config (finding 6), plus any manifest rule.

`blockerSession` returns whatever session id sits in our row's `blocking_session_id`, with **no
exclusion of our own SPID and no `program_name` self-exclusion**. `VictimKiller` has the program
guard (`internal/run/victim.go:443`) and the reasoning is written out at
`cmd/sqlgopace/main.go:515-518` — *"a stale constant would let one instance of a size-split campaign
kill another's in-flight REBUILD"* — but `BlockerKiller` never got it. Under one service login, a
`login_name` rule lets one instance kill another's rebuild (finding 3 explains how two instances
arise).

There is also **no identity re-verification between match and `KILL`**. The kill is issued from the
same DMV snapshot the decision was made on, which keeps the window short — but `Conn.Kill` is:

```go
c.pool.ExecContext(ctx, fmt.Sprintf("KILL %d", spid))
```

with no `login_time` token, no confirmation the session is still blocking us, and no
`session_id > 50` invariant (the only protection is `WHERE s.is_user_process = 1`, three layers away
in `internal/mssql/dmv.go:251`). Microsoft Learn documents the hazard: *"When the connection ends,
the integer value is released and can be reassigned to a new connection"*, and warns that a repeated
`KILL` *"might stop [a new process] if… the session ID is reassigned to a new task before the new
KILL statement runs."*

The worst instance is persisted rules: `ddl.KilledSessionFor` can write `session_id: 142` into the
manifest, and `internal/run/engine.go:1125` copies kill rules into the **recovery manifest**. Such a
rule surviving into a later run targets whatever connection now holds 142. The docs warn that
`session_id` is volatile for *ignore* rules (`docs/blocking-and-kills.md:65`) and not for kill rules.

**Smallest fix (code):** (a) add the `selfProgram` prefix guard and a `blockingSPID != ddlSPID` check
to `blockerSession`; (b) capture `login_time` with the matched blocker and re-check it in
`Conn.Kill` — recovery already has `SessionIdentity` for exactly this; (c) refuse to persist a
`session_id`-only kill rule into a recovery manifest.

---

# Tier 3 — MODERATE

## 16. `README.md` claims no raw SQL is executed; four fields say otherwise

**Location:** `README.md:38`; `internal/ddl/generate.go:227,236,110`; `SECURITY.md:53-57`
**Severity:** MODERATE — misleads the operator about the one control that matters

> "Sixteen operation types, from `rebuild_index` to `batch_delete`. **No raw SQL is ever accepted or
> executed**, which is what makes the rest of this list possible."

That sentence names `batch_delete` while `set_raw` and `where_raw` are interpolated verbatim and
executed as a plain SQL batch (`internal/mssql/conn.go:191`, no args). `SECURITY.md` states this
honestly and says *"two fields are the reason"*. It is at least **four**: `type:` on `add_column` and
`alter_column` (`generate.go:227,236`) is presence-checked only (`manifest.go:703,723` via
`requireFields`) and pasted straight into the DDL, and `data_compression:` (`generate.go:110`) has no
validation at all — nothing restricts it to `NONE|ROW|PAGE`.

Practical consequence: the security posture rests entirely on restricting write access to
`01.to_run/`, and the README tells the reader that is unnecessary.

**Smallest fix (docs):** delete or correct the README sentence, and update `SECURITY.md`'s "two
fields" to name all four. A validating allow-list for `data_compression` is a cheap code follow-up.

## 17. `alter_column` silently defaults to `NOT NULL`, and narrowing conversions round

**Location:** `internal/ddl/manifest.go:692-693`, `internal/ddl/generate.go:221-228`
**Severity:** MODERATE — silent schema and data change

`Nullable bool` is not a pointer, so an omitted `nullable:` is `false` → `NOT NULL`.
`docs/operations.md:141` shows `nullable: true  # optional` without saying what omitting it means.
An operator widening `NVARCHAR(200)` → `NVARCHAR(400)` and not thinking about nullability emits
`ALTER TABLE … ALTER COLUMN [Notes] NVARCHAR(400) NOT NULL`, which either fails (if NULLs exist) or
**succeeds and permanently changes the column's contract**.

Separately, `alter_column` is the only column operation with **no existence guard** — `add_column`,
`drop_column`, `drop_index` and `drop_constraint` all get an `IF …` wrapper (`generate.go:237-273`);
`alter_column` does not. And a narrowing numeric or temporal conversion (`DECIMAL(18,4)` →
`DECIMAL(18,2)`, `DATETIME2(7)` → `DATETIME2(0)`) succeeds while silently rounding stored values.
That is SQL Server's conversion behaviour rather than a bug here, but the tool offers no warning and
`--dry-run` will not reveal it.

**Smallest fix (code + docs):** make `Nullable *bool` and require it explicitly for `alter_column`;
document the rounding hazard on the `alter_column` page.

## 18. `--dry-run` is the designated review mechanism but does not render what runs

**Location:** `internal/ddl/generate.go:309-323`, `internal/ddl/batch.go:200-203`
**Severity:** MODERATE

`README.md:28` — *"`--dry-run --explain` renders exactly what would execute"* — and `SECURITY.md:63`
calls it *"the intended way to review one"*. For a shrink it renders a single commented placeholder:

```
-- shrink is built at run time per chunk; representative statement:
DBCC SHRINKFILE (N'all', <target_mb>) WITH ...;
```

No `TRUNCATEONLY` phase, no per-file expansion, no target sizes. For a `key_range` batch it prints
the *predicate*-strategy statement, not the `SELECT MAX(k)` / un-`TOP`ped `UPDATE` that actually runs.
The three most dangerous operation types are the three the review mechanism shows least faithfully.

**Smallest fix (docs, then code):** soften the README/SECURITY claim to match `docs/operations.md`'s
honest "representative statement"; longer term, render the real first chunk by doing the DMV reads
under `--dry-run`.

## 19. A kill never reaches the notification channels

**Location:** `docs/configuration.md:195-198`, `config.yaml:48`
**Severity:** MODERATE

The docs are candid: *"a kill never reaches these channels… Do not assume a configured webhook will
tell you about a killed maintenance job."* Combined with `kill_blockers.enabled: true` shipped
(finding 6) and `--auto` (finding 14), an unattended run can terminate production sessions and the
only record is a `.log` file nobody is watching.

**Smallest fix (code):** add `kill` to the notifiable event kinds.

## 20. The lock-escalation cap counts rows, not locks — and RCSI lifts it to 100,000

**Location:** `internal/run/batch_dml.go:383-389`, `config.yaml:85-93`, `internal/run/batch_calc.go:59-66`
**Severity:** MODERATE

`escalation_cap_rows: 4000` is justified as staying below the ~5000-lock escalation threshold, but
SQL Server counts locks per *index*: 4000 rows on a table with four nonclustered indexes is roughly
20,000 locks, and escalation fires well inside the "safe" cap. With RCSI **on**, the cap is lifted to
`max_rows: 100000` on the reasoning that readers are unaffected — true only for readers under read
committed; a table `X` lock blocks every other **writer**, plus `UPDLOCK`/`HOLDLOCK`/serializable
readers.

Compounding it, the adaptive sizer cannot react to blocking at all: `waitDeltas` never populates
`BlockingSeconds`, so both the reduce-on-blocking clause and the `BlockingSeconds == 0` growth
precondition are dead — the batch grows toward 100,000 rows blind to the harm it is causing.
`docs/specs/TODO.md:77-80` records this as known.

**Smallest fix (code):** multiply the escalation cap by the target's index count (read once at
preflight), and apply the cap regardless of RCSI. Populating `BlockingSeconds` is the real fix and is
already on the backlog.

## 21. The amplifier allow-list reaches `DBCC CHECKDB` and deployment transactions

**Location:** `internal/mssql/maintenance.go:28-38`, `internal/run/victim.go:441-455`
**Severity:** MODERATE — off by default

The built-in list is `ALTER INDEX`, `ALTER TABLE`, `CREATE INDEX`, `CREATE STATISTICS`,
`UPDATE STATISTIC`, `DROP INDEX`, `DROP TABLE`, `TRUNCATE TABLE`, `DBCC`, matched by prefix.
`BACKUP DATABASE`/`BACKUP LOG` are correctly absent. But `DBCC` catches `DBCC CHECKDB` — a
possibly-hours-long integrity check killed on the same terms as a fresh statement — and
`ALTER TABLE`/`DROP TABLE`/`TRUNCATE TABLE` are exactly what a migration or ETL script runs inside
`BEGIN TRAN`. `eligible` never consults `OpenTransactions`, even though the field is read from the
DMV and available on the session being judged. Killing statement 3 of a 12-statement deployment
transaction rolls back all twelve. The docs justify the feature as "not application work… a SQL
Agent maintenance statement", but nothing in the six conditions requires an Agent job.

**Smallest fix (code):** exclude victims with `open_transaction_count > 0` unless the operator opts
in, and split `DBCC` into the specific verbs intended.

## 22. Assorted, briefly

- **Stale key-range watermark.** `.wm` sidecars in `02.processing/` survive a failed run (only
  cleared on `runErr == nil`) and are honoured by a later manifest of the same filename. The plan
  fingerprint (`internal/run/engine.go:1553-1558`) hashes only command type and target, so changing
  the predicate does not invalidate it. **Fix:** clear the watermark on failure, or include the
  predicate in the fingerprint.
- **`batch_delete` with a time-relative predicate is not crash-idempotent.**
  `where_raw: "created_at < DATEADD(day, -30, GETDATE())"` re-evaluates on re-run, so a manifest
  crashed Monday and re-run Friday deletes four extra days. Nothing warns. **Fix:** warn at load on
  `GETDATE`/`SYSDATETIME`/`CURRENT_TIMESTAMP` in a `batch_delete` predicate.
- **`probeResumable` ignores context cancellation** (`engine.go:1297-1313`): after a hard Ctrl+C the
  probe errors instantly and loops on `time.Sleep(3s)` for the full `reconnect_timeout_minutes`, so
  "stopping now" hangs up to two minutes per interrupted operation. **Fix:** a `case <-ctx.Done()`.
- **`State.Database` records the connected database, not `manifest.Database`** (`engine.go:1467`),
  weakening the otherwise-good AG-secondary recovery guard for cross-database manifests.
- **`shrink_tempdb.targetsizemb` is validated only as `> 0`** (`manifest.go:962-967`).
  `targetsizemb: 1` drives every tempdb data file to its floor, which is a reliable way to make an
  instance slower. **Fix:** a sane minimum, or a warning below some fraction of current size.
- **The tempdb shrink samples the wrong database's log.** `cmd/sqlgopace/main.go:576-582` passes the
  user-database `conn` as the sampler probe, and `LogSpace`/`LogReuseWait` are
  `WHERE database_id = DB_ID()` (`internal/mssql/dmv.go:25,38`). The comment claiming "DMV reads are
  instance-wide" is true for `dm_exec_sessions`/`dm_exec_requests` and false for these two.
- **The matrix's `data_compression` gates are dead data.** `DataCompression` goes straight from the
  manifest into `withClause`; the matrix entry is never consulted. Harmless today (the entries are
  also wrong — compression has been available on Standard/Web/Express since 2016 SP1 per Microsoft
  Learn), but `--explain` output derived from the matrix would be misleading.
- **A failed `Claim` returns `outcomeFailed`** (`engine.go:489`) without appending to `e.failures`,
  so a benign lost race inflates the failure count and the process exit code with no detail.

---

# What the tool gets right

Worth stating, because it is unusual and should survive any fix:

- The shrink driver **cannot emit `EMPTYFILE`, `SHRINKDATABASE` or `NOTRUNCATE`** from any path, and
  a manifest carrying `emptyfile: true` is rejected at load (`internal/ddl/manifest.go:940`). It
  always runs `TRUNCATEONLY` first, never issues `BACKUP LOG`, never changes the recovery model, and
  never shrinks below used space or below the active VLFs.
- Database context is set on the **connector**, not with `USE` — the correct answer to
  `DBCC SHRINKFILE`'s documented current-database scoping.
- `shrink_tempdb` is `sysadmin`-gated and has `ABORT_AFTER_WAIT` pinned to `SELF` regardless of the
  global dangerous flag (`internal/ddl/resolve.go:241`).
- Identifier quoting is proper `QUOTENAME` semantics throughout (`internal/ddl/generate.go:56-63`);
  I found no breakout.
- Crash-orphan identification (SPID + `login_time` + a CSPRNG `CONTEXT_INFO` marker) is genuinely
  unforgeable, and adoption is passive — it never touches the server.
- `VictimKiller`'s reservation/withdraw/abandon semantics, the 3-kills-per-identity budget, and
  failed-kill-withdraws-suppression make that feature strictly non-worsening.
- `--auto` structurally cannot reach shrink or batched DML.
- Batches commit individually and the log-pressure ceiling is mandatory and validated.
- `allow_abort_blockers` is correctly `false` everywhere and labelled DANGEROUS.

---

# The two questions

## (a) Is it responsible to advertise this for production use as it stands?

**No — not in its current shipped state, and the gap is not mainly about code quality.**

This is careful, unusually well-reasoned software. The comments argue with themselves, the
recidivism budget and the withdraw-on-failed-kill semantics are better than most commercial tools
manage, and the list above is longer than most projects could write. The author has clearly thought
hard about nearly all of this.

The problem is that **the shipped defaults and the documentation disagree about what is armed, and
several guards the documentation names as protections do not protect.** A stranger calibrates their
caution from the README and `docs/`. Right now that calibration is wrong in the dangerous direction:
they are told killing is off (it is on), told no raw SQL is executed (it is), told
`confirm_full_table` guards whole-table deletes (any always-true predicate walks past it), and told
`--dry-run` shows exactly what will execute (for shrink and key-range DML it does not).

**The minimum that must change before advertising:**

1. **`kill_blockers.enabled: false`** in both `config.yaml` files (finding 6). One character, twice.
2. **Fix the `batch_update` non-termination** — the null-literal self-limit bug plus an absolute
   iteration ceiling (finding 1). This is the only finding here that silently corrupts data with no
   bound at all.
3. **Make the whole-table DELETE guard semantic rather than a key-presence check** (finding 2), or
   drop `batch_update`/`batch_delete` from the advertised feature set until it is.
4. **Add a queue lock file** taken before recovery (finding 3). A cron overlap is not exotic, and the
   failure mode is silent concurrent DDL against one object.
5. **Give `abort-resumable` a target and a confirmation** (finding 4).
6. **Fix the TUI `x`/`X` semantics** (finding 5) — at minimum a confirmation prompt, and `X` writing
   to the list that can actually fire.
7. **Report a stopped-short batch as incomplete, not done** (finding 8) — reuse the shrink path.
8. **Correct the false claims** in `README.md`, `SECURITY.md` and `docs/configuration.md`
   (findings 6, 16, 18, and the "three places" table that lists two).

Items 1, 7 and 8 are hours. Items 2, 3, 4, 5 and 6 are days, not weeks. None require redesign.

Everything else here is a legitimate "known limitation, documented honestly" — a defensible posture
for beta software, provided the documentation actually says it. The volume-space gap (finding 7) and
the Standard-edition cancel behaviour (finding 10) are the two I would want named in the README's
warning box even if they are not fixed, because an operator who knows about them can work around
them and one who does not cannot.

## (b) The shortest honest warning the README should carry

> **This tool executes destructive DDL and DML against live databases, terminates other people's
> sessions, and cannot undo any of it.** It is beta, and its production track record is one author's
> servers.
>
> Before you point it at anything you care about: take a backup you have actually restored; run one
> instance at a time (there is no lock — two concurrent runs will interfere with each other's work);
> and read `docs/blocking-and-kills.md` before arming any kill policy, because `sqlgopace init` ships
> with `kill_blockers` **enabled**.
>
> `shrink`, `batch_update` and `batch_delete` are the irreversible three. Rehearse them on a copy.
> `--dry-run --explain` shows the generated statement for ordinary DDL, but only a representative one
> for shrink and batched DML — read `docs/operations.md` for what will actually run.
>
> SqlGoPace does not check free space on the underlying disk. A rebuild, a shrink or a batch on a
> tight volume can fill it.
