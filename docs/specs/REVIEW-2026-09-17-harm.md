# Harm and vulnerability review — 0.39.0 (2026-09-17)

Scope: the tool as it stands, with the uncommitted 0.39.0 server-load work applied. The threat
model is the one this review prescribes: someone downloads a release, runs `sqlgopace init`,
edits the connection string, points it at their production server having read `README.md` but
not the source, and does not read every config key.

## What ran, and what did not

| | Result |
|---|---|
| `govulncheck ./...` | No vulnerability reachable from this code. 1 in an imported package and 18 in required modules, none called. |
| `gosec ./...` | 17 findings, all MEDIUM: 1×G201 (SQL string formatting), 7×G304 (file inclusion via variable), 5×G301 (dir 0755), 4×G306 (file 0644). Every one opened; triage below. |
| `semgrep --config auto` | **Did not run.** The rule fetch from semgrep.dev fails TLS verification on this network (intercepting proxy). Reinstalled with `--system-certs`, not re-run before this report. Treat the semgrep floor as absent, not as clean. |
| `agy` (independent reader) | **Ran twice. Refused once, then copied this repository's own earlier review.** The first run, on a prompt asking for vulnerabilities and for the input that walks past each guard, refused in 349 bytes as "unable to perform vulnerability finding, safety reviews, or identify bypasses and exploits for specific, concrete codebases" — a policy refusal on wording, not a verdict on the code. A second run on a prompt reframed to operator safety worked for fifteen minutes and produced a 55 KB report, of which **713 lines are byte-identical to `docs/specs/2026-09-01-production-harm-review.md`**, already in the tree; it says so itself ("I extracted those findings directly"). Its own contribution is the six claims triaged below. Lesson for the next panel: a clone that carries previous reviews gives a reader a cheaper path than reading code. Park those too, or demand findings that cite code the earlier review does not. |
| `codex` (independent reader) | **Ran, read the code, and wrote tests.** Fifty minutes, 244k tokens, a 69 KB report with twenty findings (F01-F20), ending on its own usage limit after the report was written. Unlike the first reader it stayed inside the clone (zero references to the main checkout; it excluded `.env*`, `*credential*` and `*connection*` from its own searches), it says explicitly that it did not re-report the defects already fixed in the tree, and it verified part of its work by writing and running tests in the clone. Two of its findings are confirmed below and promoted above everything this review had; one converges with finding 3; the remaining seventeen are listed unverified. Its full report is kept as `docs/specs/REVIEW-2026-09-17-harm-codex.md`. |

Scanner triage, since none of it reached the findings:

- **G201 `internal/mssql/load.go:108`** — `fmt.Sprintf(serverLoadSQL, recordExpr)`. The verb is
  filled from one of two file-local constants (`schedulerMonitorRecordSQL`, `nullRecordSQL`) and
  from nothing else; no manifest, config or DMV value reaches it. Not injectable.
- **G304 ×7** — each is a CLI tool opening a path its operator supplied (config, manifest,
  matrix, `.env`, state, lock). That is the program's purpose.
- **G301/G306** — real, but as a confidentiality question rather than a lint one. Finding 3.

## Findings, worst first

### 1. SEVERE — a log shrink and a `TRUNCATEONLY` pass have no yield at all unless an optional manifest key is set, and nothing the tool generates sets it

`internal/run/shrink.go:824-844`. The two shrink statements that run outside the chunk loop are
supervised by `runWatchedStatement`, which consumes samples and acts on exactly one thing:

```go
maxBlock := blockCap(res.MaxBlockMinutes)
...
case s := <-samples:
        // The poll runs for its side effects (the killers) whatever the cap says.
        if maxBlock <= 0 {
                continue
        }
```

`res.MaxBlockMinutes` comes from `options.max_block_minutes` **on the operation**, which is
optional and unset by default. When it is unset, the samples are drained for the killers' side
effects and nothing else: no blocking reaction, no log reaction. The code says so in its own
comment — "no other pressure reaction applies here" — and argues it deliberately, since an
unchunked statement has no boundary to pause at. The global `blocking_timeout_minutes: 1` that
every operator sets in `config.yaml` does not reach this path.

