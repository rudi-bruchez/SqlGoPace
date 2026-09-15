# Adversarial review: CANCEL-ONLY.md

Reviewed 2026-09-15 against `main` at 661a6a7 plus the working tree. Field evidence is read from
the two local run logs of the compression campaign the spec cites (in `04.failed/`, which is
gitignored). Operations are named here by position only.

**Verdict: revise before planning.** The retry policy in §2 goes against the spec's own field
evidence and against a living spec. §1 (the dry-run line, once its wording is fixed) and §3 (the
summary line) only add narration and can ship on their own.

---

## 1. BLOCKER — In the run the spec cites, the retry it removes completed two operations

**Claim.** "a retry restarts from zero against the same writers: it pays the cost again for the
same expected result" (l.25-27). The TODO entry it implements says: "**cannot complete**, no
matter how many times it is retried" (TODO.md:226).

**Reality.** The 33-operation manifest (SQL Server 2019 Standard, major 15, SIMPLE, ADR off,
`blocking_timeout_minutes: 1`, `max_retry_attempts: 1`). Per operation, from the `.log`:

| outcome | ops | cancels each | total duration |
|---|---|---|---|
| failed | 20 | 2 | 144–864 s |
| **success after one cancel** | **2 (ops 8, 13)** | **1** | **587 s, 716 s** |
| success, no cancel | 11 | 0 | 370–993 s |

All 44 cancels say `blocking other sessions`. Ops 8 and 13 were cancelled for blocking, retried,
and finished. The writers came and went. They were not "the same writers" for the whole run:
13 of 33 rebuilds finished on the same database in the same window.

The other log (one operation, `max_retry_attempts` then 3) shows the opposite case. There were
four blocking cancels, 59–60 minutes apart, for 12,826 s in total. None of the attempts
succeeded, and the retries cost about three hours.

So the evidence ties the retry's value to **how long the cancelled attempt ran**, not to the
kind of pressure. In the 33-operation run the fastest failures (144–174 s for *both* attempts)
mean each attempt was cancelled about as soon as `blocking_timeout` allowed. There the retry
cost roughly a minute of Sch-M plus a short rollback.

**Consequence.** Built as written, the run the spec cites ends with 22 failures instead of 20.
Two PAGE rebuilds that did land get quarantined into the recovery manifest, and each gets
another cancel exposure when that manifest is re-run.

**Fix.** Remove the futility claim, and base the rule on the evidence. One candidate (unverified,
n=2, and the report does not record when each attempt started): skip the retry when the cancelled
attempt ran longer than some multiple of `blocking_timeout`, and keep it otherwise. Whatever the
rule is, test it against this table before adopting it.

## 2. MAJOR — The harm the spec cites is left in place, and the finding that proposed its fix is not mentioned

**Claim.** The motivation is "20 operations cancelled, each after 2 to 14 minutes of work that
was then rolled back" (l.13-16).

**Reality.** Harm review finding 10 (`2026-09-01-production-harm-review.md:455-495`, SEVERE) is
about this exact path. Its proposed fix: "do not `Cancel` a non-resumable, non-cancel-safe
operation on blocking pressure alone … let `max_block_minutes` be the operator's explicit opt-in
to paying for a cancel". `TODO.md:151-152` records "giving Standard something better than cancel"
as **still open**.

CANCEL-ONLY does not change `DecideReaction` (`reaction.go:162-171`). All 20 first cancels, and
their rollbacks under Sch-M, still happen. "2 to 14 minutes" is each operation's total over
*both* attempts, so the spec removes part of that figure, not the cancel it describes.

**Consequence.** 0.34.0 reads as the Standard fix. TODO entry l.221 gets moved to *Shipped*, and
finding 10 stays open without anyone having decided against it.

**Fix.** Add a paragraph that cites finding 10, says this spec does not address the first
cancel, and says whether that is deferred or rejected, and why. Keep TODO l.221 open for the
cancel itself.

## 3. MAJOR — Reverses a living spec without superseding it

**Claim.** "Waiting for a quiet table … was considered and rejected" (l.76-78). Problem #2
presents the immediate retry as the status quo to change.

**Reality.** `SPECS.md:527-528`: "After a cancellation, we wait until there is no more blocking /
the log has dropped, then we **retry the same operation** up to `max_retry_attempts` times."
`MAINTENANCE.md:566` (`rebuild_heap`): "wait then cancel→KILL (and retry)". The code does
neither: `monitored_runner.go:68-76` retries immediately, with no wait.

