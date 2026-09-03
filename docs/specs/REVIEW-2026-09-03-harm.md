# Adversarial harm review — 2026-09-03, at v0.33.0

A historical record of a review, not a statement of current behaviour. It is not updated as
items are fixed.

## Scope and why it is narrow

A full production-harm review of this tool was run on 2026-09-01
([2026-09-01-production-harm-review.md](2026-09-01-production-harm-review.md), 22 findings,
four CATASTROPHIC). Its eight gating items shipped across 0.19.0–0.25.0, and the backlog
records what each left undone. Re-finding those wastes the review, so this pass targets:

- everything shipped **since** that review: 0.26.0 through 0.33.0;
- **0.33.0 in particular**, which landed hours before this review and has never been read by
  anyone but its author;
- ground the earlier pass did not walk: the queue lock, the notification path, and the
  interaction between connection loss and the batched-DML watermark.

## Threat model

Someone downloads a release, runs `sqlgopace init`, edits `.env` with a connection string,
and points it at a production instance having read `README.md` and `docs/running.md` but not
the source. They schedule it. They are not watching the console at 02:00.

All identifiers below are placeholders (`PRODDB`, `dbo.MEASUREMENT`, `SQLPROD01`,
`CORP\svc_loader`), per the repository rule on client identifiers.

## Phase 1 — scanner floor

| Tool | Result |
|---|---|
| `govulncheck ./...` | **No vulnerabilities in reachable code.** 1 in an imported package and 18 in required modules, none called. |
| `gosec ./...` | 16 hits: 7×G304, 5×G301, 4×G306. All opened; see *Triaged to nothing* below. **No G201/G202** — but see the note there, gosec cannot see this program's SQL. |
| `semgrep p/golang p/security-audit` | 1 hit, `internal/report/email.go:127`, missing `MinVersion`. Noise: Go ≥1.22 defaults to TLS 1.2 and the config sets `ServerName` with no `InsecureSkipVerify`, so verification is on. |

Semgrep's `--config auto` cannot run here (it requires metrics on) and the registry fetch
fails behind the corporate TLS interception; it was made to work by exporting the Windows
root store to a PEM and setting `REQUESTS_CA_BUNDLE`. CodeQL was not run.

### Triaged to nothing

- **G304 ×7** (`config.go:393`, `manifest.go:450`, `matrix.go:144`, `dotenv.go:60`,
  `profile.go:255`, `lock.go:55`, `state.go:68`) — a CLI opening files whose paths come from
  its own operator's config and arguments. There is no untrusted path source in this program.
- **G301 ×5 / G306 ×4** — `0o755` directories and `0o644` files. See MINOR-1: the file modes
  are inconsistent with the 0.29.0 sidecar hardening, but the content of the `0o644` files is
  schema metadata, not query text.
- **No SQL-injection rule fired, and that is not reassurance.** gosec's G201/G202 look for a
  format string reaching `db.Query`. This program builds every statement in `internal/ddl`
  and hands the finished string to `ExecDDL`, so the taint is invisible to the rule.
  `SECURITY.md` already counts five manifest fields interpolated verbatim into T-SQL; that
  surface is real and is out of this review's scope only because the earlier review took it.

---

# Findings

Ranked by expected harm to a stranger, worst first.

## 1. The 0.33.0 connection repair abandons a request the server never confirmed it stopped

**Severity:** SEVERE — the run continues concurrently with its own orphaned DDL, blocks
behind it, and cannot see or kill it
**Location:** `internal/mssql/conn.go` (`repairIfBroken`), `internal/run/monitored_runner.go:196-212`
**Who:** anyone whose operation is cancelled by the blocking reaction — the default path on
Standard edition, and the exact path the 0.33.0 fix was written for.
**New in 0.33.0.** Before it, this state ended the run immediately.

### What goes wrong

`repairIfBroken` decides the pinned connection is unusable, closes it, and pins a new one.
It does nothing about the **server-side request** that connection was running.

The driver's own source settles whether that request is still alive.
`go-mssqldb@v1.10.0/token.go:1296-1356`: on context cancellation it sends an attention, then
waits for the `DONE_ATTN` confirmation twice, `cancelDrainTimeout` each:

```go
const cancelDrainTimeout = 5 * time.Second      // token.go:118
...
// we did not get cancellation confirmation, something is not
// right, this connection is not usable anymore
return nil, cancelDrainError("second response", drainCtx2, tokErr2)
```