Who it happens to: **the default path, for the tool's own output**. `sqlgopace plan` emits
`max_block_minutes` only when the maintenance profile sets it (`shrink_plan.go:54`, and
`internal/maint/profile.go:151` documents `0 = omit`), and the shipped `maintenance_profile.yaml`
has no `shrink:` block at all. So every shrink manifest this tool generates runs its log shrink
and its `TRUNCATEONLY` pass with no mechanism that can make them yield, however long they block
the application. The operator's remaining options are a graceful stop or killing the session by
hand — both requiring someone to be watching, which is the situation the reaction hierarchy exists
to avoid.

What makes it worse than a gap: `docs/blocking-and-kills.md:107` says "Every operation is covered
from 0.30.0, including the two shrink statements that run outside the chunk loop." That is true of
the *cap*, and reads as true of *coverage*. An operator who has not set an optional per-operation
key will read that sentence as protection they have.

Smallest fix, and it is a default rather than code: give the shipped `maintenance_profile.yaml` a
`shrink.max_block_minutes` with a real value so generated manifests carry a cap, and say in
`docs/blocking-and-kills.md` that the coverage is by the cap and that an operation without the key
has no yield. The deeper fix — a global default for the cap, so an unset key does not mean "never"
— is a behaviour change worth its own decision.

*Found by codex (F07), which also proved it with a test in its clone. Verified here against
`shrink.go`, `shrink_plan.go`, `profile.go` and the shipped profile.*

### 2. SEVERE — a monitoring sample that fails or hangs silently disables the reaction path, and one stuck query takes both channels with it

`internal/run/executor.go:315-332`. `pumpSamples` is a single goroutine serving both safety
channels, and each read is guarded by `if err == nil`:

```go
case <-blockTicker.C:
        if st, err := sampler.Blocking(ctx, currentRules(ignore)); err == nil {
                cur.Blocking = st.Any
                ...
        }
```

There is no else. A sample that errors is dropped with no log line, no counter and no alert: the
runner keeps the **last** values of `cur` indefinitely, and if the last known state was "not
blocking, log under cap", the reaction hierarchy simply never fires again. The DDL continues,
supervised by a loop that has stopped seeing.

The second half is worse and is structural. Both channels are served by that one goroutine, and
`sampler.Blocking` is a synchronous DMV query on a connection the project deliberately gives **no
query timeout** — "Operation duration is governed by the monitoring loop and the reaction
hierarchy, never a fixed timer" is a stated rule of the codebase. So a blocking-poll query that
hangs (a DMV read caught behind metadata contention, a half-dead connection) stops the log poll
too, because the `for` loop never reaches that case. The two independent cadences the design
describes are not independent in failure.

Who it happens to: anyone whose monitoring read fails or stalls mid-operation — a permission
revoked during a long run, a transient DMV error, a connection the server is quietly starving.
Nothing in the run report or the console says the loop went blind.

Smallest fix, in two parts: count consecutive sample failures and surface them (a console alert
and a `.log` line at the first, a reaction at the nth — the engine already has the alarm shape for
this in `LogFullAlarm`); and give the two pollers their own goroutines so one stuck read cannot
silence the other. Whether a blind monitor should also *stop* the operation is a policy decision,
and the honest default is probably yes for the log channel, since that is the one protecting the
database rather than its neighbours.

*Found by codex (F01). Verified here against `executor.go`; the "no query timeout" rule that makes
the hang unbounded is the project's own, stated in `CLAUDE.md` and `config.yaml`.*


### 3. MODERATE — the connection string every install is handed disables certificate validation

`config.yaml:10`, `internal/scaffold/assets/config.yaml:10` (pinned byte-for-byte to the first),
`docs/configuration.md:17`, `docs/specs/SPECS.md:811`:

```
connection_string: "server=${DB_SERVER};...;password=${DB_PASSWORD};encrypt=true;trustServerCertificate=true;app name=SqlGoPace"
```

`encrypt=true` with `trustServerCertificate=true` is encryption without authentication: the
client encrypts to whatever answers, having verified nothing. Anyone on the path between the
operator's workstation and the server — the usual case being a jump host or a segment shared
with the application estate — can present a certificate, terminate the session, and read the SQL
login and password out of the login packet, then everything the tool does after it: the DDL it
issues, the DMV output it reads, the session text it captures.

Independently found by codex as F13, which is the strongest signal this report carries: two reviewers asking different questions landed on the same line.