The spec's own argument explains why the living rule was hollow. `BlockingOthers` counts only
sessions blocked *by our SPID* (`executor.go:364-379`). Once we cancel, it is false by
definition, so "wait until there is no more blocking" would return at once.

**Consequence.** `SPECS.md §9` and `MAINTENANCE.md` would state a retry policy that is neither
the old code nor the new design. `CLAUDE.md` requires a living spec to be superseded in place.

**Fix.** Supersede `SPECS.md:527-528` and `MAINTENANCE.md:566` in place. The note says what
changed and why the "wait for no blocking" rule was vacuous.

## 4. MAJOR — The log branch retries the case most likely to fail again, with no hysteresis

**Claim.** "transaction log over cap, alone | wait for the log to drop back under the cap … then
retry" (l.72). "Log pressure, by contrast, is observable while we are not running" (l.78-79).

**Reality.**
- Microsoft Learn, *Transaction log disk space for index operations*: "the transaction log can't
  be truncated until the index operation has completed … This is true for both offline and online
  index operations." When the operation's own log volume crossed the cap, the retry generates the
  same volume again from the same starting point.
- `waitForRelief` returns on the first sample that is not over cap (`monitored_runner.go:243-245`).
  "Over cap" is `>= log_max_percent` (`executor.go:394`). With a cap of 80, a retry can start at
  79 % and be cancelled again after about 1 % of log, paying a second rollback.
- Being able to observe relief says nothing about whether the retry can succeed.
- No field case exercises this branch: 0 of the 44 cancels (plus 4 in the other log) were for
  log pressure.

**Consequence.** An operation cancelled because it filled the log itself gets a second full
rollback. That is the same waste the spec refuses to pay for blocking. REVIEW-CANCEL-ONLY-agy #2
reaches the same conclusion.

**Fix.** Apply one rule to both kinds of pressure, whatever finding 1 settles on. If the log
retry is kept, keep it only when the pressure came from outside the operation. `Pressure`
already carries `LogReuseWait` (`reaction.go:13`), so e.g. `LOG_BACKUP` would qualify and
`ACTIVE_TRANSACTION` would not. *Unverified:* `log_reuse_wait_desc` reports the reason from the
last truncation attempt and can lag.

## 5. MAJOR — The predicate counts DDL that does almost no work as rollback-on-cancel

**Claim.** Rollback-on-cancel = run by `MonitoredRunner` ∧ not `Resumable` ∧ not `cancelSafe`
(l.32-42).

**Reality.**
- `add_column`, `drop_column`, `drop_constraint` and `drop_index` take the default branch
  (`engine.go:883-884`). `cancelSafe` excludes them (`engine.go:1751-1757`), and the matrix gives
  them no resumable option (`ddl_compatibility.yaml:50-61`).
- In the common metadata-only case, their whole cost is the wait for Sch-M. Microsoft Learn
  (*ALTER TABLE*) says that lock "must wait for all blocking transactions … During the wait time,
  the Sch-M lock blocks all other transactions that wait behind this lock". A cancel then rolls
  back essentially nothing, and a retry after the long transaction ends is cheap and likely to
  succeed.
- The spec's own Problem list (l.10) names exactly five heavy builders, the same five
  `cancelSafe`'s comment calls "heavy builders" (`engine.go:1750`).

**Consequence.**
- The dry run prints "rolls back all work" under an `ADD COLUMN … NULL`.
- A `drop_constraint` held up by one report query fails on its first cancel, where the current
  retry would probably have worked.

**Fix.** Limit the predicate to the five builders the Problem section names. Record the
exceptions as known gaps: `drop_index` on a clustered index and a non-metadata `add_column` are
size-of-data.

## 6. MAJOR — The change reaches Enterprise, but the migration note only mentions Standard

**Claim.** "An operator whose `config.yaml` sets `max_retry_attempts` to retry offline rebuilds on
Standard will see …" (l.96-98).

**Reality.**
- The predicate also covers non-resumable operations that run ONLINE on Enterprise:
  - `rebuild_heap` ONLINE and `alter_column` ONLINE, which are never resumable
    (`ddl_compatibility.yaml:46-48,72-75`);
  - `create_index` ONLINE on 2017 (resumable starts at major 15);
  - `add_constraint` before 2022;
  - any rebuild whose resumable option is turned off by override, ALL, tempdb, single partition,
    or columnstore (`resolve.go:109-142,306-317`).