`cancelDrainError` returns a `StreamError`, which `checkBadConn` (`mssql.go:288-293`) turns
into `connectionGood = false` — the state that produces the `driver: bad connection` on the
next use. So the connection is abandoned **precisely when the server has not confirmed it
stopped**, after about 10 seconds. Not confirming inside 10 s is not exotic for this
workload: a session parked on a log-flush wait does not process an attention promptly, and
in the incident that motivated 0.33.0 the cancelled rebuild had spent 296 s of its 345 s in
`Transaction log` waits.

The runner's fallback `KILL` does not cover it. `runStatement` issues that KILL only when
`done` has *not* fired within `kill_grace` (30 s shipped):

```go
select {
case <-done:
    // stopped on its own (paused for a resumable op, rolled back otherwise)
case <-time.After(r.killGrace):
    ... r.exec.Kill(context.Background(), r.exec.SPID()) ...
}
```

The driver gives up at ~10 s, so `done` wins, the comment's "rolled back otherwise" is
assumed rather than established, and **no KILL is sent**.

### The scenario

`PRODDB`, Standard edition, an 800-operation compression manifest at 02:00.
`blocking_timeout_minutes: 1`, `max_retry_attempts: 1`.

1. Operation N, `ALTER INDEX [PK_MEASUREMENT] ON [dbo].[MEASUREMENT] REBUILD`, blocks a
   loader for 60 s. Cancel reaction fires, attention sent.
2. The session is in a log wait; no `DONE_ATTN` in 10 s. `ExecDDL` returns, connection
   marked bad, no KILL. **Server-side, the rebuild is still running or rolling back, holding
   its schema-modification lock.**
3. Attempt 2 re-pins onto a new session and re-issues the same `REBUILD`.
4. The new session blocks on the old one's lock. Our monitoring keys on the *new* SPID, so
   the report attributes nothing to the ghost; the console's blocker roster shows the run
   blocked by `SqlGoPace/0.33.0` under its own login and host.
5. `max_block_minutes` cancels, retries exhaust, the operation fails — and operation N+1
   starts against the same still-locked object.

The orphan is bounded by process lifetime (exit closes the socket and SQL Server rolls it
back), so this wastes a maintenance window rather than corrupting data. On `check_db` the
same path yields two concurrent `DBCC CHECKDB` on a production instance.

### Evidence it is reachable, not theoretical

The `.log` of the run that motivated 0.33.0 shows the exact prerequisites: `reaction: cancel`
at 11:26:07, `error: execute ddl: driver: bad connection` on the retry, and no
`kill` reaction anywhere in the file — the fallback never fired.

### Smallest fix (code)

In `repairIfBroken`, **before** `old.Close()` and while the old connection is still open (so
its session id cannot yet have been reassigned): issue `KILL <c.spid>` on the monitoring
pool, narrate it, then wait — bounded — for that session's row to leave
`sys.dm_exec_requests` before publishing the new connection. If it does not leave, fail the
manifest loudly rather than starting the next operation blind.

Choosing the bound is the maintainer's call: an unbounded wait hangs the run, and a rollback
can legitimately take as long as the rebuild did. **This fix is reasoned, not tested — I did
not implement or run it.** What is verified is the driver behaviour above and that nothing in
the current path kills or waits for the orphan.

---

## 2. Two schedules configured the way the docs recommend page the operator for a success

**Severity:** MODERATE — a spurious non-zero exit on every overlap, on the configuration the
documentation tells the operator to adopt
**Location:** `internal/run/engine.go:503-507`, `internal/run/queue.go:44-74`,
`docs/running.md:53-54`
**Who:** the default path for anyone who follows the advice for running two schedules.

The queue lock (0.22.0) is per **processing** directory, and `docs/running.md:53` makes an
absolute claim about what that buys:

> Two runs on *different* processing directories never interfere, whether or not they target
> the same database.

They do interfere, because the lock does not cover `01.to_run`, which both still share.
`Discover` lists that shared directory, so both runs see manifest `M`:

```go
procPath, err := e.queue.Claim(name)   // engine.go:503 — os.Rename, atomic
if err != nil {
    fmt.Fprintf(e.out, "skip %s: %v\n", name, err)
    return outcomeFailed
}
```

`Claim` is `os.Rename`, so the manifest is **not** executed twice — that part is sound, and
it is why this is MODERATE rather than a repeat of the earlier review's finding 3. What the
documentation gets wrong is "never interfere": the loser gets `ENOENT` and records
`outcomeFailed`. `docs/running.md` states that a non-zero
exit "means the run did not complete cleanly: a manifest failed" and tells the operator to
keep a watchdog on it. So the honest configuration produces, at 02:00, an alert about a
manifest that succeeded on the other instance — and no `.log` explaining it, because the
manifest never entered this run's processing directory.