Who it happens to: **the default path**. `sqlgopace init` writes this file; the operator edits
`${DB_SERVER}` and the `.env`, and nothing in the flow asks about certificates.

What makes it a finding rather than a preference is the surrounding text. The file's own header
says *"Secrets (passwords, etc.) are NEVER stored in plaintext here"* three lines above, which is
true of the file and false of the wire. And `docs/testing.md:90` states the project's own correct
position — "a remote server usually requires `encrypt=true`; add `trustServerCertificate=true` if
its certificate is not in your trust store" — as advice for a *throwaway test container*. The
shipped production config is the one place the project contradicts itself.

Smallest fix (a default, plus one comment): remove `trustServerCertificate=true` from
`config.yaml` and its embedded twin, and say in the comment above it that a server whose
certificate is not in the client's trust store needs the flag added back, and that adding it
means the connection is encrypted but not authenticated. Expect the change to surface as a
connection failure on self-signed instances — that is the point of it, and it is one line for the
operator to add knowingly. `internal/scaffold` pins the twin, so the two move together.

### 4. MODERATE — `plan` writes manifests into the live queue non-atomically, so a concurrent run can claim half a file

`cmd/sqlgopace/plan.go:396` writes each planned manifest with `os.WriteFile` straight into
`cfg.Directories.ToRun` (`plan.go:312`). `internal/run/queue.go` `Discover` picks up every
`*.yaml` in that directory that does not start with a dot, and `Claim` renames it into
`02.processing/`.

Scenario: a run is in progress, or a scheduled one starts, while the operator plans the next wave
— the documented workflow, since `plan` is described as writing reviewable manifests into the
queue and executing nothing. `Discover` lists a file whose write is still in flight and `Claim`
renames it away mid-write.

Two outcomes. A truncation that breaks the YAML fails the manifest loudly into `04.failed/` —
noisy, recoverable, visible. A truncation that lands on an operation boundary is **still a valid
manifest**, with fewer operations than were planned: the run executes the prefix, reports success,
and the missing operations exist nowhere except in a `plan` output nobody kept. Strict decoding
and required-field validation catch the first case, not the second.

Smallest fix (code, three lines): write to `.<name>.yaml` in the same directory and `os.Rename`
it into place. The dot prefix is already invisible to the queue — `isManifest` skips dot-files
precisely so a manifest can be parked — and a rename within one directory is atomic, so the file
appears complete or not at all. `Engine.writeRecovery` (`engine.go:1457`) has the same shape but
writes into `04.failed/`, which nothing scans, so it needs no fix.

### 5. MINOR — one run artifact was world-readable while its siblings were owner-only

**Corrected after the first draft of this review, which overstated it as MODERATE.** The claim
was that the run artifacts carry other sessions' SQL text and are written `0644`. The first half
is true of the capture sidecars and the second half is not: `capture.go:160` and
`amplifier_capture.go:161` already write `0600`, as does the scaffolded `.env.example`
(`scaffold.go:38`, with a comment explaining why). The `.log` run report was `0644`, and it
carries no statement text at all — `report.go` records counts ("peak blocked: N session(s)") and
object names, not queries. The draft read the modes of the files it could grep and attributed to
them the contents of the files it had opened.

What is left, and it is small: a run wrote one file at `0644` beside three at `0600`, all naming
the same client's databases, tables and indexes. Three files of one run under two different modes
is an accident waiting to be copied. The queue directories are `0755`, so the *file names* — which
usually carry a database name — are readable by any local account, and the SQLite history is
created by the driver at whatever mode it chooses.

Fixed here by moving the `.log` to `0600` and documenting, in `docs/running.md`, what each
artifact carries — including that the capture sidecars hold verbatim statements from someone
else's application, which is the fact an operator needs before pasting one into a ticket. The
directory modes are left alone: a `0600` file inside a `0755` directory is still unreadable, and
tightening the directories would break an operator who reads their own queue as another account.

### 6. MODERATE — the data-free-space guard covers rebuilds only, and two operations in the same space class are not checked

