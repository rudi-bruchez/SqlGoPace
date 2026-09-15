# Adversarial harm review — SqlGoPace at v0.25.0 (9d61539)

Run 2026-09-01. Threat model: an operator downloads a release, runs `sqlgopace init`,
edits the connection string, and points it at a production instance having read
`README.md` but not the source. They will not read every config key.

Findings are appended as they are confirmed, worst first at the end.

## Phase 1 — scanner floor

| Tool | Result |
|---|---|
| `gosec` | 20 hits, all G301/G304/G306 (file permissions, variable file paths). No G201/G202. |
| `govulncheck` | 4 reachable stdlib vulnerabilities, all fixed in go1.26.6 (built with go1.26.5). |
| `semgrep --config auto` | 10 hits: 7 mutable action tags, 2 workflow shell interpolation, 1 missing TLS MinVersion. |
| CodeQL | Skipped (separate binary download; out of scope for a one-off run). |

Triage notes:

- **gosec found no SQL-injection hit in a program whose entire purpose is building SQL.**
  That is a blind spot, not a clean bill: G201/G202 only fire when a `fmt.Sprintf` result is
  passed *directly* to `db.Query`/`Exec`. SqlGoPace builds its T-SQL in `internal/ddl` as
  plain strings and executes them a package away, so the taint never crosses a rule. Phase 2
  covers the interpolation by hand.
- `missing-ssl-minversion` at `internal/report/email.go:127` is a **false positive**: Go's
  `crypto/tls` defaults client `MinVersion` to TLS 1.2 when the field is zero.
- The workflow findings are real but bounded (see MINOR section).

## Findings

### F1 — `k` kills the running DDL on one unconfirmed keystroke, while the far less harmful `x` requires confirmation

**Severity: SEVERE.** **Location:** `internal/tui/model.go:595`, dispatched at `cmd/sqlgopace/main.go:1161`.

In the incident console, `k` emits `ActionKillDDL` immediately — no prompt, no mode change,
no undo:

```go
case "k":
    m.emit(Action{Kind: ActionKillDDL})
```

which the host turns straight into `conn.Kill(ctx, ddlSPID)`. On a Standard-edition
instance — the edition this tool is aimed at, because Enterprise has `RESUMABLE` and
`WAIT_AT_LOW_PRIORITY` and needs far less of what SqlGoPace does — an `ALTER INDEX REBUILD`
is neither online nor resumable. Killing it four hours in starts a single-threaded rollback
that holds the same Sch-M lock for as long as it takes, cannot be paused, cannot be
cancelled, and discards every hour of work. That is the skill's definition of catastrophic
harm ("an outage the operator cannot stop") reached by one finger.

The asymmetry is the tell. The neighbouring key `x` kills a *foreign* session that is
merely waiting on us — strictly less damaging, since it frees the victim's own locks and
loses only that session's transaction — and it was given a confirmation prompt in v0.24.0
with this comment:

```go
// Confirm first. This kills a session that is waiting on us: it frees nothing,
// and rolls back whatever the session had open.
```

The reasoning applies with more force to `k`, and `k` was left alone. The review that
produced the `x` fix asked "which keys kill a foreign session" rather than "which keys
destroy hours of work", and `k` fell in the gap.

**Who it happens to:** anyone running `--tui`, the mode the README presents as the way to
watch an operation. No config opt-in.

**Smallest fix (code):** route `k` through a confirm mode as `x` already is, naming the
operation and warning that a non-resumable rollback cannot be stopped. The machinery exists
(`modeKillConfirm`, `handleKillConfirmKey`); it needs a second target rather than a new
mechanism.

### F2 — quitting the console does not stop the run, and says nothing

**Severity: MODERATE.** **Location:** `internal/tui/model.go:561-564` (emit), `cmd/sqlgopace/main.go:1157-1178` (dispatch), `cmd/sqlgopace/main.go:825-832`.

`q` (and `ctrl+c`, which bubbletea reads as a key in raw mode, not as a signal) emits
`ActionQuit` and returns `tea.Quit`. `dispatchActions` switches over `ActionKillDDL`,
`ActionKillBlocker`, `ActionDrain`, `ActionIgnoreBlocker`, `ActionArmKillRule`,
`ActionDisarmKillRule` — **there is no `ActionQuit` case anywhere in the tree** (grep
confirms it is emitted twice and handled never). So:

1. `program.Run()` returns nil, the alternate screen is torn down and everything the
   console had drawn disappears.
2. `runErr == nil`, so `cancelEngine()` is *not* called.
3. The process blocks on `r := <-done` while `ProcessAll` keeps running the DDL.
4. Engine narration goes to `io.Discard` in TUI mode (`main.go:270`), so there is no output.

The operator pressed quit on a production DDL and got a blank prompt with no message, no
progress, and no indication the statement is still executing. Pressing Ctrl+C then reaches
the real signal handler, whose messages are suppressed under `useTUI` (`main.go:324`, `331`)
— so the drain request is silent too, and only the *second* Ctrl+C does anything visible,
by hard-cancelling mid-DDL.

Continuing the run may well be the right default (quitting a monitor should not abort
surgery). Doing it silently is not.

**Who it happens to:** every `--tui` user who presses `q`.

**Smallest fix (code):** print one line to the real stdout after `program.Run()` returns
while the engine is still going — "console closed; the run continues, Ctrl+C to drain" —
and drop the `useTUI` guards on the two interrupt messages, which exist to protect a console
that is no longer on screen by the time they would print.

### F0 — the `key_range` batch walk emits an UPDATE with no row limit, and its own guard's error message routes the operator into the worst case

**Severity: CATASTROPHIC.** **Location:** `internal/ddl/batch.go:193-197` (`BatchKeyRangeUpdateSQL`),
`internal/run/batch_dml.go:322-348` (`resolveKeyColumn`), `internal/mssql/batch.go:37-46`
(`clusteringKeyColumnsSQL`).

Every other batching path in this tool bounds the statement by rows: the predicate strategy
emits `UPDATE TOP (n)` / `DELETE TOP (n)` (`batch.go:152-154`), the shrink driver chunks by
megabytes. The `key_range` UPDATE bounds nothing. Probed against the real generator:

```
NEXT  : SELECT MAX(k) FROM (SELECT TOP (5000) [EventId] AS k FROM [dbo].[MEASUREMENT]
        WHERE ([Archived] = 0) ORDER BY [EventId]) x;
UPDATE: UPDATE [dbo].[MEASUREMENT] SET [Archived] = 1
        WHERE [EventId] <= 42 AND ([Archived] = 0);
```

`batchSize` bounds how many keys are **scanned to choose the boundary**, not how many rows the
UPDATE touches. The two are equal only if the key is unique. Nothing checks that it is.

`clusteringKeyColumnsSQL` selects the clustered index by `i.index_id = 1` and never reads
`i.is_unique`. A single-column *non-unique* clustered index — `CREATE CLUSTERED INDEX
IX_MEASUREMENT_EventId ON dbo.MEASUREMENT(EventId)`, the ordinary shape for a log, event or
measurement table, which is precisely the sort of table anyone reaches for batched DML to
rewrite — passes every check. If the 5,000 lowest matching rows all carry `EventId = 42`
and the table holds 300M rows at that value, `MAX(k)` is 42 and the single statement updates
all 300M in one implicit transaction.

**The second hole is worse, because the tool asks for it.** The composite-key guard exists
only in the *inference* branch. Probed:

```
explicit key TenantId on composite key -> key="TenantId" err=<nil>
inferred on composite key              -> key="" err=key_range: dbo.MEASUREMENT has a
    composite clustered key; specify a single integer batch.key or use the predicate strategy
```

The operator hits the guard, reads "specify a single integer batch.key", writes
`batch: {key: TenantId}` — the leading column of a `(TenantId, Id)` clustered key, massively
duplicated by construction — and the explicit branch accepts it without ever reaching the
`len(cols) > 1` test. `MAX(k)` over the first 5,000 rows is a tenant id; `WHERE TenantId <= 3`
rewrites three entire tenants in one statement. **The guard's own remediation text is the
exploit.**