Teaching an on-call operator that this alert is routine is how a real failure gets ignored.

**Smallest fix (code):** treat a lost claim as a skip, not a failure — `os.Rename` failing
with `fs.ErrNotExist` while the source is gone means a peer took it. Anything else stays a
failure.

**And documentation regardless:** `docs/running.md:53` must stop saying "never interfere".
Two runs sharing `01.to_run` interfere by construction; what the lock guarantees is that they
never *sweep* each other, which is a narrower and true statement.

---

## 3. The console names one session and its kill key ends another

**Severity:** MODERATE — invites the operator to verify the wrong session before a
destructive keystroke
**Location:** `cmd/sqlgopace/main.go:998-999` (`feedConsole`), `:1216-1219` (`ActionKillDDL`)
**Who:** anyone watching a run with `--tui` after a re-pin.
**New in 0.33.0**, and half-introduced by its own fix.

0.33.0 made the kill read the live session id, because SQL Server reuses session ids and a
captured one can name somebody else's connection. The **display** was not given the same
treatment:

```go
program.Send(tui.SPIDMsg{SPID: conn.SPID()}) // show which server session is ours
```

That is sent once, before the poll loop. After a re-pin the header still shows the id the run
started with. The operator sees "our DDL is SPID 57", cross-checks 57 in SSMS — where it now
belongs to an unrelated application, if it belongs to anything — and presses `k`, which ends
session 88. Both halves are individually defensible; together they mean the console's answer
to "what am I about to kill?" is wrong.

**Smallest fix (code):** send `tui.SPIDMsg` from inside the poll loop, so the header follows
the session the way the kill does.

---

## 4. The repair ignores the reconnect budget the operator configured

**Severity:** MODERATE — an operation is consumed per attempt during a failover instead of
waiting for it
**Location:** `internal/mssql/conn.go` (`repairTimeout`), `internal/config/config.go:155-159`
**New in 0.33.0.**

`repairIfBroken` uses a hardcoded `repairTimeout = 30 * time.Second`. The config already
carries `monitoring.reconnect_timeout_minutes` (default 2), documented as how long to wait
for the server to come back, and the DSN's own login timeout is plumbed through
`WithLoginTimeout` — neither reaches the repair.

An availability-group failover that takes 90 s therefore fails the repair three times. Each
failure is charged to a different operation, so a manifest loses three operations to a
failover the operator had configured the tool to sit through. They land in `04.failed` with
a connection error, and a re-run is needed.

**Smallest fix (code):** pass the configured reconnect timeout into the connection when it is
opened and use it here. A key that exists, is documented, and does not reach the code path it
names is the class `internal/config/audit_test.go` was built for — `TestNoInertConfigKey`
does not catch this one because the key *is* read, just not here.

---

## 5. `.heaps.yaml` and the run report kept the file mode the other sidecars shed

**Severity:** WITHDRAWN — see the correction below; already recorded, with better reasoning,
in the backlog
**Location:** `cmd/sqlgopace/shrink_plan.go:163`, `internal/report/report.go:187`,
`internal/run/engine.go:1231`, `cmd/sqlgopace/plan.go:396`

0.29.0 hardened the blocked-session, contended-tail and amplifier sidecars from `0o644` to
`0o600`, because they carry captured production query text. Four siblings were not changed:
the `.heaps.yaml` advisory, the `.log` run report, the recovery manifest and generated plan
manifests, all still `0o644`.

**Correction, made after writing the above.** I claimed the exposure was schema metadata
only — "no captured query text escapes the `0o600` files". That is wrong for the `.log`, and
`docs/specs/TODO.md` already says so under *The remaining `0644` writes*, where it corrects an
earlier reviewer who made this exact mistake: `internal/run/victim.go:534` appends
`"; source: %s (login=%s host=%s)"` to a reaction detail, which the engine stores as a
`report.ReactionLine` in the run report. A third party's program, login and host therefore do
reach a `0o644` file. `.heaps.yaml` (schema, table, size, density) is as described.

That backlog entry also argues the case I did not: these are the *operator-facing* artifacts —
a `.log` a colleague reads, a manifest a second person reviews, a queue directory a scheduler
writes into — so blanket `0o600`/`0o700` would break real workflows, and the right control is
the directory's permissions. It is a live decision about who may read the queue, not an
oversight, and it is already written down with that reasoning.