`preflight.go:456`: `needsSpaceCheck := th.RequireDataFreeSpace && sizedOK && !isReorg`, where
`sizedOK` comes from `SizedOperation` (`sizes.go:18`), which returns true for exactly three types:
`RebuildIndex`, `RebuildHeap`, `ReorganizeIndex` — the last excluded again as `isReorg`, correctly,
since a reorganize builds no copy. Every other operation reaches the server with no space check.

Two of them need the headroom a rebuild needs. `create_index` materializes a whole new index —
which is the check's own stated rationale, "a rebuild materializes the new index before dropping
the old one" — and is unchecked. A table-rewriting `alter_column` (a type change, a width
narrowing, `NOT NULL` against existing data) rewrites the heap or clustered index and its
nonclustered indexes, and is unchecked. On a large table either can fill the data files and then
the volume, at which point the operation fails and rolls back, having grown the log to do it.

The documentation is **not** at fault here, and that is worth stating because the reader who
raised this direction had it backwards: `docs/configuration.md:141` says "Fail an index rebuild"
and `:145` names `rebuild_index` / `rebuild_heap` explicitly. This is a coverage gap in the guard,
not a promise the code breaks. It still earns a finding, because the model an operator carries
after reading "preflight checks free space" is not "for two of the eleven operations".

Smallest fix: extend `SizedOperation` to `CreateIndex` (its size is the index being built, which
`Rewritten` already computes), and decide deliberately about `alter_column` — its rewrite is
version- and change-dependent, so the honest minimum may be a warning that names the uncertainty
rather than a computed size. Whichever is chosen, `docs/configuration.md:141` and the `config.yaml`
comment should then name the operations covered, so the next reader need not derive the list from
`sizes.go`.

*Raised by agy as "alter_column skirts the preflight"; verified and reframed here. Its claim that
the docs overpromise is false, and `create_index` — which it did not mention — is the clearer half.*

### 7. MINOR — `progress_poll_seconds` is a required key that does nothing unless `--tui` is passed

`MonitoringConfig.ProgressPoll()` (`config.go:136`) has exactly one caller in the tree:
`main.go:407`, in the `runWithTUI` argument list. Everything it paces — the completion estimate,
session waits, data and log space for the header, and since 0.39.0 the server-load line — is read
by `feedConsole`, which exists only while the console does.

`docs/configuration.md:97` lists the key as **required** and describes it as "How often to read the
operation's completion estimate, its waits, and data/log space". An operator running unattended
from cron or SQL Agent — the mode this tool exists for — must set it, and setting it changes
nothing. Nothing breaks: the reaction path samples on `blocking_poll_seconds` and the engine's log
watch runs on `log_poll_seconds`. What is lost is the operator's belief that they tuned monitoring.

This is the repository's own named defect class (`checkpoint_between_operations` shipped parsed,
documented and dead) in a subtler form, and it is exactly why `TestNoInertConfigKey` does not catch
it: that test asks whether a key is read *anywhere*. The missing question is whether it is read on
every path its documentation claims it for. **The fix worth making is that second test** rather
than this one line — a key documented without qualification and reached only through `runWithTUI`
is mechanically detectable, and the class has now appeared twice.

Smallest fix meanwhile, documentation: say in `docs/configuration.md` and in `config.yaml` that the
key paces the `--tui` console only.

*Direction raised by agy; the consumer count and the documentation comparison verified here.*

### 8. MINOR — two shipped documents disagree about whether the blocking cap covers a log shrink

`docs/manifests.md:118` still says `max_block_minutes` "yields anyway after N minutes — except on a
log shrink and a `TRUNCATEONLY` pass, which run as one unchunked statement with no supervisor to
enforce it", and then links to `docs/blocking-and-kills.md`, which says at :107 that "Every
operation is covered from 0.30.0, including the two shrink statements that run outside the chunk
loop". The second is current behaviour (`runWatchedStatement` applies the cap); the first is
pre-0.30.0 text the release did not sweep.

The polarity is the safe one — the stale page understates the protection — so what is at risk is an
operator declining to rely on a cap that works, and the credibility of a page contradicted by the
page it cites. Smallest fix: delete the exception clause from `manifests.md`.

*Raised by agy, which called the stale page "dangerously wrong". It is stale in the direction that
costs nothing, which is the opposite polarity and worth saying.*

## Unverified — the other seventeen findings from the second reader