- For an online operation, blocking others happens in the short S/Sch-M phases, where the DDL
  queues behind a finite open transaction. Microsoft Learn (*How online index operations work*):
  "the online index operation waits until the query has finished. Unless low priority locks are
  used, this might form a blocking chain."
- The spec's "same writers" argument (l.74-77) describes offline Sch-M held for the whole run.
  It does not describe this case.

**Consequence.** Enterprise operators lose the retry without being told, and the note they read
says the change does not concern them.

**Fix.** Apply the no-retry rule only when `!Options.Online`, which is the case the argument
actually covers. Otherwise, list every affected case in the migration note. *Reasoned, not
measured.*

## 7. MINOR — The dry-run line often states the wrong cause

**Claim.** `reaction = cancel only — not resumable on this target (<tier>, major <n>)` (l.53).

**Reality.**
- Resumable can be off for reasons unrelated to the target: a per-op or config override
  (`resolve.go:308-317`), ALL, tempdb, a single partition, or columnstore (`resolve.go:109-142`).
  Some operation types have no RESUMABLE form on any target.
- `renderPlan` (`cmd/sqlgopace/main.go:1441`) receives no `Target`, so the plumbing is missing
  too.
- Gap 1 is real, though. With no override, `pickBool` marks resumable as not relevant
  (`resolve.go:320-321`), so no decision line is written. The field `.log` lists only
  `maxdop = 2` on all 33 operations.

**Fix.**
- When a `resumable` decision exists, print its `Reason`.
- Otherwise, print "not supported by <tier> major <n>" only when the matrix has a `resumable`
  entry for the command.
- Otherwise, print "<command> has no RESUMABLE form".

## 8. MINOR — The pressure is lost in `runStatement`, not in `runLoop`

**Claim.** "`runLoop` currently returns the bare sentinel `ErrCancelled`, losing which pressure
caused it" (l.63).

**Reality.**
- `runStatement` drops the pressure: `return action, nil` (`monitored_runner.go:215`).
- `runLoop`'s closure type is `func(string) (Action, error)` (`:129`), and `runLoop` has direct
  unit tests (`executor_test.go:378,451-461`).

**Consequence.** The signature change reaches those tests, and the Testing section does not
mention it. It is a local change, not a blocker (REVIEW-CANCEL-ONLY-agy #1 overstates it).

**Fix.** Say that `runStatement` returns the `Pressure` and that the closure type becomes
`func(string) (Action, Pressure, error)`.

## 9. MINOR — A warning on every operation is noise at campaign scale

**Claim.** "At the start of a rollback-on-cancel operation the engine emits a `warn` reaction
event" (l.55-57).

**Reality.** The sink writes every event to stdout and to the report (`engine.go:769-770`). An
800-operation Standard manifest, the scenario in `REVIEW-2026-09-03-harm.md:111`, gets 800
identical lines. §3's summary line already gives the aggregate.

**Fix.** Emit one line at manifest start with the count of affected operations. Keep the
per-operation line in the dry run.

## 10. MINOR — The migration section misses part of the documentation

The spec names only `config.yaml` and its twin. These also need updating:
- `docs/configuration.md:40` ("retries once on the chance the pressure has cleared") and `:102`;
- `docs/blocking-and-kills.md:14` ("with bounded retries");
- `SPECS.md §9` and `MAINTENANCE.md:566` (finding 3);
- the `0.34.0` CHANGELOG section;
- `TODO.md:231`, which claims "a previous run's measured throughput is in the history DB". The
  spec is right that it is not (`history.go:33-42`; `TODO.md:338-345` agrees).

`Run`'s bounded retry has no unit test today: no test sets `MaxRetries`, and the only mention is
a comment at `executor_test.go:453`. The new tests are its first coverage, so plan them that way.

---

## Sound

- Gap 1 is real: Standard without overrides emits no `resumable` decision, so neither
  `--explain` nor the `.log` says so.
- Excluding the shrink, tempdb-shrink and batch-DML drivers matches the routing
  (`engine.go:844-882`).
- Wrapping `ErrCancelled` is safe: no caller compares it by equality or by string.
- Rejecting "wait for a quiet table" holds on its own terms (see finding 3).
- Predicting a timeout overrun is correctly out of scope: the history DB has no per-operation
  rows.

## Shippable independently

- §1 dry-run line, with the finding 7 wording.
- §3 report summary line.

Neither changes behaviour or needs a migration note. Hold §2 until findings 1–6 are settled.