**This finding is therefore withdrawn as a finding** and left here only as a record that a
second reviewer walked into the same wrong conclusion the backlog had already corrected once.
Read `docs/specs/TODO.md` before re-raising it a third time.

---

## 6. Not re-found: finding 15 of the 2026-09-01 review is still open, and 0.33.0 widens it

**Severity:** carried over — SEVERE there, unchanged
**Location:** `internal/run/kill.go`

`BlockerKiller` still has no `program_name` self-exclusion: `AppNamePrefix` appears in
`internal/run/victim.go` and `cmd/sqlgopace/main.go` and nowhere in `kill.go`. That is
finding 15, already written up with its fix, and this review does not restate it.

What is new is the reachability. Finding 15 assumed two SqlGoPace *instances* were needed to
produce a second session carrying our identity. After 0.33.0 a **single** instance can:
the orphan of finding 1 runs under the same login, host and `program_name`, and the review's
recommended `blockingSPID != ddlSPID` guard would not exclude it either, since its id is
neither. Whoever implements finding 15's fix should exclude by program-name prefix rather
than by session id.

---

# What the software gets right

Stated briefly, because it calibrates everything above.

- **The whole-table guard is the best code in this repository.** It probes the predicate the
  statement will actually run, wraps it in `CASE` so `UNKNOWN` counts as *spared* (a plain
  `NOT (pred)` would have silently undercounted), bounds the cost with `TOP`, and — the part
  most reviewers would have got wrong — deliberately refuses to let the data-dependent
  self-limiting clause clear the verdict, because "the same manifest would fail on an
  untouched table and pass once a prior run had left rows at the target". It also **fails
  closed**: a probe that errors fails the manifest (`preflight.go:507`, `:516`) rather than
  waving the operation through. That is the single most common way a guard like this is
  wrong, and it is not wrong here.
- **The three audits are load-bearing and honest.** `TestNoInertConfigKey`,
  `TestShippedConfigStatesTheRealDefaults` and `harm_audit_test.go` mechanize exactly the
  classes that survive diff review, and `documentedDivergences` names its open defects
  instead of blessing them.
- **`govulncheck` is clean on reachable code**, the actions are pinned to SHAs, and the
  scanner floor produced nothing this review had to escalate.
- Findings 1 and 3 exist because 0.33.0 tightened the SPID discipline *at all*; the
  reasoning that produced `SessionID` is right, it was applied to four of five call sites.

# Is it responsible to ship 0.33.0 as it stands?

**Not quite — one item first.**

This needs stating plainly because it is uncomfortable: 0.33.0 is a net improvement for a
connection lost to a network blip, and a **regression for the exact case it was written
for**. Under 0.32.0 a cancelled operation whose attention went unconfirmed killed the run
immediately; the orphaned server-side request existed there too, but nothing new was started
against it and the process usually exited within seconds, closing the socket and rolling it
back. 0.33.0 keeps the orphan and starts the next operation into it. The fix repaired the
client's view of the connection without settling the server's.

Minimum before release, ordered, with rough cost:

1. **Finding 1** — KILL the abandoned session before adopting a new one, and wait, bounded,
   for its request row to clear. Half a day including a driven test; the code it modifies is
   twenty lines old. **This is the gate.**
2. **Finding 3** — move `tui.SPIDMsg` into the poll loop. Minutes. Ships with 1 or the
   console lies about what `k` kills.

Everything else can follow a release: finding 2 (small, needs a test), finding 4 (plumbing),
finding 5 (minutes), finding 6 (already scoped in the earlier review).

# The shortest honest warning the README should carry

> When SqlGoPace cancels an operation, it cannot always confirm that SQL Server stopped it.
> If that happens the run continues while the old request may still hold its locks. Before
> re-running a manifest that reports a cancelled operation, check the target instance for a
> leftover `SqlGoPace` session and end it yourself.

(That paragraph becomes unnecessary the moment finding 1 is fixed, which is the argument for
fixing it rather than documenting it.)

# What this review did not cover

Named so nobody reads its silence as a pass:

- the T-SQL generation surface — `SECURITY.md`'s five verbatim-interpolated manifest fields.
  The 2026-09-01 review took it; this one did not re-walk it.
- the maintenance planner (`internal/maint`, the `plan` subcommand) and `--auto`.
- the shrink driver beyond its interaction with the connection repair.
- the TUI beyond the SPID display and the kill key.
- CodeQL, which was not run.