Listed with codex's own severities and titles, none of them checked here. They are hypotheses: the
two that were checked held up, which raises the prior but settles nothing, and the reader could not
run anything against a real server because these rules of engagement forbade it. Full text in
`docs/specs/REVIEW-2026-09-17-harm-codex.md`.

SEVERE: F16 shipped resource guards do not prevent a full volume or a saturated server · F20
`TRUNCATEONLY` can remove the free-space reserve or shrink tempdb below the requested size · F09
five-minute self-wait limits do not bound a blocked statement, and cancellation can wait forever ·
F05 resume fingerprints ignore operation contents, so stale watermarks can skip new work · F08
crash recovery can replay committed, non-idempotent effects · F02 the fallback KILL identifies its
target only by a reusable SPID · F04 tempdb shrink monitors the wrong database's transaction log ·
F03 tempdb shrink can kill the live workload despite its documented prohibition · F06 queue-lock
deletion reopens the live-peer recovery race on Unix.

MODERATE: F11 the whole-table confirmation and cumulative-row cap still permit near-total committed
damage · F15 preview and preflight consume production resources before supervision exists · F12 an
abrupt crash does not record ownership of the resumable it leaves behind · F19 successful DDL can
lose its recovery state when archival fails · F14 planning and lifecycle output overwrite previous
work without a collision guard · F17 idempotent schema guards compare names, not the requested
definition · F10 documented legacy shrink knobs are unreachable through normal config loading ·
F18 raw enum-looking manifest fields grant arbitrary SQL while persisted diagnostics retain
application data.

Three of these deserve verification before anything else, on harm alone: **F02** (killing a
recycled session id is killing an innocent transaction), **F08** (replaying a committed
non-idempotent effect is data corruption), and **F03/F04** together (a tempdb shrink that watches
the wrong log and can kill the live workload). None of the three was examined here.

## Residuals — known, recorded, verified still true

Both are in `docs/specs/TODO.md:147-151` with their reasoning. Re-verified against the tree today
rather than taken on trust:

- **Disk free space is never read.** `sys.dm_os_volume_stats` appears nowhere in the repository.
  `require_data_free_space` checks room *inside the data files*; a file free to autogrow onto a
  full volume fails at the worst moment. The preflight's name reads as if the disk were checked.
- **Nothing binds a manifest to a server.** A manifest names a database (`ddl.Manifest.Database`)
  and nothing names the instance, so a queue planned against one server runs against whatever
  `config.yaml` points at when someone next runs it.

## Claims that did not survive verification

All from agy's second run. They are listed because a maintainer handed a reader's report deserves to
know which parts were tested, and because the pattern — confident, specific, and wrong about files it
did not open — is why a reader's finding is a hypothesis.

- **"Five documented defaults are absent from the shipped `config.yaml`"** (`require_data_free_space`,
  `history.enabled`, `max_retry_attempts`, `login_timeout_seconds`, `no_progress_before_flush`). Four
  of the five are in `config.yaml`. Only `no_progress_before_flush` is genuinely absent from the
  shipped file while `docs/configuration.md:287` documents it with a default of 2 — the harmless
  direction (the file is silent, the docs explain), and outside the reach of
  `TestShippedConfigStatesTheRealDefaults`, which compares only the keys the file states.
- **"`confirm_full_table` is bypassed by an always-true predicate"**. False against this code.
  `CheckBatchDMLSelectivity` counts the rows the filter spares and fails at zero; its own comment
  names `where_raw: "1=1"` as the case it was written for. This is the September review's finding 2,
  fixed since — reproducing it is a symptom of having read that document rather than the code.
- **"The README claims no raw SQL is executed, yet `where_raw` injects verbatim"**. The README
  sentence is in the contributing section, listing conventions for people adding capabilities
  ("manifest-driven rather than raw SQL"), not a guarantee to operators; `docs/operations.md:271`
  documents `where_raw` as "a raw predicate, interpolated verbatim".
- **"`--dry-run` is false for chunked operations"**. The observation is true — chunk sizes are
  computed at run time from live pressure — but no documentation claiming otherwise was produced, and
  none was found. Not verified either way; recorded rather than asserted.

## Checked and cleared

Named so the report's shape is not mistaken for absence of effort:

- **The batched-DML whole-table guard is a real guard.** `CheckBatchDMLSelectivity`
  (`preflight.go:669`) counts how many rows the filter actually spares and fails at zero;
  `where_raw: "1=1"` does not walk past it. This is the exact class this review's own guidance
  names as canonical — a confirmation that tests for a YAML key — and it has been fixed properly.
  Only the operator can set `confirm_full_table`; no planner path emits it.
- **`--dry-run` cannot execute anything.** `cli()` returns into `dryRunAll` (`main.go:105`) before
  any engine, connection or queue claim is constructed.
- **The batch-size escalation cap fails safe.** `batchBounds` (`batch_dml.go:382`) applies the
  4000-row cap when RCSI is *false*, which is also the zero value, so a failed or missing RCSI
  read errs toward the small batch that does not escalate to a table lock.
- **No password reaches a log or an error.** `mssql.open` (`conn.go:91`) wraps `msdsn.Parse`,
  `PingContext` and the connection pin, and none of the wrapped messages carry the DSN.
- **The SMTP path does not leak credentials.** `email.go:127` validates the server certificate
  (`tls.Config{ServerName: cfg.Host}`, no `InsecureSkipVerify`), and `smtp.PlainAuth` refuses to
  send credentials over an unencrypted connection, so `starttls: false` with a username fails the
  send rather than exposing it. What a plaintext relay does expose is the message body — object
  names — which is the operator's choice and documented as optional.

## What the software gets right

The reaction hierarchy is genuinely least-destructive-first, and the destructive options are off
in the shipped file: `kill_blockers.enabled: false`, `kill_amplifying_maintenance.enabled: false`,
`allow_abort_blockers: false`. The config audit tests (`internal/config/audit_test.go`) mechanize
two defect classes a diff review cannot see, and the shipped `config.yaml` is pinned byte-for-byte
against the scaffolded twin — which is why "defaults versus documentation", normally the richest
seam in a review like this, yielded nothing at the config-key level. `internal/tui/harm_audit_test.go`
does the same for keystroke gating. The 0.39.0 work reviewed alongside this adds no destructive
action and no config key.

## Is it responsible to ship 0.39.0 as it stands?

The answer changed when the second reader's findings were verified, and it is worth saying that
plainly rather than editing the earlier paragraph away. Before them this review had three MODERATE
findings and the answer was "yes, with one change". With findings 1 and 2 confirmed, it is:

**Not to a stranger, not yet** — not because 0.39.0 adds risk (it adds none; it is a read-only
console line) but because two paths that the documentation presents as supervised are not, and a
release is what puts them in front of someone who will trust the documentation.

In order:

1. **Finding 1** — give the shipped `maintenance_profile.yaml` a `shrink.max_block_minutes`, so the
   manifests this tool generates carry a cap, and correct `docs/blocking-and-kills.md:107` to say
   the coverage is by an optional key. A default and a sentence; an hour at most.
2. **Finding 2's first half** — surface a failed monitoring sample. Silence is the defect; a console
   alert and a `.log` line cost little and turn an invisible failure into a visible one. The second
   half (separate goroutines, and a policy for a blind monitor) is a design decision, not a patch.
3. **Finding 3** — remove `trustServerCertificate=true` from the shipped config and its twin. One
   line in two files, found independently by two reviewers.
4. **Finding 8** — delete the stale exception clause in `docs/manifests.md`. A minute.
5. **Findings 4, 5, 6** — as their own entries argue, none urgent.
6. **Finding 7 as a test**, when the next monitoring key is added.
7. **Before the release after this one: verify codex's F02, F08 and F03/F04.** They are unchecked,
   and if any holds it outranks everything above.

This review had no semgrep floor, and of its two independent readers one refused the task and one
was stopped by its own usage limit after writing its report. The seventeen unverified findings are
the largest known unknown in this document.

## The shortest honest warning the README should carry

> SqlGoPace runs DDL against a live server and can kill sessions. A shrink's log and `TRUNCATEONLY` phases will block your application for as long as the server takes unless the manifest sets `max_block_minutes` — the manifests it generates do not. The shipped connection string
> encrypts but does not verify the server's certificate — remove `trustServerCertificate=true`
> unless you know why you need it. Its run artifacts record the SQL text of the sessions that got
> in the way, so treat the queue directories as production data. It does not check free space on
> the disk, and a manifest names a database but never a server: it runs against whatever your
> config points at.