**Consequences, vendor-confirmed.** Microsoft documents lock escalation firing when "a single
Transact-SQL statement acquires at least 5,000 locks on a single reference of a table",
escalating to a table-level lock ([Transaction locking and row versioning
guide](https://learn.microsoft.com/sql/relational-databases/sql-server-transaction-locking-and-row-versioning-guide)).
So the statement takes an X TAB lock on the table and holds it for the whole update — every
reader and writer blocked, which is the exact outcome batching exists to prevent. The same
Microsoft article prescribes the remedy this tool implements *everywhere else*: "Break up
large batch operations into several smaller operations … `DELETE TOP(1000) … WHILE`".

It compounds three ways:

- **The log.** One transaction, so nothing commits until it finishes. `log_max_size_bytes`
  is sampled *between* batches; inside one there is no sampling to react to. A 50 GB cap
  cannot stop a transaction that never reaches a sampling point.
- **The reaction hierarchy inverts.** `opCaps` sets `CancelSafe: true` on the reasoning that
  "a single UPDATE/DELETE TOP commits atomically, so a stop rolls it back cleanly"
  (`batch_dml.go:352-355`). That reasoning is sound for a 1,000-row `TOP`. Applied to a
  300M-row statement, the cancel the engine believes is cheap starts a single-threaded
  rollback that holds the table lock for longer than the update did and cannot be stopped.
- **The configured protection is bypassed.** `escalation_cap_rows: 4000` ships with the
  comment "the batch ceiling applied when the database has RCSI off, to stay below the
  ~5000-lock table-escalation threshold that would otherwise freeze readers." Under
  `key_range` that cap is applied to the boundary-scan `TOP`, so the documented protection
  against escalation does not constrain the statement that escalates.

**Who it happens to:** anyone who writes `strategy: key_range` — documented as the resumable,
crash-safe strategy, i.e. the one recommended for the largest tables — against a table whose
clustered index is not unique, or who follows the composite-key error message. No flag to
opt in, no warning at plan time.

**Smallest fix (code), in order:**

1. Put a row bound on the statement. `BatchKeyRangeUpdateSQL` should emit
   `UPDATE TOP (batchSize)` and the driver should re-run the same range until it affects no
   rows before advancing the watermark. This makes the walk correct for *any* key,
   unique or not, and needs no new server round trip. **Unverified against the resume
   logic:** the watermark only advances once a range is exhausted, which appears to preserve
   the existing "boundary batch is re-applied on resume" idempotence contract, but I did not
   trace `batch_watermark.go` far enough to promise that.
2. Add `i.is_unique` to `clusteringKeyColumnsSQL` and reject a non-unique key for
   `key_range` with a message pointing at the predicate strategy. This is the cheap
   stopgap if (1) is deferred.
3. Move the `len(cols) > 1` composite test above the `op.Batch.Key != ""` branch so the
   explicit path is held to the same rule as inference — and change the inference error
   message, which currently recommends the unsafe action.

### F3 — an unquoted date in a manifest is emitted as bare T-SQL arithmetic, not as a date literal

**Severity: SEVERE.** **Location:** `internal/ddl/manifest.go:109-116` (`Literal.UnmarshalYAML`),
`internal/ddl/generate.go:325-333` (`renderLiteral`).

`renderLiteral` quotes a literal only when `l.String` is true, and `UnmarshalYAML` sets that
from `value.Tag == "!!str"`. YAML resolves an unquoted ISO date to `!!timestamp`, not `!!str`,
so it takes the unquoted branch. Probed through the real parser and generator:

```
where[0] CreatedAt >  -> Raw="2020-01-01"           String=false => [CreatedAt] > 2020-01-01
where[2] Note      =  -> Raw="2020-01-01T10:00:00Z" String=false => [Note] = 2020-01-01T10:00:00Z

STATEMENT : DELETE TOP (1000) FROM [dbo].[MEASUREMENT]
            WHERE [CreatedAt] > 2020-01-01 AND [Score] < 1.50 AND [Note] = 2020-01-01T10:00:00Z;
```

A purge manifest written the obvious way —

```yaml
- operation: batch_delete
  schema: dbo
  table: MEASUREMENT
  where: [{column: CreatedAt, op: "<", value: 2020-01-01}]
```

— does not compare against a date. It emits `[CreatedAt] < 2020-01-01`, three integers and two
minus signs.

Two outcomes, and only one of them is safe:

- **Timestamp form** (`2020-01-01T10:00:00Z`) produces a syntax error. Loud, harmless.
- **Plain date form** against a `datetime` or `smalldatetime` column is *valid T-SQL*. The
  expression evaluates as integer arithmetic (2020 − 1 − 1 = 2018) and the int is implicitly
  converted to a datetime as an offset in days from the base date. **Inference, not measured:**
  I confirmed the generated text against the real generator, and the int→`datetime` implicit
  conversion is documented behaviour, but I did not execute this against a server. Against a
  `date` or `datetime2` column it should raise an operand-type clash instead — so the silent
  case is the legacy `datetime` column, which is exactly what old tables being purged use.

The direction of the error decides the blast radius. `<` compares against roughly 1905 and
deletes almost nothing — a confusing no-op. `>` compares against roughly 1905 and matches
**every row in the table**, from a manifest whose author wrote a date filter and believes the
delete is bounded.

`CheckBatchDMLSelectivity` (added v0.21.0) is a partial backstop: it probes with the same
generated predicate, and a filter sparing zero rows fails the manifest. It does not close the
hole — a table holding any row below the bogus boundary spares rows, so the check passes and
the delete proceeds against the wrong set. And it is a coincidence, not a defence: nothing in
the parse or validate path knows a timestamp was seen.

Nothing rejects, warns about, or quotes a `!!timestamp` literal anywhere in the tree.

**Who it happens to:** anyone hand-writing a manifest with a date, which is the documented
primary workflow. The `plan` subcommand's generated manifests are unaffected. It reaches
`add_column`'s `default:` too, where the wrong value is written into a real column.

**Smallest fix (code):** treat `!!timestamp` as a string in `UnmarshalYAML` — a date literal
in T-SQL *is* a quoted string — or reject it in `Validate` with a message telling the author
to quote it. The one-line form is `l.String = value.Tag == "!!str" || value.Tag == "!!timestamp"`,
which makes the probe above emit `[CreatedAt] > N'2020-01-01'`. **Unverified:** I did not check
whether any existing test pins the current untagged rendering of a date.

### F4 — `checkpoint_between_operations` is parsed, documented, and read by nothing

**Severity: MODERATE.** **Location:** `internal/config/config.go:116`; documented at
`docs/configuration.md:100`, `config.yaml:32`, `internal/scaffold/assets/config.yaml:32`,
`docs/specs/SPECS.md:391`.

```go
CheckpointBetweenOperations bool  `yaml:"checkpoint_between_operations"`
```

That line is the field's only appearance in the tree. A grep for `Checkpoint` across every
`.go` file returns the declaration, two unrelated `CHECKPOINT;` statements hard-coded inside
the shrink driver, and nothing else. The value is parsed into the struct and never read.

Both shipped configs carry it with the comment "only has an effect under SIMPLE recovery
model", and `docs/configuration.md` states flatly: "Issue a `CHECKPOINT` between operations."
Neither is true under any recovery model.

**Who it happens to:** an operator running a long multi-operation manifest under SIMPLE
recovery who sets it to `true` — the one situation the comment invites — believing the log
will be released between operations. It is not, so the log grows across the whole manifest
until `log_max_size_bytes` stops the run or the disk fills. They are no worse off than if the
key did not exist; the harm is that the key told them a precaution was in place.

**Smallest fix:** either implement it (a `CHECKPOINT` between operations in `processOne`,
gated on the recovery model) or delete the field and all four documentation sites. Deleting
is the honest cheap option and needs a migration note, since a config setting it explicitly
would then fail to load under the tree's strict field checking.

### F5 — the TUI kills with `kill_blockers.enabled: false`, which the shipped config calls the master arm

**Severity: MODERATE.** **Location:** `internal/tui/model.go:587-594` → `cmd/sqlgopace/main.go:1162`;
claim at `internal/scaffold/assets/config.yaml:105`.

The scaffolded config says:

```yaml
kill_blockers:
  enabled: false               # master arm; kills only happen when true
```

`cfg.KillBlockers.Enabled` gates the automated `BlockerKiller` correctly (`main.go:488-505`).
It is also passed into `runWithTUI` as `killerArmed` — but tracing every use, that value
reaches only `internal/tui/view.go:401`, where it decides whether to print a warning in the
roster. It does not gate the `x` key. `handleKillConfirmKey` emits `ActionKillBlocker`
unconditionally, and `dispatchActions` runs `conn.Kill(ctx, a.SPID)`.

So with the master arm off, `x` then `y` still terminates a session. Two keystrokes, one
confirmation, no policy check.

A deliberate confirmed operator action is defensible; the config statement that "kills only
happen when true" is not. An organisation that set `enabled: false` as its control — a change
freeze, a rule that this tool may not terminate sessions on this instance — has no
enforcement, and no indication it lacks one.

**Smallest fix (documentation, or code — the maintainer's call):** either amend the config
comment and `docs/blocking-and-kills.md` to say the arm governs *automatic* kills and that the
console can always kill by hand; or honour the arm in `handleKillConfirmKey` by refusing with
a message naming the config key. The doc fix is one line and does not remove a capability an
operator in an incident may need.

### F6 — `max_block_minutes` does not apply to `shrink_log`, and only the internal docs say so

**Severity: MODERATE.** **Location:** `internal/run/shrink.go:768-800` (`runWatchedStatement`),
called from `shrinkLog` (`:589`) and `runTruncateOnly` (`:753`).

`runChunk` builds `Capabilities{CancelSafe: true, MaxBlock: blockCap(res.MaxBlockMinutes)}`
(`:708`). `runWatchedStatement` builds no `Capabilities` at all and drains its samples, with
the comment stating it plainly: "no pressure reaction applies here". So the cap is resolved
from the manifest and then discarded for the unchunked statements.

This is a known, recorded gap: `docs/specs/TODO.md:221` states it, gives the line numbers, and
explains the deferral (a log shrink is one short statement; giving `runWatchedStatement` a
supervisor touches a shared path). That reasoning is sound and I am not disputing the
deferral.

**What is a finding is where it is written down.** The exception appears in `CLAUDE.md`,
`CHANGELOG.md`, and `TODO.md` — three files an operator does not read. The operator-facing
pages state the cap without qualification:

- `docs/blocking-and-kills.md:77-79`: "after N minutes of continuous blocking it yields
  anyway, whatever the ignore rules say."
- `docs/manifests.md:117`: "`max_block_minutes` is the backstop that yields anyway after N
  minutes."
- `docs/configuration.md:261` says reactions and the cap "all apply *during* a chunk" — true,
  and silent about the statement that is not a chunk.

**Smallest fix (documentation):** one clause in `blocking-and-kills.md` and `manifests.md`
naming `shrink_log` (and the TRUNCATEONLY pass) as the exception. Cheap, and it moves an
honest internal admission to where the person relying on it will see it.

### Minor

- **`kill_amplifying_maintenance.commands` accepts a dangerously short prefix.**
  `IsAmplifyingCommand` (`internal/mssql/maintenance.go:59-77`) prefix-matches, and config
  validation rejects only entries that are *empty* after trimming. A one-character typo —
  `commands: ["S"]` — matches `SELECT` and turns a narrowly-scoped maintenance killer into
  "kill any session we block". The empty-entry trap was spotted and closed with care; the
  short-prefix sibling was not. A minimum length, or a check against known verbs, closes it.
  Requires `enabled: true` plus a hand-written list, hence minor.
- **Capture sidecars are world-readable.** `internal/run/capture.go:144` writes `0o644`
  (also `amplifier_capture.go:158`, `contended.go:158`, `engine.go:1198`, `report.go:187`).
  The contents include other sessions' login names, host names, program names and the text of
  their in-flight queries. On a shared administrative host that is readable by every local
  user. Already recorded as a SAST item in `TODO.md`.
- **Four reachable stdlib CVEs.** `govulncheck` reports GO-2026-6090, GO-2026-5972,
  GO-2026-5026 and one more, all fixed in go1.26.6; the tree builds with go1.26.5. Reached
  through `internal/report/notify.go`, `history.go` and `internal/mssql/conn.go`. A toolchain
  bump. Already in `TODO.md`.
- **`release.yml` interpolates a workflow input into a shell script** (`:31`, `:93`:
  `tag="${{ github.event.inputs.tag || github.ref_name }}"`), and both workflows pin actions
  by mutable tag. Exploitable only by someone who can already dispatch the workflow. Already
  in `TODO.md`.
- **`SECURITY.md` undercounts the verbatim-interpolated fields.** It names four; `default` on
  `add_column` is a fifth, reaching SQL through `renderLiteral` (`generate.go:234`). It is
  only dangerous via the `!!timestamp` path of F3, but the file's value is its completeness.
- **The selectivity guard can fail a safe operation.** `CheckBatchDMLSelectivity` counts rows
  spared by the *user filter*, ignoring the self-limiting clause. An idempotent
  `batch_update` whose filter is deliberately broad (`where_raw: "1=1"`, `set: {Archived: 1}`)
  touches only rows not already at the target, but is reported as a whole-table update and
  fails. The remedy the message offers is `confirm_full_table: true`, which disables the guard
  entirely — so a false positive trains operators to disarm a real protection.

## Ranking

| # | Severity | Finding |
|---|---|---|
| F0 | CATASTROPHIC | `key_range` emits an unbounded UPDATE; the composite-key guard's error message routes the operator into it |
| F1 | SEVERE | TUI `k` kills the running DDL on one unconfirmed keystroke |
| F3 | SEVERE | An unquoted date becomes bare T-SQL arithmetic, silently changing a DELETE's predicate |
| F2 | MODERATE | Quitting the console neither stops the run nor says so |
| F4 | MODERATE | `checkpoint_between_operations` is documented and read by nothing |
| F5 | MODERATE | The TUI kills with the documented master arm off |
| F6 | MODERATE | `max_block_minutes` silently excludes `shrink_log` in the operator-facing docs |

## What the software gets right

Stated briefly, because it calibrates the rest — this is careful work, and the findings above
are not the product of sloppiness.

Identifier quoting is correct everywhere, including the `]` doubling in `quoteIdent` that most
implementations miss. The one `KILL` in the tree takes an `int` and is issued on the monitoring
pool, never the execution connection. `--all-databases` eligibility excludes AG secondaries,
mirroring partners, snapshots, read-only and offline databases in a single readable predicate.
`IsAmplifyingCommand` anticipates that an empty allow-list entry is a prefix matching
everything and closes it in two places. `applyDefaults` floors the timeouts whose zero value
would be dangerous rather than merely wrong, and says why in comments. The queue lock is taken
before the recovery sweep, in the right order, and it uses an OS lock rather than a lock file
precisely so a crash releases it. `abort-resumable` now states its blast radius before acting.
Comments throughout record *why* a decision was made and what bug it fixed, which is what made
several of these findings findable at all.

## Is it responsible to ship this as it stands?

**No — not with `batch_update` / `key_range` reachable.** Everything else is shippable behind a
clear warning. Ordered by what must change first:

1. **F0, and it is not optional.** A tool whose stated purpose is to prevent a large DML from
   escalating to a table lock currently emits, on one documented code path, exactly the
   statement it exists to prevent — and its guard's error text recommends the input that
   triggers it. Cost: the stopgap (reject a non-unique or non-leading key, move the composite
   test above the explicit-key branch, fix the error message) is perhaps an hour with tests
   and closes the harm. The real fix (`UPDATE TOP (n)` with re-run-until-zero) is a half day
   and makes the walk correct for any key. Ship the stopgap now; do the real fix next.
   **Until one of them lands, `strategy: key_range` should be rejected at parse time**, which
   is a five-line change and the only option that is safe today.
2. **F3.** One line in `UnmarshalYAML` plus a test. A silent wrong predicate on a `batch_delete`
   is data loss with no error to investigate afterwards.
3. **F1.** Route `k` through the confirm mode `x` already uses. Under an hour; the machinery
   exists.
4. **F4, F5, F6** are documentation or a small default, an afternoon together. They do not
   block a release but each is a place where the docs are more reassuring than the code, and
   that is the category that costs trust once an operator discovers one.
5. **F2** and the minor items as normal maintenance.

## The shortest honest warning the README should carry

> **Beta. This tool runs `UPDATE`, `DELETE`, `DROP` and `DBCC SHRINKFILE` against production,
> and terminates other people's sessions when you arm it.**
>
> Do not use `batch: {strategy: key_range}`: it issues an UPDATE with no row limit, so on a
> table whose clustered key is not unique it can rewrite the whole table in one transaction.
> Use the default predicate strategy.
>
> Quote every date in a manifest. `value: 2020-01-01` is not a date to SQL Server — it is
> arithmetic, and it will change which rows your filter matches without any error.
>
> In the `--tui` console, `k` terminates the running DDL immediately and without confirmation.
> On Standard edition that starts a rollback you cannot stop.
>
> Run every new manifest through `--dry-run` first, and every `batch_delete` against a restored
> copy before production.
